package status

import (
	"context"
	"math"
	"strconv"
	"sync"
)

// 金额沿 CPAMP 的实际周期区分已用与总量参考，不参与路由或刷新判断。
type cycleEstimate struct {
	Status          string   `json:"status"`
	SpentUSD        *float64 `json:"spent_usd,omitempty"`
	TotalUSD        *float64 `json:"total_usd,omitempty"`
	Basis           string   `json:"basis,omitempty"`
	PriceBasis      string   `json:"price_basis"`
	Coverage        string   `json:"coverage"`
	PricingCoverage string   `json:"pricing_coverage"`
	ObservedAt      float64  `json:"observed_at"`
	CycleID         string   `json:"cycle_id,omitempty"`
	Source          string   `json:"source"`
}

type managerCycle struct {
	ID               int64  `json:"id"`
	ActivationID     int64  `json:"activation_id"`
	State            string `json:"state"`
	ActualStartMS    int64  `json:"actual_start_ms"`
	ActualEndMS      *int64 `json:"actual_end_ms"`
	ScheduledEndMS   *int64 `json:"scheduled_end_ms"`
	DurationSeconds  *int64 `json:"duration_seconds"`
	BoundaryAccuracy string `json:"boundary_accuracy"`
	ForecastEligible bool   `json:"forecast_eligible"`
}
type managerWindow struct {
	ID           string   `json:"provider_window_id"`
	Mode         string   `json:"window_mode"`
	ScopeKind    string   `json:"model_scope_kind"`
	ScopeKey     string   `json:"model_scope_key"`
	Models       []string `json:"model_ids"`
	Stale        *bool    `json:"stale"`
	Availability string   `json:"availability"`
	ObservedAtMS int64    `json:"observed_at_ms"`
	UsedPercent  *float64 `json:"used_percent"`
	Plan         string   `json:"plan_type"`
	FieldSources map[string]struct {
		ObservedAtMS int64 `json:"observed_at_ms"`
	} `json:"field_sources"`
	EndMS            *int64        `json:"cycle_end_ms"`
	DurationSeconds  *int64        `json:"duration_seconds"`
	BoundaryAccuracy string        `json:"boundary_accuracy"`
	Current          *managerCycle `json:"current_cycle"`
	Previous         *managerCycle `json:"previous_cycle"`
}
type windowUsage struct {
	RequestKey string   `json:"request_key"`
	RowKey     string   `json:"row_key"`
	WindowID   string   `json:"provider_window_id"`
	Period     string   `json:"period"`
	FromMS     int64    `json:"from_ms"`
	ToMS       int64    `json:"to_ms"`
	Matched    *bool    `json:"matched"`
	Requests   *float64 `json:"total_requests"`
	Tokens     *float64 `json:"total_tokens"`
	Cost       *float64 `json:"total_cost"`
	LastSeenMS *int64   `json:"last_seen_ms"`
	SyncStatus string   `json:"sync_status"`
	Scope      string   `json:"scope_match_status"`
	Unmatched  *float64 `json:"unmatched_requests"`
	// 当前上游未给出此证据；缺失时不能把漏报价的零成本当成真实零。
	PricingComplete *bool `json:"pricing_complete"`
}
type estimateTarget struct {
	row, window int
	definition  managerWindow
	quota       section[*quota]
	usageWindow window
	current     map[string]any
	previous    map[string]any
}

func unavailableEstimate(observed int64) *cycleEstimate {
	return &cycleEstimate{Status: "unavailable", PriceBasis: "cpamp_price_book", Coverage: "cpamp_window", PricingCoverage: "unknown", ObservedAt: float64(observed) / 1000, Source: "cpamp"}
}

func reliableBoundary(value string) bool { return value == "exact" || value == "derived" }

func managerEstimateWindow(def managerWindow, display window, now int64) (window, int64, bool) {
	if quotaWindowID(display) == "" || (def.ID != quotaWindowID(display) && def.ID != display.Kind) ||
		def.ScopeKind != "family" || def.ScopeKey != "codex_main" || len(def.Models) != 0 ||
		(def.Mode != "fixed" && def.Mode != "calendar") || def.Stale == nil || *def.Stale || def.Availability != "active" ||
		def.ObservedAtMS <= 0 || def.ObservedAtMS > now || def.EndMS == nil || *def.EndMS <= now ||
		def.DurationSeconds == nil || *def.DurationSeconds != display.DurationSeconds || display.DurationSeconds <= 0 ||
		def.UsedPercent == nil || !nonnegative(*def.UsedPercent) || *def.UsedPercent > 100 {
		return window{}, 0, false
	}
	progressAt := def.ObservedAtMS
	if source, exists := def.FieldSources["quota"]; exists {
		progressAt = source.ObservedAtMS
	}
	if progressAt <= 0 || progressAt > def.ObservedAtMS {
		return window{}, 0, false
	}
	// 估值只使用 CPAMP 的周期与比例；副本不能改动原额度展示、恢复时间和路由判断。
	w := window{Kind: display.Kind, DurationSeconds: *def.DurationSeconds, ResetAtMS: *def.EndMS,
		UsedPercent: *def.UsedPercent, RemainingPercent: 100 - *def.UsedPercent, CycleEstimate: unavailableEstimate(progressAt)}
	return w, progressAt, true
}

