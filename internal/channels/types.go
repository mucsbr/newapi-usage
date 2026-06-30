package channels

// Channel kind identifiers used by the frontend to decide how to render a card.
const (
	KindCurrency = "currency" // DeepSeek-style monetary balance.
	KindPool     = "pool"     // CPA-style account pool with quota usage.
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
