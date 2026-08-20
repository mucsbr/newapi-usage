package review

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"
	"time"
)

type storedSettings struct {
	Settings
	APIKey string
}

func (m *Manager) Settings(ctx context.Context) (Settings, error) {
	settings, err := m.loadSettings(ctx)
	if err != nil {
		if err == ErrNotConfigured {
			return Settings{Policy: DefaultPolicy, ResponseMode: "auto", ReasoningEffort: ReasoningAuto, Concurrency: 5}, nil
		}
		return Settings{}, err
	}
	return settings.Settings, nil
}

func (m *Manager) SaveSettings(ctx context.Context, input SettingsInput) (Settings, error) {
	var activeJobs int64
	if err := m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM review_jobs WHERE status IN ('queued', 'claimed', 'planning', 'running', 'paused')`).Scan(&activeJobs); err != nil {
		return Settings{}, err
	}
	if activeJobs > 0 {
		return Settings{}, fmt.Errorf("finish or cancel active review jobs before changing configuration")
	}
	baseURL, err := normalizeBaseURL(input.BaseURL)
	if err != nil {
		return Settings{}, err
	}
	model := strings.TrimSpace(input.Model)
	if model == "" {
		return Settings{}, fmt.Errorf("model is required")
	}
	policy := strings.TrimSpace(input.Policy)
	if policy == "" {
		policy = DefaultPolicy
	}
	reasoningEffort := normalizeReasoningEffort(input.ReasoningEffort)
	concurrency := normalizeConcurrency(input.Concurrency)

	apiKey := strings.TrimSpace(input.APIKey)
	var cipherText []byte
	keyTail := ""
	responseMode := "auto"
	if current, loadErr := m.loadSettings(ctx); loadErr == nil {
		if current.BaseURL == baseURL && current.Model == model && current.Policy == policy && current.ReasoningEffort == reasoningEffort {
			responseMode = current.ResponseMode
		}
		if apiKey == "" {
			apiKey = current.APIKey
		}
	}
	if apiKey == "" {
		return Settings{}, fmt.Errorf("api key is required")
	}
	cipherText, err = m.encrypt(apiKey)
	if err != nil {
		return Settings{}, err
	}
	keyTail = tail(apiKey, 6)
	now := time.Now().Unix()
	_, err = m.db.ExecContext(ctx, `INSERT INTO review_settings (
		id, base_url, api_key_cipher, key_tail, model, policy, response_mode, reasoning_effort, concurrency, updated_at
	) VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET
		base_url = excluded.base_url,
		api_key_cipher = excluded.api_key_cipher,
		key_tail = excluded.key_tail,
		model = excluded.model,
		policy = excluded.policy,
		response_mode = excluded.response_mode,
		reasoning_effort = excluded.reasoning_effort,
		concurrency = excluded.concurrency,
		updated_at = excluded.updated_at`,
		baseURL, cipherText, keyTail, model, policy, responseMode, reasoningEffort, concurrency, now)
	if err != nil {
		return Settings{}, err
	}
	return Settings{
		BaseURL:         baseURL,
		Model:           model,
		Policy:          policy,
		ResponseMode:    responseMode,
		ReasoningEffort: reasoningEffort,
		Concurrency:     concurrency,
		KeyConfigured:   true,
		KeyTail:         keyTail,
		UpdatedAt:       now,
	}, nil
}

func (m *Manager) TestSettings(ctx context.Context, input SettingsInput) (TestResult, error) {
	settings, err := m.settingsFromInput(ctx, input)
	if err != nil {
		return TestResult{}, err
	}
	decision, usage, mode, effort, err := m.callReview(ctx, settings, "这是一个用于测试审查接口连通性的普通文本。", "auto")
	if err != nil {
		return TestResult{}, err
	}
	_ = usage
	if stored, loadErr := m.loadSettings(ctx); loadErr == nil && stored.BaseURL == settings.BaseURL && stored.Model == settings.Model {
		_, _ = m.db.ExecContext(ctx, `UPDATE review_settings SET response_mode = ?, reasoning_effort = ?, updated_at = ? WHERE id = 1`, mode, effort, time.Now().Unix())
	}
	return TestResult{OK: true, ResponseMode: mode, ReasoningEffort: effort, Decision: decision}, nil
}

func (m *Manager) loadSettings(ctx context.Context) (storedSettings, error) {
	var settings storedSettings
	var cipherText []byte
	err := m.db.QueryRowContext(ctx, `SELECT base_url, api_key_cipher, key_tail, model, policy, response_mode, reasoning_effort, concurrency, updated_at FROM review_settings WHERE id = 1`).Scan(
		&settings.BaseURL,
		&cipherText,
		&settings.KeyTail,
		&settings.Model,
		&settings.Policy,
		&settings.ResponseMode,
		&settings.ReasoningEffort,
		&settings.Concurrency,
		&settings.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return storedSettings{}, ErrNotConfigured
	}
	if err != nil {
		return storedSettings{}, err
	}
	settings.APIKey, err = m.decrypt(cipherText)
	if err != nil {
		return storedSettings{}, err
	}
	settings.KeyConfigured = settings.APIKey != ""
	if settings.BaseURL == "" || settings.Model == "" || settings.APIKey == "" {
		return storedSettings{}, ErrNotConfigured
	}
	if settings.Policy == "" {
		settings.Policy = DefaultPolicy
	}
	if settings.ResponseMode == "" {
		settings.ResponseMode = "auto"
	}
	settings.ReasoningEffort = normalizeReasoningEffort(settings.ReasoningEffort)
	settings.Concurrency = normalizeConcurrency(settings.Concurrency)
	return settings, nil
}

func (m *Manager) settingsFromInput(ctx context.Context, input SettingsInput) (storedSettings, error) {
	var current storedSettings
	current, _ = m.loadSettings(ctx)
	baseURL := strings.TrimSpace(input.BaseURL)
	if baseURL == "" {
		baseURL = current.BaseURL
	}
	normalized, err := normalizeBaseURL(baseURL)
	if err != nil {
		return storedSettings{}, err
	}
	model := strings.TrimSpace(input.Model)
	if model == "" {
		model = current.Model
	}
	apiKey := strings.TrimSpace(input.APIKey)
	if apiKey == "" {
		apiKey = current.APIKey
	}
	policy := strings.TrimSpace(input.Policy)
	if policy == "" {
		policy = current.Policy
	}
	if policy == "" {
		policy = DefaultPolicy
	}
	reasoningEffort := strings.TrimSpace(input.ReasoningEffort)
	if reasoningEffort == "" {
		reasoningEffort = current.ReasoningEffort
	}
	reasoningEffort = normalizeReasoningEffort(reasoningEffort)
	concurrency := input.Concurrency
	if concurrency <= 0 {
		concurrency = current.Concurrency
	}
	concurrency = normalizeConcurrency(concurrency)
	if model == "" || apiKey == "" {
		return storedSettings{}, ErrNotConfigured
	}
	return storedSettings{Settings: Settings{BaseURL: normalized, Model: model, Policy: policy, ReasoningEffort: reasoningEffort, Concurrency: concurrency}, APIKey: apiKey}, nil
}

func normalizeReasoningEffort(value string) string {
	switch strings.TrimSpace(value) {
	case ReasoningOmit, ReasoningNoThink, ReasoningLow, ReasoningHigh:
		return strings.TrimSpace(value)
	default:
		return ReasoningAuto
	}
}

func normalizeConcurrency(value int) int {
	if value <= 0 {
		return 5
	}
	if value > 20 {
		return 20
	}
	return value
}

func normalizeBaseURL(value string) (string, error) {
	value = strings.TrimRight(strings.TrimSpace(value), "/")
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("invalid base url")
	}
	return value, nil
}

func tail(value string, size int) string {
	if size <= 0 || len(value) <= size {
		return value
	}
	return value[len(value)-size:]
}
