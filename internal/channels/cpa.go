package channels

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// cpaProvider mirrors the rate_limit probing in pool_maintainer.py's
// PoolMaintainer (fetch_auth_files + probe_one). It lists the CPA management
// auth-files, proxies a chatgpt usage call per candidate through
// /v0/management/api-call, and aggregates the per-account quota usage.
//
// Probing is expensive (one HTTP call per account), so it runs on a background
// ticker and serves a cached snapshot — mirroring internal/audit's Indexer.
type cpaProvider struct {
	label         string
	baseURL       string
	token         string
	targetType    string
	userAgent     string
	usedThreshold float64
	concurrency   int
	refreshEvery  time.Duration
	maxAccounts   int
	usageURL      string
	client        *http.Client

	wg sync.WaitGroup

	mu        sync.RWMutex
	cached    Balance
	hasCached bool
}

func newCPA(cfg cpaConfig) *cpaProvider {
	concurrency := cfg.Concurrency
	if concurrency <= 0 {
		concurrency = 20
	}
	probeTimeout := cfg.ProbeTimeout
	if probeTimeout <= 0 {
		probeTimeout = 15 * time.Second
	}
	refresh := cfg.RefreshInterval
	if refresh <= 0 {
		refresh = 300 * time.Second
	}
	return &cpaProvider{
		label:         cfg.Label,
		baseURL:       strings.TrimRight(cfg.BaseURL, "/"),
		token:         cfg.Token,
		targetType:    cfg.TargetType,
		userAgent:     cfg.UserAgent,
		usedThreshold: float64(cfg.UsedPercentThreshold),
		concurrency:   concurrency,
		refreshEvery:  refresh,
		maxAccounts:   cfg.MaxAccounts,
		usageURL:      "https://chatgpt.com/backend-api/wham/usage",
		client:        &http.Client{Timeout: probeTimeout},
	}
}

type cpaConfig struct {
	Label                string
	BaseURL              string
	Token                string
	TargetType           string
	UserAgent            string
	UsedPercentThreshold int
	Concurrency          int
	ProbeTimeout         time.Duration
	RefreshInterval      time.Duration
	MaxAccounts          int
}

func (c *cpaProvider) Start(ctx context.Context) {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.run(ctx)
	}()
}

func (c *cpaProvider) run(ctx context.Context) {
	c.refreshAndStore(ctx)
	ticker := time.NewTicker(c.refreshEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.refreshAndStore(ctx)
		}
	}
}

func (c *cpaProvider) Close() {
	c.wg.Wait()
}

func (c *cpaProvider) Snapshot() Balance {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.hasCached {
		return Balance{
			Channel:   "cpa",
			Label:     c.label,
			Kind:      KindPool,
			OK:        false,
			Error:     "refreshing",
			UpdatedAt: time.Now().Unix(),
		}
	}
	return c.cached
}

func (c *cpaProvider) refreshAndStore(ctx context.Context) {
	balance := c.refresh(ctx)
	c.mu.Lock()
	c.cached = balance
	c.hasCached = true
	c.mu.Unlock()
}

func (c *cpaProvider) refresh(ctx context.Context) Balance {
	now := time.Now().Unix()
	files, err := c.fetchAuthFiles(ctx)
	if err != nil {
		return Balance{Channel: "cpa", Label: c.label, Kind: KindPool, OK: false, Error: err.Error(), UpdatedAt: now}
	}

	candidates := make([]map[string]any, 0, len(files))
	for _, item := range files {
		if strings.EqualFold(itemType(item), c.targetType) {
			candidates = append(candidates, item)
		}
	}
	if c.maxAccounts > 0 && len(candidates) > c.maxAccounts {
		candidates = candidates[:c.maxAccounts]
	}

	accounts := make([]PoolAccount, len(candidates))
	sem := make(chan struct{}, c.concurrency)
	var wg sync.WaitGroup
	for idx := range candidates {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			accounts[i] = c.probeOne(ctx, candidates[i])
		}(idx)
	}
	wg.Wait()

	summary := PoolSummary{Total: len(candidates), Accounts: accounts}
	var remainingSum float64
	var remainingCount int
	for _, account := range accounts {
		switch {
		case account.Error != "":
			summary.Errors++
		case account.Paid:
			summary.Paid++
			summary.Probed++
		case account.UsedPercent != nil:
			summary.Probed++
			remainingSum += *account.Remaining
			remainingCount++
			if *account.UsedPercent >= c.usedThreshold {
				summary.Exhausted++
			} else {
				summary.Healthy++
			}
		default:
			summary.Probed++
		}
	}
	if remainingCount > 0 {
		summary.AvgRemaining = remainingSum / float64(remainingCount)
	}

	return Balance{
		Channel:   "cpa",
		Label:     c.label,
		Kind:      KindPool,
		OK:        true,
		UpdatedAt: now,
		Pool:      &summary,
	}
}

