package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	authCookieName = "newapi_usage_session"
	authTTL        = 7 * 24 * time.Hour
)

type loginRequest struct {
	Password string `json:"password"`
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.isPublicPath(r.URL.Path) || s.authenticated(r) {
			next.ServeHTTP(w, r)
			return
		}
		writeError(w, http.StatusUnauthorized, "authentication required")
	})
}

func (s *Server) isPublicPath(path string) bool {
	if path == "/api/health" || path == "/api/auth/status" || path == "/api/auth/login" || path == "/api/auth/logout" {
		return true
	}
	return !strings.HasPrefix(path, "/api/")
}

func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": s.authenticated(r),
	})
}

func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	provided := sha256.Sum256([]byte(req.Password))
	expected := sha256.Sum256([]byte(s.adminPassword))
	if subtle.ConstantTimeCompare(provided[:], expected[:]) != 1 {
		writeError(w, http.StatusUnauthorized, "invalid password")
		return
	}
	expiresAt := time.Now().Add(authTTL)
	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName,
		Value:    s.signSession(expiresAt.Unix()),
		Path:     "/",
		Expires:  expiresAt,
		MaxAge:   int(authTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsSecure(r),
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsSecure(r),
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) authenticated(r *http.Request) bool {
	cookie, err := r.Cookie(authCookieName)
	if err != nil {
		return false
	}
	return s.validSession(cookie.Value)
}

func (s *Server) signSession(expiresAt int64) string {
	expiresText := strconv.FormatInt(expiresAt, 10)
	mac := hmac.New(sha256.New, []byte(s.adminPassword))
	mac.Write([]byte(expiresText))
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return expiresText + "." + signature
}

func (s *Server) validSession(value string) bool {
	parts := strings.Split(value, ".")
	if len(parts) != 2 {
		return false
	}
	expiresAt, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || expiresAt <= time.Now().Unix() {
		return false
	}
	expected := s.signSession(expiresAt)
	return subtle.ConstantTimeCompare([]byte(value), []byte(expected)) == 1
}

func requestIsSecure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}
