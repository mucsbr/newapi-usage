package audit

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

type Indexer struct {
	db       *sql.DB
	cfg      Config
	resolver TokenResolver

	scanMu sync.Mutex
	wg     sync.WaitGroup

	statusMu      sync.Mutex
	lastScanAt    int64
	lastScanError string
}

type fileState struct {
	ID         int64
	Path       string
	FileID     string
	Generation int64
	Size       int64
	Offset     int64
	LineNo     int64
}

func Open(cfg Config, resolver TokenResolver) (*Indexer, error) {
	if strings.TrimSpace(cfg.IndexDSN) == "" {
		cfg.IndexDSN = "/var/lib/newapi-usage/audit.db"
	}
	if cfg.ScanInterval <= 0 {
		cfg.ScanInterval = 10 * time.Second
	}
	if cfg.LookupWindow <= 0 {
		cfg.LookupWindow = 2 * time.Minute
	}
	if cfg.MaxLinesPerScan <= 0 {
		cfg.MaxLinesPerScan = 50000
	}
	if err := ensureIndexParent(cfg.IndexDSN); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", cfg.IndexDSN)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	idx := &Indexer{db: db, cfg: cfg, resolver: resolver}
	if err := idx.initSchema(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return idx, nil
}

func (i *Indexer) Enabled() bool {
	return strings.TrimSpace(i.cfg.LogGlob) != ""
}

func (i *Indexer) Start(ctx context.Context) {
	if !i.Enabled() {
		return
	}
	i.wg.Add(1)
	go func() {
		defer i.wg.Done()
		i.run(ctx)
	}()
}

func (i *Indexer) Close() error {
	i.wg.Wait()
	return i.db.Close()
}

func (i *Indexer) run(ctx context.Context) {
	_ = i.ScanOnce(ctx)
	ticker := time.NewTicker(i.cfg.ScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = i.ScanOnce(ctx)
		}
	}
}

