package audit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"
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

	cleanupCatalogMaintenanceName = "cleanup_entry_catalog_v1"
	cleanupCatalogBatchSize       = 5000
	cleanupCatalogPause           = 100 * time.Millisecond
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
	IndexCursor       int64        `json:"index_cursor"`
	IndexMaxID        int64        `json:"index_max_id"`
	IndexProgress     float64      `json:"index_progress"`
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
	ready, indexErr, cursor, maxID := i.cleanupCatalogStatus()
	databaseBytes, reusableBytes, err := i.databaseSpace(ctx)
	if err != nil {
		return CleanupOverview{}, err
	}
	out := CleanupOverview{
		Enabled:       i.Enabled(),
		IndexReady:    ready,
		IndexError:    indexErr,
		IndexCursor:   cursor,
		IndexMaxID:    maxID,
		DefaultEnd:    time.Now().AddDate(0, 0, -30).Unix(),
		DatabaseBytes: databaseBytes,
		ReusableBytes: reusableBytes,
	}
	if maxID > 0 {
		out.IndexProgress = float64(cursor) / float64(maxID)
		if out.IndexProgress > 1 {
			out.IndexProgress = 1
		}
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
		FROM audit_cleanup_entries`).Scan(
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
	ready, _, _, _ := i.cleanupCatalogStatus()
	if !ready {
		return CleanupEstimate{}, ErrCleanupPreparing
	}

	out := CleanupEstimate{StartAt: startAt, EndAt: endAt}
	if err := i.db.QueryRowContext(ctx, `SELECT COUNT(*),
		COALESCE(MIN(created_at), 0), COALESCE(MAX(created_at), 0)
		FROM audit_cleanup_entries
		WHERE created_at >= ? AND created_at <= ?`, startAt, endAt).Scan(
		&out.EntryCount, &out.FirstMatchedAt, &out.LastMatchedAt,
	); err != nil {
		return CleanupEstimate{}, err
	}
	var sampleCount, sampleBytes int64
	if out.EntryCount > 0 {
		if err := i.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(
			COALESCE(length(e.body_gzip), 0) + COALESCE(length(CAST(e.body AS BLOB)), 0) + 384
		), 0)
		FROM audit_entries e
		JOIN (
			SELECT audit_entry_id FROM audit_cleanup_entries
			WHERE created_at >= ? AND created_at <= ?
			ORDER BY created_at, audit_entry_id LIMIT 500
		) sample ON sample.audit_entry_id = e.id`, startAt, endAt).Scan(&sampleCount, &sampleBytes); err != nil {
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
	WHERE a.request_at >= ? AND a.request_at <= ?`, startAt, endAt).Scan(&out.AlertCount, &alertBytes); err != nil {
		return CleanupEstimate{}, err
	}

	if err := i.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_cleanup_entries e
		JOIN log_audit_matches m ON m.audit_entry_id = e.audit_entry_id
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
		FROM audit_cleanup_entries e
		JOIN review_job_entries r ON r.audit_entry_id = e.audit_entry_id
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

func (i *Indexer) runCleanupCatalogBackfill(ctx context.Context) {
	for {
		ready, _, _, _ := i.cleanupCatalogStatus()
		if ready {
			i.notifyCleanupWorker()
			return
		}
		err := i.backfillCleanupCatalog(ctx)
		if err == nil {
			i.notifyCleanupWorker()
			return
		}
		if ctx.Err() != nil {
			return
		}
		_, _, cursor, maxID := i.cleanupCatalogStatus()
		i.setCleanupCatalogStatus(false, err, cursor, maxID)
		slog.Warn("audit cleanup catalog backfill failed", "error", err)
		timer := time.NewTimer(30 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (i *Indexer) backfillCleanupCatalog(ctx context.Context) error {
	cursor, completedAt, err := i.cleanupCatalogMaintenanceState(ctx)
	if err != nil {
		return err
	}
	var maxID int64
	if err := i.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM audit_entries`).Scan(&maxID); err != nil {
		return err
	}
	if completedAt > 0 {
		i.setCleanupCatalogStatus(true, nil, maxID, maxID)
		return nil
	}

	i.setCleanupCatalogStatus(false, nil, cursor, maxID)
	slog.Info("audit cleanup catalog backfill started", "cursor", cursor, "max_id", maxID, "batch_size", cleanupCatalogBatchSize)
	lastLogAt := time.Now()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var boundary int64
		var count int64
		if err := i.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0), COUNT(*) FROM (
			SELECT id FROM audit_entries WHERE id > ? ORDER BY id LIMIT ?
		)`, cursor, cleanupCatalogBatchSize).Scan(&boundary, &count); err != nil {
			return err
		}
		if count == 0 {
			now := time.Now().Unix()
			if err := i.saveCleanupCatalogState(ctx, cursor, now); err != nil {
				return err
			}
			if err := i.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM audit_entries`).Scan(&maxID); err != nil {
				return err
			}
			i.setCleanupCatalogStatus(true, nil, maxID, maxID)
			slog.Info("audit cleanup catalog backfill completed", "cursor", cursor, "max_id", maxID)
			return nil
		}

		tx, err := i.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO audit_cleanup_entries (audit_entry_id, created_at)
			SELECT id, created_at FROM audit_entries WHERE id > ? AND id <= ?`, cursor, boundary); err != nil {
			tx.Rollback()
			return err
		}
		now := time.Now().Unix()
		if _, err := tx.ExecContext(ctx, `INSERT INTO audit_maintenance (name, cursor, completed_at, updated_at)
			VALUES (?, ?, 0, ?)
			ON CONFLICT(name) DO UPDATE SET cursor = excluded.cursor, completed_at = 0, updated_at = excluded.updated_at`,
			cleanupCatalogMaintenanceName, boundary, now); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		cursor = boundary
		i.setCleanupCatalogStatus(false, nil, cursor, maxID)
		if time.Since(lastLogAt) >= 10*time.Second {
			slog.Info("audit cleanup catalog backfill progress", "cursor", cursor, "max_id", maxID)
			lastLogAt = time.Now()
		}
		if err := waitWithContext(ctx, cleanupCatalogPause); err != nil {
			return err
		}
	}
}

func (i *Indexer) runCleanupJobs(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if ready, _, _, _ := i.cleanupCatalogStatus(); ready {
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
	rows, err := tx.QueryContext(ctx, `SELECT e.id, e.request_id,
		COALESCE(length(e.body_gzip), 0) + COALESCE(length(CAST(e.body AS BLOB)), 0) + 384
		FROM audit_cleanup_entries c
		JOIN audit_entries e ON e.id = c.audit_entry_id
		WHERE c.created_at >= ? AND c.created_at <= ?
		ORDER BY c.created_at, c.audit_entry_id LIMIT ?`, job.StartAt, job.EndAt, cleanupEntryBatchSize)
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM audit_cleanup_entries WHERE audit_entry_id IN (`+placeholders(len(ids))+`)`, ids...); err != nil {
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
		FROM audit_security_alerts
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

func (i *Indexer) loadCleanupCatalogStatus(ctx context.Context) {
	cursor, completedAt, err := i.cleanupCatalogMaintenanceState(ctx)
	if err != nil {
		i.setCleanupCatalogStatus(false, err, 0, 0)
		return
	}
	var maxID int64
	if err := i.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM audit_entries`).Scan(&maxID); err != nil {
		i.setCleanupCatalogStatus(false, err, cursor, 0)
		return
	}
	if completedAt > 0 {
		var catalogMaxID int64
		if err := i.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(audit_entry_id), 0) FROM audit_cleanup_entries`).Scan(&catalogMaxID); err != nil {
			i.setCleanupCatalogStatus(false, err, cursor, maxID)
			return
		}
		if catalogMaxID < maxID {
			cursor = catalogMaxID
			completedAt = 0
			if err := i.saveCleanupCatalogState(ctx, cursor, 0); err != nil {
				i.setCleanupCatalogStatus(false, err, cursor, maxID)
				return
			}
		} else {
			cursor = maxID
		}
	}
	i.setCleanupCatalogStatus(completedAt > 0, nil, cursor, maxID)
}

