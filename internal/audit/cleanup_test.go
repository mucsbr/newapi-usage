package audit

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestCleanupEstimateAndJobDeleteOnlySelectedRange(t *testing.T) {
	dir := t.TempDir()
	idx, err := Open(Config{
		LogGlob:  filepath.Join(dir, "*.jsonl"),
		IndexDSN: filepath.Join(dir, "audit.db"),
	}, nil)
	if err != nil {
		t.Fatalf("open indexer: %v", err)
	}
	defer idx.Close()

	ctx := context.Background()
	if err := idx.prepareCleanupIndexes(ctx); err != nil {
		t.Fatalf("prepare cleanup indexes: %v", err)
	}
	idx.setCleanupIndexStatus(true, nil)
	insertCleanupTestEntry(t, idx.db, 1, 1000, "request-1", "old request body")
	insertCleanupTestEntry(t, idx.db, 2, 2000, "request-2", "middle request body")
	insertCleanupTestEntry(t, idx.db, 3, 3000, "request-3", "new request body")
	if _, err := idx.db.Exec(`INSERT INTO log_audit_matches (log_id, audit_entry_id, matched_by, matched_note, matched_at) VALUES (101, 1, 'token_time', '', 1000)`); err != nil {
		t.Fatalf("insert match: %v", err)
	}
	if _, err := idx.db.Exec(`INSERT INTO audit_security_alerts (
		source_id, source_path, source_line, byte_offset, request_id, request_at, response_at, ingested_at,
		method, path, model, token_id, key_tail, key_hash, response_status, response_content_type,
		response_body, response_total_bytes, response_truncated, alert_type, matched_text
	) VALUES ('source', 'audit.jsonl', 10, 0, 'request-1', 1000, 1001, 1001,
		'POST', '/v1/responses', 'gpt-test', 7, 'tail', 'hash', 400, 'application/json',
		'blocked', 7, 0, 'policy_violation', 'flagged')`); err != nil {
		t.Fatalf("insert alert: %v", err)
	}

	estimate, err := idx.EstimateCleanup(ctx, 900, 1500)
	if err != nil {
		t.Fatalf("estimate cleanup: %v", err)
	}
	if estimate.EntryCount != 1 || estimate.AlertCount != 1 || estimate.MatchCount != 1 {
		t.Fatalf("unexpected estimate: %+v", estimate)
	}
	if estimate.EstimatedBytes <= 0 {
		t.Fatalf("estimated bytes = %d, want positive", estimate.EstimatedBytes)
	}

	job, err := idx.CreateCleanupJob(ctx, 900, 1500)
	if err != nil {
		t.Fatalf("create cleanup job: %v", err)
	}
	if err := idx.runNextCleanupJob(ctx); err != nil {
		t.Fatalf("run cleanup job: %v", err)
	}
	job, err = idx.CleanupJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("load cleanup job: %v", err)
	}
	if job.Status != CleanupStatusCompleted || job.DeletedEntries != 1 || job.DeletedAlerts != 1 || job.DeletedMatches != 1 {
		t.Fatalf("unexpected completed job: %+v", job)
	}

	assertCleanupTableCount(t, idx.db, "audit_entries", 2)
	assertCleanupTableCount(t, idx.db, "audit_security_alerts", 0)
	assertCleanupTableCount(t, idx.db, "log_audit_matches", 0)
	var remaining int64
	if err := idx.db.QueryRow(`SELECT COUNT(*) FROM audit_entries WHERE id IN (2, 3)`).Scan(&remaining); err != nil {
		t.Fatalf("count remaining entries: %v", err)
	}
	if remaining != 2 {
		t.Fatalf("remaining selected entries = %d, want 2", remaining)
	}
}

func TestCancelQueuedCleanupJobKeepsAuditEntries(t *testing.T) {
	dir := t.TempDir()
	idx, err := Open(Config{
		LogGlob:  filepath.Join(dir, "*.jsonl"),
		IndexDSN: filepath.Join(dir, "audit.db"),
	}, nil)
	if err != nil {
		t.Fatalf("open indexer: %v", err)
	}
	defer idx.Close()

	ctx := context.Background()
	if err := idx.prepareCleanupIndexes(ctx); err != nil {
		t.Fatalf("prepare cleanup indexes: %v", err)
	}
	idx.setCleanupIndexStatus(true, nil)
	insertCleanupTestEntry(t, idx.db, 1, 1000, "request-1", "request body")

	job, err := idx.CreateCleanupJob(ctx, 900, 1100)
	if err != nil {
		t.Fatalf("create cleanup job: %v", err)
	}
	job, err = idx.CancelCleanupJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("cancel cleanup job: %v", err)
	}
	if job.Status != CleanupStatusCanceled {
		t.Fatalf("job status = %q, want canceled", job.Status)
	}
	assertCleanupTableCount(t, idx.db, "audit_entries", 1)
}

func insertCleanupTestEntry(t *testing.T, db *sql.DB, id, createdAt int64, requestID, body string) {
	t.Helper()
	bodyGzip, encoding, err := encodeBody(body)
	if err != nil {
		t.Fatalf("encode body: %v", err)
	}
	_, err = db.Exec(`INSERT INTO audit_entries (
		id, source_id, source_path, source_line, byte_offset, created_at, ingested_at,
		method, path, model, token_id, key_tail, key_hash, user_agent, client_name,
		client_version, client_variant, request_id, has_timestamp, body, body_gzip, body_encoding
	) VALUES (?, ?, 'audit.jsonl', ?, 0, ?, ?, 'POST', '/v1/responses', 'gpt-test', 7,
		'tail', 'hash', 'codex/1.0', 'codex', '1.0', '', ?, 1, '', ?, ?)`,
		id, "source-"+requestID, id, createdAt, createdAt, requestID, bodyGzip, encoding)
	if err != nil {
		t.Fatalf("insert audit entry: %v", err)
	}
}

func assertCleanupTableCount(t *testing.T, db *sql.DB, table string, want int64) {
	t.Helper()
	var got int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&got); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	if got != want {
		t.Fatalf("%s count = %d, want %d", table, got, want)
	}
}
