package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAuthMiddlewareLoginAndLogout(t *testing.T) {
	s := &Server{adminPassword: "secret", mux: http.NewServeMux()}
	s.mux.HandleFunc("/api/summary", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	s.mux.HandleFunc("/api/auth/status", s.handleAuthStatus)
	s.mux.HandleFunc("/api/auth/login", s.handleAuthLogin)
	s.mux.HandleFunc("/api/auth/logout", s.handleAuthLogout)
	handler := s.Handler()

	status := request(t, handler, http.MethodGet, "/api/auth/status", "")
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"authenticated":false`) {
		t.Fatalf("unexpected auth status: code=%d body=%s", status.Code, status.Body.String())
	}

	protected := request(t, handler, http.MethodGet, "/api/summary", "")
	if protected.Code != http.StatusUnauthorized {
		t.Fatalf("protected code = %d, want 401", protected.Code)
	}

	login := request(t, handler, http.MethodPost, "/api/auth/login", `{"password":"secret"}`)
	if login.Code != http.StatusOK {
		t.Fatalf("login code = %d body=%s", login.Code, login.Body.String())
	}
	cookies := login.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatalf("login did not set cookie")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/summary", nil)
	req.AddCookie(cookies[0])
	authed := httptest.NewRecorder()
	handler.ServeHTTP(authed, req)
	if authed.Code != http.StatusOK {
		t.Fatalf("authed code = %d body=%s", authed.Code, authed.Body.String())
	}

	logoutReq := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	logoutReq.AddCookie(cookies[0])
	logout := httptest.NewRecorder()
	handler.ServeHTTP(logout, logoutReq)
	if logout.Code != http.StatusOK {
		t.Fatalf("logout code = %d body=%s", logout.Code, logout.Body.String())
	}
}

func request(t *testing.T, handler http.Handler, method string, path string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}