func (i *Indexer) cleanupCatalogMaintenanceState(ctx context.Context) (int64, int64, error) {
	var cursor, completedAt int64
	err := i.db.QueryRowContext(ctx, `SELECT cursor, completed_at FROM audit_maintenance WHERE name = ?`, cleanupCatalogMaintenanceName).Scan(&cursor, &completedAt)
	if err == nil {
		return cursor, completedAt, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, 0, err
	}
	now := time.Now().Unix()
	if _, err := i.db.ExecContext(ctx, `INSERT INTO audit_maintenance (name, cursor, completed_at, updated_at) VALUES (?, 0, 0, ?)`, cleanupCatalogMaintenanceName, now); err != nil {
		return 0, 0, err
	}
	return 0, 0, nil
}

func (i *Indexer) saveCleanupCatalogState(ctx context.Context, cursor, completedAt int64) error {
	now := time.Now().Unix()
	_, err := i.db.ExecContext(ctx, `INSERT INTO audit_maintenance (name, cursor, completed_at, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET cursor = excluded.cursor, completed_at = excluded.completed_at, updated_at = excluded.updated_at`,
		cleanupCatalogMaintenanceName, cursor, completedAt, now)
	return err
}

func (i *Indexer) cleanupCatalogStatus() (bool, string, int64, int64) {
	i.statusMu.Lock()
	defer i.statusMu.Unlock()
	return i.cleanupCatalogReady, i.cleanupCatalogError, i.cleanupCatalogCursor, i.cleanupCatalogMaxID
}

func (i *Indexer) setCleanupCatalogStatus(ready bool, err error, cursor, maxID int64) {
	i.statusMu.Lock()
	i.cleanupCatalogReady = ready
	i.cleanupCatalogCursor = cursor
	i.cleanupCatalogMaxID = maxID
	if err == nil {
		i.cleanupCatalogError = ""
	} else {
		i.cleanupCatalogError = err.Error()
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
	return waitWithContext(ctx, cleanupBatchPause)
}

func waitWithContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
