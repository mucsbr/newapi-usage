package server

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/mucsbr/newapi-usage/internal/store"
)

type selfLoginRequest struct {
	Key string `json:"key"`
}

type selfLog struct {
	ID               int64   `json:"id"`
	CreatedAt        int64   `json:"created_at"`
	Type             int64   `json:"type"`
	ModelName        string  `json:"model_name"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	QuotaCNY         float64 `json:"quota_cny"`
	UseTime          int64   `json:"use_time"`
	IsStream         bool    `json:"is_stream"`
	ChannelName      string  `json:"channel_name"`
}

type selfLogPage struct {
	Items    []selfLog `json:"items"`
	Total    int64     `json:"total"`
	Page     int       `json:"page"`
	PageSize int       `json:"page_size"`
}

func (s *Server) handleSelfLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req selfLoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	identity, err := s.resolveSelfKey(req.Key)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid api key")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"key_name": identity.Name,
		"key_tail": identity.KeyTail,
	})
}

func (s *Server) handleSelfSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	identity, ok := s.requireSelfKey(w, r)
	if !ok {
		return
	}
	data, err := s.store.TokenSummary(r.Context(), parseTimeRange(r), identity.TokenID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, data)
}

func (s *Server) handleSelfModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	identity, ok := s.requireSelfKey(w, r)
	if !ok {
		return
	}
	data, err := s.store.ModelUsage(r.Context(), store.ModelFilter{
		TimeRange: parseTimeRange(r),
		TokenID:   identity.TokenID,
		Limit:     clampInt(queryInt(r, "limit", 100), 1, 500),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, data)
}

func (s *Server) handleSelfLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	identity, ok := s.requireSelfKey(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	data, err := s.store.Logs(r.Context(), store.LogFilter{
		TimeRange: parseTimeRange(r),
		TokenID:   identity.TokenID,
		Model:     q.Get("model"),
		LogType:   q.Get("type"),
		Page:      clampInt(queryInt(r, "page", 1), 1, 1000000),
		PageSize:  clampInt(queryInt(r, "page_size", 50), 1, 100),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	items := make([]selfLog, 0, len(data.Items))
	for _, item := range data.Items {
		items = append(items, selfLog{
			ID:               item.ID,
			CreatedAt:        item.CreatedAt,
			Type:             item.Type,
			ModelName:        item.ModelName,
			InputTokens:      item.InputTokens,
			OutputTokens:     item.OutputTokens,
			TotalTokens:      item.TotalTokens,
			CacheReadTokens:  item.CacheReadTokens,
			CacheWriteTokens: item.CacheWriteTokens,
			QuotaCNY:         item.QuotaCNY,
			UseTime:          item.UseTime,
			IsStream:         item.IsStream,
			ChannelName:      item.ChannelName,
		})
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, selfLogPage{Items: items, Total: data.Total, Page: data.Page, PageSize: data.PageSize})
}

func (s *Server) requireSelfKey(w http.ResponseWriter, r *http.Request) (store.TokenIdentity, bool) {
	key := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(key) >= 7 && strings.EqualFold(key[:7], "Bearer ") {
		key = strings.TrimSpace(key[7:])
	} else {
		key = strings.TrimSpace(r.Header.Get("X-API-Key"))
	}
	identity, err := s.resolveSelfKey(key)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid api key")
		return store.TokenIdentity{}, false
	}
	return identity, true
}

func (s *Server) resolveSelfKey(key string) (store.TokenIdentity, error) {
	key = strings.TrimSpace(key)
	if key == "" || len(key) > 512 {
		return store.TokenIdentity{}, sql.ErrNoRows
	}
	return s.store.ResolveTokenByKey(key)
}
