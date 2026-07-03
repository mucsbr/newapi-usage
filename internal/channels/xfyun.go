package channels

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type xfyunConfig struct {
	Enabled      bool
	Label        string
	BaseURL      string
	AccountsPath string
	Timeout      time.Duration
	PageSize     int
}

type xfyunProvider struct {
	label        string
	baseURL      string
	accountsPath string
	pageSize     int
	client       *http.Client
	ttl          time.Duration

	mu        sync.Mutex
	cached    Balance
	hasCached bool
	cachedAt  time.Time
}

type xfyunAccountFile struct {
	NextID   int64                `json:"next_id"`
	Accounts []xfyunAccountRecord `json:"accounts"`
}

type xfyunAccountRecord struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	SSOSessionID string `json:"sso_session_id"`
	CreatedAt    int64  `json:"created_at"`
	UpdatedAt    int64  `json:"updated_at"`
}

func newXFYun(cfg xfyunConfig) *xfyunProvider {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	label := strings.TrimSpace(cfg.Label)
	if label == "" {
		label = "讯飞 MaaS"
	}
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		baseURL = "https://maas.xfyun.cn"
	}
	pageSize := cfg.PageSize
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	return &xfyunProvider{
		label:        label,
		baseURL:      baseURL,
		accountsPath: cfg.AccountsPath,
		pageSize:     pageSize,
		client:       &http.Client{Timeout: timeout},
		ttl:          30 * time.Second,
	}
}

func (x *xfyunProvider) Balance(ctx context.Context) Balance {
	x.mu.Lock()
	if x.hasCached && time.Since(x.cachedAt) < x.ttl {
		cached := x.cached
		x.mu.Unlock()
		return cached
	}
	x.mu.Unlock()

	balance := x.fetchBalance(ctx)

	x.mu.Lock()
	x.cached = balance
	x.hasCached = true
	x.cachedAt = time.Now()
	x.mu.Unlock()
	return balance
}