func currentCycleAvailable(def managerWindow, observed, now int64) bool {
	c := def.Current
	return reliableBoundary(def.BoundaryAccuracy) && def.EndMS != nil && *def.EndMS > now &&
		c != nil && c.ID > 0 && c.ActivationID > 0 && c.State == "active" && c.ActualEndMS == nil &&
		reliableBoundary(c.BoundaryAccuracy) && c.ActualStartMS > 0 && c.ActualStartMS <= observed &&
		c.ScheduledEndMS != nil && *c.ScheduledEndMS > now &&
		c.DurationSeconds != nil && def.DurationSeconds != nil && *c.DurationSeconds == *def.DurationSeconds
}

func previousCycle(def managerWindow) *managerCycle {
	c, p := def.Current, def.Previous
	if c == nil || p == nil || p.ID <= 0 || p.ID == c.ID || p.ActivationID != c.ActivationID || p.State != "closed" ||
		!p.ForecastEligible || !reliableBoundary(p.BoundaryAccuracy) || p.ActualStartMS <= 0 || p.ActualEndMS == nil ||
		*p.ActualEndMS <= p.ActualStartMS || *p.ActualEndMS > def.ObservedAtMS || p.ActualStartMS >= c.ActualStartMS ||
		p.DurationSeconds == nil || c.DurationSeconds == nil || *p.DurationSeconds != *c.DurationSeconds {
		return nil
	}
	return p
}

func credentialTarget(a rawAccount) map[string]any {
	return map[string]any{"auth_index": a.Index, "auth_file_snapshot": a.Name, "auth_provider_snapshot": "codex",
		"auth_account_id_snapshot": a.workspace, "account_snapshot": a.member}
}

func (e *engine) usageTarget(a rawAccount, def managerWindow, period string, from, to int64) map[string]any {
	target := credentialTarget(a)
	target["row_key"], target["provider_window_id"], target["period"] = a.identity, def.ID, period
	target["request_key"] = e.opaque("cycle-usage", a.identity, def.ID, period, strconv.FormatInt(from, 10), strconv.FormatInt(to, 10))
	target["from_ms"], target["to_ms"] = from, to
	target["model_scope"] = map[string]any{"kind": "family", "key": "codex_main", "complete": true}
	return target
}

