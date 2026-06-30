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

// PoolSummary is the aggregate view of a CPA account pool.
type PoolSummary struct {
	Total        int           `json:"total"`                 // candidate auth files of the target type
	Probed       int           `json:"probed"`                // accounts that returned usable usage data
	Healthy      int           `json:"healthy"`               // free accounts below the used-percent threshold
	Exhausted    int           `json:"exhausted"`             // free accounts at/above the threshold
	Paid         int           `json:"paid"`                  // accounts with a secondary window (non-free)
	Errors       int           `json:"errors"`                // accounts that failed to probe
	AvgRemaining float64       `json:"avg_remaining_percent"` // mean remaining% over free probed accounts
	Accounts     []PoolAccount `json:"accounts,omitempty"`    // per-auth-file detail (expandable)
}

// PoolAccount is the per-auth-file detail row inside a CPA pool card.
type PoolAccount struct {
	Name        string   `json:"name"`
	Email       string   `json:"email,omitempty"`
	UsedPercent *float64 `json:"used_percent,omitempty"`
	Remaining   *float64 `json:"remaining_percent,omitempty"`
	Paid        bool     `json:"paid"`
	Error       string   `json:"error,omitempty"`
}
