package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/mucsbr/newapi-usage/internal/audit"
)

type cleanupRangeRequest struct {
	StartAt int64  `json:"start_at"`
	EndAt   int64  `json:"end_at"`
	Confirm string `json:"confirm"`
}

func (s *Server) handleAuditCleanupOverview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.audit == nil || !s.audit.Enabled() {
		writeJSON(w, http.StatusOK, audit.CleanupOverview{Enabled: false})
		return
	}
	data, err := s.audit.CleanupOverview(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, data)
}

func (s *Server) handleAuditCleanupEstimate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.audit == nil || !s.audit.Enabled() {
		writeError(w, http.StatusNotFound, "audit disabled")
		return
	}
	var req cleanupRangeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	data, err := s.audit.EstimateCleanup(r.Context(), req.StartAt, req.EndAt)
	if err != nil {
		writeCleanupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, data)
}

func (s *Server) handleAuditCleanupJobs(w http.ResponseWriter, r *http.Request) {
	if s.audit == nil || !s.audit.Enabled() {
		writeError(w, http.StatusNotFound, "audit disabled")
		return
	}
	switch r.Method {
	case http.MethodGet:
		items, err := s.audit.ListCleanupJobs(r.Context(), clampInt(queryInt(r, "limit", 10), 1, 50))
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	case http.MethodPost:
		var req cleanupRangeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if strings.TrimSpace(req.Confirm) != "清理" {
			writeError(w, http.StatusBadRequest, "confirmation text must be 清理")
			return
		}
		job, err := s.audit.CreateCleanupJob(r.Context(), req.StartAt, req.EndAt)
		if err != nil {
			writeCleanupError(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, job)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleAuditCleanupJob(w http.ResponseWriter, r *http.Request) {
	if s.audit == nil || !s.audit.Enabled() {
		writeError(w, http.StatusNotFound, "audit disabled")
		return
	}
	trimmed := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/audit/cleanup/jobs/"), "/")
	parts := strings.Split(trimmed, "/")
	jobID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || jobID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid cleanup job id")
		return
	}
	if len(parts) == 1 && r.Method == http.MethodGet {
		job, err := s.audit.CleanupJob(r.Context(), jobID)
		if err != nil {
			writeCleanupError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, job)
		return
	}
	if len(parts) == 2 && parts[1] == "cancel" && r.Method == http.MethodPost {
		job, err := s.audit.CancelCleanupJob(r.Context(), jobID)
		if err != nil {
			writeCleanupError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, job)
		return
	}
	writeError(w, http.StatusNotFound, "not found")
}

func writeCleanupError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, audit.ErrCleanupPreparing):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, audit.ErrCleanupConflict):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, audit.ErrCleanupNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	default:
		writeError(w, http.StatusBadRequest, err.Error())
	}
}