func (x *xfyunProvider) AddAccount(ctx context.Context, name string, ssoSessionID string) (XFYunAccount, error) {
	ssoSessionID = normalizeXFYunSSO(ssoSessionID)
	if ssoSessionID == "" {
		return XFYunAccount{}, fmt.Errorf("ssoSessionId is required")
	}
	now := time.Now().Unix()
	record := xfyunAccountRecord{
		Name:         strings.TrimSpace(name),
		SSOSessionID: ssoSessionID,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	x.mu.Lock()
	file, err := x.loadAccountsLocked()
	if err == nil {
		if file.NextID <= 0 {
			file.NextID = nextXFYunID(file.Accounts)
		}
		record.ID = file.NextID
		file.NextID++
		if record.Name == "" {
			record.Name = fmt.Sprintf("讯飞账号 %d", record.ID)
		}
		file.Accounts = append(file.Accounts, record)
		err = x.saveAccountsLocked(file)
	}
	x.invalidateLocked()
	x.mu.Unlock()
	if err != nil {
		return XFYunAccount{}, err
	}
	return x.fetchAccount(ctx, record), nil
}

func (x *xfyunProvider) UpdateAccount(ctx context.Context, id int64, name *string, ssoSessionID *string) (XFYunAccount, error) {
	if id <= 0 {
		return XFYunAccount{}, fmt.Errorf("invalid account id")
	}
	var record xfyunAccountRecord
	found := false
	x.mu.Lock()
	file, err := x.loadAccountsLocked()
	if err == nil {
		for idx := range file.Accounts {
			if file.Accounts[idx].ID != id {
				continue
			}
			if name != nil {
				file.Accounts[idx].Name = strings.TrimSpace(*name)
				if file.Accounts[idx].Name == "" {
					file.Accounts[idx].Name = fmt.Sprintf("讯飞账号 %d", id)
				}
			}
			if ssoSessionID != nil {
				value := normalizeXFYunSSO(*ssoSessionID)
				if value != "" {
					file.Accounts[idx].SSOSessionID = value
				}
			}
			file.Accounts[idx].UpdatedAt = time.Now().Unix()
			record = file.Accounts[idx]
			found = true
			break
		}
		if !found {
			err = fmt.Errorf("account not found")
		} else {
			err = x.saveAccountsLocked(file)
		}
	}
	x.invalidateLocked()
	x.mu.Unlock()
	if err != nil {
		return XFYunAccount{}, err
	}
	return x.fetchAccount(ctx, record), nil
}

func (x *xfyunProvider) DeleteAccount(id int64) error {
	if id <= 0 {
		return fmt.Errorf("invalid account id")
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	file, err := x.loadAccountsLocked()
	if err != nil {
		return err
	}
	accounts := file.Accounts[:0]
	found := false
	for _, account := range file.Accounts {
		if account.ID == id {
			found = true
			continue
		}
		accounts = append(accounts, account)
	}
	if !found {
		return fmt.Errorf("account not found")
	}
	file.Accounts = accounts
	x.invalidateLocked()
	return x.saveAccountsLocked(file)
}

func (x *xfyunProvider) fetchBalance(ctx context.Context) Balance {
	now := time.Now().Unix()
	records, err := x.loadAccounts()
	if err != nil {
		return Balance{Channel: "xfyun", Label: x.label, Kind: KindXFYun, OK: false, Error: err.Error(), UpdatedAt: now}
	}
	accounts := make([]XFYunAccount, 0, len(records))
	for _, record := range records {
		accounts = append(accounts, x.fetchAccount(ctx, record))
	}
	return Balance{
		Channel:   "xfyun",
		Label:     x.label,
		Kind:      KindXFYun,
		OK:        true,
		UpdatedAt: now,
		XFYun:     &XFYunSummary{Total: len(accounts), Accounts: accounts},
	}
}

func (x *xfyunProvider) fetchAccount(ctx context.Context, record xfyunAccountRecord) XFYunAccount {
	account := XFYunAccount{
		ID:        record.ID,
		Name:      record.Name,
		Status:    "unknown",
		CreatedAt: record.CreatedAt,
		UpdatedAt: record.UpdatedAt,
	}
	if strings.TrimSpace(record.SSOSessionID) == "" {
		account.Status = "expired"
		account.Error = "missing ssoSessionId"
		return account
	}

	values := url.Values{}
	values.Set("page", "1")
	values.Set("size", strconv.Itoa(x.pageSize))
	endpoint := x.baseURL + "/api/v1/gpt-finetune/coding-plan/list?" + values.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		account.Status = "error"
		account.Error = err.Error()
		return account
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cookie", "ssoSessionId="+record.SSOSessionID)

	resp, err := x.client.Do(req)
	if err != nil {
		account.Status = "error"
		account.Error = err.Error()
		return account
	}
	defer resp.Body.Close()
	account.SessionExpiresAt = xfyunSessionExpiry(resp.Cookies())
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		account.Status = "error"
		account.Error = err.Error()
		return account
	}
	if resp.StatusCode >= 400 {
		account.Status = "error"
		account.Error = fmt.Sprintf("http %d: %s", resp.StatusCode, snippet(body))
		return account
	}

	var envelope xfyunPlanEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		account.Status = "error"
		account.Error = "decode response: " + err.Error()
		return account
	}
	if !envelope.Succeed || envelope.Code != 0 {
		account.Error = strings.TrimSpace(envelope.Message)
		if account.Error == "" {
			account.Error = fmt.Sprintf("code %d", envelope.Code)
		}
		if envelope.Code == 4001 || strings.Contains(account.Error, "未登录") {
			account.Status = "expired"
		} else {
			account.Status = "error"
		}
		return account
	}

	account.Status = "ok"
	account.Plans = make([]XFYunPlan, 0, len(envelope.Data.Rows))
	for _, row := range envelope.Data.Rows {
		account.Plans = append(account.Plans, xfyunPlanFromRaw(row))
	}
	return account
}

func (x *xfyunProvider) loadAccounts() ([]xfyunAccountRecord, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	file, err := x.loadAccountsLocked()
	if err != nil {
		return nil, err
	}
	out := make([]xfyunAccountRecord, len(file.Accounts))
	copy(out, file.Accounts)
	return out, nil
}

func (x *xfyunProvider) loadAccountsLocked() (xfyunAccountFile, error) {
	if strings.TrimSpace(x.accountsPath) == "" {
		return xfyunAccountFile{}, fmt.Errorf("XFYUN_ACCOUNTS_PATH is required")
	}
	body, err := os.ReadFile(x.accountsPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return xfyunAccountFile{NextID: 1, Accounts: []xfyunAccountRecord{}}, nil
		}
		return xfyunAccountFile{}, err
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return xfyunAccountFile{NextID: 1, Accounts: []xfyunAccountRecord{}}, nil
	}
	var file xfyunAccountFile
	if err := json.Unmarshal(body, &file); err != nil {
		return xfyunAccountFile{}, fmt.Errorf("decode xfyun accounts: %w", err)
	}
	if file.NextID <= 0 {
		file.NextID = nextXFYunID(file.Accounts)
	}
	if file.Accounts == nil {
		file.Accounts = []xfyunAccountRecord{}
	}
	return file, nil
}