func (i *Indexer) ScanOnce(ctx context.Context) error {
	if !i.Enabled() {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	i.scanMu.Lock()
	defer i.scanMu.Unlock()

	paths, err := expandGlob(i.cfg.LogGlob)
	if err != nil {
		i.setLastScanError(err)
		return err
	}
	var firstErr error
	remaining := i.cfg.MaxLinesPerScan
	for _, path := range paths {
		if remaining <= 0 {
			break
		}
		processed, err := i.ingestPath(ctx, path, remaining)
		remaining -= processed
		if err != nil {
			slog.Warn("audit ingest failed", "path", path, "error", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	i.setLastScanError(firstErr)
	return firstErr
}

func (i *Indexer) Lookup(ctx context.Context, filter LookupFilter) ([]Entry, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if filter.Limit <= 0 || filter.Limit > 50 {
		filter.Limit = 10
	}
	if strings.TrimSpace(filter.RequestID) != "" {
		items, err := i.lookupByRequestID(ctx, filter.RequestID, filter.Limit)
		if err != nil {
			return nil, err
		}
		if len(items) > 0 {
			for idx := range items {
				items[idx].MatchedBy = "request_id"
				items[idx].MatchedNote = "exact request_id match"
			}
			return items, nil
		}
	}
	if filter.TokenID <= 0 || filter.CreatedAt <= 0 {
		return []Entry{}, nil
	}
	window := int64(i.cfg.LookupWindow.Seconds())
	if window <= 0 {
		window = 120
	}
	items, err := i.lookupByTokenWindow(ctx, filter, window)
	if err != nil {
		return nil, err
	}
	for idx := range items {
		items[idx].MatchedBy = "token_time"
		items[idx].MatchedNote = fmt.Sprintf("token/model within +/- %ds", window)
	}
	return items, nil
}

func (i *Indexer) Status(ctx context.Context) (Status, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	out := Status{
		Enabled:      i.Enabled(),
		LogGlob:      i.cfg.LogGlob,
		IndexDSN:     i.cfg.IndexDSN,
		ScanInterval: int64(i.cfg.ScanInterval.Seconds()),
		LookupWindow: int64(i.cfg.LookupWindow.Seconds()),
	}
	if err := i.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_files`).Scan(&out.TrackedFiles); err != nil {
		return out, err
	}
	if err := i.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MAX(created_at), 0), COALESCE(MAX(ingested_at), 0) FROM audit_entries`).Scan(&out.IndexedRows, &out.LastCreatedAt, &out.LastIngestedAt); err != nil {
		return out, err
	}
	i.statusMu.Lock()
	out.LastScanAt = i.lastScanAt
	out.LastScanError = i.lastScanError
	i.statusMu.Unlock()
	return out, nil
}

func (i *Indexer) initSchema(ctx context.Context) error {
	statements := []string{
		`PRAGMA journal_mode=WAL`,
		`CREATE TABLE IF NOT EXISTS audit_files (
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
		`CREATE TABLE IF NOT EXISTS audit_entries (
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
			body TEXT NOT NULL,
			UNIQUE(source_id, source_line)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_entries_token_time ON audit_entries(token_id, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_entries_request_id ON audit_entries(request_id)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_entries_model ON audit_entries(model)`,
	}
	for _, statement := range statements {
		if _, err := i.db.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func (i *Indexer) ingestPath(ctx context.Context, path string, maxLines int) (int, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	if info.IsDir() {
		return 0, nil
	}
	identity := fileIdentity(path, info)
	state, err := i.stateForPath(ctx, path, identity, info.Size())
	if err != nil {
		return 0, err
	}
	if info.Size() <= state.Offset {
		return 0, i.touchState(ctx, state, info.Size())
	}

	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	if _, err := file.Seek(state.Offset, io.SeekStart); err != nil {
		return 0, err
	}

	reader := bufio.NewReaderSize(file, 256*1024)
	sourceID := fmt.Sprintf("%s:%d", state.FileID, state.Generation)
	offset := state.Offset
	lineNo := state.LineNo
	processed := 0

	tx, err := i.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	resolveCache := make(map[string]ResolvedToken)
	for processed < maxLines {
		startOffset := offset
		line, err := reader.ReadString('\n')
		if len(line) == 0 && errors.Is(err, io.EOF) {
			break
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return processed, err
		}
		if errors.Is(err, io.EOF) && !strings.HasSuffix(line, "\n") {
			break
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			offset += int64(len(line))
			lineNo++
			processed++
			if errors.Is(err, io.EOF) {
				break
			}
			continue
		}
		record, parseErr := parseLine(trimmed)
		if parseErr != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			offset += int64(len(line))
			lineNo++
			processed++
			continue
		}
		lineNo++
		offset += int64(len(line))
		processed++
		if err := i.insertEntry(ctx, tx, sourceID, path, lineNo, startOffset, record, resolveCache); err != nil {
			return processed, err
		}
		if errors.Is(err, io.EOF) {
			break
		}
	}

	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx, `UPDATE audit_files SET size = ?, offset = ?, line_no = ?, last_seen_at = ?, updated_at = ? WHERE id = ?`, info.Size(), offset, lineNo, now, now, state.ID); err != nil {
		return processed, err
	}
	if err := tx.Commit(); err != nil {
		return processed, err
	}
	return processed, nil
}

func (i *Indexer) insertEntry(ctx context.Context, tx *sql.Tx, sourceID string, path string, lineNo int64, offset int64, record parsedRecord, resolveCache map[string]ResolvedToken) error {
	keyHash := ""
	keyTail := ""
	tokenID := int64(0)
	if record.APIKey != "" {
		sum := sha256.Sum256([]byte(record.APIKey))
		keyHash = hex.EncodeToString(sum[:])
		keyTail = tail(record.APIKey, 8)
		if cached, ok := resolveCache[keyHash]; ok {
			tokenID = cached.TokenID
			if cached.KeyTail != "" {
				keyTail = cached.KeyTail
			}
		} else if i.resolver != nil {
			resolved, err := i.resolver(record.APIKey)
			if err == nil {
				resolveCache[keyHash] = resolved
				tokenID = resolved.TokenID
				if resolved.KeyTail != "" {
					keyTail = resolved.KeyTail
				}
			}
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO audit_entries (
		source_id, source_path, source_line, byte_offset, created_at, ingested_at,
		method, path, model, token_id, key_tail, key_hash, request_id, body
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sourceID,
		path,
		lineNo,
		offset,
		record.CreatedAt,
		time.Now().Unix(),
		record.Method,
		record.Path,
		record.Model,
		tokenID,
		keyTail,
		keyHash,
		record.RequestID,
		record.Body,
	)
	return err
}

func (i *Indexer) stateForPath(ctx context.Context, path string, fileID string, size int64) (fileState, error) {
	var state fileState
	err := i.db.QueryRowContext(ctx, `SELECT id, path, file_id, generation, size, offset, line_no FROM audit_files WHERE path = ?`, path).Scan(
		&state.ID,
		&state.Path,
		&state.FileID,
		&state.Generation,
		&state.Size,
		&state.Offset,
		&state.LineNo,
	)
	now := time.Now().Unix()
	if errors.Is(err, sql.ErrNoRows) {
		result, err := i.db.ExecContext(ctx, `INSERT INTO audit_files (path, file_id, generation, size, offset, line_no, last_seen_at, updated_at) VALUES (?, ?, 1, ?, 0, 0, ?, ?)`, path, fileID, size, now, now)
		if err != nil {
			return fileState{}, err
		}
		id, _ := result.LastInsertId()
		return fileState{ID: id, Path: path, FileID: fileID, Generation: 1, Size: size}, nil
	}
	if err != nil {
		return fileState{}, err
	}
	if state.FileID != fileID {
		state.FileID = fileID
		state.Generation = 1
		state.Size = size
		state.Offset = 0
		state.LineNo = 0
	} else if size < state.Offset {
		state.Generation++
		state.Size = size
		state.Offset = 0
		state.LineNo = 0
	}
	if _, err := i.db.ExecContext(ctx, `UPDATE audit_files SET file_id = ?, generation = ?, size = ?, offset = ?, line_no = ?, last_seen_at = ?, updated_at = ? WHERE id = ?`, state.FileID, state.Generation, state.Size, state.Offset, state.LineNo, now, now, state.ID); err != nil {
		return fileState{}, err
	}
	return state, nil
}

func (i *Indexer) touchState(ctx context.Context, state fileState, size int64) error {
	now := time.Now().Unix()
	_, err := i.db.ExecContext(ctx, `UPDATE audit_files SET size = ?, last_seen_at = ?, updated_at = ? WHERE id = ?`, size, now, now, state.ID)
	return err
}

func (i *Indexer) lookupByRequestID(ctx context.Context, requestID string, limit int) ([]Entry, error) {
	rows, err := i.db.QueryContext(ctx, `SELECT id, created_at, ingested_at, source_path, source_line, byte_offset, method, path, model, token_id, key_tail, key_hash, request_id, body
		FROM audit_entries
		WHERE request_id = ?
		ORDER BY id DESC
		LIMIT ?`, requestID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEntries(rows)
}

func (i *Indexer) lookupByTokenWindow(ctx context.Context, filter LookupFilter, window int64) ([]Entry, error) {
	args := []any{filter.TokenID, filter.CreatedAt - window, filter.CreatedAt + window}
	conditions := []string{"token_id = ?", "created_at >= ?", "created_at <= ?"}
	if strings.TrimSpace(filter.Model) != "" {
		args = append(args, strings.TrimSpace(filter.Model))
		conditions = append(conditions, "model = ?")
	}
	args = append(args, filter.CreatedAt, filter.Limit)
	query := fmt.Sprintf(`SELECT id, created_at, ingested_at, source_path, source_line, byte_offset, method, path, model, token_id, key_tail, key_hash, request_id, body
		FROM audit_entries
		WHERE %s
		ORDER BY ABS(created_at - ?) ASC, id DESC
		LIMIT ?`, strings.Join(conditions, " AND "))
	rows, err := i.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEntries(rows)
}

func scanEntries(rows *sql.Rows) ([]Entry, error) {
	items := make([]Entry, 0)
	for rows.Next() {
		var item Entry
		if err := rows.Scan(
			&item.ID,
			&item.CreatedAt,
			&item.IngestedAt,
			&item.SourcePath,
			&item.SourceLine,
			&item.ByteOffset,
			&item.Method,
			&item.Path,
			&item.Model,
			&item.TokenID,
			&item.KeyTail,
			&item.KeyHash,
			&item.RequestID,
			&item.Body,
		); err != nil {
			return nil, err
		}
		item.Messages = NormalizeMessages(item.Body)
		items = append(items, item)
	}
	return items, rows.Err()
}

func expandGlob(globs string) ([]string, error) {
	patterns := strings.FieldsFunc(globs, func(r rune) bool {
		return r == ',' || r == '\n' || r == ';'
	})
	seen := make(map[string]bool)
	paths := make([]string, 0)
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		if info, err := os.Stat(pattern); err == nil && info.IsDir() {
			pattern = filepath.Join(pattern, "*.jsonl")
		}
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, err
		}
		for _, path := range matches {
			if !seen[path] {
				seen[path] = true
				paths = append(paths, path)
			}
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func fileIdentity(path string, info os.FileInfo) string {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return fmt.Sprintf("%d:%d", uint64(stat.Dev), uint64(stat.Ino))
	}
	return fmt.Sprintf("%s:%d:%d", path, info.ModTime().UnixNano(), info.Size())
}

func ensureIndexParent(dsn string) error {
	path := strings.TrimSpace(dsn)
	if path == "" || path == ":memory:" {
		return nil
	}
	if strings.HasPrefix(path, "file:") {
		path = strings.TrimPrefix(path, "file:")
		if idx := strings.Index(path, "?"); idx >= 0 {
			path = path[:idx]
		}
	}
	if path == "" || path == ":memory:" {
		return nil
	}
	if !filepath.IsAbs(path) && !strings.Contains(path, string(os.PathSeparator)) {
		return nil
	}
	return os.MkdirAll(filepath.Dir(path), 0750)
}

func tail(value string, n int) string {
	value = strings.TrimSpace(value)
	if len(value) <= n {
		return value
	}
	return value[len(value)-n:]
}

func (i *Indexer) setLastScanError(err error) {
	i.statusMu.Lock()
	defer i.statusMu.Unlock()
	i.lastScanAt = time.Now().Unix()
	if err == nil {
		i.lastScanError = ""
		return
	}
	i.lastScanError = err.Error()
}
