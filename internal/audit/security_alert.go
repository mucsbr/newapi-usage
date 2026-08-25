package audit

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const securityAlertRecordType = "security_alert"

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