func (x *xfyunProvider) saveAccountsLocked(file xfyunAccountFile) error {
	if err := os.MkdirAll(filepath.Dir(x.accountsPath), 0700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	tmp := x.accountsPath + ".tmp"
	if err := os.WriteFile(tmp, body, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, x.accountsPath)
}

func (x *xfyunProvider) invalidateLocked() {
	x.hasCached = false
	x.cached = Balance{}
	x.cachedAt = time.Time{}
}

func nextXFYunID(accounts []xfyunAccountRecord) int64 {
	next := int64(1)
	for _, account := range accounts {
		if account.ID >= next {
			next = account.ID + 1
		}
	}
	return next
}

func normalizeXFYunSSO(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(value), "cookie:") {
		value = strings.TrimSpace(value[len("cookie:"):])
	}
	for _, part := range strings.Split(value, ";") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "ssoSessionId=") {
			return strings.TrimSpace(strings.TrimPrefix(part, "ssoSessionId="))
		}
	}
	return value
}

func xfyunPlanFromRaw(raw xfyunPlanRaw) XFYunPlan {
	return XFYunPlan{
		AppID:     raw.AppID,
		Name:      raw.Name,
		Channel:   raw.Usage.Channel,
		ValidFrom: raw.ValidFrom,
		ExpiresAt: raw.ExpiresAt,
		Package:   xfyunUsage(raw.Usage.PackageLimit, raw.Usage.PackageUsage, raw.Usage.PackageLeft),
		RP5H:      xfyunUsage(raw.Usage.RP5HLimit, raw.Usage.RP5HUsage, nil),
		RPW:       xfyunUsage(raw.Usage.RPWLimit, raw.Usage.RPWUsage, nil),
		Daily:     xfyunUsage(raw.Usage.DailyLimit, raw.Usage.DailyUsage, nil),
	}
}

func xfyunUsage(limit *float64, usage *float64, left *float64) *XFYunUsageWindow {
	if limit == nil || *limit <= 0 {
		return nil
	}
	used := float64(0)
	if usage != nil {
		used = *usage
	}
	remaining := *limit - used
	if left != nil {
		remaining = *left
	}
	if remaining < 0 {
		remaining = 0
	}
	if remaining > *limit {
		remaining = *limit
	}
	remainingPercent := remaining / *limit * 100
	if remainingPercent < 0 {
		remainingPercent = 0
	}
	if remainingPercent > 100 {
		remainingPercent = 100
	}
	return &XFYunUsageWindow{
		Limit:            *limit,
		Usage:            used,
		Left:             remaining,
		UsedPercent:      100 - remainingPercent,
		RemainingPercent: remainingPercent,
	}
}

func xfyunSessionExpiry(cookies []*http.Cookie) string {
	for _, cookie := range cookies {
		if cookie.Name != "ssoSessionId" {
			continue
		}
		if !cookie.Expires.IsZero() {
			return cookie.Expires.Format(time.RFC3339)
		}
		if cookie.MaxAge > 0 {
			return time.Now().Add(time.Duration(cookie.MaxAge) * time.Second).Format(time.RFC3339)
		}
	}
	return ""
}

type xfyunPlanEnvelope struct {
	Code    int    `json:"code"`
	Succeed bool   `json:"succeed"`
	Message string `json:"message"`
	Data    struct {
		Rows  []xfyunPlanRaw `json:"rows"`
		Total int            `json:"total"`
	} `json:"data"`
}

type xfyunPlanRaw struct {
	AppID     string         `json:"appId"`
	Name      string         `json:"name"`
	ValidFrom string         `json:"validFrom"`
	ExpiresAt string         `json:"expiresAt"`
	Usage     xfyunUsageRaw  `json:"codingPlanUsageDTO"`
}

type xfyunUsageRaw struct {
	AppID        string   `json:"appId"`
	Channel      string   `json:"channel"`
	DailyLimit   *float64 `json:"dailyLimit"`
	DailyUsage   *float64 `json:"dailyUsage"`
	PackageLeft  *float64 `json:"packageLeft"`
	PackageLimit *float64 `json:"packageLimit"`
	PackageUsage *float64 `json:"packageUsage"`
	RP5HLimit    *float64 `json:"rp5hLimit"`
	RP5HUsage    *float64 `json:"rp5hUsage"`
	RPWLimit     *float64 `json:"rpwLimit"`
	RPWUsage     *float64 `json:"rpwUsage"`
}
