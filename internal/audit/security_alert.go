package audit

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"
)

const (
	legacySecurityAlertSplitName = "legacy_security_alert_split"
	legacySecurityAlertBatchSize = 100
	legacySecurityAlertPause     = 100 * time.Millisecond
	securityAlertRecordType      = "security_alert"
)

type legacySecurityAlertCandidate struct {
	ID         int64
	SourceID   string
	SourcePath string
	SourceLine int64
	ByteOffset int64
	RequestID  string
}

func (i *Indexer) withSecurityAlerts(ctx context.Context, items []Entry) ([]Entry, error) {
	requestIDs := entryRequestIDs(items)
	if len(requestIDs) == 0 {
		return items, nil
	}
	alertsByRequestID, err := i.securityAlertsByRequestID(ctx, requestIDs)
	if err != nil {
		return nil, err
	}
	for idx := range items {
		alerts := alertsByRequestID[items[idx].RequestID]
		items[idx].SecurityAlertCount = int64(len(alerts))
		items[idx].SecurityAlerts = alerts
	}
	return items, nil
}

func (i *Indexer) withSecurityAlertCounts(ctx context.Context, items map[int64]Entry) (map[int64]Entry, error) {
	requestIDs := make([]string, 0, len(items))
	seen := make(map[string]bool)
	for _, item := range items {
		requestID := strings.TrimSpace(item.RequestID)
		if requestID == "" || seen[requestID] {
			continue
		}
		seen[requestID] = true
		requestIDs = append(requestIDs, requestID)
	}
	if len(requestIDs) == 0 {
		return items, nil
	}
	counts, err := i.securityAlertCounts(ctx, requestIDs)
	if err != nil {
		return nil, err
	}
	for logID, item := range items {
		item.SecurityAlertCount = counts[item.RequestID]
		items[logID] = item
	}
	return items, nil
}

func entryRequestIDs(items []Entry) []string {
	requestIDs := make([]string, 0, len(items))
	seen := make(map[string]bool)
	for _, item := range items {
		requestID := strings.TrimSpace(item.RequestID)
		if requestID == "" || seen[requestID] {
			continue
		}
		seen[requestID] = true
		requestIDs = append(requestIDs, requestID)
	}
	return requestIDs
}

func (i *Indexer) securityAlertCounts(ctx context.Context, requestIDs []string) (map[string]int64, error) {
	out := make(map[string]int64)
	args := make([]any, 0, len(requestIDs))
	for _, requestID := range requestIDs {
		args = append(args, requestID)
	}
	query := fmt.Sprintf(`SELECT request_id, COUNT(*)
		FROM audit_security_alerts
		WHERE request_id IN (%s)
		GROUP BY request_id`, placeholders(len(requestIDs)))
	rows, err := i.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var requestID string
		var count int64
		if err := rows.Scan(&requestID, &count); err != nil {
			return nil, err
		}
		out[requestID] = count
	}
	return out, rows.Err()
}

func (i *Indexer) securityAlertsByRequestID(ctx context.Context, requestIDs []string) (map[string][]SecurityAlert, error) {
	out := make(map[string][]SecurityAlert)
	args := make([]any, 0, len(requestIDs))
	for _, requestID := range requestIDs {
		args = append(args, requestID)
	}
	query := fmt.Sprintf(`SELECT id, request_id, request_at, response_at, ingested_at,
		source_path, source_line, method, path, model, response_status,
		response_content_type, response_body, response_total_bytes,
		response_truncated, alert_type, matched_text
		FROM audit_security_alerts
		WHERE request_id IN (%s)
		ORDER BY response_at, id`, placeholders(len(requestIDs)))
	rows, err := i.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var item SecurityAlert
		if err := rows.Scan(
			&item.ID,
			&item.RequestID,
			&item.RequestAt,
			&item.ResponseAt,
			&item.IngestedAt,
			&item.SourcePath,
			&item.SourceLine,
			&item.Method,
			&item.Path,
			&item.Model,
			&item.ResponseStatus,
			&item.ResponseContentType,
			&item.ResponseBody,
			&item.ResponseTotalBytes,
			&item.ResponseTruncated,
			&item.AlertType,
			&item.MatchedText,
		); err != nil {
			return nil, err
		}
		out[item.RequestID] = append(out[item.RequestID], item)
	}
	return out, rows.Err()
}

func (i *Indexer) insertSecurityAlert(ctx context.Context, tx *sql.Tx, sourceID string, path string, lineNo int64, offset int64, record parsedRecord, resolveCache map[string]ResolvedToken) (bool, error) {
	tokenID, keyTail, keyHash := i.resolveRecordToken(record, resolveCache)
	alert := record.SecurityAlert
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO audit_security_alerts (
		source_id, source_path, source_line, byte_offset, request_id, request_at,
		response_at, ingested_at, method, path, model, token_id, key_tail, key_hash,
		response_status, response_content_type, response_body, response_total_bytes,
		response_truncated, alert_type, matched_text
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sourceID,
		path,
		lineNo,
		offset,
		record.RequestID,
		record.CreatedAt,
		alert.ResponseAt,
		time.Now().Unix(),
		record.Method,
		record.Path,
		record.Model,
		tokenID,
		keyTail,
		keyHash,
		alert.ResponseStatus,
		alert.ResponseContentType,
		alert.ResponseBody,
		alert.ResponseTotalBytes,
		alert.ResponseTruncated,
		alert.AlertType,
		alert.MatchedText,
	)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows > 0, err
}

