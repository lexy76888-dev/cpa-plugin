package status

import (
	"context"
	"encoding/json"
	"strings"
)

func (e *engine) recent(ctx context.Context) section[*recent] {
	now := e.now().UnixMilli()
	from := now - 86400000
	result := section[*recent]{Stale: true}
	body := map[string]any{"from_ms": from, "to_ms": now, "time_zone": "UTC",
		"filters": map[string]any{"api_key_hashes": []string{e.clientHash}}, "include": map[string]any{"events_page": map[string]int{"limit": 100}}}
	var reply struct {
		Events *struct {
			Items *[]struct {
				ID          string   `json:"request_id"`
				Hash        string   `json:"api_key_hash"`
				At          int64    `json:"timestamp_ms"`
				Model       string   `json:"model"`
				Thread      string   `json:"session_id"`
				Input       *int64   `json:"input_tokens"`
				Output      *int64   `json:"output_tokens"`
				Cached      *int64   `json:"cached_tokens"`
				CacheRead   int64    `json:"cache_read_tokens"`
				Creation    int64    `json:"cache_creation_tokens"`
				ServiceTier string   `json:"service_tier"`
				Effort      string   `json:"reasoning_effort"`
				Failure     string   `json:"fail_summary"`
				ErrorCode   string   `json:"header_error_code"`
				Reasoning   *int64   `json:"reasoning_tokens"`
				Failed      *bool    `json:"failed"`
				Status      *int     `json:"fail_status_code"`
				Duration    *float64 `json:"latency_ms"`
				TTFT        *float64 `json:"ttft_ms"`
			} `json:"items"`
			More *bool `json:"has_more"`
		} `json:"events"`
		Coverage *struct {
			RawComplete *bool `json:"raw_complete"`
		} `json:"coverage"`
	}
	if code := e.client.read(ctx, "/v0/management/monitoring/analytics", body, &reply); code != "" {
		result.Error = code
		return result
	}
	if reply.Events == nil || reply.Events.Items == nil || reply.Events.More == nil || len(*reply.Events.Items) > 100 {
		result.Error = "usage_invalid"
		return result
	}
	if reply.Coverage != nil && reply.Coverage.RawComplete != nil && !*reply.Coverage.RawComplete {
		result.Error = "usage_incomplete"
		return result
	}
	data := &recent{FromMS: from, ThroughMS: now, Truncated: *reply.Events.More, Rows: []recentRequest{}}
	for _, r := range *reply.Events.Items {
		if r.Hash != e.clientHash {
			result.Error = "usage_scope_mismatch"
			return result
		}
		if r.ID == "" || !safeName.MatchString(r.Model) || r.At < from || r.At > now || r.Input == nil || r.Output == nil || r.Cached == nil || r.Reasoning == nil || r.Failed == nil {
			result.Error = "usage_invalid"
			return result
		}
		for _, v := range []int64{*r.Input, *r.Output, *r.Cached, r.CacheRead, r.Creation, *r.Reasoning} {
			if v < 0 || v > 9007199254740991 {
				result.Error = "usage_invalid"
				return result
			}
		}
		for _, v := range []*float64{r.Duration, r.TTFT} {
			if v != nil && !nonnegative(*v) {
				result.Error = "usage_invalid"
				return result
			}
		}
		row := recentRequest{ID: e.opaque("request", r.ID), AtMS: r.At, Model: r.Model, InputTokens: *r.Input, OutputTokens: *r.Output, CachedTokens: *r.Cached, CacheReadTokens: r.CacheRead, CacheCreationTokens: r.Creation, ReasoningTokens: *r.Reasoning, Failed: *r.Failed, DurationMS: r.Duration, TTFTMS: r.TTFT}
		if safeName.MatchString(r.ServiceTier) {
			row.ServiceTier = r.ServiceTier
		}
		switch r.Effort {
		case "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra":
			row.ReasoningEffort = r.Effort
		}
		if row.Failed {
			row.FailureReason = failureCategory(r.Failure, r.ErrorCode, r.Status)
		}
		if uuidPattern.MatchString(r.Thread) {
			row.ThreadID = r.Thread
		}
		if row.Failed && r.Status != nil && *r.Status >= 100 && *r.Status <= 599 {
			row.Status = r.Status
		}
		data.Rows = append(data.Rows, row)
	}
	result.Data = data
	result.ObservedAtMS = now
	result.Stale = false
	return result
}

