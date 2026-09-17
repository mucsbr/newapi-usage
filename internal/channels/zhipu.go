package channels

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const zhipuFetchConcurrency = 5

type zhipuProvider struct {
	label        string
	base         string
	legacyAPIKey string
	accountsPath string
	client       *http.Client
	ttl          time.Duration

	mu        sync.Mutex
	cached    Balance
	hasCached bool
	cachedAt  time.Time
}

type zhipuConfig struct {
	Label        string
	BaseURL      string
	LegacyAPIKey string
	AccountsPath string
	Timeout      time.Duration
}

type zhipuAccountFile struct {
	NextID   int64                `json:"next_id"`
	Accounts []zhipuAccountRecord `json:"accounts"`
}

type zhipuAccountRecord struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	APIKey    string `json:"api_key"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

type zhipuQuotaResponse struct {
	Code    int    `json:"code"`
	Message string `json:"msg"`
	Success bool   `json:"success"`
	Data    struct {
		Level  string            `json:"level"`
		Limits []zhipuQuotaLimit `json:"limits"`
	} `json:"data"`
}

type zhipuQuotaLimit struct {
	Type          string              `json:"type"`
	Unit          int                 `json:"unit"`
	Number        int                 `json:"number"`
	Usage         float64             `json:"usage"`
	CurrentValue  float64             `json:"currentValue"`
	Remaining     float64             `json:"remaining"`
	Percentage    float64             `json:"percentage"`
	NextResetTime int64               `json:"nextResetTime"`
	UsageDetails  []zhipuUsageDetails `json:"usageDetails"`
}

type zhipuUsageDetails struct {
	ModelCode string  `json:"modelCode"`
	Usage     float64 `json:"usage"`
}

func newZhipu(cfg zhipuConfig) *zhipuProvider {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	label := strings.TrimSpace(cfg.Label)
	if label == "" {
		label = "智谱 GLM"
	}
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		baseURL = "https://open.bigmodel.cn"
	}
	return &zhipuProvider{
		label:        label,
		base:         baseURL,
		legacyAPIKey: normalizeZhipuAPIKey(cfg.LegacyAPIKey),
		accountsPath: strings.TrimSpace(cfg.AccountsPath),
		client:       &http.Client{Timeout: timeout},
		ttl:          30 * time.Second,
	}
}

func (z *zhipuProvider) Balance(ctx context.Context) Balance {
	return z.balance(ctx, false)
}

func (z *zhipuProvider) Refresh(ctx context.Context) Balance {
	return z.balance(ctx, true)
}

func (z *zhipuProvider) balance(ctx context.Context, force bool) Balance {
	z.mu.Lock()
	if !force && z.hasCached && time.Since(z.cachedAt) < z.ttl {
		cached := z.cached
		z.mu.Unlock()
		return cached
	}
	z.mu.Unlock()

	balance := z.fetchBalance(ctx)
	z.mu.Lock()
	z.cached = balance
	z.hasCached = true
	z.cachedAt = time.Now()
	z.mu.Unlock()
	return balance
}

func (z *zhipuProvider) AddAccount(ctx context.Context, name string, apiKey string) (ZhipuAccount, error) {
	apiKey = normalizeZhipuAPIKey(apiKey)
	if apiKey == "" {
		return ZhipuAccount{}, fmt.Errorf("api key is required")
	}
	now := time.Now().Unix()
	record := zhipuAccountRecord{Name: strings.TrimSpace(name), APIKey: apiKey, CreatedAt: now, UpdatedAt: now}
	z.mu.Lock()
	file, err := z.loadAccountsLocked()
	if err == nil {
		for _, account := range file.Accounts {
			if account.APIKey == apiKey {
				err = fmt.Errorf("api key already exists")
				break
			}
		}
	}
	if err == nil {
		if file.NextID <= 0 {
			file.NextID = nextZhipuID(file.Accounts)
		}
		record.ID = file.NextID
		file.NextID++
		if record.Name == "" {
			record.Name = fmt.Sprintf("智谱账号 %d", record.ID)
		}
		file.Accounts = append(file.Accounts, record)
		err = z.saveAccountsLocked(file)
	}
	z.invalidateLocked()
	z.mu.Unlock()
	if err != nil {
		return ZhipuAccount{}, err
	}
	return z.fetchAccount(ctx, record), nil
}

func (z *zhipuProvider) UpdateAccount(ctx context.Context, id int64, name *string, apiKey *string) (ZhipuAccount, error) {
	if id <= 0 {
		return ZhipuAccount{}, fmt.Errorf("invalid account id")
	}
	var record zhipuAccountRecord
	found := false
	z.mu.Lock()
	file, err := z.loadAccountsLocked()
	if err == nil {
		for idx := range file.Accounts {
			if file.Accounts[idx].ID != id {
				continue
			}
			if name != nil {
				file.Accounts[idx].Name = strings.TrimSpace(*name)
				if file.Accounts[idx].Name == "" {
					file.Accounts[idx].Name = fmt.Sprintf("智谱账号 %d", id)
				}
			}
			if apiKey != nil {
				value := normalizeZhipuAPIKey(*apiKey)
				if value == "" {
					err = fmt.Errorf("api key is required")
					break
				}
				for other := range file.Accounts {
					if file.Accounts[other].ID != id && file.Accounts[other].APIKey == value {
						err = fmt.Errorf("api key already exists")
						break
					}
				}
				if err != nil {
					break
				}
				file.Accounts[idx].APIKey = value
			}
			file.Accounts[idx].UpdatedAt = time.Now().Unix()
			record = file.Accounts[idx]
			found = true
			break
		}
		if err == nil && !found {
			err = fmt.Errorf("account not found")
		}
		if err == nil {
			err = z.saveAccountsLocked(file)
		}
	}
	z.invalidateLocked()
	z.mu.Unlock()
	if err != nil {
		return ZhipuAccount{}, err
	}
	return z.fetchAccount(ctx, record), nil
}

func (z *zhipuProvider) DeleteAccount(id int64) error {
	if id <= 0 {
		return fmt.Errorf("invalid account id")
	}
	z.mu.Lock()
	defer z.mu.Unlock()
	file, err := z.loadAccountsLocked()
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
	z.invalidateLocked()
	return z.saveAccountsLocked(file)
}

func (z *zhipuProvider) fetchBalance(ctx context.Context) Balance {
	now := time.Now().Unix()
	records, err := z.loadAccounts()
	if err != nil {
		return Balance{Channel: "zhipu", Label: z.label, Kind: KindZhipu, OK: false, Error: err.Error(), UpdatedAt: now}
	}
	accounts := make([]ZhipuAccount, len(records))
	sem := make(chan struct{}, zhipuFetchConcurrency)
	var wg sync.WaitGroup
	for idx := range records {
		idx := idx
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				accounts[idx] = zhipuAccountError(records[idx], ctx.Err())
				return
			}
			accounts[idx] = z.fetchAccount(ctx, records[idx])
		}()
	}
	wg.Wait()
	return Balance{Channel: "zhipu", Label: z.label, Kind: KindZhipu, OK: true, UpdatedAt: now, Zhipu: &ZhipuSummary{Total: len(accounts), Accounts: accounts}}
}

func (z *zhipuProvider) fetchAccount(ctx context.Context, record zhipuAccountRecord) ZhipuAccount {
	account := ZhipuAccount{ID: record.ID, Name: record.Name, KeyTail: zhipuKeyTail(record.APIKey), Status: "unknown", CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt}
	level, limits, err := z.fetchQuota(ctx, record.APIKey)
	if err != nil {
		account.Status = "error"
		account.Error = err.Error()
		return account
	}
	account.Status = "ok"
	account.Level = level
	account.Limits = limits
	return account
}

func zhipuAccountError(record zhipuAccountRecord, err error) ZhipuAccount {
	message := "request canceled"
	if err != nil {
		message = err.Error()
	}
	return ZhipuAccount{ID: record.ID, Name: record.Name, KeyTail: zhipuKeyTail(record.APIKey), Status: "error", Error: message, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt}
}

func (z *zhipuProvider) fetchQuota(ctx context.Context, apiKey string) (string, []ZhipuLimit, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, z.base+"/api/monitor/usage/quota/limit", nil)
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Authorization", "Bearer "+normalizeZhipuAPIKey(apiKey))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := z.client.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", nil, err
	}
	if resp.StatusCode >= 400 {
		return "", nil, fmt.Errorf("zhipu http %d", resp.StatusCode)
	}
	var parsed zhipuQuotaResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", nil, fmt.Errorf("decode zhipu quota: %w", err)
	}
	if !parsed.Success || parsed.Code != http.StatusOK {
		return "", nil, fmt.Errorf("zhipu response %d: %s", parsed.Code, strings.TrimSpace(parsed.Message))
	}
	limits := make([]ZhipuLimit, 0, len(parsed.Data.Limits))
	for _, raw := range parsed.Data.Limits {
		limits = append(limits, normalizeZhipuLimit(raw))
	}
	sort.SliceStable(limits, func(i, j int) bool { return zhipuLimitRank(limits[i]) < zhipuLimitRank(limits[j]) })
	return parsed.Data.Level, limits, nil
}

func (z *zhipuProvider) loadAccounts() ([]zhipuAccountRecord, error) {
	z.mu.Lock()
	defer z.mu.Unlock()
	file, err := z.loadAccountsLocked()
	if err != nil {
		return nil, err
	}
	out := make([]zhipuAccountRecord, len(file.Accounts))
	copy(out, file.Accounts)
	return out, nil
}

func (z *zhipuProvider) loadAccountsLocked() (zhipuAccountFile, error) {
	if strings.TrimSpace(z.accountsPath) == "" {
		return zhipuAccountFile{}, fmt.Errorf("ZHIPU_ACCOUNTS_PATH is required")
	}
	body, err := os.ReadFile(z.accountsPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return zhipuAccountFile{}, err
		}
		file := zhipuAccountFile{NextID: 1, Accounts: []zhipuAccountRecord{}}
		if z.legacyAPIKey != "" {
			now := time.Now().Unix()
			file.Accounts = append(file.Accounts, zhipuAccountRecord{ID: 1, Name: "默认账号", APIKey: z.legacyAPIKey, CreatedAt: now, UpdatedAt: now})
			file.NextID = 2
		}
		if err := z.saveAccountsLocked(file); err != nil {
			return zhipuAccountFile{}, err
		}
		return file, nil
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return zhipuAccountFile{NextID: 1, Accounts: []zhipuAccountRecord{}}, nil
	}
	var file zhipuAccountFile
	if err := json.Unmarshal(body, &file); err != nil {
		return zhipuAccountFile{}, fmt.Errorf("decode zhipu accounts: %w", err)
	}
	if file.NextID <= 0 {
		file.NextID = nextZhipuID(file.Accounts)
	}
	if file.Accounts == nil {
		file.Accounts = []zhipuAccountRecord{}
	}
	return file, nil
}

func (z *zhipuProvider) saveAccountsLocked(file zhipuAccountFile) error {
	if err := os.MkdirAll(filepath.Dir(z.accountsPath), 0700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	tmp := z.accountsPath + ".tmp"
	if err := os.WriteFile(tmp, body, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, z.accountsPath)
}

func (z *zhipuProvider) invalidateLocked() {
	z.hasCached = false
	z.cached = Balance{}
	z.cachedAt = time.Time{}
}

func nextZhipuID(accounts []zhipuAccountRecord) int64 {
	next := int64(1)
	for _, account := range accounts {
		if account.ID >= next {
			next = account.ID + 1
		}
	}
	return next
}

func normalizeZhipuAPIKey(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(strings.ToLower(value), "bearer ") {
		value = strings.TrimSpace(value[len("bearer "):])
	}
	return value
}

func zhipuKeyTail(value string) string {
	value = normalizeZhipuAPIKey(value)
	if len(value) <= 8 {
		return value
	}
	return value[len(value)-8:]
}

func normalizeZhipuLimit(raw zhipuQuotaLimit) ZhipuLimit {
	usedPercent := clampPercent(raw.Percentage)
	limit := ZhipuLimit{Type: raw.Type, Name: zhipuLimitName(raw), Unit: raw.Unit, Number: raw.Number, UsedPercent: usedPercent, RemainingPercent: 100 - usedPercent, NextResetAt: millisecondsToSeconds(raw.NextResetTime)}
	if raw.Type == "TIME_LIMIT" {
		limit.Total = raw.Usage
		limit.Used = raw.CurrentValue
		limit.Remaining = raw.Remaining
		limit.Details = make([]ZhipuUsageDetail, 0, len(raw.UsageDetails))
		for _, detail := range raw.UsageDetails {
			limit.Details = append(limit.Details, ZhipuUsageDetail{ModelCode: detail.ModelCode, Usage: detail.Usage})
		}
	}
	return limit
}

func zhipuLimitName(raw zhipuQuotaLimit) string {
	if raw.Type == "TIME_LIMIT" {
		return "工具调用（月）"
	}
	if raw.Type == "TOKENS_LIMIT" {
		switch {
		case raw.Unit == 3 && raw.Number == 5:
			return "5小时"
		case raw.Unit == 6 && raw.Number == 1:
			return "周"
		default:
			return "模型额度"
		}
	}
	return raw.Type
}

func zhipuLimitRank(limit ZhipuLimit) int {
	switch limit.Name {
	case "5小时":
		return 0
	case "周":
		return 1
	case "工具调用（月）":
		return 2
	default:
		return 3
	}
}

func clampPercent(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

func millisecondsToSeconds(value int64) int64 {
	if value <= 0 {
		return 0
	}
	if value > 1_000_000_000_000 {
		return value / 1000
	}
	return value
}
