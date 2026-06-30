package channels

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// deepSeekProvider fetches the DeepSeek account balance. The endpoint is cheap
// (a single call) so it is fetched live, with a short internal cache to absorb
// rapid page reloads and a last-good fallback for transient failures.
type deepSeekProvider struct {
	label  string
	apiKey string
	base   string
	client *http.Client
	ttl    time.Duration

	mu        sync.Mutex
	cached    Balance
	hasCached bool
	cachedAt  time.Time
	lastGood  *Balance
}

func newDeepSeek(label, apiKey, base string, timeout time.Duration) *deepSeekProvider {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &deepSeekProvider{
		label:  label,
		apiKey: apiKey,
		base:   base,
		client: &http.Client{Timeout: timeout},
		ttl:    30 * time.Second,
	}
}

// deepSeekResponse mirrors the documented /user/balance shape.
type deepSeekResponse struct {
	IsAvailable  bool `json:"is_available"`
	BalanceInfos []struct {
		Currency        string `json:"currency"`
		TotalBalance    string `json:"total_balance"`
		GrantedBalance  string `json:"granted_balance"`
		ToppedUpBalance string `json:"topped_up_balance"`
	} `json:"balance_infos"`
}

func (d *deepSeekProvider) Balance(ctx context.Context) Balance {
	d.mu.Lock()
	if d.hasCached && time.Since(d.cachedAt) < d.ttl {
		cached := d.cached
		d.mu.Unlock()
		return cached
	}
	d.mu.Unlock()

	balance, err := d.fetch(ctx)

	d.mu.Lock()
	defer d.mu.Unlock()
	if err != nil {
		if d.lastGood != nil {
			stale := *d.lastGood
			stale.Error = "stale: " + err.Error()
			return stale
		}
		fail := Balance{
			Channel:   "deepseek",
			Label:     d.label,
			Kind:      KindCurrency,
			OK:        false,
			Error:     err.Error(),
			UpdatedAt: time.Now().Unix(),
		}
		d.cached = fail
		d.hasCached = true
		d.cachedAt = time.Now()
		return fail
	}
	d.cached = balance
	d.hasCached = true
	d.cachedAt = time.Now()
	good := balance
	d.lastGood = &good
	return balance
}

func (d *deepSeekProvider) fetch(ctx context.Context) (Balance, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.base+"/user/balance", nil)
	if err != nil {
		return Balance{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+d.apiKey)

	resp, err := d.client.Do(req)
	if err != nil {
		return Balance{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Balance{}, err
	}
	if resp.StatusCode >= 400 {
		return Balance{}, fmt.Errorf("deepseek http %d", resp.StatusCode)
	}

	var parsed deepSeekResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return Balance{}, fmt.Errorf("decode deepseek balance: %w", err)
	}

	available := parsed.IsAvailable
	out := Balance{
		Channel:     "deepseek",
		Label:       d.label,
		Kind:        KindCurrency,
		OK:          true,
		UpdatedAt:   time.Now().Unix(),
		IsAvailable: &available,
		Currencies:  make([]CurrencyBalance, 0, len(parsed.BalanceInfos)),
	}
	for _, info := range parsed.BalanceInfos {
		out.Currencies = append(out.Currencies, CurrencyBalance{
			Currency:        info.Currency,
			TotalBalance:    info.TotalBalance,
			GrantedBalance:  info.GrantedBalance,
			ToppedUpBalance: info.ToppedUpBalance,
		})
	}
	return out, nil
}
