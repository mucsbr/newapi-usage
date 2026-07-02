package channels

// Channel kind identifiers used by the frontend to decide how to render a card.
const (
	KindCurrency = "currency" // DeepSeek-style monetary balance.
	KindPool     = "pool"     // CPA-style account pool with quota usage.
	KindSub2API  = "sub2api"  // Sub2API account list with estimated/live usage.
)

// Balance is one channel card in the "渠道余额" area.
//
// A channel maps to exactly one card. For currency channels (DeepSeek) the
// Currencies slice carries the per-currency monetary balance; for pool channels
// (CPA) the Pool field carries the aggregated account-pool summary.
type Balance struct {
	Channel   string `json:"channel"` // stable id, e.g. "deepseek" / "cpa"
	Label     string `json:"label"`   // display name
	Kind      string `json:"kind"`    // KindCurrency | KindPool
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
	UpdatedAt int64  `json:"updated_at"`

	// Currency channels (DeepSeek).
	IsAvailable *bool             `json:"is_available,omitempty"`
	Currencies  []CurrencyBalance `json:"currencies,omitempty"`

	// Pool channels (CPA).
	Pool *PoolSummary `json:"pool,omitempty"`

	// Sub2API account channel.
	Sub2API *Sub2APISummary `json:"sub2api,omitempty"`
}

// CurrencyBalance mirrors one entry of DeepSeek's balance_infos array. Amounts
// are kept as strings to preserve the exact precision returned by the API.
type CurrencyBalance struct {
	Currency        string `json:"currency"`
	TotalBalance    string `json:"total_balance"`
	GrantedBalance  string `json:"granted_balance"`
	ToppedUpBalance string `json:"topped_up_balance"`
}

// PoolSummary is the per-account view of a CPA account pool. No cross-account
// aggregation is done — each auth file is shown on its own with both rate-limit
// windows.
type PoolSummary struct {
	Total    int           `json:"total"`    // candidate auth files of the target type
	Accounts []PoolAccount `json:"accounts"` // one row per auth file
}

// PoolAccount is one auth file's usage. ChatGPT's wham/usage exposes two
// rate-limit windows: primary = 5-hour limit, secondary = weekly limit.
type PoolAccount struct {
	Name      string       `json:"name"`
	Email     string       `json:"email,omitempty"`
	Primary   *WindowUsage `json:"primary_window,omitempty"`   // 5-hour limit
	Secondary *WindowUsage `json:"secondary_window,omitempty"` // weekly limit
	Error     string       `json:"error,omitempty"`
}

// WindowUsage is one rate-limit window's usage. Remaining is 100 - UsedPercent.
type WindowUsage struct {
	UsedPercent float64 `json:"used_percent"`
	Remaining   float64 `json:"remaining_percent"`
}

// Sub2APISummary lists Sub2API accounts. OAuth accounts can be refreshed
// individually for live usage, while API-key accounts only expose list metadata.
type Sub2APISummary struct {
	Total    int              `json:"total"`
	Accounts []Sub2APIAccount `json:"accounts"`
}

type Sub2APIAccount struct {
	ID                 int64                `json:"id"`
	Name               string               `json:"name"`
	Email              string               `json:"email,omitempty"`
	Platform           string               `json:"platform"`
	Type               string               `json:"type"`
	Status             string               `json:"status"`
	ErrorMessage       string               `json:"error_message,omitempty"`
	Schedulable        bool                 `json:"schedulable"`
	CurrentConcurrency int                  `json:"current_concurrency"`
	Concurrency        int                  `json:"concurrency"`
	Groups             []string             `json:"groups,omitempty"`
	ProxyName          string               `json:"proxy_name,omitempty"`
	LastUsedAt         string               `json:"last_used_at,omitempty"`
	UpdatedAt          string               `json:"updated_at,omitempty"`
	SessionStatus      string               `json:"session_window_status,omitempty"`
	SessionWindowStart string               `json:"session_window_start,omitempty"`
	SessionWindowEnd   string               `json:"session_window_end,omitempty"`
	CanRefreshUsage    bool                 `json:"can_refresh_usage"`
	UsageWindows       []Sub2APIUsageWindow `json:"usage_windows,omitempty"`
}

type Sub2APIUsage struct {
	AccountID int64                `json:"account_id"`
	UpdatedAt string               `json:"updated_at,omitempty"`
	Windows   []Sub2APIUsageWindow `json:"windows"`
}

type Sub2APIUsageWindow struct {
	Name             string  `json:"name"`
	Source           string  `json:"source"`
	UsedPercent      float64 `json:"used_percent"`
	ResetsAt         string  `json:"resets_at,omitempty"`
	RemainingSeconds int64   `json:"remaining_seconds,omitempty"`
	Requests         int64   `json:"requests,omitempty"`
	Tokens           int64   `json:"tokens,omitempty"`
	Cost             float64 `json:"cost,omitempty"`
}
