package audit

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIndexerIncrementalImport(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "request-body-1.jsonl")
	indexPath := filepath.Join(dir, "audit.db")

	line1 := `{"time":1000,"method":"POST","path":"/v1/chat/completions","user_agent":"codex-tui/0.135.0 (Mac OS 26.2.0; arm64)","headers":{"authorization":"Bearer sk-prod"},"body":{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}}` + "\n"
	if err := os.WriteFile(logPath, []byte(line1), 0600); err != nil {
		t.Fatalf("write log: %v", err)
	}

	idx, err := Open(Config{
		LogGlob:         filepath.Join(dir, "*.jsonl"),
		IndexDSN:        indexPath,
		MaxLinesPerScan: 100,
	}, func(key string) (ResolvedToken, error) {
		if key != "sk-prod" {
			t.Fatalf("unexpected key resolution: %q", key)
		}
		return ResolvedToken{TokenID: 7, Name: "prod", KeyTail: "sk-prod"}, nil
	})
	if err != nil {
		t.Fatalf("open indexer: %v", err)
	}
	defer idx.Close()

	if err := idx.ScanOnce(context.Background()); err != nil {
		t.Fatalf("first scan: %v", err)
	}
	assertIndexedRows(t, idx, 1)

	items, err := idx.Lookup(context.Background(), LookupFilter{TokenID: 7, Model: "gpt-4o", CreatedAt: 1000, LogID: 123})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items len = %d", len(items))
	}
	if items[0].Messages[0].Content != "hello" {
		t.Fatalf("unexpected messages: %+v", items[0].Messages)
	}
	var storedBody string
	var compressedLen int
	var bodyEncoding string
	if err := idx.db.QueryRow(`SELECT body, length(body_gzip), body_encoding FROM audit_entries WHERE id = ?`, items[0].ID).Scan(&storedBody, &compressedLen, &bodyEncoding); err != nil {
		t.Fatalf("read compressed body fields: %v", err)
	}
	if storedBody != "" || compressedLen <= 0 || bodyEncoding != bodyEncodingGzip {
		t.Fatalf("body was not stored compressed: body_len=%d compressed_len=%d encoding=%q", len(storedBody), compressedLen, bodyEncoding)
	}
	if items[0].ClientName != "codex" || items[0].ClientVersion != "0.135.0" || items[0].ClientVariant != "tui" {
		t.Fatalf("unexpected client info: %+v", items[0])
	}

	cached, err := idx.Lookup(context.Background(), LookupFilter{LogID: 123})
	if err != nil {
		t.Fatalf("cached lookup: %v", err)
	}
	if len(cached) != 1 || cached[0].ID != items[0].ID || !strings.Contains(cached[0].MatchedNote, "cached match") {
		t.Fatalf("unexpected cached match: %+v", cached)
	}

	clients, err := idx.LookupClientInfo(context.Background(), []LookupFilter{
		{LogID: 123, TokenID: 7, Model: "gpt-4o", CreatedAt: 1000},
	})
	if err != nil {
		t.Fatalf("batch client lookup: %v", err)
	}
	if clients[123].ClientName != "codex" || clients[123].ClientVersion != "0.135.0" || clients[123].ClientVariant != "tui" {
		t.Fatalf("unexpected batch client info: %+v", clients[123])
	}

	appendLine(t, logPath, `{"time":1001,"method":"POST","path":"/v1/messages","headers":{"x-api-key":"sk-prod"},"body":{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hi claude"}]}}`+"\n")
	newLogPath := filepath.Join(dir, "request-body-2.jsonl")
	if err := os.WriteFile(newLogPath, []byte(`{"time":1002,"method":"POST","path":"/v1/chat/completions","headers":{"authorization":"Bearer sk-prod"},"body":{"model":"gpt-4o","messages":[{"role":"user","content":"new file"}]}}`+"\n"), 0600); err != nil {
		t.Fatalf("write second log: %v", err)
	}

	if err := idx.ScanOnce(context.Background()); err != nil {
		t.Fatalf("second scan: %v", err)
	}
	assertIndexedRows(t, idx, 3)

	if err := idx.ScanOnce(context.Background()); err != nil {
		t.Fatalf("third scan: %v", err)
	}
	assertIndexedRows(t, idx, 3)
}

