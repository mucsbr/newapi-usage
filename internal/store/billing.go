package store

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/mucsbr/newapi-usage/internal/config"
)

const (
	defaultQuotaPerUnit = 500000.0
	defaultUSDToCNY     = 7.3
)

type billingSettings struct {
	quotaPerUnit float64
	usdToCNY     float64
}

type logBillingBreakdown struct {
	quotaCNY          float64
	cacheReadTokens   int64
	cacheWriteTokens  int64
	inputCostCNY      float64
	outputCostCNY     float64
	cacheReadCostCNY  float64
	cacheWriteCostCNY float64
	otherCostCNY      float64
}

type logBillingMeta struct {
	modelRatio              float64
	groupRatio              float64
	completionRatio         float64
	cacheRatio              float64
	cacheCreationRatio      float64
	cacheCreation5mRatio    float64
	cacheCreation1hRatio    float64
	cacheReadTokens         int64
	cacheWriteTokens        int64
	cacheCreationTokens     int64
	cacheCreationTokens5m   int64
	cacheCreationTokens1h   int64
	inputTokensTotal        int64
	anthropicUsageSemantic  bool
	tieredBilling           bool
	modelPrice              float64
	hasRatioBillingMetadata bool
}

func (s *Store) billingSettings(ctx context.Context) billingSettings {
	settings := billingSettings{
		quotaPerUnit: defaultQuotaPerUnit,
		usdToCNY:     defaultUSDToCNY,
	}
	for _, item := range []struct {
		key   string
		apply func(float64)
	}{
		{"QuotaPerUnit", func(value float64) { settings.quotaPerUnit = value }},
		{"USDExchangeRate", func(value float64) { settings.usdToCNY = value }},
	} {
		value, err := s.optionValue(ctx, item.key)
		if err != nil {
			continue
		}
		parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err == nil && parsed > 0 {
			item.apply(parsed)
		}
	}
	return settings
}

func (s *Store) optionValue(ctx context.Context, key string) (string, error) {
	query := fmt.Sprintf("SELECT value FROM options WHERE %s = %s LIMIT 1", s.optionKeyColumn(), s.placeholder(1))
	var value string
	err := s.db.QueryRowContext(ctx, query, key).Scan(&value)
	return value, err
}

func (s *Store) optionKeyColumn() string {
	if s.driver == config.DriverPostgres {
		return `"key"`
	}
	return "`key`"
}

func quotaToCNY(quota float64, settings billingSettings) float64 {
	if settings.quotaPerUnit <= 0 || settings.usdToCNY <= 0 {
		return 0
	}
	return quota / settings.quotaPerUnit * settings.usdToCNY
}

func (s *Store) enrichUsageLog(item *UsageLog, settings billingSettings) {
	if item == nil {
		return
	}
	breakdown := calculateLogBilling(item.Quota, item.InputTokens, item.OutputTokens, item.Other, settings)
	item.QuotaCNY = breakdown.quotaCNY
	item.CacheReadTokens = breakdown.cacheReadTokens
	item.CacheWriteTokens = breakdown.cacheWriteTokens
}

