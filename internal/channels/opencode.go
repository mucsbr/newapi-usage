package channels

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"
)

type openCodeProvider struct {
	label       string
	baseURL     string
	username    string
	password    string
	timeout     time.Duration
	concurrency int
}

type openCodeConfig struct {
	Label       string
	BaseURL     string
	Username    string
	Password    string
	Timeout     time.Duration
	Concurrency int
}

type openCodeAccountRaw struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Username     string `json:"username"`
	Enabled      bool   `json:"enabled"`
	AccountType  string `json:"account_type"`
	SetupStep    string `json:"setup_step"`
	PurchaseDate string `json:"purchase_date"`
	ExpiresOn    string `json:"expires_on"`
	LastError    string `json:"last_error"`
	AuthError    string `json:"auth_error"`
}

type openCodeLimits struct {
	FiveHour float64 `json:"window_5h"`
	Week     float64 `json:"window_week"`
	Month    float64 `json:"window_month"`
}

type openCodePricingRaw struct {
	Limits openCodeLimits `json:"limits"`
}

type openCodeUsageRaw struct {
	AccountID  string  `json:"account_id"`
	FiveHour   float64 `json:"window_5h"`
	Week       float64 `json:"window_week"`
	Month      float64 `json:"window_month"`
	Reset5H    *string `json:"resets_in_5h"`
	ResetWeek  *string `json:"resets_in_week"`
	ResetMonth *string `json:"resets_in_month"`
}

type openCodeRefreshRaw struct {
	Usage  openCodeUsageRaw `json:"usage"`
	Source string           `json:"source"`
}

func newOpenCode(cfg openCodeConfig) *openCodeProvider {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	concurrency := cfg.Concurrency
	if concurrency <= 0 {
		concurrency = 5
	}
	return &openCodeProvider{
		label:       cfg.Label,
		baseURL:     strings.TrimRight(cfg.BaseURL, "/"),
		username:    cfg.Username,
		password:    cfg.Password,
		timeout:     timeout,
		concurrency: concurrency,
	}
}

func (o *openCodeProvider) Balance(ctx context.Context) Balance {
	now := time.Now().Unix()
	client, err := o.authenticatedClient(ctx)
	if err != nil {
		return Balance{Channel: "opencode", Label: o.label, Kind: KindOpenCode, OK: false, Error: err.Error(), UpdatedAt: now}
	}

	accounts, err := o.fetchAccounts(ctx, client)
	if err != nil {
		return Balance{Channel: "opencode", Label: o.label, Kind: KindOpenCode, OK: false, Error: err.Error(), UpdatedAt: now}
	}
	limits, err := o.fetchLimits(ctx, client)
	if err != nil {
		return Balance{Channel: "opencode", Label: o.label, Kind: KindOpenCode, OK: false, Error: err.Error(), UpdatedAt: now}
	}

	items := make([]OpenCodeAccount, len(accounts))
	sem := make(chan struct{}, o.concurrency)
	var wg sync.WaitGroup
	for index := range accounts {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			items[i] = o.accountWithUsage(ctx, client, accounts[i], limits)
		}(index)
	}
	wg.Wait()

	return Balance{
		Channel:   "opencode",
		Label:     o.label,
		Kind:      KindOpenCode,
		OK:        true,
		UpdatedAt: now,
		OpenCode:  &OpenCodeSummary{Total: len(items), Accounts: items},
	}
}

func (o *openCodeProvider) RefreshUsage(ctx context.Context, accountID string) (OpenCodeUsage, error) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return OpenCodeUsage{}, fmt.Errorf("invalid account id")
	}
	client, err := o.authenticatedClient(ctx)
	if err != nil {
		return OpenCodeUsage{}, err
	}
	limits, err := o.fetchLimits(ctx, client)
	if err != nil {
		return OpenCodeUsage{}, err
	}
	endpoint := o.apiURL("/accounts/" + url.PathEscape(accountID) + "/usage/refresh")
	body, err := o.doJSON(ctx, client, http.MethodPost, endpoint, nil)
	if err != nil {
		return OpenCodeUsage{}, err
	}
	var response openCodeRefreshRaw
	if err := json.Unmarshal(body, &response); err != nil {
		return OpenCodeUsage{}, fmt.Errorf("decode usage refresh: %w", err)
	}
	source := strings.TrimSpace(response.Source)
	if source == "" {
		source = "live"
	}
	return OpenCodeUsage{
		AccountID: accountID,
		Source:    source,
		Windows:   openCodeUsageWindows(response.Usage, limits, "live"),
	}, nil
}