func (i *Indexer) runLegacySecurityAlertSplit(ctx context.Context) {
	if err := i.splitLegacySecurityAlerts(ctx); err != nil && ctx.Err() == nil {
		slog.Warn("legacy security alert split failed", "error", err)
	}
}

func (i *Indexer) splitLegacySecurityAlerts(ctx context.Context) error {
	cursor, completedAt, err := i.maintenanceState(ctx, legacySecurityAlertSplitName)
	if err != nil {
		return err
	}
	if completedAt > 0 {
		return nil
	}

	total := 0
	slog.Info("legacy security alert split started", "cursor", cursor, "batch_size", legacySecurityAlertBatchSize)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		batch, err := i.nextLegacySecurityAlertBatch(ctx, cursor)
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			now := time.Now().Unix()
			if err := i.saveMaintenanceState(ctx, legacySecurityAlertSplitName, cursor, now); err != nil {
				return err
			}
			slog.Info("legacy security alert split completed", "migrated_rows", total, "cursor", cursor)
			return nil
		}

		migrated, nextCursor, err := i.splitLegacySecurityAlertBatch(ctx, batch)
		if err != nil {
			return err
		}
		total += migrated
		cursor = nextCursor
		if err := i.saveMaintenanceState(ctx, legacySecurityAlertSplitName, cursor, 0); err != nil {
			return err
		}
		slog.Info("legacy security alert split batch", "migrated_rows", migrated, "total_rows", total, "cursor", cursor)

		timer := time.NewTimer(legacySecurityAlertPause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (i *Indexer) maintenanceState(ctx context.Context, name string) (int64, int64, error) {
	var cursor int64
	var completedAt int64
	err := i.db.QueryRowContext(ctx, `SELECT cursor, completed_at FROM audit_maintenance WHERE name = ?`, name).Scan(&cursor, &completedAt)
	if err == nil {
		return cursor, completedAt, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, 0, err
	}
	now := time.Now().Unix()
	if _, err := i.db.ExecContext(ctx, `INSERT INTO audit_maintenance (name, cursor, completed_at, updated_at) VALUES (?, 0, 0, ?)`, name, now); err != nil {
		return 0, 0, err
	}
	return 0, 0, nil
}

func (i *Indexer) saveMaintenanceState(ctx context.Context, name string, cursor int64, completedAt int64) error {
	now := time.Now().Unix()
	_, err := i.db.ExecContext(ctx, `INSERT INTO audit_maintenance (name, cursor, completed_at, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			cursor = excluded.cursor,
			completed_at = excluded.completed_at,
			updated_at = excluded.updated_at`, name, cursor, completedAt, now)
	return err
}

func (i *Indexer) nextLegacySecurityAlertBatch(ctx context.Context, cursor int64) ([]legacySecurityAlertCandidate, error) {
	rows, err := i.db.QueryContext(ctx, `SELECT e.id, e.source_id, e.source_path, e.source_line, e.byte_offset, e.request_id
		FROM audit_entries e
		WHERE e.id > ? AND e.request_id <> ''
			AND EXISTS (
				SELECT 1 FROM audit_entries duplicate
				WHERE duplicate.request_id = e.request_id AND duplicate.id <> e.id
			)
		ORDER BY e.id
		LIMIT ?`, cursor, legacySecurityAlertBatchSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]legacySecurityAlertCandidate, 0, legacySecurityAlertBatchSize)
	for rows.Next() {
		var item legacySecurityAlertCandidate
		if err := rows.Scan(&item.ID, &item.SourceID, &item.SourcePath, &item.SourceLine, &item.ByteOffset, &item.RequestID); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (i *Indexer) splitLegacySecurityAlertBatch(ctx context.Context, batch []legacySecurityAlertCandidate) (int, int64, error) {
	migrated := 0
	var cursor int64
	resolveCache := make(map[string]ResolvedToken)
	for _, item := range batch {
		cursor = item.ID
		line, err := readAuditLineAt(item.SourcePath, item.ByteOffset)
		if err != nil {
			continue
		}
		record, err := parseLine(strings.TrimSpace(line), i.timeLocation)
		if err != nil || record.RecordType != securityAlertRecordType || record.RequestID != item.RequestID {
			continue
		}
		tx, err := i.db.BeginTx(ctx, nil)
		if err != nil {
			return migrated, cursor, err
		}
		inserted, err := i.insertSecurityAlert(ctx, tx, item.SourceID, item.SourcePath, item.SourceLine, item.ByteOffset, record, resolveCache)
		if err == nil {
			err = i.replaceLegacySecurityAlertEntry(ctx, tx, item)
		}
		if err != nil {
			tx.Rollback()
			return migrated, cursor, err
		}
		if err := tx.Commit(); err != nil {
			return migrated, cursor, err
		}
		if inserted {
			migrated++
		}
	}
	return migrated, cursor, nil
}

func (i *Indexer) replaceLegacySecurityAlertEntry(ctx context.Context, tx *sql.Tx, item legacySecurityAlertCandidate) error {
	var replacementID int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM audit_entries
		WHERE request_id = ? AND id <> ?
		ORDER BY id
		LIMIT 1`, item.RequestID, item.ID).Scan(&replacementID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if replacementID > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE log_audit_matches SET audit_entry_id = ? WHERE audit_entry_id = ?`, replacementID, item.ID); err != nil {
			return err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `DELETE FROM log_audit_matches WHERE audit_entry_id = ?`, item.ID); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM audit_entries WHERE id = ?`, item.ID)
	return err
}

func readAuditLineAt(path string, offset int64) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return "", err
	}
	return bufio.NewReaderSize(file, 256*1024).ReadString('\n')
}
