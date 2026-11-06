package status

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// 只保存已成立的总量参考，不保存费用、请求或自行维护周期边界。
type estimateReference struct {
	Account         string  `json:"account"`
	Credential      string  `json:"credential"`
	Plan            string  `json:"plan"`
	Window          string  `json:"window"`
	Duration        int64   `json:"duration"`
	Activation      int64   `json:"activation"`
	Cycle           int64   `json:"cycle"`
	CycleStart      int64   `json:"cycle_start"`
	Observed        int64   `json:"observed"`
	Prices          string  `json:"prices"`
	Total           float64 `json:"total"`
	PricingCoverage string  `json:"pricing_coverage"`
	Blocked         bool    `json:"blocked,omitempty"`
}

type referenceFile struct {
	Schema int                          `json:"schema"`
	Scope  string                       `json:"scope"`
	Items  map[string]estimateReference `json:"items"`
}

func (e *engine) referenceKey(a rawAccount, w window) string {
	return e.opaque("estimate-reference", a.identity, quotaWindowID(w), strconv.FormatInt(w.DurationSeconds, 10))
}

func (e *engine) referenceCredential(a rawAccount) string {
	// updated_at 随普通请求及凭据刷新变化；持久参考只绑定稳定账号身份。
	return e.opaque("estimate-credential", a.identity)
}

func (e *engine) priceIdentity(prices section[map[string]map[string]any]) string {
	if prices.Stale || prices.Error != "" || len(prices.Data) == 0 {
		return ""
	}
	raw, err := json.Marshal(prices.Data)
	if err != nil {
		return ""
	}
	return e.opaque("estimate-prices", string(raw))
}

func (e *engine) loadReferences() {
	if !e.syncQuota || e.referencePath == "" {
		return
	}
	raw, err := privateFile(e.referencePath, 0)
	if err != nil {
		return
	}
	var stored referenceFile
	if strictJSON(raw, &stored) != nil || stored.Schema != 1 || stored.Scope != e.referenceScope || stored.Items == nil {
		return
	}
	for key, r := range stored.Items {
		if len(key) != 64 || len(r.Account) != 64 || len(r.Credential) != 64 || len(r.Prices) != 64 || r.Plan == "" || r.Duration <= 0 ||
			r.Activation <= 0 || r.Cycle <= 0 || r.CycleStart <= 0 || r.Observed < r.CycleStart || !nonnegative(r.Total) ||
			(r.PricingCoverage != "complete" && r.PricingCoverage != "unknown") || (r.Total == 0 && r.PricingCoverage != "complete") {
			return
		}
	}
	e.references = stored.Items
}

