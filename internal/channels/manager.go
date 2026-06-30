package channels

import (
	"context"

	"github.com/mucsbr/newapi-usage/internal/config"
)

// Manager owns the channel balance providers. DeepSeek is fetched live (cheap),
// while CPA runs a background refresh and is read from cache.
type Manager struct {
	deepseek *deepSeekProvider
	cpa      *cpaProvider
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
			Label:                cfg.CPALabel,
			BaseURL:              cfg.CPABaseURL,
			Token:                cfg.CPAToken,
			TargetType:           cfg.CPATargetType,
			UserAgent:            cfg.CPAUserAgent,
			UsedPercentThreshold: cfg.CPAUsedPercentThreshold,
			Concurrency:          cfg.CPAProbeConcurrency,
			ProbeTimeout:         cfg.CPAProbeTimeout,
			RefreshInterval:      cfg.CPARefreshInterval,
			MaxAccounts:          cfg.CPAMaxAccounts,
		})
	}
	return m
}

// Enabled reports whether any channel is configured.
func (m *Manager) Enabled() bool {
	return m != nil && (m.deepseek != nil || m.cpa != nil)
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
	out := make([]Balance, 0, 2)
	if m.deepseek != nil {
		out = append(out, m.deepseek.Balance(ctx))
	}
	if m.cpa != nil {
		out = append(out, m.cpa.Snapshot())
	}
	return out
}
