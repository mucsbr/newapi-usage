package channels

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

type zhipuProvider struct {
	label  string
	base   string
	apiKey string
	client *http.Client
	ttl    time.Duration

	mu        sync.Mutex
	cached    Balance
	hasCached bool
	cachedAt  time.Time
	lastGood  *Balance
}

type zhipuConfig struct {
	Label   string
	BaseURL string
	APIKey  string
	Timeout time.Duration
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
	return &zhipuProvider{
		label:  cfg.Label,
		base:   strings.TrimRight(cfg.BaseURL, "/"),
		apiKey: strings.TrimSpace(cfg.APIKey),
		client: &http.Client{Timeout: timeout},
		ttl:    30 * time.Second,
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

	balance, err := z.fetch(ctx)
	z.mu.Lock()
	defer z.mu.Unlock()
	if err != nil {
		if z.lastGood != nil {
			stale := *z.lastGood
			stale.Error = "stale: " + err.Error()
			return stale
		}
		failed := Balance{Channel: "zhipu", Label: z.label, Kind: KindZhipu, OK: false, Error: err.Error(), UpdatedAt: time.Now().Unix()}
		z.cached = failed
		z.hasCached = true
		z.cachedAt = time.Now()
		return failed
	}
	z.cached = balance
	z.hasCached = true
	z.cachedAt = time.Now()
	good := balance
	z.lastGood = &good
	return balance
}

func (z *zhipuProvider) fetch(ctx context.Context) (Balance, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, z.base+"/api/monitor/usage/quota/limit", nil)
	if err != nil {
		return Balance{}, err
	}
	authorization := z.apiKey
	if !strings.HasPrefix(strings.ToLower(authorization), "bearer ") {
		authorization = "Bearer " + authorization
	}
	req.Header.Set("Authorization", authorization)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := z.client.Do(req)
	if err != nil {
		return Balance{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Balance{}, err
	}
	if resp.StatusCode >= 400 {
		return Balance{}, fmt.Errorf("zhipu http %d", resp.StatusCode)
	}
	var parsed zhipuQuotaResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return Balance{}, fmt.Errorf("decode zhipu quota: %w", err)
	}
	if !parsed.Success || parsed.Code != http.StatusOK {
		return Balance{}, fmt.Errorf("zhipu response %d: %s", parsed.Code, strings.TrimSpace(parsed.Message))
	}

	limits := make([]ZhipuLimit, 0, len(parsed.Data.Limits))
	for _, raw := range parsed.Data.Limits {
		limits = append(limits, normalizeZhipuLimit(raw))
	}
	sort.SliceStable(limits, func(i, j int) bool { return zhipuLimitRank(limits[i]) < zhipuLimitRank(limits[j]) })
	return Balance{
		Channel:   "zhipu",
		Label:     z.label,
		Kind:      KindZhipu,
		OK:        true,
		UpdatedAt: time.Now().Unix(),
		Zhipu:     &ZhipuSummary{Level: parsed.Data.Level, Limits: limits},
	}, nil
}

func normalizeZhipuLimit(raw zhipuQuotaLimit) ZhipuLimit {
	usedPercent := clampPercent(raw.Percentage)
	limit := ZhipuLimit{
		Type:             raw.Type,
		Name:             zhipuLimitName(raw),
		Unit:             raw.Unit,
		Number:           raw.Number,
		UsedPercent:      usedPercent,
		RemainingPercent: 100 - usedPercent,
		NextResetAt:      millisecondsToSeconds(raw.NextResetTime),
	}
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
