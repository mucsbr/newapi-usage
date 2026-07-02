package channels

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const ikunQuotaDivisor = 500000.0

type ikunConfig struct {
	Label             string
	BaseURL           string
	AccessToken       string
	UserID            int64
	Sub2APIAccountID  int64
	Sub2APIAccountKey string
}

func (c ikunConfig) Enabled() bool {
	return strings.TrimSpace(c.AccessToken) != "" && c.UserID > 0
}

type ikunProvider struct {
	label             string
	baseURL           string
	accessToken       string
	userID            int64
	sub2APIAccountID  int64
	sub2APIAccountKey string
	client            *http.Client
	ttl               time.Duration

	mu        sync.Mutex
	cached    Sub2APIAccountQuota
	hasCached bool
	cachedAt  time.Time
	lastGood  *Sub2APIAccountQuota
}

func newIkun(cfg ikunConfig, timeout time.Duration) *ikunProvider {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	label := strings.TrimSpace(cfg.Label)
	if label == "" {
		label = "Ikun"
	}
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		baseURL = "https://api.ikuncode.cc"
	}
	accountKey := strings.ToLower(strings.TrimSpace(cfg.Sub2APIAccountKey))
	if accountKey == "" {
		accountKey = "ikun"
	}
	return &ikunProvider{
		label:             label,
		baseURL:           baseURL,
		accessToken:       strings.TrimSpace(cfg.AccessToken),
		userID:            cfg.UserID,
		sub2APIAccountID:  cfg.Sub2APIAccountID,
		sub2APIAccountKey: accountKey,
		client:            &http.Client{Timeout: timeout},
		ttl:               30 * time.Second,
	}
}

func (i *ikunProvider) Quota(ctx context.Context) (Sub2APIAccountQuota, error) {
	i.mu.Lock()
	if i.hasCached && time.Since(i.cachedAt) < i.ttl {
		cached := i.cached
		i.mu.Unlock()
		return cached, nil
	}
	i.mu.Unlock()

	quota, err := i.fetch(ctx)

	i.mu.Lock()
	defer i.mu.Unlock()
	if err != nil {
		if i.lastGood != nil {
			stale := *i.lastGood
			stale.Error = "stale: " + err.Error()
			i.cached = stale
			i.hasCached = true
			i.cachedAt = time.Now()
			return stale, nil
		}
		return Sub2APIAccountQuota{}, err
	}
	i.cached = quota
	i.hasCached = true
	i.cachedAt = time.Now()
	good := quota
	i.lastGood = &good
	return quota, nil
}

func (i *ikunProvider) fetch(ctx context.Context) (Sub2APIAccountQuota, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, i.baseURL+"/api/user/self", nil)
	if err != nil {
		return Sub2APIAccountQuota{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", i.accessToken)
	req.Header.Set("New-Api-User", strconv.FormatInt(i.userID, 10))

	resp, err := i.client.Do(req)
	if err != nil {
		return Sub2APIAccountQuota{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Sub2APIAccountQuota{}, err
	}
	if resp.StatusCode >= 400 {
		return Sub2APIAccountQuota{}, fmt.Errorf("ikun http %d: %s", resp.StatusCode, snippet(body))
	}

	var envelope ikunSelfEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return Sub2APIAccountQuota{}, fmt.Errorf("decode ikun self: %w", err)
	}
	if !envelope.Success {
		message := strings.TrimSpace(envelope.Message)
		if message == "" {
			message = "request failed"
		}
		return Sub2APIAccountQuota{}, fmt.Errorf("ikun: %s", message)
	}
	return Sub2APIAccountQuota{
		Source:       "ikun",
		Label:        i.label,
		UserID:       envelope.Data.ID,
		Username:     envelope.Data.Username,
		Quota:        envelope.Data.Quota,
		UsedQuota:    envelope.Data.UsedQuota,
		BalanceCNY:   quotaToCNY(envelope.Data.Quota),
		UsedCNY:      quotaToCNY(envelope.Data.UsedQuota),
		RequestCount: envelope.Data.RequestCount,
		UpdatedAt:    time.Now().Unix(),
	}, nil
}

func (i *ikunProvider) matchesSub2Account(raw sub2AccountRaw) bool {
	if i.sub2APIAccountID > 0 {
		return raw.ID == i.sub2APIAccountID
	}
	if i.sub2APIAccountKey != "" {
		name := strings.ToLower(strings.TrimSpace(raw.Name))
		if name == i.sub2APIAccountKey || strings.Contains(name, i.sub2APIAccountKey) {
			return true
		}
	}
	baseURL := strings.TrimRight(strings.ToLower(fieldString(raw.Credentials, "base_url")), "/")
	return baseURL != "" && baseURL == strings.TrimRight(strings.ToLower(i.baseURL), "/")
}

func quotaToCNY(quota int64) float64 {
	return math.Round(float64(quota)/ikunQuotaDivisor*100) / 100
}

type ikunSelfEnvelope struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Data    struct {
		ID           int64  `json:"id"`
		Username     string `json:"username"`
		Quota        int64  `json:"quota"`
		UsedQuota    int64  `json:"used_quota"`
		RequestCount int64  `json:"request_count"`
	} `json:"data"`
}