// 逐批补充只读估值，沿父级取消信号退出，不以账号总数或整轮时间丢弃尾部。
func (e *engine) cycleEstimates(parent context.Context, files []rawAccount, rows []account, prices section[map[string]map[string]any]) {
	for i := range rows {
		q := &rows[i].Quota
		if q.Data == nil {
			continue
		}
		// 失败保留的旧额度可能仍被缓存引用，不能原地修改旧快照。
		copyQuota := *q.Data
		copyQuota.Windows = append([]window(nil), q.Data.Windows...)
		q.Data = &copyQuota
		for j := range q.Data.Windows {
			q.Data.Windows[j].CycleEstimate = unavailableEstimate(q.ObservedAtMS)
		}
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var wg sync.WaitGroup
	slots := make(chan struct{}, cap(e.client.slots))
	priceIdentity := e.priceIdentity(prices)
	for start := 0; start < len(rows); start += 100 {
		end := start + 100
		if end > len(rows) {
			end = len(rows)
		}
		if ctx.Err() != nil {
			break
		}
		slots <- struct{}{}
		wg.Add(1)
		go func(start, end int) {
			defer wg.Done()
			defer func() { <-slots }()
			e.estimateBatch(ctx, files, rows, start, end, priceIdentity)
		}(start, end)
	}
	wg.Wait()
}

func (e *engine) estimateBatch(ctx context.Context, files []rawAccount, rows []account, start, end int, prices string) {
	accounts := []map[string]any{}
	indices := map[string]int{}
	for i := start; i < end; i++ {
		q := rows[i].Quota
		if q.Data == nil || q.Stale || q.Error != "" || q.Data.Scope != "codex_main" || q.ObservedAtMS <= 0 || (e.syncQuota && !rows[i].quotaSynced) {
			continue
		}
		accounts = append(accounts, map[string]any{"row_key": rows[i].ID, "provider": "codex", "account": credentialTarget(files[i])})
		indices[rows[i].ID] = i
	}
	if len(accounts) == 0 {
		return
	}
	var definitions struct {
		GeneratedAtMS int64 `json:"generated_at_ms"`
		Items         []struct {
			RowKey   string          `json:"row_key"`
			Provider string          `json:"provider"`
			Windows  []managerWindow `json:"windows"`
		} `json:"items"`
	}
	at := e.now().UnixMilli()
	body := map[string]any{"accounts": accounts, "now_ms": at, "include_inactive": false}
	if e.client.read(ctx, "/v0/management/quota-snapshots/query", body, &definitions) != "" ||
		definitions.GeneratedAtMS < at-5000 || definitions.GeneratedAtMS > e.now().UnixMilli()+5000 {
		return
	}
	counts := map[string]int{}
	for _, item := range definitions.Items {
		counts[item.RowKey]++
	}
	targets := []estimateTarget{}
	requests := []map[string]any{}
	for _, item := range definitions.Items {
		i, ok := indices[item.RowKey]
		if !ok || counts[item.RowKey] != 1 || item.Provider != "codex" {
			continue
		}
		q := rows[i].Quota
		for j, w := range q.Data.Windows {
			duplicateWindow := false
			for k, other := range q.Data.Windows {
				if j != k && other.DurationSeconds == w.DurationSeconds {
					duplicateWindow = true
				}
			}
			if duplicateWindow {
				continue
			}
			var matched *managerWindow
			ambiguous := false
			for _, def := range item.Windows {
				if def.ID == quotaWindowID(w) || def.ID == w.Kind {
					if matched != nil {
						ambiguous = true
						break
					}
					copyDef := def
					matched = &copyDef
				}
			}
			if matched == nil || ambiguous {
				continue
			}
			estimateWindow, progressAt, valid := managerEstimateWindow(*matched, w, e.now().UnixMilli())
			if !valid {
				continue
			}
			estimateQuota := q
			estimateQuota.ObservedAtMS = progressAt
			estimateData := *q.Data
			estimateData.Plan = matched.Plan
			estimateQuota.Data = &estimateData
			if !currentCycleAvailable(*matched, progressAt, e.now().UnixMilli()) || matched.Current.ActualStartMS >= progressAt {
				// 满额临时边界仍由 CPAMP 保留；只延续历史总量，绝不套旧起点计算本期费用。
				if rows[i].quotaSynced && matched.Current != nil && e.previousReferenceAllowed(files[i], estimateQuota, estimateWindow, *matched, prices) {
					e.historicalEstimate(files[i], estimateQuota, &estimateWindow, *matched, prices)
					q.Data.Windows[j].CycleEstimate = estimateWindow.CycleEstimate
				}
				continue
			}
			current := e.usageTarget(files[i], *matched, "current", matched.Current.ActualStartMS, progressAt)
			target := estimateTarget{row: i, window: j, definition: *matched, quota: estimateQuota, usageWindow: estimateWindow, current: current}
			requests = append(requests, current)
			if previous := previousCycle(*matched); previous != nil {
				target.previous = e.usageTarget(files[i], *matched, "previous", previous.ActualStartMS, *previous.ActualEndMS)
				requests = append(requests, target.previous)
			}
			targets = append(targets, target)
		}
	}
	if len(requests) == 0 {
		return
	}
	var reply struct {
		GeneratedAtMS int64         `json:"generated_at_ms"`
		Items         []windowUsage `json:"items"`
	}
	// 请求分批传输，400 仅是单次打包大小，尾部继续同一次采集。
	for start := 0; start < len(requests); start += 400 {
		end := start + 400
		if end > len(requests) {
			end = len(requests)
		}
		part := reply
		if e.client.read(ctx, "/v0/management/monitoring/account-window-usage", map[string]any{"windows": requests[start:end]}, &part) != "" ||
			part.GeneratedAtMS < at-5000 || part.GeneratedAtMS > e.now().UnixMilli()+5000 || len(part.Items) > end-start {
			return
		}
		reply.Items = append(reply.Items, part.Items...)
	}
	usage, duplicates := map[string]windowUsage{}, map[string]bool{}
	for _, item := range reply.Items {
		if _, exists := usage[item.RequestKey]; exists {
			duplicates[item.RequestKey] = true
		}
		usage[item.RequestKey] = item
	}
	for _, target := range targets {
		q := &target.quota
		w := &target.usageWindow
		if !currentCycleAvailable(target.definition, q.ObservedAtMS, e.now().UnixMilli()) {
			continue
		}
		currentKey := target.current["request_key"].(string)
		current, exists := usage[currentKey]
		if !exists || duplicates[currentKey] || !validWindowUsage(current, target.current) {
			continue
		}
		estimate := unavailableEstimate(q.ObservedAtMS)
		rows[target.row].Quota.Data.Windows[target.window].CycleEstimate = estimate
		estimate.CycleID = e.opaque("cycle", rows[target.row].ID, "codex_main", target.definition.ID,
			strconv.FormatInt(target.definition.Current.ID, 10), strconv.FormatInt(target.definition.Current.ActualStartMS, 10))
		if *current.Matched && (*current.Cost > 0 || current.PricingComplete != nil && *current.PricingComplete) {
			spent := *current.Cost
			estimate.SpentUSD = &spent
			if current.PricingComplete != nil && *current.PricingComplete {
				estimate.PricingCoverage = "complete"
			}
		}
		if current.PricingComplete != nil && !*current.PricingComplete {
			e.blockReference(files[target.row], *w)
			w.CycleEstimate = estimate
			continue
		}
		w.CycleEstimate = estimate
		if target.previous != nil {
			previousKey := target.previous["request_key"].(string)
			if previous, ok := usage[previousKey]; ok && !duplicates[previousKey] && validWindowUsage(previous, target.previous) && previous.PricingComplete != nil && !*previous.PricingComplete {
				e.blockReference(files[target.row], *w)
			}
		}
		previousAllowed := !e.syncQuota || e.previousReferenceAllowed(files[target.row], *q, *w, target.definition, prices)
		if e.syncQuota {
			e.historicalEstimate(files[target.row], *q, w, target.definition, prices)
			if w.CycleEstimate.Status == "estimated" {
				continue
			}
		}
		total, basis := 0.0, ""
		pricingCoverage := "unknown"
		if target.definition.Current.ForecastEligible && *current.Matched && priceUsable(current) && w.UsedPercent > 0 && w.UsedPercent <= 100 && (*current.Requests > 0 || *current.Tokens > 0 || *current.Cost > 0) {
			total, basis = math.Max(*current.Cost, math.Round(*current.Cost*100/w.UsedPercent*100)/100), "quota"
			if current.PricingComplete != nil {
				pricingCoverage = "complete"
			}
		} else if previousAllowed && target.definition.Current.ForecastEligible && w.UsedPercent == 0 && target.previous != nil {
			previousKey := target.previous["request_key"].(string)
			previous, exists := usage[previousKey]
			if exists && !duplicates[previousKey] && validWindowUsage(previous, target.previous) &&
				priceUsable(previous) && *previous.Matched && *previous.Cost >= *current.Cost &&
				*previous.Requests >= *current.Requests && *previous.Tokens >= *current.Tokens &&
				(*previous.Requests > 0 || *previous.Tokens > 0 || *previous.Cost > 0) {
				total, basis = *previous.Cost, "previous"
				if previous.PricingComplete != nil && (!*current.Matched || current.PricingComplete != nil && *current.PricingComplete) {
					pricingCoverage = "complete"
				}
			}
		}
		if basis != "" && nonnegative(total) {
			estimate.Status, estimate.TotalUSD, estimate.Basis, estimate.PricingCoverage = "estimated", &total, basis, pricingCoverage
		} else if w.UsedPercent == 0 {
			estimate.Status = "insufficient"
		}
		w.CycleEstimate = estimate
		if e.syncQuota {
			e.rememberEstimate(files[target.row], *q, *w, target.definition, prices)
		}
	}
}

// 缺覆盖元数据与已知缺价不同：前者只允许正成本参考，零值须有明确完整计价证据。
func priceUsable(item windowUsage) bool {
	if item.PricingComplete != nil {
		return *item.PricingComplete
	}
	return item.Cost != nil && *item.Cost > 0
}

func validWindowUsage(item windowUsage, target map[string]any) bool {
	if item.RequestKey != target["request_key"] || item.RowKey != target["row_key"] || item.WindowID != target["provider_window_id"] ||
		item.Period != target["period"] || item.FromMS != target["from_ms"] || item.ToMS != target["to_ms"] ||
		item.Matched == nil || item.Scope != "complete" || item.Unmatched == nil || *item.Unmatched != 0 {
		return false
	}
	for _, metric := range []*float64{item.Requests, item.Tokens, item.Cost} {
		if metric == nil || !nonnegative(*metric) {
			return false
		}
	}
	if *item.Requests != math.Trunc(*item.Requests) || *item.Tokens != math.Trunc(*item.Tokens) {
		return false
	}
	if *item.Matched {
		return item.SyncStatus == "ready" && item.LastSeenMS != nil && *item.LastSeenMS >= item.FromMS && *item.LastSeenMS < item.ToMS
	}
	return item.SyncStatus == "empty" && *item.Requests == 0 && *item.Tokens == 0 && *item.Cost == 0 && item.LastSeenMS == nil
}
