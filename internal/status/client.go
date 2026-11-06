package status

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"
)

type managerClient struct {
	origin, key string
	http        *http.Client
	slots       chan struct{}
}

func newClient(origin, key string) *managerClient {
	return &managerClient{origin: origin, key: key, slots: make(chan struct{}, 6), http: &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect_refused") },
		Transport: &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, TLSHandshakeTimeout: 3 * time.Second,
			MaxConnsPerHost: 6, MaxIdleConnsPerHost: 6, IdleConnTimeout: 90 * time.Second,
			ResponseHeaderTimeout: 6 * time.Second},
	}}
}

// 每次阻塞读取保留原八秒空闲容限，持续到达的数据不受总耗时限制。
type idleResponse struct{ io.ReadCloser }

func (r idleResponse) Read(p []byte) (int, error) {
	timer := time.AfterFunc(8*time.Second, func() { r.ReadCloser.Close() })
	defer timer.Stop()
	return r.ReadCloser.Read(p)
}

// 路径和方法只由采集器决定；客户端不能选择 URL、账号或历史归属。
func (c *managerClient) read(ctx context.Context, path string, body any, target any) string {
	u, err := url.Parse(path)
	if err != nil || u.IsAbs() || u.Host != "" || u.Fragment != "" {
		return "request_invalid"
	}
	allowed := map[string]bool{"/v0/management/auth-files": false, "/v0/management/auth-files/models": false,
		"/usage-service/quota-cooldowns": false, "/v0/management/model-prices": false,
		"/v0/management/monitoring/analytics": true, "/v0/management/api-call": true,
		"/v0/management/quota-snapshots/query": true, "/v0/management/monitoring/account-window-usage": true}
	post, ok := allowed[u.Path]
	if !ok || post != (body != nil) {
		return "request_invalid"
	}
	if u.Path != "/v0/management/auth-files/models" && u.RawQuery != "" {
		return "request_invalid"
	}
	return c.request(ctx, path, body, target)
}

// 唯一写入口只接收采集器生成的额度观测，不开放任意路径或客户端载荷。
func (c *managerClient) saveQuota(ctx context.Context, entries []quotaWrite, target any) string {
	return c.request(ctx, "/v0/management/quota-snapshots", map[string]any{"entries": entries}, target)
}

func (c *managerClient) request(ctx context.Context, path string, body any, target any) string {
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	case <-ctx.Done():
		return "timeout"
	}
	method := http.MethodGet
	var data []byte
	var err error
	if body != nil {
		method = http.MethodPost
		data, err = json.Marshal(body)
		if err != nil {
			return "request_invalid"
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.origin+path, bytes.NewReader(data))
	if err != nil {
		return "request_invalid"
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			return "timeout"
		}
		return "transport_error"
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			return "manager_auth_failed"
		}
		return "manager_http_error"
	}
	// JSON 解码器沿响应流读取；合法集合不因字节数被拒绝。
	decoder := json.NewDecoder(idleResponse{resp.Body})
	if decodeStreamValue(decoder, reflect.ValueOf(target).Elem()) != nil {
		return "response_invalid"
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return "response_invalid"
	}
	return ""
}

// 根集合逐项解码，避免 encoding/json 为整个顶层数组保留原始响应缓冲。
// 仅元数据留在目标结构；未知字段沿 token 流越过，不累计原始副本。
func decodeStreamValue(decoder *json.Decoder, target reflect.Value) error {
	kind := target.Type()
	for kind.Kind() == reflect.Pointer {
		kind = kind.Elem()
	}
	unmarshaler := reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()
	custom := target.Type().Implements(unmarshaler) || target.CanAddr() && target.Addr().Type().Implements(unmarshaler)
	if custom || (kind.Kind() != reflect.Struct && kind.Kind() != reflect.Map && kind.Kind() != reflect.Slice) ||
		(kind.Kind() == reflect.Slice && kind.Elem().Kind() == reflect.Uint8) ||
		(kind.Kind() == reflect.Map && kind.Key().Kind() != reflect.String) {
		return decoder.Decode(target.Addr().Interface())
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token == nil {
		target.SetZero()
		return nil
	}
	for target.Kind() == reflect.Pointer {
		if target.IsNil() {
			target.Set(reflect.New(target.Type().Elem()))
		}
		target = target.Elem()
	}
	opening := json.Delim('{')
	closing := json.Delim('}')
	if target.Kind() == reflect.Slice {
		opening = json.Delim('[')
		closing = json.Delim(']')
	}
	if token != opening {
		return errors.New("response_invalid")
	}
	switch target.Kind() {
	case reflect.Slice:
		target.Set(reflect.MakeSlice(target.Type(), 0, 0))
		for decoder.More() {
			item := reflect.New(target.Type().Elem()).Elem()
			if err := decodeStreamValue(decoder, item); err != nil {
				return err
			}
			target.Set(reflect.Append(target, item))
		}
	case reflect.Map:
		target.Set(reflect.MakeMap(target.Type()))
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok {
				return errors.New("response_invalid")
			}
			item := reflect.New(target.Type().Elem()).Elem()
			if err := decodeStreamValue(decoder, item); err != nil {
				return err
			}
			target.SetMapIndex(reflect.ValueOf(name).Convert(target.Type().Key()), item)
		}
	case reflect.Struct:
		fields := map[string]int{}
		for i := 0; i < target.NumField(); i++ {
			field := target.Type().Field(i)
			if !target.Field(i).CanSet() {
				continue
			}
			name := strings.Split(field.Tag.Get("json"), ",")[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = field.Name
			}
			fields[name] = i
			fields[strings.ToLower(name)] = i
		}
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok {
				return errors.New("response_invalid")
			}
			index, ok := fields[name]
			if !ok {
				index, ok = fields[strings.ToLower(name)]
			}
			if ok {
				if err := decodeStreamValue(decoder, target.Field(index)); err != nil {
					return err
				}
			} else if err := skipStreamValue(decoder); err != nil {
				return err
			}
		}
	}
	token, err = decoder.Token()
	if err != nil {
		return err
	}
	if token != closing {
		return errors.New("response_invalid")
	}
	return nil
}

func skipStreamValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	depth := 0
	if token == json.Delim('{') || token == json.Delim('[') {
		depth = 1
	}
	for depth > 0 {
		token, err = decoder.Token()
		if err != nil {
			return err
		}
		switch token {
		case json.Delim('{'), json.Delim('['):
			depth++
		case json.Delim('}'), json.Delim(']'):
			depth--
		}
	}
	return nil
}