func (c *cpaProvider) fetchAuthFiles(ctx context.Context) ([]map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v0/management/auth-files", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("auth-files http %d", resp.StatusCode)
	}
	var parsed struct {
		Files []map[string]any `json:"files"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode auth-files: %w", err)
	}
	return parsed.Files, nil
}

// apiCallResponse is the envelope returned by /v0/management/api-call: it wraps
// the proxied call's status code and stringified body.
type apiCallResponse struct {
	StatusCode int    `json:"status_code"`
	Body       string `json:"body"`
}

func (c *cpaProvider) probeOne(ctx context.Context, item map[string]any) PoolAccount {
	account := PoolAccount{
		Name:  itemName(item),
		Email: extractEmail(item),
	}
	authIndex, ok := item["auth_index"]
	if !ok || authIndex == nil || fmt.Sprint(authIndex) == "" {
		account.Error = "missing auth_index"
		return account
	}

	header := map[string]string{
		"Authorization": "Bearer $TOKEN$",
		"Content-Type":  "application/json",
		"User-Agent":    c.userAgent,
	}
	if accountID := extractAccountID(item); accountID != "" {
		header["Chatgpt-Account-Id"] = accountID
	}
	payload := map[string]any{
		"authIndex": authIndex,
		"method":    "GET",
		"url":       c.usageURL,
		"header":    header,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		account.Error = err.Error()
		return account
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v0/management/api-call", bytes.NewReader(raw))
	if err != nil {
		account.Error = err.Error()
		return account
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		account.Error = err.Error()
		return account
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		account.Error = err.Error()
		return account
	}
	if resp.StatusCode >= 400 {
		account.Error = fmt.Sprintf("api-call http %d", resp.StatusCode)
		return account
	}

	var envelope apiCallResponse
	if err := json.Unmarshal(body, &envelope); err != nil {
		account.Error = "decode api-call: " + err.Error()
		return account
	}
	if envelope.StatusCode == 401 {
		account.Error = "401"
		return account
	}
	if envelope.StatusCode != 200 {
		account.Error = fmt.Sprintf("usage status %d", envelope.StatusCode)
		return account
	}

	var usage struct {
		RateLimit struct {
			PrimaryWindow struct {
				UsedPercent *float64 `json:"used_percent"`
			} `json:"primary_window"`
			SecondaryWindow json.RawMessage `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	if err := json.Unmarshal([]byte(envelope.Body), &usage); err != nil {
		account.Error = "decode usage body"
		return account
	}

	if len(usage.RateLimit.SecondaryWindow) > 0 && string(usage.RateLimit.SecondaryWindow) != "null" {
		account.Paid = true
		return account
	}
	if used := usage.RateLimit.PrimaryWindow.UsedPercent; used != nil {
		usedValue := *used
		remaining := 100 - usedValue
		account.UsedPercent = &usedValue
		account.Remaining = &remaining
	}
	return account
}

// --- auth-file field extraction (mirrors pool_maintainer.py helpers) ---

func itemType(item map[string]any) string {
	if v := fieldString(item, "type"); v != "" {
		return v
	}
	return fieldString(item, "typo")
}

func itemName(item map[string]any) string {
	if v := fieldString(item, "name"); v != "" {
		return v
	}
	return fieldString(item, "id")
}

func extractAccountID(item map[string]any) string {
	for _, key := range []string{"chatgpt_account_id", "chatgptAccountId", "account_id", "accountId"} {
		if v := fieldString(item, key); v != "" {
			return v
		}
	}
	return ""
}

func extractEmail(item map[string]any) string {
	for _, key := range []string{"email", "account"} {
		v := strings.ToLower(fieldString(item, key))
		if strings.Contains(v, "@") {
			return v
		}
	}
	if extra, ok := item["extra"].(map[string]any); ok {
		for _, key := range []string{"email", "account"} {
			v := strings.ToLower(fieldString(extra, key))
			if strings.Contains(v, "@") {
				return v
			}
		}
	}
	name := strings.ToLower(itemName(item))
	name = strings.TrimSuffix(name, ".json")
	if strings.Contains(name, "@") {
		return name
	}
	return ""
}

func fieldString(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok {
		return ""
	}
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprint(t))
	}
}