func (s *Store) modelUsageWithBilling(ctx context.Context, filter ModelFilter) ([]ModelUsage, error) {
	ctx, cancel := s.context(ctx)
	defer cancel()
	if filter.TokenID <= 0 {
		return []ModelUsage{}, nil
	}
	if filter.Limit <= 0 || filter.Limit > 500 {
		filter.Limit = 100
	}

	args := []any{filter.TokenID}
	conditions := []string{"l.type IN (2, 5)", "l.token_id = " + s.placeholder(len(args))}
	if filter.Start > 0 {
		args = append(args, filter.Start)
		conditions = append(conditions, "l.created_at >= "+s.placeholder(len(args)))
	}
	if filter.End > 0 {
		args = append(args, filter.End)
		conditions = append(conditions, "l.created_at <= "+s.placeholder(len(args)))
	}
	query := `
		SELECT
			COALESCE(NULLIF(l.model_name, ''), 'unknown'),
			COALESCE(l.type, 0),
			COALESCE(l.prompt_tokens, 0),
			COALESCE(l.completion_tokens, 0),
			COALESCE(l.quota, 0),
			COALESCE(l.created_at, 0),
			COALESCE(l.other, '')
		FROM logs l
		WHERE ` + strings.Join(conditions, " AND ")

	settings := s.billingSettings(ctx)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byModel := make(map[string]*ModelUsage)
	for rows.Next() {
		var modelName string
		var logType int64
		var promptTokens, completionTokens, quota, createdAt int64
		var other string
		if err := rows.Scan(&modelName, &logType, &promptTokens, &completionTokens, &quota, &createdAt, &other); err != nil {
			return nil, err
		}
		item := byModel[modelName]
		if item == nil {
			item = &ModelUsage{ModelName: modelName}
			byModel[modelName] = item
		}
		item.RequestCount++
		if logType == 2 {
			item.SuccessCount++
		} else if logType == 5 {
			item.ErrorCount++
		}
		item.InputTokens += promptTokens
		item.OutputTokens += completionTokens
		item.TotalTokens += promptTokens + completionTokens
		item.Quota += quota
		if item.FirstUsedAt == 0 || (createdAt > 0 && createdAt < item.FirstUsedAt) {
			item.FirstUsedAt = createdAt
		}
		if createdAt > item.LastUsedAt {
			item.LastUsedAt = createdAt
		}

		breakdown := calculateLogBilling(quota, promptTokens, completionTokens, other, settings)
		item.QuotaCNY += breakdown.quotaCNY
		item.CacheReadTokens += breakdown.cacheReadTokens
		item.CacheWriteTokens += breakdown.cacheWriteTokens
		item.InputCostCNY += breakdown.inputCostCNY
		item.OutputCostCNY += breakdown.outputCostCNY
		item.CacheReadCostCNY += breakdown.cacheReadCostCNY
		item.CacheWriteCostCNY += breakdown.cacheWriteCostCNY
		item.OtherCostCNY += breakdown.otherCostCNY
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	items := make([]ModelUsage, 0, len(byModel))
	for _, item := range byModel {
		items = append(items, *item)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].TotalTokens == items[j].TotalTokens {
			if items[i].RequestCount == items[j].RequestCount {
				return items[i].ModelName < items[j].ModelName
			}
			return items[i].RequestCount > items[j].RequestCount
		}
		return items[i].TotalTokens > items[j].TotalTokens
	})
	if len(items) > filter.Limit {
		items = items[:filter.Limit]
	}
	return items, nil
}

func calculateLogBilling(quota, promptTokens, completionTokens int64, other string, settings billingSettings) logBillingBreakdown {
	result := logBillingBreakdown{quotaCNY: quotaToCNY(float64(quota), settings)}
	meta := parseLogBillingMeta(other)
	result.cacheReadTokens = meta.cacheReadTokens
	result.cacheWriteTokens = meta.cacheWriteTokens
	if quota == 0 {
		return result
	}
	if !meta.hasRatioBillingMetadata || meta.tieredBilling || meta.modelPrice != 0 {
		result.otherCostCNY = result.quotaCNY
		return result
	}

	scale := meta.modelRatio * meta.groupRatio
	baseInputTokens := promptTokens
	if !meta.anthropicUsageSemantic {
		baseInputTokens -= meta.cacheReadTokens + meta.cacheWriteTokens
		if baseInputTokens < 0 {
			baseInputTokens = 0
		}
	}

	inputQuota := float64(baseInputTokens) * scale
	outputQuota := float64(completionTokens) * meta.completionRatio * scale
	cacheReadQuota := float64(meta.cacheReadTokens) * meta.cacheRatio * scale
	cacheWriteQuota := cacheWriteQuota(meta) * scale
	componentQuota := inputQuota + outputQuota + cacheReadQuota + cacheWriteQuota
	otherQuota := float64(quota) - componentQuota
	if math.Abs(otherQuota) < 1 {
		otherQuota = 0
	}

	result.inputCostCNY = quotaToCNY(inputQuota, settings)
	result.outputCostCNY = quotaToCNY(outputQuota, settings)
	result.cacheReadCostCNY = quotaToCNY(cacheReadQuota, settings)
	result.cacheWriteCostCNY = quotaToCNY(cacheWriteQuota, settings)
	result.otherCostCNY = quotaToCNY(otherQuota, settings)
	return result
}