func (o *openCodeProvider) accountWithUsage(ctx context.Context, client *http.Client, raw openCodeAccountRaw, limits openCodeLimits) OpenCodeAccount {
	account := OpenCodeAccount{
		ID:              raw.ID,
		Name:            raw.Name,
		Username:        raw.Username,
		Enabled:         raw.Enabled,
		AccountType:     raw.AccountType,
		SetupStep:       raw.SetupStep,
		PurchaseDate:    raw.PurchaseDate,
		ExpiresOn:       raw.ExpiresOn,
		CanRefreshUsage: raw.Enabled && strings.EqualFold(raw.AccountType, "managed") && strings.EqualFold(raw.SetupStep, "ready"),
	}
	if raw.AuthError != "" {
		account.Error = raw.AuthError
	} else if raw.LastError != "" {
		account.Error = raw.LastError
	}

	usage, err := o.fetchUsage(ctx, client, raw.ID)
	if err != nil {
		account.Error = err.Error()
		return account
	}
	account.UsageWindows = openCodeUsageWindows(usage, limits, "estimated")
	return account
}

func (o *openCodeProvider) authenticatedClient(ctx context.Context) (*http.Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: o.timeout, Jar: jar}
	payload, err := json.Marshal(map[string]string{"username": o.username, "password": o.password})
	if err != nil {
		return nil, err
	}
	if _, err := o.doJSON(ctx, client, http.MethodPost, o.apiURL("/auth/login"), payload); err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}
	return client, nil
}

func (o *openCodeProvider) fetchAccounts(ctx context.Context, client *http.Client) ([]openCodeAccountRaw, error) {
	body, err := o.doJSON(ctx, client, http.MethodGet, o.apiURL("/accounts"), nil)
	if err != nil {
		return nil, fmt.Errorf("accounts: %w", err)
	}
	var accounts []openCodeAccountRaw
	if err := json.Unmarshal(body, &accounts); err != nil {
		return nil, fmt.Errorf("decode accounts: %w", err)
	}
	return accounts, nil
}

func (o *openCodeProvider) fetchLimits(ctx context.Context, client *http.Client) (openCodeLimits, error) {
	body, err := o.doJSON(ctx, client, http.MethodGet, o.apiURL("/pricing"), nil)
	if err != nil {
		return openCodeLimits{}, fmt.Errorf("pricing: %w", err)
	}
	var pricing openCodePricingRaw
	if err := json.Unmarshal(body, &pricing); err != nil {
		return openCodeLimits{}, fmt.Errorf("decode pricing: %w", err)
	}
	return pricing.Limits, nil
}

func (o *openCodeProvider) fetchUsage(ctx context.Context, client *http.Client, accountID string) (openCodeUsageRaw, error) {
	endpoint := o.apiURL("/accounts/" + url.PathEscape(accountID) + "/usage")
	body, err := o.doJSON(ctx, client, http.MethodGet, endpoint, nil)
	if err != nil {
		return openCodeUsageRaw{}, fmt.Errorf("usage: %w", err)
	}
	var usage openCodeUsageRaw
	if err := json.Unmarshal(body, &usage); err != nil {
		return openCodeUsageRaw{}, fmt.Errorf("decode usage: %w", err)
	}
	return usage, nil
}

func (o *openCodeProvider) doJSON(ctx context.Context, client *http.Client, method string, endpoint string, body []byte) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, snippet(raw))
	}
	return raw, nil
}

func (o *openCodeProvider) apiURL(path string) string {
	return o.baseURL + "/dashboard/api" + path
}

func openCodeUsageWindows(usage openCodeUsageRaw, limits openCodeLimits, source string) []OpenCodeUsageWindow {
	return []OpenCodeUsageWindow{
		openCodeUsageWindow("5h", source, usage.FiveHour, limits.FiveHour, usage.Reset5H),
		openCodeUsageWindow("周", source, usage.Week, limits.Week, usage.ResetWeek),
		openCodeUsageWindow("月", source, usage.Month, limits.Month, usage.ResetMonth),
	}
}

func openCodeUsageWindow(name string, source string, used float64, limit float64, resetsAt *string) OpenCodeUsageWindow {
	if used < 0 {
		used = 0
	}
	remaining := limit - used
	if remaining < 0 {
		remaining = 0
	}
	usedPercent := 0.0
	if limit > 0 {
		usedPercent = used / limit * 100
	}
	if usedPercent > 100 {
		usedPercent = 100
	}
	reset := ""
	if resetsAt != nil {
		reset = *resetsAt
	}
	return OpenCodeUsageWindow{
		Name:             name,
		Source:           source,
		LimitUSD:         limit,
		UsedUSD:          used,
		RemainingUSD:     remaining,
		UsedPercent:      usedPercent,
		RemainingPercent: 100 - usedPercent,
		ResetsAt:         reset,
	}
}
