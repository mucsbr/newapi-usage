package channels

import (
	"context"
	"errors"

	"github.com/mucsbr/newapi-usage/internal/config"
)

var errSub2APINotConfigured = errors.New("sub2api is not configured")

// Manager owns the channel balance providers. DeepSeek is fetched live (cheap),
// while CPA runs a background refresh and is read from cache.
type Manager struct {
	deepseek *deepSeekProvider
	cpa      *cpaProvider
	sub2api  *sub2APIProvider
}

// New builds a Manager from config. It returns a non-nil Manager even when no
// channel is configured; callers should check Enabled.
func New(cfg config.Config) *Manager {
	m := &Manager{}
	if cfg.DeepSeekEnabled() {
		m.deepseek = newDeepSeek(cfg.DeepSeekLabel, cfg.DeepSeekAPIKey, cfg.DeepSeekAPIBase, cfg.QueryTimeout)
	}
	if cfg.CPAEnabled() {
		m.cpa = newCPA(cpaConfig{
			Label:           cfg.CPALabel,
			BaseURL:         cfg.CPABaseURL,
			Token:           cfg.CPAToken,
			TargetType:      cfg.CPATargetType,
			UserAgent:       cfg.CPAUserAgent,
			Concurrency:     cfg.CPAProbeConcurrency,
			ProbeTimeout:    cfg.CPAProbeTimeout,
			RefreshInterval: cfg.CPARefreshInterval,
			MaxAccounts:     cfg.CPAMaxAccounts,
		})
	}
	if cfg.Sub2APIEnabled() {
		m.sub2api = newSub2API(sub2APIConfig{
			Label:    cfg.Sub2APILabel,
			BaseURL:  cfg.Sub2APIBaseURL,
			APIKey:   cfg.Sub2APIKey,
			Timezone: cfg.Sub2APITimezone,
			Timeout:  cfg.Sub2APITimeout,
			PageSize: cfg.Sub2APIPageSize,
			Ikun: ikunConfig{
				Label:             cfg.IkunLabel,
				BaseURL:           cfg.IkunAPIBase,
				AccessToken:       cfg.IkunAccessToken,
				UserID:            cfg.IkunUserID,
				Sub2APIAccountID:  cfg.IkunSub2APIAccountID,
				Sub2APIAccountKey: cfg.IkunSub2APIAccountKey,
			},
		})
	}
	return m
}

// Enabled reports whether any channel is configured.
func (m *Manager) Enabled() bool {
	return m != nil && (m.deepseek != nil || m.cpa != nil || m.sub2api != nil)
}

// Start launches background refresh for providers that need it (CPA).
func (m *Manager) Start(ctx context.Context) {
	if m == nil || m.cpa == nil {
		return
	}
	m.cpa.Start(ctx)
}

// Close waits for background goroutines to finish. The caller must cancel the
// context passed to Start first.
func (m *Manager) Close() {
	if m == nil || m.cpa == nil {
		return
	}
	m.cpa.Close()
}

// Snapshot returns the current balance for every configured channel.
func (m *Manager) Snapshot(ctx context.Context) []Balance {
	if m == nil {
		return []Balance{}
	}
	out := make([]Balance, 0, 3)
	if m.deepseek != nil {
		out = append(out, m.deepseek.Balance(ctx))
	}
	if m.cpa != nil {
		out = append(out, m.cpa.Snapshot())
	}
	if m.sub2api != nil {
		out = append(out, m.sub2api.Balance(ctx))
	}
	return out
}

func (m *Manager) Sub2APIUsage(ctx context.Context, accountID int64, force bool, timezone string) (Sub2APIUsage, error) {
	if m == nil || m.sub2api == nil {
		return Sub2APIUsage{}, errSub2APINotConfigured
	}
	return m.sub2api.FetchUsage(ctx, accountID, force, timezone)
}