func cacheWriteQuota(meta logBillingMeta) float64 {
	if meta.cacheWriteTokens == 0 {
		return 0
	}
	if meta.cacheCreationTokens5m == 0 && meta.cacheCreationTokens1h == 0 {
		return float64(meta.cacheWriteTokens) * meta.cacheCreationRatio
	}
	regular := meta.cacheWriteTokens - meta.cacheCreationTokens5m - meta.cacheCreationTokens1h
	if regular < 0 {
		regular = 0
	}
	return float64(regular)*meta.cacheCreationRatio +
		float64(meta.cacheCreationTokens5m)*meta.cacheCreation5mRatio +
		float64(meta.cacheCreationTokens1h)*meta.cacheCreation1hRatio
}

func parseLogBillingMeta(other string) logBillingMeta {
	var raw map[string]json.RawMessage
	if json.Unmarshal([]byte(other), &raw) != nil || raw == nil {
		return logBillingMeta{}
	}
	meta := logBillingMeta{
		modelRatio:             rawFloat(raw, "model_ratio"),
		groupRatio:             rawFloat(raw, "group_ratio"),
		completionRatio:        rawFloat(raw, "completion_ratio"),
		cacheRatio:             rawFloat(raw, "cache_ratio"),
		cacheCreationRatio:     rawFloat(raw, "cache_creation_ratio"),
		cacheCreation5mRatio:   rawFloat(raw, "cache_creation_ratio_5m"),
		cacheCreation1hRatio:   rawFloat(raw, "cache_creation_ratio_1h"),
		cacheReadTokens:        rawInt(raw, "cache_tokens"),
		cacheCreationTokens:    rawInt(raw, "cache_creation_tokens"),
		cacheCreationTokens5m:  rawInt(raw, "cache_creation_tokens_5m"),
		cacheCreationTokens1h:  rawInt(raw, "cache_creation_tokens_1h"),
		inputTokensTotal:       rawInt(raw, "input_tokens_total"),
		modelPrice:             rawFloat(raw, "model_price"),
		anthropicUsageSemantic: rawString(raw, "usage_semantic") == "anthropic" || rawBool(raw, "claude"),
		tieredBilling:          rawString(raw, "billing_mode") == "tiered_expr",
	}
	meta.cacheWriteTokens = rawInt(raw, "cache_write_tokens")
	if meta.cacheWriteTokens == 0 {
		meta.cacheWriteTokens = cacheWriteTokens(meta.cacheCreationTokens, meta.cacheCreationTokens5m, meta.cacheCreationTokens1h)
	}
	_, hasModelRatio := raw["model_ratio"]
	_, hasGroupRatio := raw["group_ratio"]
	_, hasCompletionRatio := raw["completion_ratio"]
	meta.hasRatioBillingMetadata = hasModelRatio && hasGroupRatio && hasCompletionRatio
	return meta
}

func cacheWriteTokens(total, fiveMinute, oneHour int64) int64 {
	split := fiveMinute + oneHour
	if split > 0 && total < split {
		return split
	}
	return total
}

func rawFloat(raw map[string]json.RawMessage, key string) float64 {
	value, ok := raw[key]
	if !ok {
		return 0
	}
	var number float64
	if json.Unmarshal(value, &number) == nil {
		return number
	}
	var text string
	if json.Unmarshal(value, &text) == nil {
		number, _ = strconv.ParseFloat(strings.TrimSpace(text), 64)
		return number
	}
	return 0
}

func rawInt(raw map[string]json.RawMessage, key string) int64 {
	return int64(math.Round(rawFloat(raw, key)))
}

func rawString(raw map[string]json.RawMessage, key string) string {
	var value string
	_ = json.Unmarshal(raw[key], &value)
	return value
}

func rawBool(raw map[string]json.RawMessage, key string) bool {
	var value bool
	_ = json.Unmarshal(raw[key], &value)
	return value
}
