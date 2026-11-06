package status

import "encoding/json"

type section[T any] struct {
	ObservedAtMS int64  `json:"observed_at_ms"`
	Stale        bool   `json:"stale"`
	Error        string `json:"error,omitempty"`
	Data         T      `json:"data"`
}
type window struct {
	Kind             string         `json:"kind"`
	UsedPercent      float64        `json:"used_percent"`
	RemainingPercent float64        `json:"remaining_percent"`
	DurationSeconds  int64          `json:"duration_seconds"`
	ResetAtMS        int64          `json:"reset_at_ms"`
	CycleEstimate    *cycleEstimate `json:"cycle_estimate,omitempty"`
}
type quota struct {
	Plan         string   `json:"plan_type,omitempty"`
	Allowed      *bool    `json:"allowed"`
	LimitReached *bool    `json:"limit_reached"`
	Windows      []window `json:"windows"`
	Scope        string   `json:"scope"`
}
type runtimeCooldown struct {
	Scope     string `json:"scope"`
	Model     string `json:"model_key,omitempty"`
	Reason    string `json:"reason"`
	RetryAtMS int64  `json:"retry_at_ms"`
}
type account struct {
	ID           string                     `json:"id"`
	Label        string                     `json:"label"`
	Disabled     bool                       `json:"disabled"`
	Unavailable  bool                       `json:"unavailable"`
	Status       string                     `json:"status"`
	ObservedAtMS int64                      `json:"observed_at_ms"`
	Routing      section[[]runtimeCooldown] `json:"routing"`
	Models       section[[]string]          `json:"models"`
	Quota        section[*quota]            `json:"quota"`
	binding      string
	quotaSynced  bool
}
type cooldown struct {
	AccountID   string `json:"account_id"`
	Owner       string `json:"owner"`
	Reason      string `json:"reason"`
	RecoverAtMS int64  `json:"recover_at_ms"`
}
type recentRequest struct {
	ID                  string   `json:"id"`
	AtMS                int64    `json:"at_ms"`
	Model               string   `json:"model"`
	ThreadID            string   `json:"thread_id,omitempty"`
	InputTokens         int64    `json:"input_tokens"`
	OutputTokens        int64    `json:"output_tokens"`
	CachedTokens        int64    `json:"cached_tokens"`
	CacheCreationTokens int64    `json:"cache_creation_tokens"`
	ServiceTier         string   `json:"service_tier,omitempty"`
	ReasoningEffort     string   `json:"reasoning_effort,omitempty"`
	FailureReason       string   `json:"failure_reason,omitempty"`
	CacheReadTokens     int64    `json:"cache_read_tokens"`
	ReasoningTokens     int64    `json:"reasoning_tokens"`
	Failed              bool     `json:"failed"`
	Status              *int     `json:"failure_status_code,omitempty"`
	DurationMS          *float64 `json:"duration_ms,omitempty"`
	TTFTMS              *float64 `json:"ttft_ms,omitempty"`
}
type recent struct {
	FromMS    int64           `json:"from_ms"`
	ThroughMS int64           `json:"through_ms"`
	Truncated bool            `json:"truncated"`
	Rows      []recentRequest `json:"rows"`
}
type snapshot struct {
	ClientScope         string                             `json:"client_scope"`
	StartedAtMS         int64                              `json:"started_at_ms"`
	SchemaVersion       int                                `json:"schema_version"`
	GeneratedAtMS       int64                              `json:"generated_at_ms"`
	RefreshAfterSeconds int                                `json:"refresh_after_seconds"`
	Complete            bool                               `json:"complete"`
	Accounts            section[[]account]                 `json:"accounts"`
	Cooldowns           section[[]cooldown]                `json:"cooldowns"`
	Recent              section[*recent]                   `json:"recent"`
	Prices              section[map[string]map[string]any] `json:"prices"`
}
type rawAccount struct {
	ID                                   string                     `json:"id"`
	Index                                string                     `json:"auth_index"`
	Name                                 string                     `json:"name"`
	Provider                             string                     `json:"provider"`
	Type                                 string                     `json:"type"`
	Email                                string                     `json:"email"`
	Account                              string                     `json:"account"`
	AccountSnapshot                      string                     `json:"account_snapshot"`
	AccountSnapshotCamel                 string                     `json:"accountSnapshot"`
	Disabled                             *bool                      `json:"disabled"`
	Unavailable                          bool                       `json:"unavailable"`
	Status                               string                     `json:"status"`
	Runtime                              json.RawMessage            `json:"cooldowns"`
	Updated                              json.RawMessage            `json:"updated_at"`
	Claims                               map[string]json.RawMessage `json:"id_token"`
	Metadata                             map[string]json.RawMessage `json:"metadata"`
	Workspace                            string                     `json:"chatgpt_account_id"`
	AccountID                            string                     `json:"account_id"`
	member, workspace, identity, binding string
}
type rawCooldown struct {
	Index       string `json:"authIndex"`
	Name        string `json:"authFileName"`
	Member      string `json:"accountSnapshot"`
	Provider    string `json:"provider"`
	Owner       string `json:"owner"`
	Reason      string `json:"reasonCode"`
	RecoverAtMS int64  `json:"recoverAtMs"`
}
type modelCache struct {
	Binding string
	Value   section[[]string]
}
