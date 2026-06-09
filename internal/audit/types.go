package audit

import "time"

type Config struct {
	LogGlob         string
	IndexDSN        string
	ScanInterval    time.Duration
	LookupWindow    time.Duration
	MaxLinesPerScan int
}

type ResolvedToken struct {
	TokenID int64
	Name    string
	KeyTail string
}

type TokenResolver func(key string) (ResolvedToken, error)

type Entry struct {
	ID          int64     `json:"id"`
	CreatedAt   int64     `json:"created_at"`
	IngestedAt  int64     `json:"ingested_at"`
	SourcePath  string    `json:"source_path"`
	SourceLine  int64     `json:"source_line"`
	ByteOffset  int64     `json:"byte_offset"`
	Method      string    `json:"method"`
	Path        string    `json:"path"`
	Model       string    `json:"model"`
	TokenID     int64     `json:"token_id"`
	KeyTail     string    `json:"key_tail"`
	KeyHash     string    `json:"key_hash"`
	RequestID   string    `json:"request_id"`
	Body        string    `json:"body"`
	Messages    []Message `json:"messages"`
	MatchedBy   string    `json:"matched_by"`
	MatchedNote string    `json:"matched_note"`
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type LookupFilter struct {
	RequestID string
	TokenID   int64
	Model     string
	CreatedAt int64
	Limit     int
}

type Status struct {
	Enabled        bool   `json:"enabled"`
	LogGlob        string `json:"log_glob"`
	IndexDSN       string `json:"index_dsn"`
	ScanInterval   int64  `json:"scan_interval_seconds"`
	LookupWindow   int64  `json:"lookup_window_seconds"`
	TrackedFiles   int64  `json:"tracked_files"`
	IndexedRows    int64  `json:"indexed_rows"`
	LastCreatedAt  int64  `json:"last_created_at"`
	LastIngestedAt int64  `json:"last_ingested_at"`
	LastScanAt     int64  `json:"last_scan_at"`
	LastScanError  string `json:"last_scan_error,omitempty"`
}

type parsedRecord struct {
	CreatedAt int64
	Method    string
	Path      string
	Model     string
	RequestID string
	APIKey    string
	Body      string
}