func TestIndexerSkipsUnfinishedLine(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "request-body.jsonl")
	indexPath := filepath.Join(dir, "audit.db")
	line := `{"time":1000,"method":"POST","path":"/v1/chat/completions","headers":{"authorization":"Bearer sk-prod"},"body":{"model":"gpt-4o","messages":[{"role":"user","content":"unfinished"}]}}`
	if err := os.WriteFile(logPath, []byte(line), 0600); err != nil {
		t.Fatalf("write log: %v", err)
	}

	idx, err := Open(Config{
		LogGlob:         filepath.Join(dir, "*.jsonl"),
		IndexDSN:        indexPath,
		MaxLinesPerScan: 100,
	}, func(key string) (ResolvedToken, error) {
		return ResolvedToken{TokenID: 7, KeyTail: "sk-prod"}, nil
	})
	if err != nil {
		t.Fatalf("open indexer: %v", err)
	}
	defer idx.Close()

	if err := idx.ScanOnce(context.Background()); err != nil {
		t.Fatalf("first scan: %v", err)
	}
	assertIndexedRows(t, idx, 0)

	appendLine(t, logPath, "\n")
	if err := idx.ScanOnce(context.Background()); err != nil {
		t.Fatalf("second scan: %v", err)
	}
	assertIndexedRows(t, idx, 1)
}

func TestIndexerNoTimestampFallsBackToLatestTokenCandidate(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "request-body.jsonl")
	indexPath := filepath.Join(dir, "audit.db")
	line := `{"method":"POST","path":"/v1/chat/completions","headers":{"authorization":"Bearer sk-prod"},"body":{"model":"gpt-4o","messages":[{"role":"user","content":"no timestamp"}]}}` + "\n"
	if err := os.WriteFile(logPath, []byte(line), 0600); err != nil {
		t.Fatalf("write log: %v", err)
	}

	idx, err := Open(Config{
		LogGlob:         filepath.Join(dir, "*.jsonl"),
		IndexDSN:        indexPath,
		MaxLinesPerScan: 100,
	}, func(key string) (ResolvedToken, error) {
		return ResolvedToken{TokenID: 7, KeyTail: "sk-prod"}, nil
	})
	if err != nil {
		t.Fatalf("open indexer: %v", err)
	}
	defer idx.Close()

	if err := idx.ScanOnce(context.Background()); err != nil {
		t.Fatalf("scan: %v", err)
	}

	items, err := idx.Lookup(context.Background(), LookupFilter{TokenID: 7, Model: "gpt-4o", CreatedAt: 1000})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items len = %d", len(items))
	}
	if items[0].HasTimestamp {
		t.Fatalf("expected no timestamp: %+v", items[0])
	}
	if items[0].MatchedBy != "token_latest" {
		t.Fatalf("matched_by = %q", items[0].MatchedBy)
	}
	if items[0].Messages[0].Content != "no timestamp" {
		t.Fatalf("unexpected messages: %+v", items[0].Messages)
	}
}

