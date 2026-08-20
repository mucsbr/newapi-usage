package review

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mucsbr/newapi-usage/internal/audit"
)

type workEntry struct {
	ID            int64
	AuditEntryID  int64
	TokenID       int64
	CreatedAt     int64
	ParentEntryID int64
	DeltaStart    int
	ContentHash   string
}

func (m *Manager) runNextJob(ctx context.Context) error {
	job, err := m.nextQueuedJob(ctx)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	var planned int64
	if err := m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM review_job_entries WHERE job_id = ?`, job.ID).Scan(&planned); err != nil {
		return err
	}
	if planned != job.TotalEntries {
		if _, err := m.db.ExecContext(ctx, `UPDATE review_jobs SET status = ?, started_at = CASE WHEN started_at = 0 THEN ? ELSE started_at END, error = '', updated_at = ? WHERE id = ? AND status = ?`,
			StatusPlanning, time.Now().Unix(), time.Now().Unix(), job.ID, StatusQueued); err != nil {
			return err
		}
		job.Status = StatusPlanning
		if err := m.planJob(ctx, job); err != nil {
			if strings.HasPrefix(err.Error(), "job stopped:") {
				return nil
			}
			m.failJob(context.Background(), job.ID, err)
			return err
		}
	}
	if _, err := m.db.ExecContext(ctx, `UPDATE review_jobs SET status = ?, started_at = CASE WHEN started_at = 0 THEN ? ELSE started_at END, updated_at = ? WHERE id = ? AND status IN (?, ?)`,
		StatusRunning, time.Now().Unix(), time.Now().Unix(), job.ID, StatusQueued, StatusPlanning); err != nil {
		return err
	}
	settings, err := m.loadSettings(ctx)
	if err != nil {
		m.failJob(context.Background(), job.ID, err)
		return err
	}
	configHash := reviewConfigHash(settings)
	if job.ConfigHash != "" && job.ConfigHash != configHash {
		err := fmt.Errorf("review configuration changed after job creation")
		m.failJob(context.Background(), job.ID, err)
		return err
	}
	slog.Info("review job started", "job_id", job.ID, "model", settings.Model, "role_mode", job.RoleMode)
	for {
		status, err := m.currentJobStatus(ctx, job.ID)
		if err != nil {
			return err
		}
		if status != StatusRunning {
			return nil
		}
		entry, err := m.nextWorkEntry(ctx, job.ID)
		if err == sql.ErrNoRows {
			_, err = m.db.ExecContext(ctx, `UPDATE review_jobs SET status = ?, completed_at = ?, updated_at = ? WHERE id = ? AND status = ?`,
				StatusCompleted, time.Now().Unix(), time.Now().Unix(), job.ID, StatusRunning)
			if err == nil {
				if completed, loadErr := m.Job(ctx, job.ID); loadErr == nil {
					slog.Info("review job completed", "job_id", job.ID, "entries", completed.ProcessedEntries, "model_calls", completed.ReviewedUnits, "cache_hits", completed.CacheHits, "flagged", completed.FlaggedEntries, "errors", completed.ErrorEntries)
				}
			}
			return err
		}
		if err != nil {
			return err
		}
		if err := m.processEntry(ctx, job, entry, settings, configHash); err != nil {
			if !errors.Is(err, context.Canceled) {
				m.failJob(context.Background(), job.ID, err)
			}
			return err
		}
	}
}

func (m *Manager) nextQueuedJob(ctx context.Context) (Job, error) {
	return scanJob(m.db.QueryRowContext(ctx, jobSelect()+` WHERE status = ? ORDER BY id LIMIT 1`, StatusQueued))
}

func (m *Manager) currentJobStatus(ctx context.Context, jobID int64) (string, error) {
	var status string
	err := m.db.QueryRowContext(ctx, `SELECT status FROM review_jobs WHERE id = ?`, jobID).Scan(&status)
	return status, err
}

func (m *Manager) nextWorkEntry(ctx context.Context, jobID int64) (workEntry, error) {
	var entry workEntry
	err := m.db.QueryRowContext(ctx, `SELECT id, audit_entry_id, token_id, created_at, parent_audit_entry_id, delta_start_index, content_hash
		FROM review_job_entries WHERE job_id = ? AND status = 'pending'
		ORDER BY token_id, created_at, audit_entry_id LIMIT 1`, jobID).Scan(
		&entry.ID, &entry.AuditEntryID, &entry.TokenID, &entry.CreatedAt,
		&entry.ParentEntryID, &entry.DeltaStart, &entry.ContentHash,
	)
	return entry, err
}

func (m *Manager) processEntry(ctx context.Context, job Job, entry workEntry, settings storedSettings, configHash string) error {
	parentDecision, parentScore, parentResultID := m.parentEffective(ctx, job.ID, entry.ParentEntryID)
	delta := Decision{Decision: DecisionPass, Categories: []string{}}
	resultID := int64(0)
	cacheHit := false
	called := false
	usage := tokenUsage{}
	itemErr := ""

	if entry.ContentHash != "" {
		cached, cachedID, err := m.cachedResult(ctx, entry.ContentHash, configHash)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if err == nil {
			delta, resultID, cacheHit = cached, cachedID, true
		} else {
			content, err := m.entryDeltaContent(ctx, entry.AuditEntryID, entry.DeltaStart, job.RoleMode)
			if err != nil {
				itemErr = err.Error()
			} else if contentHash(content) != entry.ContentHash {
				itemErr = "request content changed during review"
			} else {
				delta, usage, err = m.callReviewWithRetry(ctx, settings, content)
				if err != nil {
					itemErr = err.Error()
				} else {
					called = true
					resultID, err = m.storeResult(ctx, entry.ContentHash, configHash, delta, usage)
					if err != nil {
						return err
					}
				}
			}
		}
	}

	status := "done"
	if itemErr != "" {
		status = "error"
		delta = Decision{Decision: DecisionReview, RiskScore: 50, Categories: []string{"其他风险"}, Reason: "审查失败，需要人工确认"}
	}
	effectiveDecision, effectiveScore, inherited := combineDecision(parentDecision, parentScore, delta.Decision, delta.RiskScore)
	effectiveResultID := resultID
	if inherited {
		effectiveResultID = parentResultID
	}
	flagged := effectiveDecision != DecisionPass
	now := time.Now().Unix()
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE review_job_entries SET
		result_id = ?, delta_decision = ?, delta_risk_score = ?, effective_result_id = ?, effective_decision = ?,
		effective_risk_score = ?, inherited = ?, cache_hit = ?, prompt_tokens = ?, completion_tokens = ?,
		status = ?, error = ?, updated_at = ? WHERE id = ? AND status = 'pending'`,
		resultID, delta.Decision, delta.RiskScore, effectiveResultID, effectiveDecision, effectiveScore,
		inherited, cacheHit, usage.Prompt, usage.Completion, status, itemErr, now, entry.ID)
	if err != nil {
		tx.Rollback()
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE review_jobs SET
		processed_entries = processed_entries + 1,
		reviewed_units = reviewed_units + ?, cache_hits = cache_hits + ?,
		flagged_entries = flagged_entries + ?, error_entries = error_entries + ?,
		prompt_tokens = prompt_tokens + ?, completion_tokens = completion_tokens + ?, updated_at = ?
		WHERE id = ?`, boolInt(called), boolInt(cacheHit), boolInt(flagged), boolInt(itemErr != ""),
		usage.Prompt, usage.Completion, now, job.ID)
	if err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (m *Manager) entryDeltaContent(ctx context.Context, auditEntryID int64, start int, roleMode string) (string, error) {
	var body string
	var bodyGzip []byte
	var encoding string
	if err := m.db.QueryRowContext(ctx, `SELECT body, body_gzip, body_encoding FROM audit_entries WHERE id = ?`, auditEntryID).Scan(&body, &bodyGzip, &encoding); err != nil {
		return "", err
	}
	decoded, err := audit.DecodeStoredBody(body, bodyGzip, encoding)
	if err != nil {
		return "", err
	}
	content, _ := deltaContent(audit.NormalizeMessages(decoded), start, roleMode)
	return content, nil
}

func (m *Manager) parentEffective(ctx context.Context, jobID, parentEntryID int64) (string, int, int64) {
	if parentEntryID <= 0 {
		return DecisionPass, 0, 0
	}
	var decision string
	var score int
	var resultID int64
	err := m.db.QueryRowContext(ctx, `SELECT effective_decision, effective_risk_score, effective_result_id FROM review_job_entries
		WHERE job_id = ? AND audit_entry_id = ? AND status IN ('done', 'error')`, jobID, parentEntryID).Scan(&decision, &score, &resultID)
	if err != nil || decision == "" {
		return DecisionPass, 0, 0
	}
	return decision, score, resultID
}

func (m *Manager) cachedResult(ctx context.Context, hash, configHash string) (Decision, int64, error) {
	var decision Decision
	var id int64
	var categories string
	err := m.db.QueryRowContext(ctx, `SELECT id, decision, risk_score, categories, reason, confidence
		FROM review_results WHERE content_hash = ? AND config_hash = ?`, hash, configHash).Scan(
		&id, &decision.Decision, &decision.RiskScore, &categories, &decision.Reason, &decision.Confidence)
	if err != nil {
		return Decision{}, 0, err
	}
	_ = json.Unmarshal([]byte(categories), &decision.Categories)
	return decision, id, nil
}

func (m *Manager) storeResult(ctx context.Context, hash, configHash string, decision Decision, usage tokenUsage) (int64, error) {
	categories, _ := json.Marshal(decision.Categories)
	now := time.Now().Unix()
	_, err := m.db.ExecContext(ctx, `INSERT OR IGNORE INTO review_results (
		content_hash, config_hash, decision, risk_score, categories, reason, confidence,
		prompt_tokens, completion_tokens, created_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		hash, configHash, decision.Decision, decision.RiskScore, string(categories),
		decision.Reason, decision.Confidence, usage.Prompt, usage.Completion, now)
	if err != nil {
		return 0, err
	}
	var id int64
	err = m.db.QueryRowContext(ctx, `SELECT id FROM review_results WHERE content_hash = ? AND config_hash = ?`, hash, configHash).Scan(&id)
	return id, err
}

