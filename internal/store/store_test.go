package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/mucsbr/newapi-usage/internal/config"
	_ "modernc.org/sqlite"
)

func TestStoreQueries(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()

	s := &Store{
		db:      db,
		driver:  config.DriverSQLite,
		timeout: 5 * time.Second,
	}

	execMany(t, db, []string{
		`CREATE TABLE tokens (
			id INTEGER PRIMARY KEY,
			user_id INTEGER,
			key TEXT,
			name TEXT
		)`,
		`CREATE TABLE logs (
			id INTEGER PRIMARY KEY,
			user_id INTEGER,
			created_at INTEGER,
			type INTEGER,
			content TEXT,
			username TEXT,
			token_name TEXT,
			model_name TEXT,
			quota INTEGER,
			prompt_tokens INTEGER,
			completion_tokens INTEGER,
			use_time INTEGER,
			is_stream BOOLEAN,
			channel_id INTEGER,
			channel_name TEXT,
			token_id INTEGER,
			ip TEXT,
			other TEXT,
			request_id TEXT
		)`,
		`INSERT INTO tokens (id, user_id, key, name) VALUES
			(1, 10, 'abcdef1234567890', 'prod-key'),
			(2, 20, 'xyz9876543210000', 'test-key')`,
		`INSERT INTO logs (
			id, user_id, created_at, type, content, username, token_name, model_name,
			quota, prompt_tokens, completion_tokens, use_time, is_stream, channel_id,
			channel_name, token_id, ip, other, request_id
		) VALUES
			(1, 10, 1000, 2, 'ok', 'alice', 'prod-key', 'gpt-4o', 30, 10, 20, 120, 1, 7, 'openai', 1, '1.1.1.1', '{}', 'req-1'),
			(2, 10, 1001, 2, 'ok', 'alice', 'prod-key', 'claude-3', 40, 15, 25, 140, 0, 8, 'claude', 1, '1.1.1.1', '{}', 'req-2'),
			(3, 20, 1002, 5, 'err', 'bob', 'test-key', 'gpt-4o', 0, 0, 0, 30, 0, 7, 'openai', 2, '2.2.2.2', '{}', 'req-3')`,
	})

	summary, err := s.Summary(context.Background(), TimeRange{})
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if summary.RequestCount != 3 || summary.InputTokens != 25 || summary.OutputTokens != 45 {
		t.Fatalf("unexpected summary: %+v", summary)
	}

	keys, err := s.KeyUsage(context.Background(), KeyFilter{Limit: 10})
	if err != nil {
		t.Fatalf("key usage: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("keys len = %d", len(keys))
	}
	if keys[0].TokenID != 1 || keys[0].TotalTokens != 70 || keys[0].KeyTail != "34567890" {
		t.Fatalf("unexpected first key: %+v", keys[0])
	}

	models, err := s.ModelUsage(context.Background(), ModelFilter{TokenID: 1, Limit: 10})
	if err != nil {
		t.Fatalf("model usage: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("models len = %d", len(models))
	}

	logs, err := s.Logs(context.Background(), LogFilter{TokenID: 1, Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	if logs.Total != 2 || len(logs.Items) != 2 {
		t.Fatalf("unexpected logs: %+v", logs)
	}
	if logs.Items[0].RequestID != "req-2" || logs.Items[0].TotalTokens != 40 {
		t.Fatalf("unexpected first log: %+v", logs.Items[0])
	}

	keyNameLogs, err := s.Logs(context.Background(), LogFilter{KeyName: "prod", Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("logs by key name: %v", err)
	}
	if keyNameLogs.Total != 2 || len(keyNameLogs.Items) != 2 {
		t.Fatalf("unexpected key-name logs: total=%d items=%d", keyNameLogs.Total, len(keyNameLogs.Items))
	}
	for _, item := range keyNameLogs.Items {
		if item.TokenID != 1 {
			t.Fatalf("key-name filter leaked token %d", item.TokenID)
		}
	}

	token, err := s.ResolveTokenByKey("sk-abcdef1234567890")
	if err != nil {
		t.Fatalf("resolve prefixed token key: %v", err)
	}
	if token.TokenID != 1 || token.KeyTail != "34567890" {
		t.Fatalf("unexpected resolved token: %+v", token)
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
