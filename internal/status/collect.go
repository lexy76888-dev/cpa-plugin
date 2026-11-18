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

func storedAccountQuota(a rawAccount, stored map[string]StoredQuota, now int64) section[*quota] {
	result := section[*quota]{Stale: true, Error: "quota_not_observed"}
	v, ok := stored[a.Index]
	if !ok || (a.ID != "" && a.ID != v.AuthID) ||
		(a.member != "" && v.Member != "" && !strings.EqualFold(a.member, v.Member)) ||
		v.ObservedAtMS <= 0 || v.ObservedAtMS > now {
		return result
	}
	if a.workspace != "" && a.workspace != v.Workspace {
		// 自然回包可只保存绑定的账号摘要；此处沿用同一身份，不读取凭据。
		if v.Workspace != "" || sha256.Sum256([]byte(a.workspace)) != v.SubjectHash {
			return result
		}
	}
	q := &quota{Scope: "codex_main", Plan: v.Plan}
	stale, blocked := v.Stale, v.Blocked
	for _, w := range v.Windows {
		if !nonnegative(w.Used) || w.Used > 100 || w.Seconds <= 0 || w.EndMS <= 0 {
			continue
		}
		stale = stale || w.EndMS <= now
		blocked = blocked || w.Used >= 100
		q.Windows = append(q.Windows, window{Kind: w.Kind, UsedPercent: w.Used, RemainingPercent: 100 - w.Used, DurationSeconds: w.Seconds, ResetAtMS: w.EndMS})
	}
	if len(q.Windows) == 0 {
		return result
	}
	allowed := !blocked
	q.Allowed, q.LimitReached = &allowed, &blocked
	result = section[*quota]{Data: q, ObservedAtMS: v.ObservedAtMS, Stale: stale}
	if stale {
		result.Error = "quota_stale"
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

func (e *engine) collect() *snapshot {
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
	var stored map[string]StoredQuota
	if code == "" && e.storedQuota != nil {
		stored = e.storedQuota()
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
					row.Quota = storedAccountQuota(a, stored, e.now().UnixMilli())
					row.Models = e.models(work, a, row.ObservedAtMS)
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
		// 身份复核后批量读取 CPAMP 已有周期、额度和金额。

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