func TestIndexerParsesOpenRestyLocalTimeWithConfiguredTimezone(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "request-body.jsonl")
	indexPath := filepath.Join(dir, "audit.db")
	line := `{"time":"2026-06-10 12:34:56","method":"POST","path":"/v1/chat/completions","headers":{"authorization":"Bearer sk-prod"},"body":{"model":"gpt-4o","messages":[{"role":"user","content":"local time"}]}}` + "\n"
	if err := os.WriteFile(logPath, []byte(line), 0600); err != nil {
		t.Fatalf("write log: %v", err)
	}

	idx, err := Open(Config{
		LogGlob:         filepath.Join(dir, "*.jsonl"),
		IndexDSN:        indexPath,
		TimeZone:        "UTC",
		MaxLinesPerScan: 100,
	}, func(key string) (ResolvedToken, error) {
		return ResolvedToken{TokenID: 7, KeyTail: "sk-prod"}, nil
	})
	if err != nil {
		t.Fatalf("open indexer: %v", err)
	}
	defer idx.Close()

	if err := idx.ScanOnce(context.Background()); err != nil {
		t.Fatalf("scan: %v", err)
	}

	location, err := time.LoadLocation("UTC")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	createdAt := time.Date(2026, 6, 10, 12, 34, 56, 0, location).Unix()
	items, err := idx.Lookup(context.Background(), LookupFilter{TokenID: 7, Model: "gpt-4o", CreatedAt: createdAt})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items len = %d", len(items))
	}
	if !items[0].HasTimestamp {
		t.Fatalf("expected timestamp: %+v", items[0])
	}
	if items[0].CreatedAt != createdAt {
		t.Fatalf("created_at = %d, want %d", items[0].CreatedAt, createdAt)
	}
}

func TestLookupUsesEstimatedRequestStartTime(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "request-body.jsonl")
	indexPath := filepath.Join(dir, "audit.db")
	line := `{"time":1000,"method":"POST","path":"/v1/chat/completions","headers":{"authorization":"Bearer sk-prod"},"body":{"model":"gpt-4o","messages":[{"role":"user","content":"start time"}]}}` + "\n"
	if err := os.WriteFile(logPath, []byte(line), 0600); err != nil {
		t.Fatalf("write log: %v", err)
	}

	idx, err := Open(Config{
		LogGlob:         filepath.Join(dir, "*.jsonl"),
		IndexDSN:        indexPath,
		LookupWindow:    2 * time.Second,
		MaxLinesPerScan: 100,
	}, func(key string) (ResolvedToken, error) {
		return ResolvedToken{TokenID: 7, KeyTail: "sk-prod"}, nil
	})
	if err != nil {
		t.Fatalf("open indexer: %v", err)
	}
	defer idx.Close()

	if err := idx.ScanOnce(context.Background()); err != nil {
		t.Fatalf("scan: %v", err)
	}

	items, err := idx.Lookup(context.Background(), LookupFilter{TokenID: 7, Model: "gpt-4o", CreatedAt: 1010, UseTime: 10})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items len = %d", len(items))
	}
	if items[0].MatchedBy != "token_time" {
		t.Fatalf("matched_by = %q", items[0].MatchedBy)
	}

	items, err = idx.Lookup(context.Background(), LookupFilter{TokenID: 7, Model: "gpt-4o", CreatedAt: 1010})
	if err != nil {
		t.Fatalf("lookup without use time: %v", err)
	}
	if len(items) != 1 || items[0].MatchedBy != "token_latest" {
		t.Fatalf("expected latest fallback without use time, got %+v", items)
	}
}

