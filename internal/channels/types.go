package channels

// Channel kind identifiers used by the frontend to decide how to render a card.
const (
	KindCurrency = "currency" // DeepSeek-style monetary balance.
	KindPool     = "pool"     // CPA-style account pool with quota usage.
	KindSub2API  = "sub2api"  // Sub2API account list with estimated/live usage.
	KindOpenCode = "opencode" // OpenCode Go Manager account usage.
	KindZhipu    = "zhipu"    // Zhipu GLM Coding Plan quota usage.
	KindXFYun    = "xfyun"    // XFYun MaaS coding-plan package and rate quotas.
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

	// OpenCode Go Manager account channel.
	OpenCode *OpenCodeSummary `json:"opencode,omitempty"`

	// Zhipu GLM Coding Plan quota channel.
	Zhipu *ZhipuSummary `json:"zhipu,omitempty"`

	// XFYun MaaS coding-plan channel.
	XFYun *XFYunSummary `json:"xfyun,omitempty"`
}

type Descriptor struct {
	Channel string `json:"channel"`
	Label   string `json:"label"`
	Kind    string `json:"kind"`
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

// PoolAccount is one auth file's usage. The API's primary and secondary
// windows can change over time; LimitWindowSeconds identifies each window.
type PoolAccount struct {
	Name      string       `json:"name"`
	Email     string       `json:"email,omitempty"`
	Primary   *WindowUsage `json:"primary_window,omitempty"`
	Secondary *WindowUsage `json:"secondary_window,omitempty"`
	Error     string       `json:"error,omitempty"`
}

// WindowUsage is one rate-limit window's usage. Remaining is 100 - UsedPercent.
type WindowUsage struct {
	UsedPercent        float64 `json:"used_percent"`
	Remaining          float64 `json:"remaining_percent"`
	LimitWindowSeconds int64   `json:"limit_window_seconds,omitempty"`
	ResetsAt           int64   `json:"resets_at,omitempty"`
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
	ExternalQuota      *Sub2APIAccountQuota `json:"external_quota,omitempty"`
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
	RemainingPercent float64 `json:"remaining_percent"`
	ResetsAt         string  `json:"resets_at,omitempty"`
	RemainingSeconds int64   `json:"remaining_seconds,omitempty"`
	Requests         int64   `json:"requests,omitempty"`
	Tokens           int64   `json:"tokens,omitempty"`
	Cost             float64 `json:"cost,omitempty"`
}

type Sub2APIAccountQuota struct {
	Source       string  `json:"source"`
	Label        string  `json:"label"`
	UserID       int64   `json:"user_id,omitempty"`
	Username     string  `json:"username,omitempty"`
	Quota        int64   `json:"quota"`
	UsedQuota    int64   `json:"used_quota"`
	BalanceCNY   float64 `json:"balance_cny"`
	UsedCNY      float64 `json:"used_cny"`
	RequestCount int64   `json:"request_count"`
	UpdatedAt    int64   `json:"updated_at"`
	Error        string  `json:"error,omitempty"`
}

type OpenCodeSummary struct {
	Total    int               `json:"total"`
	Accounts []OpenCodeAccount `json:"accounts"`
}

type OpenCodeAccount struct {
	ID              string                `json:"id"`
	Name            string                `json:"name"`
	Username        string                `json:"username,omitempty"`
	Enabled         bool                  `json:"enabled"`
	AccountType     string                `json:"account_type"`
	SetupStep       string                `json:"setup_step"`
	PurchaseDate    string                `json:"purchase_date,omitempty"`
	ExpiresOn       string                `json:"expires_on,omitempty"`
	CanRefreshUsage bool                  `json:"can_refresh_usage"`
	UsageWindows    []OpenCodeUsageWindow `json:"usage_windows,omitempty"`
	Error           string                `json:"error,omitempty"`
}

type OpenCodeUsage struct {
	AccountID string                `json:"account_id"`
	Source    string                `json:"source"`
	Windows   []OpenCodeUsageWindow `json:"windows"`
}

type OpenCodeUsageWindow struct {
	Name             string  `json:"name"`
	Source           string  `json:"source"`
	LimitUSD         float64 `json:"limit_usd"`
	UsedUSD          float64 `json:"used_usd"`
	RemainingUSD     float64 `json:"remaining_usd"`
	UsedPercent      float64 `json:"used_percent"`
	RemainingPercent float64 `json:"remaining_percent"`
	ResetsAt         string  `json:"resets_at,omitempty"`
}

type ZhipuSummary struct {
	Level  string       `json:"level"`
	Limits []ZhipuLimit `json:"limits"`
}

type ZhipuLimit struct {
	Type             string             `json:"type"`
	Name             string             `json:"name"`
	Unit             int                `json:"unit,omitempty"`
	Number           int                `json:"number,omitempty"`
	UsedPercent      float64            `json:"used_percent"`
	RemainingPercent float64            `json:"remaining_percent"`
	Total            float64            `json:"total,omitempty"`
	Used             float64            `json:"used,omitempty"`
	Remaining        float64            `json:"remaining,omitempty"`
	NextResetAt      int64              `json:"next_reset_at,omitempty"`
	Details          []ZhipuUsageDetail `json:"details,omitempty"`
}

type ZhipuUsageDetail struct {
	ModelCode string  `json:"model_code"`
	Usage     float64 `json:"usage"`
}

type XFYunSummary struct {
	Total    int            `json:"total"`
	Accounts []XFYunAccount `json:"accounts"`
}

type XFYunAccount struct {
	ID               int64       `json:"id"`
	Name             string      `json:"name"`
	Status           string      `json:"status"`
	Error            string      `json:"error,omitempty"`
	CreatedAt        int64       `json:"created_at,omitempty"`
	UpdatedAt        int64       `json:"updated_at,omitempty"`
	SessionExpiresAt string      `json:"session_expires_at,omitempty"`
	Plans            []XFYunPlan `json:"plans,omitempty"`
}

type XFYunPlan struct {
	AppID     string            `json:"app_id"`
	Name      string            `json:"name"`
	Channel   string            `json:"channel"`
	ValidFrom string            `json:"valid_from,omitempty"`
	ExpiresAt string            `json:"expires_at,omitempty"`
	Package   *XFYunUsageWindow `json:"package,omitempty"`
	RP5H      *XFYunUsageWindow `json:"rp5h,omitempty"`
	RPW       *XFYunUsageWindow `json:"rpw,omitempty"`
	Daily     *XFYunUsageWindow `json:"daily,omitempty"`
}

type XFYunUsageWindow struct {
	Limit            float64 `json:"limit"`
	Usage            float64 `json:"usage"`
	Left             float64 `json:"left"`
	UsedPercent      float64 `json:"used_percent"`
	RemainingPercent float64 `json:"remaining_percent"`
}