func (m *Manager) callReviewWithRetry(ctx context.Context, settings storedSettings, content string) (Decision, tokenUsage, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		decision, usage, mode, err := m.callReview(ctx, settings, content, settings.ResponseMode)
		if err == nil {
			if settings.ResponseMode == "auto" && mode != "" {
				_, _ = m.db.ExecContext(ctx, `UPDATE review_settings SET response_mode = ?, updated_at = ? WHERE id = 1`, mode, time.Now().Unix())
				settings.ResponseMode = mode
			}
			return decision, usage, nil
		}
		lastErr = err
		var httpErr *reviewHTTPError
		retryable := asHTTPError(err, &httpErr) && (httpErr.Status == http.StatusTooManyRequests || httpErr.Status >= 500)
		if !retryable && attempt >= 1 {
			break
		}
		delay := time.Duration(1<<attempt) * time.Second
		if retryable {
			delay = retryAfterDelay(httpErr.RetryAfter, delay)
		}
		select {
		case <-ctx.Done():
			return Decision{}, tokenUsage{}, ctx.Err()
		case <-time.After(delay):
		}
	}
	return Decision{}, tokenUsage{}, lastErr
}

func retryAfterDelay(value string, fallback time.Duration) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		delay := time.Duration(seconds) * time.Second
		if delay > 2*time.Minute {
			return 2 * time.Minute
		}
		return delay
	}
	if when, err := http.ParseTime(value); err == nil {
		delay := time.Until(when)
		if delay > 0 {
			if delay > 2*time.Minute {
				return 2 * time.Minute
			}
			return delay
		}
	}
	return fallback
}

func reviewConfigHash(settings storedSettings) string {
	value := strings.Join([]string{settings.BaseURL, settings.Model, settings.Policy, "schema-v1"}, "\x00")
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func contentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func combineDecision(parent string, parentScore int, delta string, deltaScore int) (string, int, bool) {
	if decisionRank(parent) > decisionRank(delta) || (decisionRank(parent) == decisionRank(delta) && parentScore > deltaScore) {
		return parent, parentScore, parent != DecisionPass
	}
	return delta, deltaScore, false
}

func decisionRank(value string) int {
	switch value {
	case DecisionBlock:
		return 2
	case DecisionReview:
		return 1
	default:
		return 0
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (m *Manager) failJob(ctx context.Context, jobID int64, jobErr error) {
	_, _ = m.db.ExecContext(ctx, `UPDATE review_jobs SET status = ?, error = ?, completed_at = ?, updated_at = ? WHERE id = ?`,
		StatusFailed, truncateError(jobErr), time.Now().Unix(), time.Now().Unix(), jobID)
}

func truncateError(err error) string {
	if err == nil {
		return ""
	}
	value := err.Error()
	if len(value) > 1000 {
		value = value[:1000]
	}
	return value
}
