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
	db           *sql.DB
	cfg          Config
	resolver     TokenResolver
	timeLocation *time.Location

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

type scanStats struct {
	MatchedFiles    int
	ScannedFiles    int
	UnchangedFiles  int
	ProcessedLines  int
	InsertedEntries int
	BlankLines      int
	InvalidLines    int
	UnfinishedLines int
	BytesRead       int64
	LineLimitHit    bool
}

type fileScanResult struct {
	ScannedFiles    int
	UnchangedFiles  int
	ProcessedLines  int
	InsertedEntries int
	BlankLines      int
	InvalidLines    int
	UnfinishedLines int
	BytesRead       int64
}

func (s *scanStats) add(result fileScanResult) {
	s.ScannedFiles += result.ScannedFiles
	s.UnchangedFiles += result.UnchangedFiles
	s.ProcessedLines += result.ProcessedLines
	s.InsertedEntries += result.InsertedEntries
	s.BlankLines += result.BlankLines
	s.InvalidLines += result.InvalidLines
	s.UnfinishedLines += result.UnfinishedLines
	s.BytesRead += result.BytesRead
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
	location, err := loadTimeLocation(cfg.TimeZone)
	if err != nil {
		return nil, err
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
	idx := &Indexer{db: db, cfg: cfg, resolver: resolver, timeLocation: location}
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

	started := time.Now()
	paths, err := expandGlob(i.cfg.LogGlob)
	if err != nil {
		i.setLastScanError(err)
		return err
	}
	stats := scanStats{MatchedFiles: len(paths)}
	var firstErr error
	remaining := i.cfg.MaxLinesPerScan
	for _, path := range paths {
		if remaining <= 0 {
			stats.LineLimitHit = true
			break
		}
		result, err := i.ingestPath(ctx, path, remaining)
		stats.add(result)
		remaining -= result.ProcessedLines
		if err != nil {
			slog.Warn("audit ingest failed", "path", path, "error", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	i.setLastScanError(firstErr)
	attrs := []any{
		"glob", i.cfg.LogGlob,
		"matched_files", stats.MatchedFiles,
		"scanned_files", stats.ScannedFiles,
		"unchanged_files", stats.UnchangedFiles,
		"processed_lines", stats.ProcessedLines,
		"inserted_entries", stats.InsertedEntries,
		"blank_lines", stats.BlankLines,
		"invalid_lines", stats.InvalidLines,
		"unfinished_lines", stats.UnfinishedLines,
		"bytes_read", stats.BytesRead,
		"line_limit_hit", stats.LineLimitHit,
		"duration_ms", time.Since(started).Milliseconds(),
	}
	if firstErr != nil {
		attrs = append(attrs, "error", firstErr)
		slog.Warn("audit scan completed", attrs...)
	} else {
		slog.Info("audit scan completed", attrs...)
	}
	return firstErr
}

func (i *Indexer) Lookup(ctx context.Context, filter LookupFilter) ([]Entry, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if filter.Limit <= 0 || filter.Limit > 50 {
		filter.Limit = 10
	}
	if filter.LogID > 0 {
		items, err := i.lookupCachedMatch(ctx, filter.LogID)
		if err != nil {
			return nil, err
		}
		if len(items) > 0 {
			return items, nil
		}
	}
	if filter.TokenID > 0 && filter.CreatedAt > 0 {
		window := int64(i.cfg.LookupWindow.Seconds())
		if window <= 0 {
			window = 120
		}
		center := lookupCenter(filter)
		items, err := i.lookupByTokenWindow(ctx, filter, window)
		if err != nil {
			return nil, err
		}
		if len(items) > 0 {
			for idx := range items {
				items[idx].MatchedBy = "token_time"
				items[idx].MatchedNote = fmt.Sprintf("same token within +/- %ds around estimated request start %d; model match is ranked first", window, center)
			}
			i.rememberLookup(ctx, filter.LogID, items)
			return items, nil
		}
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
			i.rememberLookup(ctx, filter.LogID, items)
			return items, nil
		}
	}
	if filter.TokenID <= 0 {
		return []Entry{}, nil
	}
	items, err := i.lookupLatestByToken(ctx, filter)
	if err != nil {
		return nil, err
	}
	for idx := range items {
		items[idx].MatchedBy = "token_latest"
		items[idx].MatchedNote = "audit log has no usable timestamp match; newest same-token candidates are shown with model match ranked first"
	}
	return items, nil
}

func (i *Indexer) rememberLookup(ctx context.Context, logID int64, items []Entry) {
	if logID <= 0 || len(items) == 0 {
		return
	}
	first := items[0]
	if first.MatchedBy != "token_time" && first.MatchedBy != "request_id" {
		return
	}
	if err := i.rememberMatch(ctx, logID, first); err != nil {
		slog.Warn("audit match cache failed", "log_id", logID, "audit_entry_id", first.ID, "error", err)
	}
}

func (i *Indexer) LookupClientInfo(ctx context.Context, filters []LookupFilter) (map[int64]Entry, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	out := make(map[int64]Entry)
	cleaned := make([]LookupFilter, 0, len(filters))
	logIDs := make([]int64, 0, len(filters))
	seenLogID := make(map[int64]bool)
	for _, filter := range filters {
		if filter.LogID <= 0 || seenLogID[filter.LogID] {
			continue
		}
		seenLogID[filter.LogID] = true
		cleaned = append(cleaned, filter)
		logIDs = append(logIDs, filter.LogID)
	}
	if len(cleaned) == 0 {
		return out, nil
	}

	cached, err := i.lookupCachedClients(ctx, logIDs)
	if err != nil {
		return nil, err
	}
	for logID, entry := range cached {
		out[logID] = entry
	}

	pending := make([]LookupFilter, 0, len(cleaned))
	for _, filter := range cleaned {
		if _, ok := out[filter.LogID]; ok {
			continue
		}
		if filter.TokenID <= 0 || filter.CreatedAt <= 0 {
			continue
		}
		pending = append(pending, filter)
	}
	if len(pending) == 0 {
		return out, nil
	}

	candidates, err := i.lookupClientCandidates(ctx, pending)
	if err != nil {
		return nil, err
	}
	window := int64(i.cfg.LookupWindow.Seconds())
	if window <= 0 {
		window = 120
	}
	for _, filter := range pending {
		entry, ok := bestClientCandidate(filter, candidates, window)
		if !ok {
			continue
		}
		entry.MatchedBy = "token_time"
		entry.MatchedNote = fmt.Sprintf("same token within +/- %ds around estimated request start %d; model match is ranked first", window, lookupCenter(filter))
		out[filter.LogID] = entry
		i.rememberLookup(ctx, filter.LogID, []Entry{entry})
	}
	return out, nil
}

func (i *Indexer) Status(ctx context.Context) (Status, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	out := Status{
		Enabled:      i.Enabled(),
		LogGlob:      i.cfg.LogGlob,
		IndexDSN:     i.cfg.IndexDSN,
		TimeZone:     i.cfg.TimeZone,
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
			user_agent TEXT NOT NULL DEFAULT '',
			client_name TEXT NOT NULL DEFAULT '',
			client_version TEXT NOT NULL DEFAULT '',
			client_variant TEXT NOT NULL DEFAULT '',
			request_id TEXT NOT NULL DEFAULT '',
			has_timestamp INTEGER NOT NULL DEFAULT 0,
			body TEXT NOT NULL,
			body_gzip BLOB,
			body_encoding TEXT NOT NULL DEFAULT '',
			UNIQUE(source_id, source_line)
		)`,
		`CREATE TABLE IF NOT EXISTS log_audit_matches (
			log_id INTEGER PRIMARY KEY,
			audit_entry_id INTEGER NOT NULL,
			matched_by TEXT NOT NULL DEFAULT '',
			matched_note TEXT NOT NULL DEFAULT '',
			matched_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_entries_token_time ON audit_entries(token_id, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_entries_request_id ON audit_entries(request_id)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_entries_model ON audit_entries(model)`,
		`CREATE INDEX IF NOT EXISTS idx_log_audit_matches_entry ON log_audit_matches(audit_entry_id)`,
	}
	for _, statement := range statements {
		if _, err := i.db.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	columns := []struct {
		name       string
		definition string
	}{
		{name: "has_timestamp", definition: "INTEGER NOT NULL DEFAULT 0"},
		{name: "user_agent", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "client_name", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "client_version", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "client_variant", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "body_gzip", definition: "BLOB"},
		{name: "body_encoding", definition: "TEXT NOT NULL DEFAULT ''"},
	}
	for _, column := range columns {
		if err := i.addColumnIfMissing(ctx, "audit_entries", column.name, column.definition); err != nil {
			return err
		}
	}
	if err := i.compressLegacyBodies(ctx); err != nil {
		return err
	}
	return nil
}

func (i *Indexer) addColumnIfMissing(ctx context.Context, table string, column string, definition string) error {
	rows, err := i.db.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name string
		var columnType string
		var notNull int
		var defaultValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = i.db.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, definition))
	return err
}

func (i *Indexer) compressLegacyBodies(ctx context.Context) error {
	const batchSize = 200
	total := 0
	for {
		rows, err := i.db.QueryContext(ctx, `SELECT id, body
			FROM audit_entries
			WHERE body <> '' AND NOT (body_encoding = ? AND COALESCE(length(body_gzip), 0) > 0)
			ORDER BY id
			LIMIT ?`, bodyEncodingGzip, batchSize)
		if err != nil {
			return err
		}
		type legacyBody struct {
			id   int64
			body string
		}
		batch := make([]legacyBody, 0, batchSize)
		for rows.Next() {
			var item legacyBody
			if err := rows.Scan(&item.id, &item.body); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, item)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if len(batch) == 0 {
			break
		}

		tx, err := i.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		for _, item := range batch {
			bodyGzip, bodyEncoding, err := encodeBody(item.body)
			if err != nil {
				tx.Rollback()
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE audit_entries
				SET body = '', body_gzip = ?, body_encoding = ?
				WHERE id = ? AND body <> ''`, bodyGzip, bodyEncoding, item.id); err != nil {
				tx.Rollback()
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		total += len(batch)
	}
	if total > 0 {
		slog.Info("compressed legacy audit bodies", "rows", total)
	}
	if _, err := i.db.ExecContext(ctx, `UPDATE audit_entries
		SET body = ''
		WHERE body <> '' AND body_encoding = ? AND COALESCE(length(body_gzip), 0) > 0`, bodyEncodingGzip); err != nil {
		return err
	}
	return nil
}

func (i *Indexer) ingestPath(ctx context.Context, path string, maxLines int) (fileScanResult, error) {
	result := fileScanResult{}
	info, err := os.Stat(path)
	if err != nil {
		return result, err
	}
	if info.IsDir() {
		return result, nil
	}
	result.ScannedFiles = 1
	identity := fileIdentity(path, info)
	state, err := i.stateForPath(ctx, path, identity, info.Size())
	if err != nil {
		return result, err
	}
	if info.Size() <= state.Offset {
		result.UnchangedFiles = 1
		return result, i.touchState(ctx, state, info.Size())
	}

	file, err := os.Open(path)
	if err != nil {
		return result, err
	}
	defer file.Close()
	if _, err := file.Seek(state.Offset, io.SeekStart); err != nil {
		return result, err
	}

	reader := bufio.NewReaderSize(file, 256*1024)
	sourceID := fmt.Sprintf("%s:%d", state.FileID, state.Generation)
	offset := state.Offset
	lineNo := state.LineNo

	tx, err := i.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()

	resolveCache := make(map[string]ResolvedToken)
	for result.ProcessedLines < maxLines {
		startOffset := offset
		line, err := reader.ReadString('\n')
		if len(line) == 0 && errors.Is(err, io.EOF) {
			break
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return result, err
		}
		if errors.Is(err, io.EOF) && !strings.HasSuffix(line, "\n") {
			result.UnfinishedLines++
			break
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			offset += int64(len(line))
			lineNo++
			result.ProcessedLines++
			result.BlankLines++
			result.BytesRead += int64(len(line))
			if errors.Is(err, io.EOF) {
				break
			}
			continue
		}
		record, parseErr := parseLine(trimmed, i.timeLocation)
		if parseErr != nil {
			offset += int64(len(line))
			lineNo++
			result.ProcessedLines++
			result.InvalidLines++
			result.BytesRead += int64(len(line))
			continue
		}
		lineNo++
		offset += int64(len(line))
		result.ProcessedLines++
		result.BytesRead += int64(len(line))
		inserted, err := i.insertEntry(ctx, tx, sourceID, path, lineNo, startOffset, record, resolveCache)
		if err != nil {
			return result, err
		}
		if inserted {
			result.InsertedEntries++
		}
		if errors.Is(err, io.EOF) {
			break
		}
	}

	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx, `UPDATE audit_files SET size = ?, offset = ?, line_no = ?, last_seen_at = ?, updated_at = ? WHERE id = ?`, info.Size(), offset, lineNo, now, now, state.ID); err != nil {
		return result, err
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	return result, nil
}

func (i *Indexer) insertEntry(ctx context.Context, tx *sql.Tx, sourceID string, path string, lineNo int64, offset int64, record parsedRecord, resolveCache map[string]ResolvedToken) (bool, error) {
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
	bodyGzip, bodyEncoding, err := encodeBody(record.Body)
	if err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO audit_entries (
		source_id, source_path, source_line, byte_offset, created_at, ingested_at,
		method, path, model, token_id, key_tail, key_hash, user_agent, client_name,
		client_version, client_variant, request_id, has_timestamp, body, body_gzip, body_encoding
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
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
		record.UserAgent,
		record.ClientName,
		record.ClientVersion,
		record.ClientVariant,
		record.RequestID,
		record.HasTimestamp,
		"",
		bodyGzip,
		bodyEncoding,
	)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows > 0, err
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
	query := fmt.Sprintf(`SELECT %s
		FROM audit_entries
		WHERE request_id = ?
		ORDER BY id DESC
		LIMIT ?`, entrySelectColumns(""))
	rows, err := i.db.QueryContext(ctx, query, requestID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEntries(rows)
}

func (i *Indexer) lookupCachedMatch(ctx context.Context, logID int64) ([]Entry, error) {
	query := fmt.Sprintf(`SELECT %s, m.matched_by, m.matched_note
		FROM log_audit_matches m
		JOIN audit_entries e ON e.id = m.audit_entry_id
		WHERE m.log_id = ?
		LIMIT 1`, entrySelectColumns("e"))
	rows, err := i.db.QueryContext(ctx, query, logID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items, err := scanEntries(rows)
	if err != nil {
		return nil, err
	}
	for idx := range items {
		if items[idx].MatchedNote == "" {
			items[idx].MatchedNote = "cached match"
		} else {
			items[idx].MatchedNote = "cached match; " + items[idx].MatchedNote
		}
	}
	return items, nil
}

func (i *Indexer) rememberMatch(ctx context.Context, logID int64, entry Entry) error {
	_, err := i.db.ExecContext(ctx, `INSERT OR IGNORE INTO log_audit_matches (
		log_id, audit_entry_id, matched_by, matched_note, matched_at
	) VALUES (?, ?, ?, ?, ?)`, logID, entry.ID, entry.MatchedBy, entry.MatchedNote, time.Now().Unix())
	return err
}

func (i *Indexer) lookupCachedClients(ctx context.Context, logIDs []int64) (map[int64]Entry, error) {
	out := make(map[int64]Entry)
	if len(logIDs) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(logIDs))
	for _, logID := range logIDs {
		args = append(args, logID)
	}
	query := fmt.Sprintf(`SELECT m.log_id, e.id, e.created_at, e.ingested_at, e.source_path, e.source_line,
			e.byte_offset, e.method, e.path, e.model, e.token_id, e.key_tail, e.key_hash,
			e.user_agent, e.client_name, e.client_version, e.client_variant, e.request_id,
			e.has_timestamp, m.matched_by, m.matched_note
		FROM log_audit_matches m
		JOIN audit_entries e ON e.id = m.audit_entry_id
		WHERE m.log_id IN (%s)`, placeholders(len(logIDs)))
	rows, err := i.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var logID int64
		var item Entry
		if err := rows.Scan(
			&logID,
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
			&item.UserAgent,
			&item.ClientName,
			&item.ClientVersion,
			&item.ClientVariant,
			&item.RequestID,
			&item.HasTimestamp,
			&item.MatchedBy,
			&item.MatchedNote,
		); err != nil {
			return nil, err
		}
		out[logID] = item
	}
	return out, rows.Err()
}

func (i *Indexer) lookupClientCandidates(ctx context.Context, filters []LookupFilter) ([]Entry, error) {
	if len(filters) == 0 {
		return nil, nil
	}
	window := int64(i.cfg.LookupWindow.Seconds())
	if window <= 0 {
		window = 120
	}
	minCreated := int64(0)
	maxCreated := int64(0)
	tokenIDs := make(map[int64]bool)
	keyTails := make(map[string]bool)
	for _, filter := range filters {
		center := lookupCenter(filter)
		if center <= 0 {
			continue
		}
		start := center - window
		end := center + window
		if minCreated == 0 || start < minCreated {
			minCreated = start
		}
		if end > maxCreated {
			maxCreated = end
		}
		if filter.TokenID > 0 {
			tokenIDs[filter.TokenID] = true
		}
		if strings.TrimSpace(filter.KeyTail) != "" {
			keyTails[strings.TrimSpace(filter.KeyTail)] = true
		}
	}
	if minCreated == 0 || maxCreated == 0 || (len(tokenIDs) == 0 && len(keyTails) == 0) {
		return nil, nil
	}

	args := []any{minCreated, maxCreated}
	identity := make([]string, 0, 2)
	if len(tokenIDs) > 0 {
		values := make([]int64, 0, len(tokenIDs))
		for tokenID := range tokenIDs {
			values = append(values, tokenID)
		}
		sort.Slice(values, func(a, b int) bool { return values[a] < values[b] })
		placeholders := make([]string, 0, len(values))
		for _, tokenID := range values {
			args = append(args, tokenID)
			placeholders = append(placeholders, "?")
		}
		identity = append(identity, "token_id IN ("+strings.Join(placeholders, ", ")+")")
	}
	if len(keyTails) > 0 {
		values := make([]string, 0, len(keyTails))
		for keyTail := range keyTails {
			values = append(values, keyTail)
		}
		sort.Strings(values)
		placeholders := make([]string, 0, len(values))
		for _, keyTail := range values {
			args = append(args, keyTail)
			placeholders = append(placeholders, "?")
		}
		identity = append(identity, "(token_id = 0 AND key_tail IN ("+strings.Join(placeholders, ", ")+"))")
	}

	query := fmt.Sprintf(`SELECT id, created_at, ingested_at, source_path, source_line, byte_offset,
			method, path, model, token_id, key_tail, key_hash, user_agent, client_name,
			client_version, client_variant, request_id, has_timestamp
		FROM audit_entries
		WHERE has_timestamp = 1 AND created_at >= ? AND created_at <= ? AND (%s)
		ORDER BY created_at DESC, id DESC`, strings.Join(identity, " OR "))
	rows, err := i.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

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
			&item.UserAgent,
			&item.ClientName,
			&item.ClientVersion,
			&item.ClientVariant,
			&item.RequestID,
			&item.HasTimestamp,
		); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (i *Indexer) lookupByTokenWindow(ctx context.Context, filter LookupFilter, window int64) ([]Entry, error) {
	model := strings.TrimSpace(filter.Model)
	center := lookupCenter(filter)
	identityWhere, args := tokenIdentityWhere(filter)
	args = append(args, center-window, center+window)
	modelOrder := "CASE WHEN 1=1 THEN 0 ELSE 0 END"
	if model != "" {
		args = append(args, model)
		modelOrder = "CASE WHEN model = ? THEN 0 ELSE 1 END"
	}
	args = append(args, center, filter.CreatedAt, filter.Limit)
	query := fmt.Sprintf(`SELECT %s
		FROM audit_entries
		WHERE %s AND has_timestamp = 1 AND created_at >= ? AND created_at <= ?
		ORDER BY %s ASC, ABS(created_at - ?) ASC, ABS(created_at - ?) ASC, id DESC
		LIMIT ?`, entrySelectColumns(""), identityWhere, modelOrder)
	rows, err := i.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEntries(rows)
}

func lookupCenter(filter LookupFilter) int64 {
	if filter.CreatedAt <= 0 {
		return 0
	}
	if filter.UseTime > 0 && filter.UseTime < 24*60*60 {
		return filter.CreatedAt - filter.UseTime
	}
	return filter.CreatedAt
}

func (i *Indexer) lookupLatestByToken(ctx context.Context, filter LookupFilter) ([]Entry, error) {
	model := strings.TrimSpace(filter.Model)
	identityWhere, args := tokenIdentityWhere(filter)
	modelOrder := "CASE WHEN 1=1 THEN 0 ELSE 0 END"
	if model != "" {
		args = append(args, model)
		modelOrder = "CASE WHEN model = ? THEN 0 ELSE 1 END"
	}
	args = append(args, filter.Limit)
	query := fmt.Sprintf(`SELECT %s
		FROM audit_entries
		WHERE %s
		ORDER BY %s ASC, id DESC
		LIMIT ?`, entrySelectColumns(""), identityWhere, modelOrder)
	rows, err := i.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEntries(rows)
}

func tokenIdentityWhere(filter LookupFilter) (string, []any) {
	keyTail := strings.TrimSpace(filter.KeyTail)
	if keyTail == "" {
		return "token_id = ?", []any{filter.TokenID}
	}
	return "(token_id = ? OR (token_id = 0 AND key_tail = ?))", []any{filter.TokenID, keyTail}
}

func bestClientCandidate(filter LookupFilter, candidates []Entry, window int64) (Entry, bool) {
	center := lookupCenter(filter)
	if center <= 0 {
		return Entry{}, false
	}
	model := strings.TrimSpace(filter.Model)
	bestSet := false
	var best Entry
	bestModelRank := 0
	bestStartDistance := int64(0)
	bestEndDistance := int64(0)
	for _, candidate := range candidates {
		if !matchesTokenIdentity(filter, candidate) {
			continue
		}
		if candidate.CreatedAt < center-window || candidate.CreatedAt > center+window {
			continue
		}
		modelRank := 0
		if model != "" && candidate.Model != model {
			modelRank = 1
		}
		startDistance := absInt64(candidate.CreatedAt - center)
		endDistance := absInt64(candidate.CreatedAt - filter.CreatedAt)
		if !bestSet ||
			modelRank < bestModelRank ||
			(modelRank == bestModelRank && startDistance < bestStartDistance) ||
			(modelRank == bestModelRank && startDistance == bestStartDistance && endDistance < bestEndDistance) ||
			(modelRank == bestModelRank && startDistance == bestStartDistance && endDistance == bestEndDistance && candidate.ID > best.ID) {
			bestSet = true
			best = candidate
			bestModelRank = modelRank
			bestStartDistance = startDistance
			bestEndDistance = endDistance
		}
	}
	return best, bestSet
}

func matchesTokenIdentity(filter LookupFilter, candidate Entry) bool {
	if filter.TokenID > 0 && candidate.TokenID == filter.TokenID {
		return true
	}
	return candidate.TokenID == 0 && strings.TrimSpace(filter.KeyTail) != "" && candidate.KeyTail == strings.TrimSpace(filter.KeyTail)
}

func absInt64(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}

func placeholders(count int) string {
	if count <= 0 {
		return ""
	}
	values := make([]string, count)
	for idx := range values {
		values[idx] = "?"
	}
	return strings.Join(values, ", ")
}

func entrySelectColumns(alias string) string {
	columns := entryColumnNames()
	if alias == "" {
		return strings.Join(columns, ", ")
	}
	for idx, column := range columns {
		columns[idx] = alias + "." + column
	}
	return strings.Join(columns, ", ")
}

func entryColumnNames() []string {
	return []string{
		"id",
		"created_at",
		"ingested_at",
		"source_path",
		"source_line",
		"byte_offset",
		"method",
		"path",
		"model",
		"token_id",
		"key_tail",
		"key_hash",
		"user_agent",
		"client_name",
		"client_version",
		"client_variant",
		"request_id",
		"has_timestamp",
		"body",
		"body_gzip",
		"body_encoding",
	}
}

func scanEntries(rows *sql.Rows) ([]Entry, error) {
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	baseColumnCount := len(entryColumnNames())
	hasMatchColumns := len(columns) >= baseColumnCount+2
	items := make([]Entry, 0)
	for rows.Next() {
		var item Entry
		var bodyGzip []byte
		var bodyEncoding string
		dest := []any{
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
			&item.UserAgent,
			&item.ClientName,
			&item.ClientVersion,
			&item.ClientVariant,
			&item.RequestID,
			&item.HasTimestamp,
			&item.Body,
			&bodyGzip,
			&bodyEncoding,
		}
		if hasMatchColumns {
			dest = append(dest, &item.MatchedBy, &item.MatchedNote)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		item.Body, err = decodeBody(item.Body, bodyGzip, bodyEncoding)
		if err != nil {
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
