package audit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	CleanupStatusQueued    = "queued"
	CleanupStatusRunning   = "running"
	CleanupStatusCompleted = "completed"
	CleanupStatusCanceled  = "canceled"
	CleanupStatusFailed    = "failed"

	cleanupEntryBatchSize = 200
	cleanupAlertBatchSize = 500
	cleanupBatchPause     = 100 * time.Millisecond
)

var (
	ErrCleanupPreparing = errors.New("cleanup index is still preparing")
	ErrCleanupConflict  = errors.New("another maintenance task is active")
	ErrCleanupNotFound  = errors.New("cleanup job not found")
)

type CleanupOverview struct {
	Enabled           bool         `json:"enabled"`
	IndexReady        bool         `json:"index_ready"`
	IndexError        string       `json:"index_error,omitempty"`
	OldestAt          int64        `json:"oldest_at"`
	NewestAt          int64        `json:"newest_at"`
	TotalEntries      int64        `json:"total_entries"`
	DefaultStart      int64        `json:"default_start"`
	DefaultEnd        int64        `json:"default_end"`
	DatabaseBytes     int64        `json:"database_bytes"`
	ReusableBytes     int64        `json:"reusable_bytes"`
	LatestCleanupJobs []CleanupJob `json:"latest_cleanup_jobs"`
}

type CleanupEstimate struct {
	StartAt             int64 `json:"start_at"`
	EndAt               int64 `json:"end_at"`
	FirstMatchedAt      int64 `json:"first_matched_at"`
	LastMatchedAt       int64 `json:"last_matched_at"`
	EntryCount          int64 `json:"entry_count"`
	AlertCount          int64 `json:"alert_count"`
	MatchCount          int64 `json:"match_count"`
	ReviewEntryCount    int64 `json:"review_entry_count"`
	EstimatedBytes      int64 `json:"estimated_bytes"`
	DatabaseBytes       int64 `json:"database_bytes"`
	ReusableBytesBefore int64 `json:"reusable_bytes_before"`
}

type CleanupJob struct {
	ID                  int64  `json:"id"`
	StartAt             int64  `json:"start_at"`
	EndAt               int64  `json:"end_at"`
	Status              string `json:"status"`
	EstimatedEntries    int64  `json:"estimated_entries"`
	EstimatedAlerts     int64  `json:"estimated_alerts"`
	EstimatedMatches    int64  `json:"estimated_matches"`
	EstimatedReviewRows int64  `json:"estimated_review_entries"`
	EstimatedBytes      int64  `json:"estimated_bytes"`
	DeletedEntries      int64  `json:"deleted_entries"`
	DeletedAlerts       int64  `json:"deleted_alerts"`
	DeletedMatches      int64  `json:"deleted_matches"`
	DeletedReviewRows   int64  `json:"deleted_review_entries"`
	DeletedBytes        int64  `json:"deleted_bytes"`
	CancelRequested     bool   `json:"cancel_requested"`
	Error               string `json:"error,omitempty"`
	CreatedAt           int64  `json:"created_at"`
	StartedAt           int64  `json:"started_at"`
	CompletedAt         int64  `json:"completed_at"`
	UpdatedAt           int64  `json:"updated_at"`
}

