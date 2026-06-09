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

	"github.com/mucsbr/newapi-usage/internal/store"
)

//go:embed web
var embeddedFiles embed.FS

type Server struct {
	store *store.Store
	mux   *http.ServeMux
}

func New(st *store.Store) *Server {
	s := &Server{store: st, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler {
	return loggingMiddleware(s.mux)
}

func (s *Server) routes() {
	staticFS, err := fs.Sub(embeddedFiles, "web")
	if err != nil {
		panic(err)
	}
	s.mux.Handle("/", http.FileServer(http.FS(staticFS)))
	s.mux.HandleFunc("/api/health", s.handleHealth)
	s.mux.HandleFunc("/api/summary", s.handleSummary)
	s.mux.HandleFunc("/api/keys", s.handleKeys)
	s.mux.HandleFunc("/api/logs", s.handleLogs)
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
	writeJSON(w, http.StatusOK, data)
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
