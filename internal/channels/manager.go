package channels

import (
	"context"
	"errors"

	"github.com/mucsbr/newapi-usage/internal/config"
)

var errSub2APINotConfigured = errors.New("sub2api is not configured")
var errOpenCodeNotConfigured = errors.New("opencode is not configured")
var errXFYunNotConfigured = errors.New("xfyun is not configured")

// Manager owns the channel balance providers. DeepSeek is fetched live (cheap),
// while CPA runs a background refresh and is read from cache.
type Manager struct {
	deepseek *deepSeekProvider
	cpa      *cpaProvider
	sub2api  *sub2APIProvider
	opencode *openCodeProvider
	zhipu    *zhipuProvider
	xfyun    *xfyunProvider
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
	if cfg.OpenCodeEnabled() {
		m.opencode = newOpenCode(openCodeConfig{
			Label:       cfg.OpenCodeLabel,
			BaseURL:     cfg.OpenCodeBaseURL,
			Username:    cfg.OpenCodeUsername,
			Password:    cfg.OpenCodePassword,
			Timeout:     cfg.OpenCodeTimeout,
			Concurrency: cfg.OpenCodeConcurrency,
		})
	}
	if cfg.ZhipuEnabled() {
		m.zhipu = newZhipu(zhipuConfig{
			Label:   cfg.ZhipuLabel,
			BaseURL: cfg.ZhipuAPIBase,
			APIKey:  cfg.ZhipuAPIKey,
			Timeout: cfg.ZhipuTimeout,
		})
	}
	if cfg.XFYunChannelEnabled() {
		m.xfyun = newXFYun(xfyunConfig{
			Enabled:      cfg.XFYunEnabled,
			Label:        cfg.XFYunLabel,
			BaseURL:      cfg.XFYunAPIBase,
			AccountsPath: cfg.XFYunAccountsPath,
			Timeout:      cfg.XFYunTimeout,
			PageSize:     cfg.XFYunPageSize,
		})
	}
	return m
}

// Enabled reports whether any channel is configured.
func (m *Manager) Enabled() bool {
	return m != nil && (m.deepseek != nil || m.cpa != nil || m.sub2api != nil || m.opencode != nil || m.zhipu != nil || m.xfyun != nil)
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
	out := make([]Balance, 0, 6)
	if m.deepseek != nil {
		out = append(out, m.deepseek.Balance(ctx))
	}
	if m.cpa != nil {
		out = append(out, m.cpa.Snapshot())
	}
	if m.sub2api != nil {
		out = append(out, m.sub2api.Balance(ctx))
	}
	if m.opencode != nil {
		out = append(out, m.opencode.Balance(ctx))
	}
	if m.zhipu != nil {
		out = append(out, m.zhipu.Balance(ctx))
	}
	if m.xfyun != nil {
		out = append(out, m.xfyun.Balance(ctx))
	}
	return out
}

func (m *Manager) RefreshOpenCodeUsage(ctx context.Context, accountID string) (OpenCodeUsage, error) {
	if m == nil || m.opencode == nil {
		return OpenCodeUsage{}, errOpenCodeNotConfigured
	}
	return m.opencode.RefreshUsage(ctx, accountID)
}

func (m *Manager) Sub2APIUsage(ctx context.Context, accountID int64, force bool, timezone string) (Sub2APIUsage, error) {
	if m == nil || m.sub2api == nil {
		return Sub2APIUsage{}, errSub2APINotConfigured
	}
	return m.sub2api.FetchUsage(ctx, accountID, force, timezone)
}

func (m *Manager) AddXFYunAccount(ctx context.Context, name string, ssoSessionID string) (XFYunAccount, error) {
	if m == nil || m.xfyun == nil {
		return XFYunAccount{}, errXFYunNotConfigured
	}
	return m.xfyun.AddAccount(ctx, name, ssoSessionID)
}

func (m *Manager) UpdateXFYunAccount(ctx context.Context, id int64, name *string, ssoSessionID *string) (XFYunAccount, error) {
	if m == nil || m.xfyun == nil {
		return XFYunAccount{}, errXFYunNotConfigured
	}
	return m.xfyun.UpdateAccount(ctx, id, name, ssoSessionID)
}

func (m *Manager) DeleteXFYunAccount(id int64) error {
	if m == nil || m.xfyun == nil {
		return errXFYunNotConfigured
	}
	return m.xfyun.DeleteAccount(id)
}
