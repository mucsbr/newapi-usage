package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/mucsbr/newapi-usage/internal/review"
)

func (s *Server) handleReviewConfig(w http.ResponseWriter, r *http.Request) {
	if !s.reviewAvailable(w) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		settings, err := s.reviewer.Settings(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, settings)
	case http.MethodPut:
		var input review.SettingsInput
		if err := decodeLimitedJSON(w, r, &input); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		settings, err := s.reviewer.SaveSettings(r.Context(), input)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, settings)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleReviewConfigTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.reviewAvailable(w) {
		return
	}
	var input review.SettingsInput
	if err := decodeLimitedJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	result, err := s.reviewer.TestSettings(r.Context(), input)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleReviewKeys(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.reviewAvailable(w) {
		return
	}
	items, err := s.store.TokenOptions(r.Context(), clampInt(queryInt(r, "limit", 500), 1, 2000))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleReviewModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.reviewAvailable(w) {
		return
	}
	tokenIDs := parseInt64List(r.URL.Query().Get("token_ids"), 500)
	items, err := s.reviewer.ModelOptions(
		r.Context(),
		tokenIDs,
		int64(queryInt(r, "start", 0)),
		int64(queryInt(r, "end", 0)),
	)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) handleReviewJobs(w http.ResponseWriter, r *http.Request) {
	if !s.reviewAvailable(w) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		jobs, err := s.reviewer.ListJobs(r.Context(), clampInt(queryInt(r, "limit", 30), 1, 100))
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, jobs)
	case http.MethodPost:
		var input review.JobInput
		if err := decodeLimitedJSON(w, r, &input); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		job, err := s.reviewer.CreateJob(r.Context(), input)
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, review.ErrNotConfigured) {
				status = http.StatusConflict
			}
			writeError(w, status, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, job)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleReviewJob(w http.ResponseWriter, r *http.Request) {
	if !s.reviewAvailable(w) {
		return
	}
	trimmed := strings.TrimPrefix(r.URL.Path, "/api/review/jobs/")
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	if len(parts) == 0 || len(parts) > 2 {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	jobID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || jobID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid job id")
		return
	}
	if len(parts) == 1 && r.Method == http.MethodGet {
		job, err := s.reviewer.Job(r.Context(), jobID)
		s.writeReviewJob(w, job, err)
		return
	}
	if len(parts) == 1 && r.Method == http.MethodDelete {
		if err := s.reviewer.DeleteJob(r.Context(), jobID); err != nil {
			if errors.Is(err, review.ErrNotFound) {
				writeError(w, http.StatusNotFound, err.Error())
				return
			}
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	if len(parts) != 2 {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if parts[1] == "results" && r.Method == http.MethodGet {
		results, err := s.reviewer.Results(r.Context(), jobID, review.ResultFilter{
			Decision: r.URL.Query().Get("decision"),
			TokenID:  int64(queryInt(r, "token_id", 0)),
			Page:     clampInt(queryInt(r, "page", 1), 1, 1000000),
			PageSize: clampInt(queryInt(r, "page_size", 50), 1, 200),
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, results)
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var job review.Job
	switch parts[1] {
	case "pause":
		job, err = s.reviewer.PauseJob(r.Context(), jobID)
	case "resume":
		job, err = s.reviewer.ResumeJob(r.Context(), jobID)
	case "cancel":
		job, err = s.reviewer.CancelJob(r.Context(), jobID)
	default:
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	s.writeReviewJob(w, job, err)
}

func (s *Server) writeReviewJob(w http.ResponseWriter, job review.Job, err error) {
	if errors.Is(err, review.ErrNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) reviewAvailable(w http.ResponseWriter) bool {
	if s.reviewer == nil {
		writeError(w, http.StatusServiceUnavailable, "audit review is unavailable")
		return false
	}
	return true
}

func decodeLimitedJSON(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	return json.NewDecoder(r.Body).Decode(target)
}

func parseInt64List(value string, limit int) []int64 {
	seen := make(map[int64]bool)
	items := make([]int64, 0)
	for _, part := range strings.Split(value, ",") {
		parsed, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil || parsed <= 0 || seen[parsed] {
			continue
		}
		seen[parsed] = true
		items = append(items, parsed)
		if len(items) >= limit {
			break
		}
	}
	return items
}
