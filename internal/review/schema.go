package review

import (
	"context"
	"fmt"
)

func (m *Manager) initSchema(ctx context.Context) error {
	statements := []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA busy_timeout=5000`,
		`CREATE TABLE IF NOT EXISTS review_settings (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			base_url TEXT NOT NULL DEFAULT '',
			api_key_cipher BLOB,
			key_tail TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			policy TEXT NOT NULL DEFAULT '',
			response_mode TEXT NOT NULL DEFAULT 'auto',
			reasoning_effort TEXT NOT NULL DEFAULT 'auto',
			updated_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS review_jobs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			token_ids TEXT NOT NULL,
			models TEXT NOT NULL DEFAULT '[]',
			start_at INTEGER NOT NULL,
			end_at INTEGER NOT NULL,
			role_mode TEXT NOT NULL DEFAULT 'user',
			review_model TEXT NOT NULL DEFAULT '',
			reasoning_effort TEXT NOT NULL DEFAULT 'auto',
			config_hash TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'queued',
			max_entry_id INTEGER NOT NULL DEFAULT 0,
			total_entries INTEGER NOT NULL DEFAULT 0,
			processed_entries INTEGER NOT NULL DEFAULT 0,
			review_units INTEGER NOT NULL DEFAULT 0,
			reviewed_units INTEGER NOT NULL DEFAULT 0,
			cache_hits INTEGER NOT NULL DEFAULT 0,
			flagged_entries INTEGER NOT NULL DEFAULT 0,
			error_entries INTEGER NOT NULL DEFAULT 0,
			estimated_chars INTEGER NOT NULL DEFAULT 0,
			prompt_tokens INTEGER NOT NULL DEFAULT 0,
			completion_tokens INTEGER NOT NULL DEFAULT 0,
			error TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			started_at INTEGER NOT NULL DEFAULT 0,
			completed_at INTEGER NOT NULL DEFAULT 0,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS review_results (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			content_hash TEXT NOT NULL,
			config_hash TEXT NOT NULL,
			decision TEXT NOT NULL,
			risk_score INTEGER NOT NULL,
			categories TEXT NOT NULL DEFAULT '[]',
			reason TEXT NOT NULL DEFAULT '',
			confidence REAL NOT NULL DEFAULT 0,
			prompt_tokens INTEGER NOT NULL DEFAULT 0,
			completion_tokens INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL,
			UNIQUE(content_hash, config_hash)
		)`,
		`CREATE TABLE IF NOT EXISTS review_job_entries (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			job_id INTEGER NOT NULL,
			audit_entry_id INTEGER NOT NULL,
			token_id INTEGER NOT NULL,
			created_at INTEGER NOT NULL,
			request_model TEXT NOT NULL DEFAULT '',
			parent_audit_entry_id INTEGER NOT NULL DEFAULT 0,
			delta_start_index INTEGER NOT NULL DEFAULT 0,
			delta_message_count INTEGER NOT NULL DEFAULT 0,
			content_hash TEXT NOT NULL DEFAULT '',
			content_chars INTEGER NOT NULL DEFAULT 0,
			result_id INTEGER NOT NULL DEFAULT 0,
			delta_decision TEXT NOT NULL DEFAULT '',
			delta_risk_score INTEGER NOT NULL DEFAULT 0,
			effective_result_id INTEGER NOT NULL DEFAULT 0,
			effective_decision TEXT NOT NULL DEFAULT '',
			effective_risk_score INTEGER NOT NULL DEFAULT 0,
			inherited INTEGER NOT NULL DEFAULT 0,
			cache_hit INTEGER NOT NULL DEFAULT 0,
			prompt_tokens INTEGER NOT NULL DEFAULT 0,
			completion_tokens INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT 'pending',
			error TEXT NOT NULL DEFAULT '',
			updated_at INTEGER NOT NULL,
			UNIQUE(job_id, audit_entry_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_review_jobs_status ON review_jobs(status, id)`,
		`CREATE INDEX IF NOT EXISTS idx_review_job_entries_pending ON review_job_entries(job_id, status, token_id, created_at, audit_entry_id)`,
		`CREATE INDEX IF NOT EXISTS idx_review_job_entries_risk ON review_job_entries(job_id, effective_risk_score DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_review_results_hash ON review_results(content_hash, config_hash)`,
	}
	for _, statement := range statements {
		if _, err := m.db.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	for _, column := range []struct {
		table      string
		name       string
		definition string
	}{
		{table: "review_settings", name: "reasoning_effort", definition: "TEXT NOT NULL DEFAULT 'auto'"},
		{table: "review_jobs", name: "reasoning_effort", definition: "TEXT NOT NULL DEFAULT 'auto'"},
		{table: "review_jobs", name: "models", definition: "TEXT NOT NULL DEFAULT '[]'"},
	} {
		if err := m.addColumnIfMissing(ctx, column.table, column.name, column.definition); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) addColumnIfMissing(ctx context.Context, table, column, definition string) error {
	rows, err := m.db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid int
		var name, dataType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	_, err = m.db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition))
	return err
}
