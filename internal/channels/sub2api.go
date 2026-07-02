package channels

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type sub2APIProvider struct {
	label    string
	baseURL  string
	apiKey   string
	timezone string
	pageSize int
	client   *http.Client
}

type sub2APIConfig struct {
	Label    string
	BaseURL  string
	APIKey   string
	Timezone string
	Timeout  time.Duration
	PageSize int
}

func newSub2API(cfg sub2APIConfig) *sub2APIProvider {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	pageSize := cfg.PageSize
	if pageSize <= 0 {
		pageSize = 50
	}
	if pageSize > 500 {
		pageSize = 500
	}
	timezone := strings.TrimSpace(cfg.Timezone)
	if timezone == "" {
		timezone = "Asia/Shanghai"
	}
	return &sub2APIProvider{
		label:    cfg.Label,
		baseURL:  strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:   cfg.APIKey,
		timezone: timezone,
		pageSize: pageSize,
		client:   &http.Client{Timeout: timeout},
	}
}

func (s *sub2APIProvider) Balance(ctx context.Context) Balance {
	now := time.Now().Unix()
	accounts, total, err := s.fetchAccounts(ctx)
	if err != nil {
		return Balance{Channel: "sub2api", Label: s.label, Kind: KindSub2API, OK: false, Error: err.Error(), UpdatedAt: now}
	}
	return Balance{
		Channel:   "sub2api",
		Label:     s.label,
		Kind:      KindSub2API,
		OK:        true,
		UpdatedAt: now,
		Sub2API:   &Sub2APISummary{Total: total, Accounts: accounts},
	}
}

func (s *sub2APIProvider) FetchUsage(ctx context.Context, accountID int64, force bool, timezone string) (Sub2APIUsage, error) {
	if accountID <= 0 {
		return Sub2APIUsage{}, fmt.Errorf("invalid account id")
	}
	if strings.TrimSpace(timezone) == "" {
		timezone = s.timezone
	}
	values := url.Values{}
	values.Set("source", "active")
	values.Set("force", strconv.FormatBool(force))
	values.Set("timezone", timezone)
	endpoint := fmt.Sprintf("%s/api/v1/admin/accounts/%d/usage?%s", s.baseURL, accountID, values.Encode())

	body, err := s.getJSON(ctx, endpoint)
	if err != nil {
		return Sub2APIUsage{}, err
	}
	var envelope sub2UsageEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return Sub2APIUsage{}, fmt.Errorf("decode usage: %w", err)
	}
	if envelope.Code != 0 {
		return Sub2APIUsage{}, fmt.Errorf("usage code %d: %s", envelope.Code, envelope.Message)
	}
	return Sub2APIUsage{
		AccountID: accountID,
		UpdatedAt: envelope.Data.UpdatedAt,
		Windows:   usageWindowsFromLive(envelope.Data),
	}, nil
}

func (s *sub2APIProvider) fetchAccounts(ctx context.Context) ([]Sub2APIAccount, int, error) {
	values := url.Values{}
	values.Set("page", "1")
	values.Set("page_size", strconv.Itoa(s.pageSize))
	values.Set("platform", "")
	values.Set("type", "")
	values.Set("status", "")
	values.Set("privacy_mode", "")
	values.Set("group", "")
	values.Set("search", "")
	values.Set("sort_by", "status")
	values.Set("sort_order", "asc")
	values.Set("lite", "1")
	values.Set("timezone", s.timezone)
	endpoint := s.baseURL + "/api/v1/admin/accounts?" + values.Encode()

	body, err := s.getJSON(ctx, endpoint)
	if err != nil {
		return nil, 0, err
	}
	var envelope sub2AccountsEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, 0, fmt.Errorf("decode accounts: %w", err)
	}
	if envelope.Code != 0 {
		return nil, 0, fmt.Errorf("accounts code %d: %s", envelope.Code, envelope.Message)
	}
	items := make([]Sub2APIAccount, 0, len(envelope.Data.Items))
	for _, item := range envelope.Data.Items {
		items = append(items, accountFromSub2Raw(item))
	}
	total := envelope.Data.Total
	if total == 0 {
		total = len(items)
	}
	return items, total, nil
}

func (s *sub2APIProvider) getJSON(ctx context.Context, endpoint string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", s.apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, snippet(body))
	}
	return body, nil
}

type sub2AccountsEnvelope struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		Items []sub2AccountRaw `json:"items"`
		Total int              `json:"total"`
	} `json:"data"`
}

type sub2AccountRaw struct {
	ID                    int64          `json:"id"`
	Name                  string         `json:"name"`
	Platform              string         `json:"platform"`
	Type                  string         `json:"type"`
	Status                string         `json:"status"`
	ErrorMessage          string         `json:"error_message"`
	Schedulable           bool           `json:"schedulable"`
	CurrentConcurrency    int            `json:"current_concurrency"`
	Concurrency           int            `json:"concurrency"`
	LastUsedAt            string         `json:"last_used_at"`
	UpdatedAt             string         `json:"updated_at"`
	SessionWindowStart    string         `json:"session_window_start"`
	SessionWindowEnd      string         `json:"session_window_end"`
	SessionWindowStatus   string         `json:"session_window_status"`
	Extra                 map[string]any `json:"extra"`
	Credentials           map[string]any `json:"credentials"`
	Groups                []sub2GroupRaw `json:"groups"`
	Proxy                 *struct {
		Name string `json:"name"`
	} `json:"proxy"`
}

type sub2GroupRaw struct {
	Name string `json:"name"`
}

