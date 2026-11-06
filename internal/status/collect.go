package status

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var safeName = regexp.MustCompile(`^[A-Za-z0-9._:/-]+$`)
var emailPattern = regexp.MustCompile(`^[A-Za-z0-9._+%-]+@[A-Za-z0-9.-]+$`)
var uuidPattern = regexp.MustCompile(`^[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}$`)

func (e *engine) opaque(kind string, values ...string) string {
	h := hmac.New(sha256.New, e.token[:])
	raw, _ := json.Marshal(append([]string{kind}, values...))
	h.Write(raw)
	return hex.EncodeToString(h.Sum(nil))
}

func (e *engine) accounts(ctx context.Context) ([]rawAccount, string) {
	var reply struct {
		Files   *[]rawAccount `json:"files"`
		HasMore bool          `json:"has_more"`
	}
	if code := e.client.read(ctx, "/v0/management/auth-files", nil, &reply); code != "" {
		return nil, code
	}
	if reply.Files == nil || reply.HasMore {
		return nil, "account_list_incomplete"
	}
	result := []rawAccount{}
	identities := map[string]bool{}
	indices := map[string]bool{}
	for _, a := range *reply.Files {
		provider := a.Provider
		if provider == "" {
			provider = a.Type
		}
		if provider != "codex" {
			continue
		}
		if a.Index == "" || a.Name == "" || a.Disabled == nil || indices[a.Index] {
			return nil, "account_identity_invalid"
		}
		members, workspaces := map[string]bool{}, map[string]bool{}
		for _, s := range []string{a.Email, a.Account, a.AccountSnapshot, a.AccountSnapshotCamel} {
			if emailPattern.MatchString(s) {
				members[strings.ToLower(s)] = true
			}
		}
		for _, s := range []string{a.Workspace, a.AccountID} {
			if s != "" {
				workspaces[s] = true
			}
		}
		for _, fields := range []map[string]json.RawMessage{a.Claims, a.Metadata} {
			for _, k := range []string{"chatgpt_account_id", "chatgptAccountId", "account_id", "accountId", "email"} {
				raw, ok := fields[k]
				if !ok || string(raw) == "null" {
					continue
				}
				var s string
				if json.Unmarshal(raw, &s) != nil {
					return nil, "account_identity_invalid"
				}
				if s == "" {
					continue
				}
				if k == "email" {
					if emailPattern.MatchString(s) {
						members[strings.ToLower(s)] = true
					}
				} else {
					workspaces[s] = true
				}
			}
		}
		if len(members) > 1 || len(workspaces) > 1 {
			return nil, "account_identity_ambiguous"
		}
		for s := range members {
			a.member = s
		}
		for s := range workspaces {
			a.workspace = s
		}
		if strings.ContainsAny(a.workspace, "\r\n") {
			return nil, "account_identity_invalid"
		}
		a.identity = e.opaque("account", a.Index, a.Name, a.member, a.workspace)
		a.binding = e.opaque("binding", a.identity, string(a.Updated), a.Status, fmtBool(*a.Disabled), fmtBool(a.Unavailable))
		if identities[a.identity] {
			return nil, "account_identity_ambiguous"
		}
		identities[a.identity] = true
		indices[a.Index] = true
		result = append(result, a)
	}
	return result, ""
}
func fmtBool(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func (e *engine) models(ctx context.Context, a rawAccount, now int64) section[[]string] {
	e.cacheMu.Lock()
	previous, ok := e.modelsCache[a.identity]
	e.cacheMu.Unlock()
	if ok && previous.Binding == a.binding && now-previous.Value.ObservedAtMS >= 0 && now-previous.Value.ObservedAtMS < 21600000 {
		return previous.Value
	}
	var reply struct {
		Models *[]struct {
			ID string `json:"id"`
		} `json:"models"`
	}
	code := e.client.read(ctx, "/v0/management/auth-files/models?"+url.Values{"name": {a.Name}}.Encode(), nil, &reply)
	result := section[[]string]{Data: []string{}, Stale: true, Error: code}
	if code == "" && (reply.Models == nil) {
		result.Error = "models_invalid"
	}
	if result.Error == "" {
		for _, m := range *reply.Models {
			if !safeName.MatchString(m.ID) {
				result.Error = "models_invalid"
				break
			}
			result.Data = append(result.Data, m.ID)
		}
	}
	if result.Error != "" {
		if ok && previous.Binding == a.binding {
			result.Data = previous.Value.Data
			result.ObservedAtMS = previous.Value.ObservedAtMS
		}
		return result
	}
	sort.Strings(result.Data)
	result.ObservedAtMS = e.now().UnixMilli()
	result.Stale = false
	e.cacheMu.Lock()
	e.modelsCache[a.identity] = modelCache{Binding: a.binding, Value: result}
	e.cacheMu.Unlock()
	return result
}

// 只投影路由分类需要的冷却信息，单账号格式错误不清空其余账号。
func (e *engine) runtimeCooling(a rawAccount) section[[]runtimeCooldown] {
	result := section[[]runtimeCooldown]{Stale: true, Data: []runtimeCooldown{}, Error: "routing_invalid"}
	var rows []struct {
		Scope  string          `json:"scope"`
		Model  string          `json:"model_key"`
		Reason string          `json:"reason"`
		Retry  json.RawMessage `json:"retry_at"`
	}
	if len(a.Runtime) == 0 || string(a.Runtime) == "null" || json.Unmarshal(a.Runtime, &rows) != nil {
		return result
	}
	for _, r := range rows {
		if r.Scope != "credential" && r.Scope != "model" || r.Model != "" && !safeName.MatchString(r.Model) {
			return result
		}
		var seconds float64
		var date string
		if json.Unmarshal(r.Retry, &date) == nil {
			parsed, err := time.Parse(time.RFC3339Nano, date)
			if err != nil {
				return result
			}
			seconds = float64(parsed.UnixMilli()) / 1000
		} else if json.Unmarshal(r.Retry, &seconds) != nil || !nonnegative(seconds) {
			return result
		}
		reason := "other"
		if r.Reason == "quota" || r.Reason == "credential_quota" {
			reason = r.Reason
		}
		result.Data = append(result.Data, runtimeCooldown{Scope: r.Scope, Model: r.Model, Reason: reason, RetryAtMS: int64(seconds * 1000)})
	}
	result.Stale, result.Error, result.ObservedAtMS = false, "", e.now().UnixMilli()
	return result
}

func (e *engine) queryQuota(ctx context.Context, a rawAccount) section[*quota] {
	at := e.now().UnixMilli()
	result := section[*quota]{Stale: true}
	header := map[string]string{"Authorization": "Bearer $TOKEN$", "Content-Type": "application/json", "User-Agent": "codex_cli_rs/0.159.2"}
	if a.workspace != "" {
		header["Chatgpt-Account-Id"] = a.workspace
	}
	body := map[string]any{"authIndex": a.Index, "method": "GET", "url": "https://chatgpt.com/backend-api/wham/usage", "header": header}
	var reply struct {
		Status      int             `json:"status_code"`
		StatusCamel int             `json:"statusCode"`
		Body        json.RawMessage `json:"body"`
	}
	if code := e.client.read(ctx, "/v0/management/api-call", body, &reply); code != "" {
		result.Error = code
		return result
	}
	if reply.Status == 0 {
		reply.Status = reply.StatusCamel
	}
	if reply.Status != 200 {
		result.Error = "quota_http_error"
		if reply.Status == 401 || reply.Status == 403 {
			result.Error = "quota_auth_failed"
		}
		return result
	}
	raw := reply.Body
	if len(raw) > 0 && raw[0] == '"' {
		var text string
		if json.Unmarshal(raw, &text) != nil {
			result.Error = "quota_invalid"
			return result
		}
		raw = []byte(text)
	}
	type rawWindow struct {
		Used    *float64 `json:"used_percent"`
		Seconds *int64   `json:"limit_window_seconds"`
		Reset   *float64 `json:"reset_at"`
		After   *float64 `json:"reset_after_seconds"`
	}
	type limits struct {
		Allowed   *bool      `json:"allowed"`
		Reached   *bool      `json:"limit_reached"`
		Primary   *rawWindow `json:"primary_window"`
		Secondary *rawWindow `json:"secondary_window"`
	}
	var payload struct {
		Plan   string  `json:"plan_type"`
		Limits *limits `json:"rate_limit"`
		Camel  *limits `json:"rateLimit"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		result.Error = "quota_invalid"
		return result
	}
	lim := payload.Limits
	if lim == nil {
		lim = payload.Camel
	}
	if lim == nil {
		result.Error = "quota_invalid"
		return result
	}
	q := &quota{Allowed: lim.Allowed, LimitReached: lim.Reached, Windows: []window{}, Scope: "codex_main"}
	switch payload.Plan {
	case "free", "go", "plus", "pro", "team", "business", "enterprise", "edu":
		q.Plan = payload.Plan
	}
	for i, w := range []*rawWindow{lim.Primary, lim.Secondary} {
		if w == nil {
			continue
		}
		if w.Used == nil || *w.Used < 0 || *w.Used > 100 || w.Seconds == nil || *w.Seconds <= 0 || *w.Seconds > 366*86400 {
			result.Error = "quota_invalid"
			return result
		}
		reset := float64(0)
		if w.Reset != nil {
			reset = *w.Reset * 1000
		} else if w.After != nil {
			reset = float64(at) + *w.After*1000
		}
		if reset <= 0 || reset > float64(at)+366*86400000 {
			result.Error = "quota_invalid"
			return result
		}
		kind := "primary"
		if i == 1 {
			kind = "secondary"
		}
		q.Windows = append(q.Windows, window{Kind: kind, UsedPercent: *w.Used, RemainingPercent: 100 - *w.Used, DurationSeconds: *w.Seconds, ResetAtMS: int64(reset)})
	}
	if len(q.Windows) == 0 {
		result.Error = "quota_invalid"
		return result
	}
	result.Data = q
	result.ObservedAtMS = at
	result.Stale = false
	for _, w := range q.Windows {
		if w.ResetAtMS <= e.now().UnixMilli() {
			result.Stale = true
			result.Error = "quota_expired"
		}
	}
	return result
}

func (e *engine) cooling(ctx context.Context) section[[]rawCooldown] {
	var reply struct {
		Items *[]rawCooldown `json:"items"`
	}
	code := e.client.read(ctx, "/usage-service/quota-cooldowns", nil, &reply)
	if code == "" && (reply.Items == nil) {
		code = "cooldowns_invalid"
	}
	if code != "" {
		return section[[]rawCooldown]{Stale: true, Error: code}
	}
	return section[[]rawCooldown]{ObservedAtMS: e.now().UnixMilli(), Data: *reply.Items}
}

func (e *engine) collect(previous *snapshot) *snapshot {
	ctx, cancel := context.WithCancel(e.lifecycle)
	defer cancel()
	work, stop := context.WithCancel(ctx)
	defer stop()
	result := &snapshot{SchemaVersion: 2, ClientScope: e.opaque("client", e.clientHash), StartedAtMS: e.now().UnixMilli(), RefreshAfterSeconds: 60}
	var files []rawAccount
	var code string
	var cool section[[]rawCooldown]
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); cool = e.cooling(work) }()
	go func() { defer wg.Done(); result.Recent = e.recent(work) }()
	go func() { defer wg.Done(); result.Prices = e.prices(work) }()
	files, code = e.accounts(work)
	result.Accounts = section[[]account]{ObservedAtMS: e.now().UnixMilli(), Stale: code != "", Error: code, Data: []account{}}
	if code != "" {
		result.Accounts.ObservedAtMS = 0
	}
	old := map[string]account{}
	if previous != nil {
		for _, a := range previous.Accounts.Data {
			old[a.ID] = a
		}
	}
	if code == "" {
		rows := make([]account, len(files))
		jobs := make(chan int)
		var workers sync.WaitGroup
		for n := 0; n < 6; n++ {
			workers.Add(1)
			go func() {
				defer workers.Done()
				for i := range jobs {
					a := files[i]
					row := account{ID: a.identity, Label: "Account " + a.identity[:8], Disabled: *a.Disabled, Unavailable: a.Unavailable, Status: "unknown", ObservedAtMS: e.now().UnixMilli(), binding: a.binding}
					if a.member != "" {
						parts := strings.SplitN(a.member, "@", 2)
						local := parts[0]
						if len(local) > 3 {
							local = local[:3]
						}
						row.Label = local + "***@" + parts[1]
					}
					switch a.Status {
					case "active", "disabled", "error", "ready":
						row.Status = a.Status
					}
					row.Routing = e.runtimeCooling(a)
					row.Quota = e.queryQuota(work, a)
					row.Models = e.models(work, a, row.ObservedAtMS)
					if prior, ok := old[a.identity]; ok && prior.binding == a.binding && row.Quota.Error != "" && row.Quota.Data == nil {
						row.Quota.Data = prior.Quota.Data
						row.Quota.ObservedAtMS = prior.Quota.ObservedAtMS
					}
					rows[i] = row
				}
			}()
		}
		for i := range files {
			jobs <- i
		}
		close(jobs)
		workers.Wait()
		wg.Wait()
		// 查询完成后复核身份；重登录、删除或禁用变化的结果不能移接给新账号。
		current, verify := e.accounts(ctx)
		same := map[string]string{}
		for _, a := range current {
			same[a.identity] = a.binding
		}
		if verify != "" {
			result.Accounts.Error = verify
			result.Accounts.Stale = true
		}
		if verify == "" && len(current) != len(files) {
			result.Accounts.Error = "membership_changed"
			result.Accounts.Stale = true
		}
		for i := range rows {
			if verify != "" || same[rows[i].ID] != rows[i].binding {
				rows[i].Quota = section[*quota]{Stale: true, Error: "identity_unconfirmed"}
				rows[i].Models = section[[]string]{Stale: true, Error: "identity_unconfirmed"}
				rows[i].Routing = section[[]runtimeCooldown]{Stale: true, Error: "identity_unconfirmed"}
				result.Accounts.Error = "membership_changed"
				result.Accounts.Stale = true
			}
		}
		// 只在身份复核后保存本次成功观测，再从同一 CPAMP 生命周期读取金额。
		e.syncQuotaObservations(ctx, files, rows)
		e.cycleEstimates(ctx, files, rows, result.Prices)
		e.saveReferences(files)
		result.Accounts.Data = rows
	}
	wg.Wait()
	result.Cooldowns = section[[]cooldown]{ObservedAtMS: cool.ObservedAtMS, Stale: cool.Stale, Error: cool.Error, Data: []cooldown{}}
	for _, c := range cool.Data {
		if c.Provider != "codex" {
			continue
		}
		matches := []rawAccount{}
		for _, a := range files {
			if c.Index != "" {
				if c.Index != a.Index {
					continue
				}
			} else if c.Name == "" || c.Name != a.Name {
				continue
			}
			if c.Member != "" && strings.ToLower(c.Member) != a.member {
				continue
			}
			matches = append(matches, a)
		}
		if len(matches) != 1 {
			continue
		}
		owner, reason := "other", "other"
		if c.Owner == "cpamp_usage_429" {
			owner = c.Owner
		}
		if c.Reason == "codex_usage_limit_reached" {
			reason = c.Reason
		}
		result.Cooldowns.Data = append(result.Cooldowns.Data, cooldown{AccountID: matches[0].identity, Owner: owner, Reason: reason, RecoverAtMS: c.RecoverAtMS})
	}
	if result.Accounts.Stale {
		result.Cooldowns.Stale = true
		result.Cooldowns.Error = "identity_unconfirmed"
	}
	e.cacheMu.Lock()
	active := map[string]bool{}
	for _, a := range files {
		active[a.identity] = true
	}
	for key := range e.modelsCache {
		if !active[key] {
			delete(e.modelsCache, key)
		}
	}
	e.cacheMu.Unlock()
	result.Complete = !result.Accounts.Stale && !result.Cooldowns.Stale && !result.Recent.Stale && !result.Prices.Stale
	for _, a := range result.Accounts.Data {
		if a.Quota.Stale || a.Models.Stale || a.Routing.Stale {
			result.Complete = false
		}
	}
	result.GeneratedAtMS = e.now().UnixMilli()
	return result
}

func nonnegative(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 9007199254740991
}
