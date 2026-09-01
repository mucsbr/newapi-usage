package review

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

func (m *Manager) CreateJob(ctx context.Context, input JobInput) (Job, error) {
	var cleanupTableExists int
	if err := m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'audit_cleanup_jobs'`).Scan(&cleanupTableExists); err != nil {
		return Job{}, err
	}
	if cleanupTableExists > 0 {
		var activeCleanup int64
		if err := m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_cleanup_jobs WHERE status IN ('queued', 'running')`).Scan(&activeCleanup); err != nil {
			return Job{}, err
		}
		if activeCleanup > 0 {
			return Job{}, fmt.Errorf("audit cleanup is active; wait for it to finish before creating a review job")
		}
	}
	settings, err := m.loadSettings(ctx)
	if err != nil {
		return Job{}, err
	}
	input.TokenIDs = normalizeTokenIDs(input.TokenIDs)
	if len(input.TokenIDs) == 0 {
		return Job{}, fmt.Errorf("at least one key is required")
	}
	if len(input.TokenIDs) > 500 {
		return Job{}, fmt.Errorf("too many keys")
	}
	input.Models = normalizeModels(input.Models)
	if len(input.Models) == 0 {
		return Job{}, fmt.Errorf("at least one request model is required")
	}
	if len(input.Models) > 500 {
		return Job{}, fmt.Errorf("too many request models")
	}
	if input.Start <= 0 || input.End <= 0 || input.End < input.Start {
		return Job{}, fmt.Errorf("invalid time range")
	}
	input.RoleMode = normalizeRoleMode(input.RoleMode)
	placeholders := sqlPlaceholders(len(input.TokenIDs))
	args := make([]any, 0, len(input.TokenIDs)+len(input.Models)+2)
	for _, tokenID := range input.TokenIDs {
		args = append(args, tokenID)
	}
	args = append(args, input.Start, input.End)
	for _, model := range input.Models {
		args = append(args, model)
	}
	query := fmt.Sprintf(`SELECT COALESCE(MAX(id), 0), COUNT(*) FROM audit_entries
		WHERE token_id IN (%s) AND created_at >= ? AND created_at <= ? AND model IN (%s)`,
		placeholders, sqlPlaceholders(len(input.Models)))
	var maxEntryID, totalEntries int64
	if err := m.db.QueryRowContext(ctx, query, args...).Scan(&maxEntryID, &totalEntries); err != nil {
		return Job{}, err
	}
	tokenJSON, _ := json.Marshal(input.TokenIDs)
	modelJSON, _ := json.Marshal(input.Models)
	now := time.Now().Unix()
	configHash := reviewConfigHash(settings)
	result, err := m.db.ExecContext(ctx, `INSERT INTO review_jobs (
		token_ids, models, start_at, end_at, role_mode, review_model, reasoning_effort, concurrency, config_hash, status,
		max_entry_id, total_entries, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(tokenJSON), string(modelJSON), input.Start, input.End, input.RoleMode, settings.Model,
		settings.ReasoningEffort, settings.Concurrency, configHash, StatusQueued, maxEntryID, totalEntries, now, now)
	if err != nil {
		return Job{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Job{}, err
	}
	m.notify()
	return m.Job(ctx, id)
}

func (m *Manager) ModelOptions(ctx context.Context, tokenIDs []int64, start, end int64) ([]ModelOption, error) {
	tokenIDs = normalizeTokenIDs(tokenIDs)
	if len(tokenIDs) == 0 {
		return []ModelOption{}, nil
	}
	if start <= 0 || end <= 0 || end < start {
		return nil, fmt.Errorf("invalid time range")
	}
	args := make([]any, 0, len(tokenIDs)+2)
	for _, tokenID := range tokenIDs {
		args = append(args, tokenID)
	}
	args = append(args, start, end)
	query := fmt.Sprintf(`SELECT model, COUNT(*) FROM audit_entries
		WHERE token_id IN (%s) AND created_at >= ? AND created_at <= ? AND model <> ''
		GROUP BY model ORDER BY COUNT(*) DESC, model LIMIT 500`, sqlPlaceholders(len(tokenIDs)))
	rows, err := m.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]ModelOption, 0)
	for rows.Next() {
		var item ModelOption
		if err := rows.Scan(&item.Model, &item.RequestCount); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (m *Manager) ListJobs(ctx context.Context, limit int) ([]Job, error) {
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	rows, err := m.db.QueryContext(ctx, jobSelect()+` ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Job, 0)
	for rows.Next() {
		item, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (m *Manager) Job(ctx context.Context, id int64) (Job, error) {
	row := m.db.QueryRowContext(ctx, jobSelect()+` WHERE id = ?`, id)
	item, err := scanJob(row)
	if err == sql.ErrNoRows {
		return Job{}, ErrNotFound
	}
	return item, err
}

func (m *Manager) PauseJob(ctx context.Context, id int64) (Job, error) {
	return m.setJobStatus(ctx, id, StatusPaused, []string{StatusQueued, StatusClaimed, StatusPlanning, StatusRunning})
}

func (m *Manager) ResumeJob(ctx context.Context, id int64) (Job, error) {
	job, err := m.setJobStatus(ctx, id, StatusQueued, []string{StatusPaused, StatusFailed})
	if err == nil {
		m.notify()
	}
	return job, err
}

func (m *Manager) CancelJob(ctx context.Context, id int64) (Job, error) {
	return m.setJobStatus(ctx, id, StatusCanceled, []string{StatusQueued, StatusClaimed, StatusPlanning, StatusRunning, StatusPaused, StatusFailed})
}

func (m *Manager) DeleteJob(ctx context.Context, id int64) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM review_jobs WHERE id = ?`, id).Scan(&status); err != nil {
		tx.Rollback()
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		return err
	}
	if status != StatusCompleted && status != StatusCanceled && status != StatusFailed {
		tx.Rollback()
		return fmt.Errorf("active review job cannot be deleted")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM review_job_entries WHERE job_id = ?`, id); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM review_jobs WHERE id = ?`, id); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (m *Manager) setJobStatus(ctx context.Context, id int64, status string, allowed []string) (Job, error) {
	placeholders := sqlPlaceholders(len(allowed))
	args := []any{status, status, time.Now().Unix(), id}
	for _, value := range allowed {
		args = append(args, value)
	}
	result, err := m.db.ExecContext(ctx, fmt.Sprintf(`UPDATE review_jobs SET status = ?, completed_at = CASE WHEN ? = 'queued' THEN 0 ELSE completed_at END, error = '', updated_at = ? WHERE id = ? AND status IN (%s)`, placeholders), args...)
	if err != nil {
		return Job{}, err
	}
	changed, _ := result.RowsAffected()
	if changed == 0 {
		if _, err := m.Job(ctx, id); err != nil {
			return Job{}, err
		}
		return Job{}, fmt.Errorf("job cannot transition to %s", status)
	}
	return m.Job(ctx, id)
}

func (m *Manager) Results(ctx context.Context, jobID int64, filter ResultFilter) (ResultPage, error) {
	if filter.Page <= 0 {
		filter.Page = 1
	}
	if filter.PageSize <= 0 || filter.PageSize > 200 {
		filter.PageSize = 50
	}
	conditions := []string{"e.job_id = ?", "e.status IN ('done', 'error')"}
	args := []any{jobID}
	if filter.TokenID > 0 {
		conditions = append(conditions, "e.token_id = ?")
		args = append(args, filter.TokenID)
	}
	if filter.Decision == DecisionPass || filter.Decision == DecisionReview || filter.Decision == DecisionBlock {
		conditions = append(conditions, "e.effective_decision = ?")
		args = append(args, filter.Decision)
	}
	where := " WHERE " + strings.Join(conditions, " AND ")
	var total int64
	if err := m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM review_job_entries e`+where, args...).Scan(&total); err != nil {
		return ResultPage{}, err
	}
	offset := (filter.Page - 1) * filter.PageSize
	queryArgs := append(append([]any{}, args...), filter.PageSize, offset)
	rows, err := m.db.QueryContext(ctx, `SELECT
		e.id, e.job_id, e.audit_entry_id,
		COALESCE((SELECT MIN(log_id) FROM log_audit_matches WHERE audit_entry_id = e.audit_entry_id), 0),
		e.token_id, e.created_at, e.request_model, e.delta_message_count,
		COALESCE(dr.decision, '通过'), COALESCE(dr.risk_score, 0), COALESCE(dr.categories, '[]'),
		COALESCE(dr.reason, ''), COALESCE(dr.confidence, 0),
		COALESCE(er.decision, e.effective_decision), COALESCE(er.risk_score, e.effective_risk_score),
		COALESCE(er.categories, '[]'), COALESCE(er.reason, ''), COALESCE(er.confidence, 0),
		e.effective_decision, e.effective_risk_score,
		e.inherited, e.cache_hit, e.status, e.error
	FROM review_job_entries e
	LEFT JOIN review_results dr ON dr.id = e.result_id
	LEFT JOIN review_results er ON er.id = e.effective_result_id`+where+`
	ORDER BY e.effective_risk_score DESC, e.created_at DESC, e.audit_entry_id DESC
	LIMIT ? OFFSET ?`, queryArgs...)
	if err != nil {
		return ResultPage{}, err
	}
	defer rows.Close()
	items := make([]ResultItem, 0)
	for rows.Next() {
		var item ResultItem
		var deltaCategories, effectiveCategories string
		if err := rows.Scan(
			&item.ID, &item.JobID, &item.AuditEntryID, &item.LogID, &item.TokenID,
			&item.CreatedAt, &item.RequestModel, &item.DeltaMessageCount,
			&item.DeltaDecision.Decision, &item.DeltaDecision.RiskScore, &deltaCategories,
			&item.DeltaDecision.Reason, &item.DeltaDecision.Confidence,
			&item.EffectiveResult.Decision, &item.EffectiveResult.RiskScore, &effectiveCategories,
			&item.EffectiveResult.Reason, &item.EffectiveResult.Confidence,
			&item.EffectiveDecision, &item.EffectiveRiskScore, &item.Inherited,
			&item.CacheHit, &item.Status, &item.Error,
		); err != nil {
			return ResultPage{}, err
		}
		_ = json.Unmarshal([]byte(deltaCategories), &item.DeltaDecision.Categories)
		_ = json.Unmarshal([]byte(effectiveCategories), &item.EffectiveResult.Categories)
		if item.Status == "error" {
			item.DeltaDecision = Decision{Decision: DecisionReview, RiskScore: 50, Categories: []string{"其他风险"}, Reason: "审查失败，需要人工确认"}
			if len(item.EffectiveResult.Categories) == 0 && item.EffectiveResult.Reason == "" {
				item.EffectiveResult = item.DeltaDecision
			}
		}
		items = append(items, item)
	}
	return ResultPage{Items: items, Total: total, Page: filter.Page, PageSize: filter.PageSize}, rows.Err()
}