type sub2UsageEnvelope struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    sub2UsageData `json:"data"`
}

type sub2UsageData struct {
	UpdatedAt string             `json:"updated_at"`
	FiveHour  sub2UsageWindowRaw `json:"five_hour"`
	SevenDay  sub2UsageWindowRaw `json:"seven_day"`
}

type sub2UsageWindowRaw struct {
	Utilization      any    `json:"utilization"`
	ResetsAt         string `json:"resets_at"`
	RemainingSeconds int64  `json:"remaining_seconds"`
	WindowStats      struct {
		Requests int64   `json:"requests"`
		Tokens   int64   `json:"tokens"`
		Cost     float64 `json:"cost"`
	} `json:"window_stats"`
}

func accountFromSub2Raw(raw sub2AccountRaw) Sub2APIAccount {
	groups := make([]string, 0, len(raw.Groups))
	for _, group := range raw.Groups {
		if strings.TrimSpace(group.Name) != "" {
			groups = append(groups, group.Name)
		}
	}
	proxyName := ""
	if raw.Proxy != nil {
		proxyName = raw.Proxy.Name
	}
	return Sub2APIAccount{
		ID:                 raw.ID,
		Name:               raw.Name,
		Email:              sub2Email(raw),
		Platform:           raw.Platform,
		Type:               raw.Type,
		Status:             raw.Status,
		ErrorMessage:       raw.ErrorMessage,
		Schedulable:        raw.Schedulable,
		CurrentConcurrency: raw.CurrentConcurrency,
		Concurrency:        raw.Concurrency,
		Groups:             groups,
		ProxyName:          proxyName,
		LastUsedAt:         raw.LastUsedAt,
		UpdatedAt:          raw.UpdatedAt,
		SessionStatus:      raw.SessionWindowStatus,
		SessionWindowStart: raw.SessionWindowStart,
		SessionWindowEnd:   raw.SessionWindowEnd,
		CanRefreshUsage:    strings.EqualFold(raw.Type, "oauth"),
		UsageWindows:       usageWindowsFromAccount(raw),
	}
}

func usageWindowsFromAccount(raw sub2AccountRaw) []Sub2APIUsageWindow {
	windows := make([]Sub2APIUsageWindow, 0, 2)
	if raw.Extra == nil {
		return windows
	}
	if value, ok := raw.Extra["session_window_utilization"]; ok {
		used := normalizeUsedPercent(asFloat(value))
		windows = append(windows, Sub2APIUsageWindow{
			Name:             "5h 预估",
			Source:           "estimated",
			UsedPercent:      used,
			RemainingPercent: remainingPercent(used),
			ResetsAt:         raw.SessionWindowEnd,
		})
	}
	if value, ok := raw.Extra["passive_usage_7d_utilization"]; ok {
		used := normalizeUsedPercent(asFloat(value))
		windows = append(windows, Sub2APIUsageWindow{
			Name:             "7d 预估",
			Source:           "estimated",
			UsedPercent:      used,
			RemainingPercent: remainingPercent(used),
			ResetsAt:         unixLikeTime(raw.Extra["passive_usage_7d_reset"]),
		})
	}
	return windows
}

func usageWindowsFromLive(data sub2UsageData) []Sub2APIUsageWindow {
	windows := make([]Sub2APIUsageWindow, 0, 2)
	if win, ok := liveWindow("5h 实时", data.FiveHour); ok {
		windows = append(windows, win)
	}
	if win, ok := liveWindow("7d 实时", data.SevenDay); ok {
		windows = append(windows, win)
	}
	return windows
}

func liveWindow(name string, raw sub2UsageWindowRaw) (Sub2APIUsageWindow, bool) {
	used := normalizeUsedPercent(asFloat(raw.Utilization))
	if used == 0 && raw.ResetsAt == "" && raw.RemainingSeconds == 0 && raw.WindowStats.Requests == 0 && raw.WindowStats.Tokens == 0 {
		return Sub2APIUsageWindow{}, false
	}
	return Sub2APIUsageWindow{
		Name:             name,
		Source:           "live",
		UsedPercent:      used,
		RemainingPercent: remainingPercent(used),
		ResetsAt:         raw.ResetsAt,
		RemainingSeconds: raw.RemainingSeconds,
		Requests:         raw.WindowStats.Requests,
		Tokens:           raw.WindowStats.Tokens,
		Cost:             raw.WindowStats.Cost,
	}, true
}

func sub2Email(raw sub2AccountRaw) string {
	for _, source := range []map[string]any{raw.Credentials, raw.Extra} {
		for _, key := range []string{"email_address", "email", "account"} {
			value := strings.TrimSpace(fieldString(source, key))
			if strings.Contains(value, "@") {
				return value
			}
		}
	}
	return ""
}

func normalizeUsedPercent(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 0 && value <= 1 {
		value *= 100
	}
	if value > 100 {
		return 100
	}
	return value
}

func remainingPercent(used float64) float64 {
	remaining := 100 - used
	if remaining < 0 {
		return 0
	}
	if remaining > 100 {
		return 100
	}
	return remaining
}

func asFloat(value any) float64 {
	switch v := value.(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case json.Number:
		n, _ := v.Float64()
		return n
	case string:
		n, _ := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return n
	default:
		return 0
	}
}

func unixLikeTime(value any) string {
	seconds := int64(asFloat(value))
	if seconds <= 0 {
		return ""
	}
	return time.Unix(seconds, 0).Format(time.RFC3339)
}