func cleanPrice(raw map[string]json.RawMessage, tier bool) (map[string]any, bool) {
	result := map[string]any{}
	for _, k := range []string{"prompt", "completion", "cache", "cacheRead", "cacheCreation", "thresholdTokens"} {
		if v, ok := raw[k]; ok {
			var n float64
			if string(v) == "null" || json.Unmarshal(v, &n) != nil || !nonnegative(n) {
				return nil, false
			}
			result[k] = n
		}
	}
	for _, k := range []string{"promptConfigured", "completionConfigured", "cacheConfigured", "cacheReadConfigured", "cacheCreationConfigured"} {
		if v, ok := raw[k]; ok {
			var b bool
			if string(v) == "null" || json.Unmarshal(v, &b) != nil {
				return nil, false
			}
			result[k] = b
		}
	}
	for _, k := range []string{"serviceTier", "mode"} {
		if v, ok := raw[k]; ok {
			var s string
			if json.Unmarshal(v, &s) != nil || !safeName.MatchString(s) {
				return nil, false
			}
			result[k] = s
		}
	}
	for _, k := range []string{"contextTiers", "serviceTiers"} {
		if v, ok := raw[k]; ok {
			if tier {
				return nil, false
			}
			var rows []map[string]json.RawMessage
			if json.Unmarshal(v, &rows) != nil {
				return nil, false
			}
			out := []map[string]any{}
			for _, row := range rows {
				p, valid := cleanPrice(row, true)
				if !valid {
					return nil, false
				}
				out = append(out, p)
			}
			result[k] = out
		}
	}
	return result, true
}

func (e *engine) prices(ctx context.Context) section[map[string]map[string]any] {
	e.cacheMu.Lock()
	previous := e.priceCache
	e.cacheMu.Unlock()
	now := e.now().UnixMilli()
	if previous.Data != nil && now-previous.ObservedAtMS >= 0 && now-previous.ObservedAtMS < 86400000 {
		return previous
	}
	var reply struct {
		Prices map[string]map[string]json.RawMessage `json:"prices"`
	}
	code := e.client.read(ctx, "/v0/management/model-prices", nil, &reply)
	result := section[map[string]map[string]any]{Stale: true, Error: code, Data: map[string]map[string]any{}}
	if code == "" && (reply.Prices == nil) {
		result.Error = "prices_invalid"
	}
	if result.Error == "" {
		for model, raw := range reply.Prices {
			p, ok := cleanPrice(raw, false)
			if !ok || !safeName.MatchString(model) {
				result.Error = "prices_invalid"
				break
			}
			result.Data[model] = p
		}
	}
	if result.Error != "" {
		result.Data = previous.Data
		result.ObservedAtMS = previous.ObservedAtMS
		return result
	}
	result.ObservedAtMS = e.now().UnixMilli()
	result.Stale = false
	e.cacheMu.Lock()
	e.priceCache = result
	e.cacheMu.Unlock()
	return result
}

// 错误正文仅在进程内分类，不向客户端返回原文。
func failureCategory(summary, code string, status *int) string {
	value := strings.ToLower(code + " " + summary)
	for _, group := range []struct {
		reason  string
		markers []string
	}{
		{"quota_exhausted", []string{"usage_limit_reached", "usage_limit_exceeded", "insufficient_quota"}},
		{"model_unavailable", []string{"model_not_found", "model_not_available", "unsupported_model"}},
		{"no_available_account", []string{"no_available_auth", "no available auth", "no available credential", "auth_unavailable"}},
		{"cancelled", []string{"context canceled", "context cancelled", "request cancelled", "request canceled"}},
		{"timeout", []string{"timeout", "timed out"}},
		{"connection_failed", []string{"connection refused", "connection reset", "no such host", "tls handshake"}},
	} {
		for _, marker := range group.markers {
			if strings.Contains(value, marker) {
				return group.reason
			}
		}
	}
	if status != nil {
		switch *status {
		case 401:
			return "authentication_failed"
		case 403:
			return "access_denied"
		case 404:
			return "endpoint_not_found"
		case 408:
			return "timeout"
		case 429:
			return "rate_limited"
		}
		if *status >= 500 && *status <= 599 {
			return "upstream_error"
		}
	}
	return "request_failed"
}