func TestLookupMatchesLegacyZeroTokenByKeyTail(t *testing.T) {
	dir := t.TempDir()
	indexPath := filepath.Join(dir, "audit.db")

	idx, err := Open(Config{
		LogGlob:         filepath.Join(dir, "*.jsonl"),
		IndexDSN:        indexPath,
		MaxLinesPerScan: 100,
	}, nil)
	if err != nil {
		t.Fatalf("open indexer: %v", err)
	}
	defer idx.Close()

	_, err = idx.db.Exec(`INSERT INTO audit_entries (
		source_id, source_path, source_line, byte_offset, created_at, ingested_at,
		method, path, model, token_id, key_tail, key_hash, request_id, has_timestamp, body
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"legacy:1",
		"/audit/request-body.jsonl",
		1,
		0,
		1000,
		1001,
		"POST",
		"/v1/messages",
		"gpt-4o",
		0,
		"34567890",
		"hash",
		"",
		1,
		`{"model":"gpt-4o","messages":[{"role":"user","content":"legacy"}]}`,
	)
	if err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}

	items, err := idx.Lookup(context.Background(), LookupFilter{TokenID: 7, KeyTail: "34567890", Model: "gpt-4o", CreatedAt: 1000, LogID: 789})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items len = %d", len(items))
	}
	if items[0].Messages[0].Content != "legacy" {
		t.Fatalf("unexpected legacy item: %+v", items[0])
	}
}

func TestIndexerMigratesOldAuditSchema(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "request-body.jsonl")
	indexPath := filepath.Join(dir, "audit.db")
	line := `{"time":1000,"method":"POST","path":"/v1/messages","headers":{"authorization":"Bearer sk-prod","user-agent":"claude-cli/2.1.170 (external, cli, agent-sdk/0.2.123)"},"body":{"model":"claude-sonnet-4","messages":[{"role":"user","content":"migrated"}]}}` + "\n"
	if err := os.WriteFile(logPath, []byte(line), 0600); err != nil {
		t.Fatalf("write log: %v", err)
	}

	db, err := sql.Open("sqlite", indexPath)
	if err != nil {
		t.Fatalf("open old sqlite: %v", err)
	}
	execMany(t, db, []string{
		`CREATE TABLE audit_files (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			path TEXT NOT NULL UNIQUE,
			file_id TEXT NOT NULL,
			generation INTEGER NOT NULL DEFAULT 1,
			size INTEGER NOT NULL DEFAULT 0,
			offset INTEGER NOT NULL DEFAULT 0,
			line_no INTEGER NOT NULL DEFAULT 0,
			last_seen_at INTEGER NOT NULL DEFAULT 0,
			updated_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE audit_entries (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			source_id TEXT NOT NULL,
			source_path TEXT NOT NULL,
			source_line INTEGER NOT NULL,
			byte_offset INTEGER NOT NULL,
			created_at INTEGER NOT NULL,
			ingested_at INTEGER NOT NULL,
			method TEXT NOT NULL DEFAULT '',
			path TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			token_id INTEGER NOT NULL DEFAULT 0,
			key_tail TEXT NOT NULL DEFAULT '',
			key_hash TEXT NOT NULL DEFAULT '',
			request_id TEXT NOT NULL DEFAULT '',
			has_timestamp INTEGER NOT NULL DEFAULT 0,
			body TEXT NOT NULL,
			UNIQUE(source_id, source_line)
		)`,
	})
	if err := db.Close(); err != nil {
		t.Fatalf("close old sqlite: %v", err)
	}

	idx, err := Open(Config{
		LogGlob:         filepath.Join(dir, "*.jsonl"),
		IndexDSN:        indexPath,
		MaxLinesPerScan: 100,
	}, func(key string) (ResolvedToken, error) {
		return ResolvedToken{TokenID: 7, KeyTail: "sk-prod"}, nil
	})
	if err != nil {
		t.Fatalf("open indexer: %v", err)
	}
	defer idx.Close()

	if err := idx.ScanOnce(context.Background()); err != nil {
		t.Fatalf("scan: %v", err)
	}
	items, err := idx.Lookup(context.Background(), LookupFilter{TokenID: 7, Model: "claude-sonnet-4", CreatedAt: 1000, LogID: 456})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items len = %d", len(items))
	}
	if items[0].ClientName != "claude" || items[0].ClientVersion != "2.1.170" || items[0].ClientVariant != "cli-agent" {
		t.Fatalf("unexpected migrated client info: %+v", items[0])
	}
}

func TestIndexerCompressesLegacyBodyInMaintenance(t *testing.T) {
	dir := t.TempDir()
	indexPath := filepath.Join(dir, "audit.db")
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"legacy body"}]}`

	db, err := sql.Open("sqlite", indexPath)
	if err != nil {
		t.Fatalf("open old sqlite: %v", err)
	}
	execMany(t, db, []string{
		`CREATE TABLE audit_entries (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			source_id TEXT NOT NULL,
			source_path TEXT NOT NULL,
			source_line INTEGER NOT NULL,
			byte_offset INTEGER NOT NULL,
			created_at INTEGER NOT NULL,
			ingested_at INTEGER NOT NULL,
			method TEXT NOT NULL DEFAULT '',
			path TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			token_id INTEGER NOT NULL DEFAULT 0,
			key_tail TEXT NOT NULL DEFAULT '',
			key_hash TEXT NOT NULL DEFAULT '',
			request_id TEXT NOT NULL DEFAULT '',
			has_timestamp INTEGER NOT NULL DEFAULT 0,
			body TEXT NOT NULL,
			UNIQUE(source_id, source_line)
		)`,
	})
	if _, err := db.Exec(`INSERT INTO audit_entries (
		source_id, source_path, source_line, byte_offset, created_at, ingested_at,
		method, path, model, token_id, key_tail, key_hash, request_id, has_timestamp, body
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"source-1",
		"/audit/request-body.jsonl",
		1,
		0,
		1000,
		1001,
		"POST",
		"/v1/chat/completions",
		"gpt-4o",
		7,
		"sk-prod",
		"hash",
		"",
		1,
		body,
	); err != nil {
		t.Fatalf("insert legacy body: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close old sqlite: %v", err)
	}

	idx, err := Open(Config{
		IndexDSN: indexPath,
	}, nil)
	if err != nil {
		t.Fatalf("open indexer: %v", err)
	}
	defer idx.Close()

	var bodyBeforeMaintenance string
	if err := idx.db.QueryRow(`SELECT body FROM audit_entries WHERE id = 1`).Scan(&bodyBeforeMaintenance); err != nil {
		t.Fatalf("read body before maintenance: %v", err)
	}
	if bodyBeforeMaintenance != body {
		t.Fatalf("open should not block on legacy compression: body=%q", bodyBeforeMaintenance)
	}

	if err := idx.compressLegacyBodies(context.Background()); err != nil {
		t.Fatalf("compress legacy bodies: %v", err)
	}

	var storedBody string
	var compressedLen int
	var bodyEncoding string
	if err := idx.db.QueryRow(`SELECT body, length(body_gzip), body_encoding FROM audit_entries WHERE id = 1`).Scan(&storedBody, &compressedLen, &bodyEncoding); err != nil {
		t.Fatalf("read compressed body fields: %v", err)
	}
	if storedBody != "" || compressedLen <= 0 || bodyEncoding != bodyEncodingGzip {
		t.Fatalf("legacy body was not compressed: body_len=%d compressed_len=%d encoding=%q", len(storedBody), compressedLen, bodyEncoding)
	}

	items, err := idx.Lookup(context.Background(), LookupFilter{TokenID: 7, Model: "gpt-4o", CreatedAt: 1000})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if len(items) != 1 || items[0].Messages[0].Content != "legacy body" {
		t.Fatalf("unexpected decoded legacy body: %+v", items)
	}
}

func assertIndexedRows(t *testing.T, idx *Indexer, want int64) {
	t.Helper()
	status, err := idx.Status(context.Background())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.IndexedRows != want {
		t.Fatalf("indexed rows = %d, want %d", status.IndexedRows, want)
	}
}

func appendLine(t *testing.T, path string, line string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	defer file.Close()
	if _, err := file.WriteString(line); err != nil {
		t.Fatalf("append: %v", err)
	}
}

func execMany(t *testing.T, db *sql.DB, statements []string) {
	t.Helper()
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("exec failed: %v\n%s", err, statement)
		}
	}
}