func (i *Indexer) CleanupOverview(ctx context.Context) (CleanupOverview, error) {
	if i == nil {
		return CleanupOverview{Enabled: false}, nil
	}
	ready, indexErr := i.cleanupIndexStatus()
	databaseBytes, reusableBytes, err := i.databaseSpace(ctx)
	if err != nil {
		return CleanupOverview{}, err
	}
	out := CleanupOverview{
		Enabled:       i.Enabled(),
		IndexReady:    ready,
		IndexError:    indexErr,
		DefaultEnd:    time.Now().AddDate(0, 0, -30).Unix(),
		DatabaseBytes: databaseBytes,
		ReusableBytes: reusableBytes,
	}
	jobs, err := i.ListCleanupJobs(ctx, 10)
	if err != nil {
		return CleanupOverview{}, err
	}
	out.LatestCleanupJobs = jobs
	if !ready {
		return out, nil
	}
	if err := i.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MIN(created_at), 0), COALESCE(MAX(created_at), 0)
		FROM audit_entries INDEXED BY idx_audit_entries_created_at`).Scan(
		&out.TotalEntries, &out.OldestAt, &out.NewestAt,
	); err != nil {
		return CleanupOverview{}, err
	}
	out.DefaultStart = out.OldestAt
	return out, nil
}

func (i *Indexer) EstimateCleanup(ctx context.Context, startAt, endAt int64) (CleanupEstimate, error) {
	if err := validateCleanupRange(startAt, endAt); err != nil {
		return CleanupEstimate{}, err
	}
	ready, _ := i.cleanupIndexStatus()
	if !ready {
		return CleanupEstimate{}, ErrCleanupPreparing
	}

	out := CleanupEstimate{StartAt: startAt, EndAt: endAt}
	if err := i.db.QueryRowContext(ctx, `SELECT COUNT(*),
		COALESCE(MIN(created_at), 0), COALESCE(MAX(created_at), 0)
		FROM audit_entries INDEXED BY idx_audit_entries_created_at
		WHERE created_at >= ? AND created_at <= ?`, startAt, endAt).Scan(
		&out.EntryCount, &out.FirstMatchedAt, &out.LastMatchedAt,
	); err != nil {
		return CleanupEstimate{}, err
	}
	var sampleCount, sampleBytes int64
	if out.EntryCount > 0 {
		if err := i.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(stored_bytes), 0) FROM (
			SELECT COALESCE(length(body_gzip), 0) + COALESCE(length(CAST(body AS BLOB)), 0) + 384 AS stored_bytes
			FROM audit_entries INDEXED BY idx_audit_entries_created_at
			WHERE created_at >= ? AND created_at <= ?
			ORDER BY created_at, id LIMIT 1000
		)`, startAt, endAt).Scan(&sampleCount, &sampleBytes); err != nil {
			return CleanupEstimate{}, err
		}
	}
	var entryBytes int64
	if sampleCount > 0 {
		entryBytes = sampleBytes * out.EntryCount / sampleCount
	}

	var alertBytes int64
	if err := i.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(
		COALESCE(length(CAST(response_body AS BLOB)), 0) +
		COALESCE(length(CAST(source_id AS BLOB)), 0) + COALESCE(length(CAST(source_path AS BLOB)), 0) +
		COALESCE(length(CAST(request_id AS BLOB)), 0) + COALESCE(length(CAST(model AS BLOB)), 0) +
		COALESCE(length(CAST(key_hash AS BLOB)), 0) + COALESCE(length(CAST(matched_text AS BLOB)), 0) + 256
	), 0) FROM audit_security_alerts a
	WHERE (a.request_at >= ? AND a.request_at <= ?)
		OR (a.request_id <> '' AND EXISTS (
			SELECT 1 FROM audit_entries e INDEXED BY idx_audit_entries_request_id
			WHERE e.request_id = a.request_id AND e.created_at >= ? AND e.created_at <= ?
		))`, startAt, endAt, startAt, endAt).Scan(&out.AlertCount, &alertBytes); err != nil {
		return CleanupEstimate{}, err
	}

	if err := i.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_entries e INDEXED BY idx_audit_entries_created_at
		JOIN log_audit_matches m ON m.audit_entry_id = e.id
		WHERE e.created_at >= ? AND e.created_at <= ?`, startAt, endAt).Scan(&out.MatchCount); err != nil {
		return CleanupEstimate{}, err
	}

	var reviewBytes int64
	if exists, err := i.tableExists(ctx, "review_job_entries"); err != nil {
		return CleanupEstimate{}, err
	} else if exists {
		if err := i.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(
			COALESCE(length(CAST(r.content_hash AS BLOB)), 0) + COALESCE(length(CAST(r.error AS BLOB)), 0) + 256
		), 0)
		FROM audit_entries e INDEXED BY idx_audit_entries_created_at
		JOIN review_job_entries r ON r.audit_entry_id = e.id
		WHERE e.created_at >= ? AND e.created_at <= ?`, startAt, endAt).Scan(&out.ReviewEntryCount, &reviewBytes); err != nil {
			return CleanupEstimate{}, err
		}
	}

	out.EstimatedBytes = entryBytes + alertBytes + reviewBytes + out.MatchCount*96
	databaseBytes, reusableBytes, err := i.databaseSpace(ctx)
	out.DatabaseBytes = databaseBytes
	out.ReusableBytesBefore = reusableBytes
	return out, err
}

func (i *Indexer) CreateCleanupJob(ctx context.Context, startAt, endAt int64) (CleanupJob, error) {
	estimate, err := i.EstimateCleanup(ctx, startAt, endAt)
	if err != nil {
		return CleanupJob{}, err
	}
	if estimate.EntryCount == 0 && estimate.AlertCount == 0 {
		return CleanupJob{}, fmt.Errorf("selected time range has no audit data")
	}

	tx, err := i.db.BeginTx(ctx, nil)
	if err != nil {
		return CleanupJob{}, err
	}
	defer tx.Rollback()
	var active int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_cleanup_jobs WHERE status IN (?, ?)`, CleanupStatusQueued, CleanupStatusRunning).Scan(&active); err != nil {
		return CleanupJob{}, err
	}
	if active > 0 {
		return CleanupJob{}, ErrCleanupConflict
	}
	if exists, err := tableExistsTx(ctx, tx, "review_jobs"); err != nil {
		return CleanupJob{}, err
	} else if exists {
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM review_jobs WHERE status IN ('queued', 'claimed', 'planning', 'running', 'paused')`).Scan(&active); err != nil {
			return CleanupJob{}, err
		}
		if active > 0 {
			return CleanupJob{}, fmt.Errorf("%w: pause, cancel, or finish active review jobs first", ErrCleanupConflict)
		}
	}

	now := time.Now().Unix()
	result, err := tx.ExecContext(ctx, `INSERT INTO audit_cleanup_jobs (
		start_at, end_at, status, estimated_entries, estimated_alerts, estimated_matches,
		estimated_review_entries, estimated_bytes, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		startAt, endAt, CleanupStatusQueued, estimate.EntryCount, estimate.AlertCount, estimate.MatchCount,
		estimate.ReviewEntryCount, estimate.EstimatedBytes, now, now)
	if err != nil {
		return CleanupJob{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return CleanupJob{}, err
	}
	if err := tx.Commit(); err != nil {
		return CleanupJob{}, err
	}
	i.notifyCleanupWorker()
	return i.CleanupJob(ctx, id)
}

func (i *Indexer) CleanupJob(ctx context.Context, id int64) (CleanupJob, error) {
	if id <= 0 {
		return CleanupJob{}, ErrCleanupNotFound
	}
	job, err := scanCleanupJob(i.db.QueryRowContext(ctx, cleanupJobSelect()+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return CleanupJob{}, ErrCleanupNotFound
	}
	return job, err
}

func (i *Indexer) ListCleanupJobs(ctx context.Context, limit int) ([]CleanupJob, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	rows, err := i.db.QueryContext(ctx, cleanupJobSelect()+` ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]CleanupJob, 0, limit)
	for rows.Next() {
		item, err := scanCleanupJob(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (i *Indexer) CancelCleanupJob(ctx context.Context, id int64) (CleanupJob, error) {
	now := time.Now().Unix()
	result, err := i.db.ExecContext(ctx, `UPDATE audit_cleanup_jobs SET
		cancel_requested = 1,
		status = CASE WHEN status = ? THEN ? ELSE status END,
		completed_at = CASE WHEN status = ? THEN ? ELSE completed_at END,
		updated_at = ?
		WHERE id = ? AND status IN (?, ?)`,
		CleanupStatusQueued, CleanupStatusCanceled, CleanupStatusQueued, now, now, id, CleanupStatusQueued, CleanupStatusRunning)
	if err != nil {
		return CleanupJob{}, err
	}
	changed, _ := result.RowsAffected()
	if changed == 0 {
		if _, err := i.CleanupJob(ctx, id); err != nil {
			return CleanupJob{}, err
		}
		return CleanupJob{}, fmt.Errorf("cleanup job cannot be canceled")
	}
	return i.CleanupJob(ctx, id)
}

func (i *Indexer) runCleanupIndexPreparation(ctx context.Context) {
	for {
		ready, _ := i.cleanupIndexStatus()
		if ready {
			i.notifyCleanupWorker()
			return
		}
		slog.Info("audit cleanup index preparation started")
		err := i.prepareCleanupIndexes(ctx)
		i.setCleanupIndexStatus(err == nil, err)
		if err == nil {
			slog.Info("audit cleanup index preparation completed")
			i.notifyCleanupWorker()
			return
		}
		if ctx.Err() != nil {
			return
		}
		slog.Warn("audit cleanup index preparation failed", "error", err)
		timer := time.NewTimer(30 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (i *Indexer) prepareCleanupIndexes(ctx context.Context) error {
	db, err := sql.Open("sqlite", i.cfg.IndexDSN)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.ExecContext(ctx, `PRAGMA busy_timeout=60000`); err != nil {
		return err
	}
	statements := []string{
		`CREATE INDEX IF NOT EXISTS idx_audit_entries_created_at ON audit_entries(created_at, id)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_security_alerts_request_at ON audit_security_alerts(request_at, id)`,
		`CREATE INDEX IF NOT EXISTS idx_review_job_entries_audit_entry ON review_job_entries(audit_entry_id)`,
	}
	for _, statement := range statements {
		if strings.Contains(statement, "review_job_entries") {
			exists, err := tableExistsDB(ctx, db, "review_job_entries")
			if err != nil {
				return err
			}
			if !exists {
				continue
			}
		}
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func (i *Indexer) runCleanupJobs(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if ready, _ := i.cleanupIndexStatus(); ready {
			if err := i.runNextCleanupJob(ctx); err != nil && !errors.Is(err, context.Canceled) {
				slog.Warn("audit cleanup job processing failed", "error", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-i.cleanupWake:
		}
	}
}

func (i *Indexer) runNextCleanupJob(ctx context.Context) error {
	job, err := i.claimNextCleanupJob(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	slog.Info("audit cleanup job started", "job_id", job.ID, "start_at", job.StartAt, "end_at", job.EndAt, "estimated_entries", job.EstimatedEntries, "estimated_bytes", job.EstimatedBytes)
	if err := i.executeCleanupJob(ctx, job); err != nil {
		if errors.Is(err, context.Canceled) {
			return err
		}
		if failErr := i.failCleanupJob(context.Background(), job.ID, err); failErr != nil {
			return fmt.Errorf("cleanup failed: %v; status update failed: %w", err, failErr)
		}
		return err
	}
	return nil
}

func (i *Indexer) claimNextCleanupJob(ctx context.Context) (CleanupJob, error) {
	tx, err := i.db.BeginTx(ctx, nil)
	if err != nil {
		return CleanupJob{}, err
	}
	defer tx.Rollback()
	job, err := scanCleanupJob(tx.QueryRowContext(ctx, cleanupJobSelect()+` WHERE status = ? ORDER BY id LIMIT 1`, CleanupStatusQueued))
	if err != nil {
		return CleanupJob{}, err
	}
	if job.CancelRequested {
		return CleanupJob{}, sql.ErrNoRows
	}
	now := time.Now().Unix()
	result, err := tx.ExecContext(ctx, `UPDATE audit_cleanup_jobs SET status = ?, started_at = CASE WHEN started_at = 0 THEN ? ELSE started_at END, updated_at = ? WHERE id = ? AND status = ?`, CleanupStatusRunning, now, now, job.ID, CleanupStatusQueued)
	if err != nil {
		return CleanupJob{}, err
	}
	changed, _ := result.RowsAffected()
	if changed == 0 {
		return CleanupJob{}, sql.ErrNoRows
	}
	if err := tx.Commit(); err != nil {
		return CleanupJob{}, err
	}
	job.Status = CleanupStatusRunning
	job.StartedAt = now
	return job, nil
}

func (i *Indexer) executeCleanupJob(ctx context.Context, job CleanupJob) error {
	reviewTables, err := i.tableExists(ctx, "review_job_entries")
	if err != nil {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		canceled, err := i.cleanupCancelRequested(ctx, job.ID)
		if err != nil {
			return err
		}
		if canceled {
			return i.finishCleanupJob(ctx, job.ID, CleanupStatusCanceled)
		}
		count, err := i.cleanupEntryBatch(ctx, job, reviewTables)
		if err != nil {
			return err
		}
		if count == 0 {
			break
		}
		if err := waitCleanupBatch(ctx); err != nil {
			return err
		}
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		canceled, err := i.cleanupCancelRequested(ctx, job.ID)
		if err != nil {
			return err
		}
		if canceled {
			return i.finishCleanupJob(ctx, job.ID, CleanupStatusCanceled)
		}
		count, err := i.cleanupAlertBatch(ctx, job)
		if err != nil {
			return err
		}
		if count == 0 {
			break
		}
		if err := waitCleanupBatch(ctx); err != nil {
			return err
		}
	}
	if err := i.finishCleanupJob(ctx, job.ID, CleanupStatusCompleted); err != nil {
		return err
	}
	completed, _ := i.CleanupJob(ctx, job.ID)
	slog.Info("audit cleanup job completed", "job_id", job.ID, "deleted_entries", completed.DeletedEntries, "deleted_alerts", completed.DeletedAlerts, "deleted_bytes", completed.DeletedBytes)
	return nil
}

type cleanupEntryRow struct {
	id        int64
	requestID string
	bytes     int64
}

func (i *Indexer) cleanupEntryBatch(ctx context.Context, job CleanupJob, reviewTables bool) (int, error) {
	tx, err := i.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id, request_id,
		COALESCE(length(body_gzip), 0) + COALESCE(length(CAST(body AS BLOB)), 0) + 384
		FROM audit_entries INDEXED BY idx_audit_entries_created_at
		WHERE created_at >= ? AND created_at <= ?
		ORDER BY created_at, id LIMIT ?`, job.StartAt, job.EndAt, cleanupEntryBatchSize)
	if err != nil {
		return 0, err
	}
	batch := make([]cleanupEntryRow, 0, cleanupEntryBatchSize)
	for rows.Next() {
		var item cleanupEntryRow
		if err := rows.Scan(&item.id, &item.requestID, &item.bytes); err != nil {
			rows.Close()
			return 0, err
		}
		batch = append(batch, item)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if len(batch) == 0 {
		return 0, nil
	}

	ids := make([]any, 0, len(batch))
	requestIDs := make([]any, 0, len(batch))
	requestSeen := make(map[string]bool)
	var deletedBytes int64
	for _, item := range batch {
		ids = append(ids, item.id)
		deletedBytes += item.bytes
		if item.requestID != "" && !requestSeen[item.requestID] {
			requestSeen[item.requestID] = true
			requestIDs = append(requestIDs, item.requestID)
		}
	}

	var deletedReviewRows int64
	if reviewTables {
		jobIDs, err := cleanupReviewJobIDs(ctx, tx, ids)
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE review_job_entries SET parent_audit_entry_id = 0 WHERE parent_audit_entry_id IN (`+placeholders(len(ids))+`)`, ids...); err != nil {
			return 0, err
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM review_job_entries WHERE audit_entry_id IN (`+placeholders(len(ids))+`)`, ids...)
		if err != nil {
			return 0, err
		}
		deletedReviewRows, _ = result.RowsAffected()
		for _, reviewJobID := range jobIDs {
			if err := reconcileReviewJob(ctx, tx, reviewJobID); err != nil {
				return 0, err
			}
		}
	}

	matchResult, err := tx.ExecContext(ctx, `DELETE FROM log_audit_matches WHERE audit_entry_id IN (`+placeholders(len(ids))+`)`, ids...)
	if err != nil {
		return 0, err
	}
	deletedMatches, _ := matchResult.RowsAffected()
	var deletedAlerts int64
	if len(requestIDs) > 0 {
		alertResult, err := tx.ExecContext(ctx, `DELETE FROM audit_security_alerts WHERE request_id IN (`+placeholders(len(requestIDs))+`)`, requestIDs...)
		if err != nil {
			return 0, err
		}
		deletedAlerts, _ = alertResult.RowsAffected()
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM audit_entries WHERE id IN (`+placeholders(len(ids))+`)`, ids...); err != nil {
		return 0, err
	}
	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx, `UPDATE audit_cleanup_jobs SET
		deleted_entries = deleted_entries + ?, deleted_alerts = deleted_alerts + ?,
		deleted_matches = deleted_matches + ?, deleted_review_entries = deleted_review_entries + ?,
		deleted_bytes = deleted_bytes + ?, updated_at = ? WHERE id = ?`,
		len(batch), deletedAlerts, deletedMatches, deletedReviewRows, deletedBytes, now, job.ID); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(batch), nil
}

func (i *Indexer) cleanupAlertBatch(ctx context.Context, job CleanupJob) (int, error) {
	tx, err := i.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id,
		COALESCE(length(CAST(response_body AS BLOB)), 0) + COALESCE(length(CAST(matched_text AS BLOB)), 0) + 256
		FROM audit_security_alerts INDEXED BY idx_audit_security_alerts_request_at
		WHERE request_at >= ? AND request_at <= ? ORDER BY request_at, id LIMIT ?`, job.StartAt, job.EndAt, cleanupAlertBatchSize)
	if err != nil {
		return 0, err
	}
	ids := make([]any, 0, cleanupAlertBatchSize)
	var deletedBytes int64
	for rows.Next() {
		var id, size int64
		if err := rows.Scan(&id, &size); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
		deletedBytes += size
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM audit_security_alerts WHERE id IN (`+placeholders(len(ids))+`)`, ids...)
	if err != nil {
		return 0, err
	}
	deleted, _ := result.RowsAffected()
	if _, err := tx.ExecContext(ctx, `UPDATE audit_cleanup_jobs SET deleted_alerts = deleted_alerts + ?, deleted_bytes = deleted_bytes + ?, updated_at = ? WHERE id = ?`, deleted, deletedBytes, time.Now().Unix(), job.ID); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(ids), nil
}

func cleanupReviewJobIDs(ctx context.Context, tx *sql.Tx, ids []any) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT job_id FROM review_job_entries WHERE audit_entry_id IN (`+placeholders(len(ids))+`)`, ids...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		items = append(items, id)
	}
	return items, rows.Err()
}

func reconcileReviewJob(ctx context.Context, tx *sql.Tx, jobID int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE review_jobs SET
		total_entries = (SELECT COUNT(*) FROM review_job_entries WHERE job_id = ?),
		processed_entries = (SELECT COUNT(*) FROM review_job_entries WHERE job_id = ? AND status IN ('done', 'error')),
		review_units = (SELECT COUNT(*) FROM review_job_entries WHERE job_id = ? AND content_hash <> ''),
		reviewed_units = (SELECT COUNT(*) FROM review_job_entries WHERE job_id = ? AND status = 'done' AND cache_hit = 0 AND result_id > 0),
		cache_hits = (SELECT COALESCE(SUM(cache_hit), 0) FROM review_job_entries WHERE job_id = ?),
		flagged_entries = (SELECT COUNT(*) FROM review_job_entries WHERE job_id = ? AND status IN ('done', 'error') AND effective_decision <> '通过'),
		error_entries = (SELECT COUNT(*) FROM review_job_entries WHERE job_id = ? AND status = 'error'),
		estimated_chars = (SELECT COALESCE(SUM(content_chars), 0) FROM review_job_entries WHERE job_id = ?),
		prompt_tokens = (SELECT COALESCE(SUM(prompt_tokens), 0) FROM review_job_entries WHERE job_id = ?),
		completion_tokens = (SELECT COALESCE(SUM(completion_tokens), 0) FROM review_job_entries WHERE job_id = ?),
		updated_at = ? WHERE id = ?`,
		jobID, jobID, jobID, jobID, jobID, jobID, jobID, jobID, jobID, jobID, time.Now().Unix(), jobID)
	return err
}

func (i *Indexer) cleanupCancelRequested(ctx context.Context, id int64) (bool, error) {
	var canceled bool
	err := i.db.QueryRowContext(ctx, `SELECT cancel_requested FROM audit_cleanup_jobs WHERE id = ?`, id).Scan(&canceled)
	return canceled, err
}

func (i *Indexer) finishCleanupJob(ctx context.Context, id int64, status string) error {
	now := time.Now().Unix()
	_, err := i.db.ExecContext(ctx, `UPDATE audit_cleanup_jobs SET status = ?, completed_at = ?, updated_at = ? WHERE id = ? AND status = ?`, status, now, now, id, CleanupStatusRunning)
	return err
}

func (i *Indexer) failCleanupJob(ctx context.Context, id int64, cause error) error {
	now := time.Now().Unix()
	_, err := i.db.ExecContext(ctx, `UPDATE audit_cleanup_jobs SET status = ?, error = ?, completed_at = ?, updated_at = ? WHERE id = ?`, CleanupStatusFailed, cause.Error(), now, now, id)
	return err
}

func (i *Indexer) cleanupIndexesState(ctx context.Context) (bool, string) {
	required := []string{"idx_audit_entries_created_at", "idx_audit_security_alerts_request_at"}
	if exists, err := i.tableExists(ctx, "review_job_entries"); err != nil {
		return false, err.Error()
	} else if exists {
		required = append(required, "idx_review_job_entries_audit_entry")
	}
	for _, name := range required {
		var count int
		if err := i.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, name).Scan(&count); err != nil {
			return false, err.Error()
		}
		if count == 0 {
			return false, ""
		}
	}
	return true, ""
}

func (i *Indexer) cleanupIndexStatus() (bool, string) {
	i.statusMu.Lock()
	defer i.statusMu.Unlock()
	return i.cleanupIndexReady, i.cleanupIndexError
}

func (i *Indexer) setCleanupIndexStatus(ready bool, err error) {
	i.statusMu.Lock()
	i.cleanupIndexReady = ready
	if err == nil {
		i.cleanupIndexError = ""
	} else {
		i.cleanupIndexError = err.Error()
	}
	i.statusMu.Unlock()
}

func (i *Indexer) notifyCleanupWorker() {
	select {
	case i.cleanupWake <- struct{}{}:
	default:
	}
}

func (i *Indexer) databaseSpace(ctx context.Context) (int64, int64, error) {
	var pageSize, pageCount, freePages int64
	if err := i.db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
		return 0, 0, err
	}
	if err := i.db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pageCount); err != nil {
		return 0, 0, err
	}
	if err := i.db.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&freePages); err != nil {
		return 0, 0, err
	}
	return pageSize * pageCount, pageSize * freePages, nil
}

func (i *Indexer) tableExists(ctx context.Context, name string) (bool, error) {
	return tableExistsDB(ctx, i.db, name)
}

func tableExistsDB(ctx context.Context, db *sql.DB, name string) (bool, error) {
	var count int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&count)
	return count > 0, err
}

func tableExistsTx(ctx context.Context, tx *sql.Tx, name string) (bool, error) {
	var count int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&count)
	return count > 0, err
}

func validateCleanupRange(startAt, endAt int64) error {
	if startAt <= 0 || endAt <= 0 || endAt < startAt {
		return fmt.Errorf("invalid cleanup time range")
	}
	return nil
}

func cleanupJobSelect() string {
	return `SELECT id, start_at, end_at, status,
		estimated_entries, estimated_alerts, estimated_matches, estimated_review_entries, estimated_bytes,
		deleted_entries, deleted_alerts, deleted_matches, deleted_review_entries, deleted_bytes,
		cancel_requested, error, created_at, started_at, completed_at, updated_at
		FROM audit_cleanup_jobs`
}

type cleanupRowScanner interface {
	Scan(dest ...any) error
}

func scanCleanupJob(row cleanupRowScanner) (CleanupJob, error) {
	var item CleanupJob
	err := row.Scan(
		&item.ID, &item.StartAt, &item.EndAt, &item.Status,
		&item.EstimatedEntries, &item.EstimatedAlerts, &item.EstimatedMatches, &item.EstimatedReviewRows, &item.EstimatedBytes,
		&item.DeletedEntries, &item.DeletedAlerts, &item.DeletedMatches, &item.DeletedReviewRows, &item.DeletedBytes,
		&item.CancelRequested, &item.Error, &item.CreatedAt, &item.StartedAt, &item.CompletedAt, &item.UpdatedAt,
	)
	return item, err
}

func waitCleanupBatch(ctx context.Context) error {
	timer := time.NewTimer(cleanupBatchPause)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