func jobSelect() string {
	return `SELECT id, token_ids, models, start_at, end_at, role_mode, review_model, reasoning_effort, concurrency, config_hash, status, max_entry_id,
		total_entries, processed_entries, review_units, reviewed_units, cache_hits,
		flagged_entries, error_entries, estimated_chars, prompt_tokens, completion_tokens,
		error, created_at, started_at, completed_at, updated_at FROM review_jobs`
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanJob(row rowScanner) (Job, error) {
	var item Job
	var tokenJSON, modelJSON string
	err := row.Scan(
		&item.ID, &tokenJSON, &modelJSON, &item.Start, &item.End, &item.RoleMode, &item.ReviewModel, &item.ReasoningEffort, &item.Concurrency, &item.ConfigHash, &item.Status,
		&item.MaxEntryID, &item.TotalEntries, &item.ProcessedEntries, &item.ReviewUnits,
		&item.ReviewedUnits, &item.CacheHits, &item.FlaggedEntries, &item.ErrorEntries,
		&item.EstimatedChars, &item.PromptTokens, &item.CompletionTokens, &item.Error,
		&item.CreatedAt, &item.StartedAt, &item.CompletedAt, &item.UpdatedAt,
	)
	if err != nil {
		return Job{}, err
	}
	_ = json.Unmarshal([]byte(tokenJSON), &item.TokenIDs)
	_ = json.Unmarshal([]byte(modelJSON), &item.Models)
	return item, nil
}

func normalizeTokenIDs(values []int64) []int64 {
	seen := make(map[int64]bool)
	out := make([]int64, 0, len(values))
	for _, value := range values {
		if value > 0 && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func normalizeModels(values []string) []string {
	seen := make(map[string]bool)
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func normalizeRoleMode(value string) string {
	switch value {
	case RoleUserTool, RoleAll:
		return value
	default:
		return RoleUser
	}
}

func sqlPlaceholders(count int) string {
	if count <= 0 {
		return ""
	}
	return strings.TrimRight(strings.Repeat("?,", count), ",")
}