func (e *engine) saveReferences(files []rawAccount) {
	if !e.syncQuota {
		return
	}
	e.referenceMu.Lock()
	defer e.referenceMu.Unlock()
	active := map[string]bool{}
	for _, a := range files {
		active[a.identity] = true
	}
	for key, r := range e.references {
		if !active[r.Account] {
			delete(e.references, key)
		}
	}
	if e.referencePath == "" {
		return
	}
	parent := filepath.Dir(e.referencePath)
	info, err := os.Lstat(parent)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0022 != 0 {
		return
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || (owner.Uid != uint32(os.Geteuid()) && owner.Uid != 0) {
		return
	}
	if info, err := os.Lstat(e.referencePath); err == nil {
		if !info.Mode().IsRegular() {
			return
		}
		if _, err := privateFile(e.referencePath, 0); err != nil {
			return
		}
	} else if !os.IsNotExist(err) {
		return
	}
	raw, err := json.Marshal(referenceFile{Schema: 1, Scope: e.referenceScope, Items: e.references})
	if err != nil {
		return
	}
	f, err := os.CreateTemp(parent, ".estimate-reference-*")
	if err != nil {
		return
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return
	}
	_ = os.Rename(f.Name(), e.referencePath)
}

func (e *engine) previousReferenceAllowed(a rawAccount, q section[*quota], w window, def managerWindow, prices string) bool {
	if prices == "" || q.Data.Plan == "" {
		return false
	}
	e.referenceMu.Lock()
	defer e.referenceMu.Unlock()
	key := e.referenceKey(a, w)
	r, exists := e.references[key]
	if !exists {
		return true
	}
	if r.Credential != e.referenceCredential(a) || r.Plan != q.Data.Plan || r.Prices != prices || r.Activation != def.Current.ActivationID {
		r.Blocked = true
		e.references[key] = r
	}
	return !r.Blocked
}

func (e *engine) blockReference(a rawAccount, w window) {
	e.referenceMu.Lock()
	defer e.referenceMu.Unlock()
	key := e.referenceKey(a, w)
	if r, exists := e.references[key]; exists {
		r.Blocked = true
		e.references[key] = r
	}
}

func (e *engine) rememberEstimate(a rawAccount, q section[*quota], w window, def managerWindow, prices string) {
	x := w.CycleEstimate
	if prices == "" || q.Data.Plan == "" || x == nil || x.Status != "estimated" || x.TotalUSD == nil || (x.Basis != "quota" && x.Basis != "previous") {
		return
	}
	e.referenceMu.Lock()
	defer e.referenceMu.Unlock()
	key := e.referenceKey(a, w)
	old, exists := e.references[key]
	// 满额期间沿用既有参考，不能把反复读取或再次重置算成新的拟合样本。
	if exists && w.UsedPercent == 0 && old.Credential == e.referenceCredential(a) && old.Plan == q.Data.Plan && old.Prices == prices && old.Activation == def.Current.ActivationID {
		return
	}
	e.references[key] = estimateReference{Account: a.identity, Credential: e.referenceCredential(a), Plan: q.Data.Plan,
		Window: def.ID, Duration: w.DurationSeconds, Activation: def.Current.ActivationID, Cycle: def.Current.ID,
		CycleStart: def.Current.ActualStartMS, Observed: q.ObservedAtMS, Prices: prices, Total: *x.TotalUSD, PricingCoverage: x.PricingCoverage}
}

func (e *engine) historicalEstimate(a rawAccount, q section[*quota], w *window, def managerWindow, prices string) {
	c := def.Current
	// 仅在新鲜、满额且归属仍成立时延续总量；费用必须另由当前匹配周期读取。
	if prices == "" || w.UsedPercent != 0 || q.Data.Plan == "" || def.Availability != "active" || def.ScopeKind != "family" || def.ScopeKey != "codex_main" ||
		len(def.Models) != 0 || def.ID != quotaWindowID(*w) || def.DurationSeconds == nil || *def.DurationSeconds != w.DurationSeconds ||
		def.Mode != "fixed" || def.UsedPercent == nil || *def.UsedPercent != 0 || q.ObservedAtMS > def.ObservedAtMS ||
		c == nil || c.ActivationID <= 0 || c.ID <= 0 || (c.State != "active" && c.State != "provisional") || c.ActualEndMS != nil {
		return
	}
	e.referenceMu.Lock()
	r, exists := e.references[e.referenceKey(a, *w)]
	e.referenceMu.Unlock()
	if !exists || r.Blocked || r.Credential != e.referenceCredential(a) || r.Plan != q.Data.Plan || r.Prices != prices || r.Activation != c.ActivationID ||
		r.Window != def.ID || r.Duration != w.DurationSeconds || r.Cycle > c.ID || r.CycleStart > c.ActualStartMS || r.Observed > q.ObservedAtMS ||
		(r.Cycle == c.ID && r.CycleStart != c.ActualStartMS) {
		return
	}
	x := w.CycleEstimate
	if x == nil {
		x = unavailableEstimate(q.ObservedAtMS)
	}
	if x.SpentUSD != nil && *x.SpentUSD > r.Total {
		return
	}
	total := r.Total
	x.Status, x.TotalUSD, x.Basis, x.PricingCoverage = "estimated", &total, "history", r.PricingCoverage
	x.CycleID = e.opaque("cycle", a.identity, "codex_main", def.ID, strconv.FormatInt(c.ID, 10), strconv.FormatInt(c.ActualStartMS, 10))
	w.CycleEstimate = x
}
