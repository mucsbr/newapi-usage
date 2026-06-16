package server

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mucsbr/newapi-usage/internal/audit"
	"github.com/mucsbr/newapi-usage/internal/store"
)

//go:embed web
var embeddedFiles embed.FS

type Server struct {
	store         *store.Store
	audit         *audit.Indexer
	adminPassword string
	mux           *http.ServeMux
}

func New(st *store.Store, aud *audit.Indexer, adminPassword string) *Server {
	s := &Server{store: st, audit: aud, adminPassword: adminPassword, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler {
	return loggingMiddleware(s.authMiddleware(s.mux))
}

func (s *Server) routes() {
	staticFS, err := fs.Sub(embeddedFiles, "web")
	if err != nil {
		panic(err)
	}
	s.mux.Handle("/", http.FileServer(http.FS(staticFS)))
	s.mux.HandleFunc("/api/health", s.handleHealth)
	s.mux.HandleFunc("/api/auth/status", s.handleAuthStatus)
	s.mux.HandleFunc("/api/auth/login", s.handleAuthLogin)
	s.mux.HandleFunc("/api/auth/logout", s.handleAuthLogout)
	s.mux.HandleFunc("/api/summary", s.handleSummary)
	s.mux.HandleFunc("/api/keys", s.handleKeys)
	s.mux.HandleFunc("/api/logs", s.handleLogs)
	s.mux.HandleFunc("/api/logs/", s.handleLogSubroutes)
	s.mux.HandleFunc("/api/audit/status", s.handleAuditStatus)
	s.mux.HandleFunc("/api/keys/", s.handleKeySubroutes)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	data, err := s.store.Summary(r.Context(), parseTimeRange(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, data)
}

func (s *Server) handleKeys(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	q := r.URL.Query()
	data, err := s.store.KeyUsage(r.Context(), store.KeyFilter{
		TimeRange: parseTimeRange(r),
		Query:     q.Get("q"),
		Limit:     clampInt(queryInt(r, "limit", 100), 1, 500),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, data)
}

func (s *Server) handleKeySubroutes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	trimmed := strings.TrimPrefix(r.URL.Path, "/api/keys/")
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	if len(parts) != 2 || parts[1] != "models" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	tokenID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || tokenID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid token id")
		return
	}
	data, err := s.store.ModelUsage(r.Context(), store.ModelFilter{
		TimeRange: parseTimeRange(r),
		TokenID:   tokenID,
		Limit:     clampInt(queryInt(r, "limit", 100), 1, 500),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, data)
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	q := r.URL.Query()
	data, err := s.store.Logs(r.Context(), store.LogFilter{
		TimeRange: parseTimeRange(r),
		TokenID:   int64(queryInt(r, "token_id", 0)),
		Model:     q.Get("model"),
		Query:     q.Get("q"),
		LogType:   q.Get("type"),
		Page:      clampInt(queryInt(r, "page", 1), 1, 1000000),
		PageSize:  clampInt(queryInt(r, "page_size", 100), 1, 500),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.enrichLogsWithAudit(r.Context(), data.Items)
	writeJSON(w, http.StatusOK, data)
}

func (s *Server) enrichLogsWithAudit(ctx context.Context, items []store.UsageLog) {
	if s.audit == nil || !s.audit.Enabled() || len(items) == 0 {
		return
	}
	if err := s.audit.ScanOnce(ctx); err != nil {
		slog.Warn("audit scan before log enrichment failed", "error", err)
	}
	filters := make([]audit.LookupFilter, 0, len(items))
	for _, item := range items {
		filters = append(filters, audit.LookupFilter{
			RequestID: item.RequestID,
			TokenID:   item.TokenID,
			KeyTail:   item.KeyTail,
			Model:     item.ModelName,
			CreatedAt: item.CreatedAt,
			UseTime:   item.UseTime,
			LogID:     item.ID,
			Limit:     1,
		})
	}
	clients, err := s.audit.LookupClientInfo(ctx, filters)
	if err != nil {
		slog.Warn("audit client batch lookup failed", "error", err)
		return
	}
	for idx := range items {
		if entry, ok := clients[items[idx].ID]; ok {
			items[idx].ClientName = entry.ClientName
			items[idx].ClientVersion = entry.ClientVersion
			items[idx].ClientVariant = entry.ClientVariant
			items[idx].UserAgent = entry.UserAgent
		}
	}
}

func (s *Server) handleLogSubroutes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	trimmed := strings.TrimPrefix(r.URL.Path, "/api/logs/")
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	if len(parts) != 2 || parts[1] != "audit" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	logID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || logID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid log id")
		return
	}
	logItem, err := s.store.LogByID(r.Context(), logID)
	if err != nil {
		writeError(w, http.StatusNotFound, "log not found")
		return
	}
	out := logAuditResponse{
		Enabled: s.audit != nil && s.audit.Enabled(),
		Log:     logItem,
		Items:   []audit.Entry{},
	}
	if s.audit == nil || !s.audit.Enabled() {
		writeJSON(w, http.StatusOK, out)
		return
	}
	filter := audit.LookupFilter{
		RequestID: logItem.RequestID,
		TokenID:   logItem.TokenID,
		KeyTail:   logItem.KeyTail,
		Model:     logItem.ModelName,
		CreatedAt: logItem.CreatedAt,
		UseTime:   logItem.UseTime,
		LogID:     logItem.ID,
		Limit:     10,
	}
	items, err := s.audit.Lookup(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(items) == 0 {
		if scanErr := s.audit.ScanOnce(r.Context()); scanErr != nil {
			slog.Warn("audit scan before lookup retry failed", "log_id", logItem.ID, "error", scanErr)
		} else {
			items, err = s.audit.Lookup(r.Context(), filter)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
	}
	out.Items = items
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAuditStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.audit == nil {
		writeJSON(w, http.StatusOK, audit.Status{Enabled: false})
		return
	}
	status, err := s.audit.Status(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func parseTimeRange(r *http.Request) store.TimeRange {
	return store.TimeRange{
		Start: int64(queryInt(r, "start", 0)),
		End:   int64(queryInt(r, "end", 0)),
	}
}

func queryInt(r *http.Request, key string, fallback int) int {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}

func clampInt(value int, min int, max int) int {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"status":  status,
		},
	})
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		if !strings.HasPrefix(r.URL.Path, "/api/health") {
			slog.Info("request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", rw.status,
				"duration_ms", time.Since(start).Milliseconds(),
				"remote", r.RemoteAddr,
			)
		}
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	if status < 100 {
		status = http.StatusOK
	}
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func IsServerClosed(err error) bool {
	return errors.Is(err, http.ErrServerClosed)
}

type logAuditResponse struct {
	Enabled bool           `json:"enabled"`
	Log     store.UsageLog `json:"log"`
	Items   []audit.Entry  `json:"items"`
}
