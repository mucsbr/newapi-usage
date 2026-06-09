package audit

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIndexerIncrementalImport(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "request-body-1.jsonl")
	indexPath := filepath.Join(dir, "audit.db")

	line1 := `{"time":1000,"method":"POST","path":"/v1/chat/completions","headers":{"authorization":"Bearer sk-prod"},"body":{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}}` + "\n"
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

	items, err := idx.Lookup(context.Background(), LookupFilter{TokenID: 7, Model: "gpt-4o", CreatedAt: 1000})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items len = %d", len(items))
	}
	if items[0].Messages[0].Content != "hello" {
		t.Fatalf("unexpected messages: %+v", items[0].Messages)
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

func TestIndexerParsesOpenRestyLocalTime(t *testing.T) {
	t.Setenv("TZ", "Asia/Shanghai")
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

	location, err := time.LoadLocation("Asia/Shanghai")
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
