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
	query := fmt.Sprintf(`SELECT %s
		FROM audit_security_alerts
		WHERE request_id IN (%s)
		ORDER BY response_at, id`, securityAlertSelectColumns("audit_security_alerts"), placeholders(len(requestIDs)))
	rows, err := i.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items, err := scanSecurityAlerts(rows)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		out[item.RequestID] = append(out[item.RequestID], item)
	}
	return out, nil
}

func (i *Indexer) ListSecurityAlerts(ctx context.Context, filter SecurityAlertFilter) (SecurityAlertPage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if filter.Page <= 0 {
		filter.Page = 1
	}
	if filter.PageSize <= 0 || filter.PageSize > 200 {
		filter.PageSize = 50
	}
	conditions := []string{"1 = 1"}
	args := make([]any, 0)
	if filter.Start > 0 {
		conditions = append(conditions, "a.request_at >= ?")
		args = append(args, filter.Start)
	}
	if filter.End > 0 {
		conditions = append(conditions, "a.request_at <= ?")
		args = append(args, filter.End)
	}
	if filter.TokenID > 0 {
		conditions = append(conditions, "a.token_id = ?")
		args = append(args, filter.TokenID)
	}
	if model := strings.TrimSpace(filter.Model); model != "" {
		conditions = append(conditions, "a.model = ?")
		args = append(args, model)
	}
	if query := strings.ToLower(strings.TrimSpace(filter.Query)); query != "" {
		pattern := "%" + query + "%"
		conditions = append(conditions, `(LOWER(a.request_id) LIKE ? OR LOWER(a.matched_text) LIKE ? OR LOWER(a.alert_type) LIKE ? OR LOWER(a.model) LIKE ? OR LOWER(a.key_tail) LIKE ?)`)
		args = append(args, pattern, pattern, pattern, pattern, pattern)
	}
	where := "WHERE " + strings.Join(conditions, " AND ")

	var total int64
	if err := i.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_security_alerts a `+where, args...).Scan(&total); err != nil {
		return SecurityAlertPage{}, err
	}
	offset := (filter.Page - 1) * filter.PageSize
	queryArgs := append([]any{}, args...)
	queryArgs = append(queryArgs, filter.PageSize, offset)
	query := fmt.Sprintf(`SELECT %s
		FROM audit_security_alerts a
		%s
		ORDER BY CASE WHEN a.response_at > 0 THEN a.response_at ELSE a.request_at END DESC, a.id DESC
		LIMIT ? OFFSET ?`, securityAlertSelectColumns("a"), where)
	rows, err := i.db.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return SecurityAlertPage{}, err
	}
	defer rows.Close()
	items, err := scanSecurityAlerts(rows)
	if err != nil {
		return SecurityAlertPage{}, err
	}
	return SecurityAlertPage{Items: items, Total: total, Page: filter.Page, PageSize: filter.PageSize}, nil
}

func (i *Indexer) SecurityAlertByID(ctx context.Context, id int64) (SecurityAlert, error) {
	if i == nil || id <= 0 {
		return SecurityAlert{}, sql.ErrNoRows
	}
	rows, err := i.db.QueryContext(ctx, `SELECT `+securityAlertSelectColumns("a")+` FROM audit_security_alerts a WHERE a.id = ? LIMIT 1`, id)
	if err != nil {
		return SecurityAlert{}, err
	}
	defer rows.Close()
	items, err := scanSecurityAlerts(rows)
	if err != nil {
		return SecurityAlert{}, err
	}
	if len(items) == 0 {
		return SecurityAlert{}, sql.ErrNoRows
	}
	return items[0], nil
}

func securityAlertSelectColumns(alias string) string {
	prefix := ""
	if alias != "" {
		prefix = alias + "."
	}
	return fmt.Sprintf(`%[1]sid,
		COALESCE((SELECT MIN(e.id) FROM audit_entries e WHERE e.request_id = %[1]srequest_id), 0),
		%[1]srequest_id, %[1]srequest_at, %[1]sresponse_at, %[1]singested_at,
		%[1]ssource_path, %[1]ssource_line, %[1]smethod, %[1]spath, %[1]smodel,
		%[1]stoken_id, %[1]skey_tail, %[1]sresponse_status, %[1]sresponse_content_type,
		%[1]sresponse_body, %[1]sresponse_total_bytes, %[1]sresponse_truncated,
		%[1]salert_type, %[1]smatched_text`, prefix)
}

func scanSecurityAlerts(rows *sql.Rows) ([]SecurityAlert, error) {
	items := make([]SecurityAlert, 0)
	for rows.Next() {
		var item SecurityAlert
		if err := rows.Scan(
			&item.ID,
			&item.AuditEntryID,
			&item.RequestID,
			&item.RequestAt,
			&item.ResponseAt,
			&item.IngestedAt,
			&item.SourcePath,
			&item.SourceLine,
			&item.Method,
			&item.Path,
			&item.Model,
			&item.TokenID,
			&item.KeyTail,
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
		items = append(items, item)
	}
	return items, rows.Err()
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
