package status

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const pluginID = "cpa-quota"
const resourcePath = "/v0/resource/plugins/cpa-quota/snapshot"

type managementRequest struct {
	Method, Path string
	Headers      http.Header
	Query        url.Values
	Body         []byte
}
type managementResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}
type flight struct {
	done  chan struct{}
	value *snapshot
}
type engine struct {
	client                        *managerClient
	clientHash                    string
	token                         [32]byte
	now                           func() time.Time
	mu                            sync.Mutex
	running                       *flight
	last                          *snapshot
	cacheMu                       sync.Mutex
	modelsCache                   map[string]modelCache
	priceCache                    section[map[string]map[string]any]
	syncQuota                     bool
	referenceMu                   sync.Mutex
	references                    map[string]estimateReference
	referencePath, referenceScope string
	lifecycle                     context.Context
	stop                          context.CancelFunc
}

func newEngine(c config, key string, token []byte) *engine {
	e := &engine{client: newClient(c.ManagerURL, key), clientHash: c.ClientKeySHA256, token: sha256.Sum256(token), now: time.Now, modelsCache: map[string]modelCache{}, syncQuota: c.SyncQuotaSnapshots,
		references: map[string]estimateReference{}, referencePath: c.EstimateStateFile}
	e.referenceScope = e.opaque("estimate-scope", c.ManagerURL, key, c.ClientKeySHA256)
	e.lifecycle, e.stop = context.WithCancel(context.Background())
	e.loadReferences()
	return e
}

// 热重载先结束旧采集与落盘，再让新实例读取参考，避免两个代际互相覆盖。
func (e *engine) close() {
	e.mu.Lock()
	e.stop()
	running := e.running
	e.mu.Unlock()
	if running != nil {
		<-running.done
	}
	e.client.http.CloseIdleConnections()
}

func response(status int, value any) managementResponse {
	body, err := json.Marshal(value)
	if err != nil {
		status = 500
		body = []byte(`{"error":"serialization_failed"}`)
	}
	return managementResponse{StatusCode: status, Headers: http.Header{"Content-Type": {"application/json; charset=utf-8"}, "Cache-Control": {"no-store"}, "X-Content-Type-Options": {"nosniff"}}, Body: body}
}

func (e *engine) handle(req managementRequest) managementResponse {
	// Resource 路由没有宿主认证，必须在任何网络读取或缓存命中之前验证专用令牌。
	values := []string{}
	origin := false
	for k, v := range req.Headers {
		if http.CanonicalHeaderKey(k) == "Authorization" {
			values = append(values, v...)
		}
		if http.CanonicalHeaderKey(k) == "Origin" && len(v) > 0 {
			origin = true
		}
	}
	if len(values) != 1 || len(values[0]) < 8 || len(values[0]) > 4103 || values[0][:7] != "Bearer " {
		return response(401, map[string]string{"error": "unauthorized"})
	}
	hash := sha256.Sum256([]byte(values[0][7:]))
	if subtle.ConstantTimeCompare(hash[:], e.token[:]) != 1 {
		return response(401, map[string]string{"error": "unauthorized"})
	}
	if origin {
		return response(403, map[string]string{"error": "browser_request_refused"})
	}
	if req.Method != "GET" || req.Path != resourcePath || len(req.Body) != 0 {
		return response(400, map[string]string{"error": "request_invalid"})
	}
	force := false
	for k, v := range req.Query {
		if k != "refresh" || len(v) != 1 || v[0] != "1" {
			return response(400, map[string]string{"error": "query_invalid"})
		}
		force = true
	}
	value, limited := e.get(force)
	if limited {
		return response(429, map[string]any{"error": "refresh_rate_limited", "retry_after_seconds": 5})
	}
	// 复制后再标记跨周期数据，不能篡改共享缓存或制造新的额度观测时间。
	raw, _ := json.Marshal(value)
	var out snapshot
	_ = json.Unmarshal(raw, &out)
	now := e.now().UnixMilli()
	for i := range out.Accounts.Data {
		a := &out.Accounts.Data[i]
		if a.Quota.Data != nil {
			for j := range a.Quota.Data.Windows {
				w := &a.Quota.Data.Windows[j]
				if w.ResetAtMS <= now {
					w.CycleEstimate = unavailableEstimate(a.Quota.ObservedAtMS)
					a.Quota.Stale = true
					a.Quota.Error = "quota_expired"
					out.Complete = false
				}
			}
		}
	}
	return response(200, map[string]any{"served_at_ms": now, "snapshot": out})
}

func (e *engine) get(force bool) (*snapshot, bool) {
	e.mu.Lock()
	if e.lifecycle.Err() != nil {
		e.mu.Unlock()
		return &snapshot{SchemaVersion: 2, ClientScope: e.opaque("client", e.clientHash), GeneratedAtMS: e.now().UnixMilli(), Accounts: section[[]account]{Stale: true, Error: "configuration_changed"}}, false
	}
	if e.running != nil {
		running := e.running
		e.mu.Unlock()
		<-running.done
		return running.value, false
	}
	now := e.now().UnixMilli()
	if e.last != nil {
		age := now - e.last.GeneratedAtMS
		freshAge := now - e.last.StartedAtMS
		expired := false
		for _, a := range e.last.Accounts.Data {
			if a.Quota.Data != nil {
				for _, w := range a.Quota.Data.Windows {
					if w.ResetAtMS <= now {
						expired = true
					}
				}
			}
		}
		if force && age >= 0 && age < 5000 {
			e.mu.Unlock()
			return nil, true
		}
		if age >= 0 && !force && ((e.last.Complete && !expired && freshAge >= 0 && freshAge < 60000) || age < 5000) {
			value := e.last
			e.mu.Unlock()
			return value, false
		}
	}
	running := &flight{done: make(chan struct{})}
	e.running = running
	previous := e.last
	e.mu.Unlock()
	value := func() (result *snapshot) {
		// 即使内部异常也收尾在途请求，避免后续读取永久等待。
		defer func() {
			if recover() != nil {
				result = &snapshot{SchemaVersion: 2, ClientScope: e.opaque("client", e.clientHash), GeneratedAtMS: e.now().UnixMilli(), RefreshAfterSeconds: 60, Accounts: section[[]account]{Stale: true, Error: "collection_failed"}, Cooldowns: section[[]cooldown]{Stale: true, Error: "collection_failed"}, Recent: section[*recent]{Stale: true, Error: "collection_failed"}, Prices: section[map[string]map[string]any]{Stale: true, Error: "collection_failed"}}
			}
		}()
		return e.collect(previous)
	}()
	e.mu.Lock()
	e.last = value
	running.value = value
	e.running = nil
	close(running.done)
	e.mu.Unlock()
	return value, false
}
