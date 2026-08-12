package channels

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOpenCodeBalanceAndLiveRefresh(t *testing.T) {
	const accountID = "3e0a71f2-7315-4b1f-876f-4c8ca31d0643"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/dashboard/api/auth/login" {
			if r.Method != http.MethodPost {
				http.Error(w, "method", http.StatusMethodNotAllowed)
				return
			}
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"username":"admin"`) || !strings.Contains(string(body), `"password":"secret"`) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "ocg_dashboard_session", Value: "session", Path: "/"})
			_, _ = io.WriteString(w, `{"ok":true}`)
			return
		}
		if cookie, err := r.Cookie("ocg_dashboard_session"); err != nil || cookie.Value != "session" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/dashboard/api/accounts":
			_, _ = io.WriteString(w, fmt.Sprintf(`[{"id":%q,"name":"managed","username":"user","enabled":true,"account_type":"managed","setup_step":"ready","expires_on":"2026-09-10"}]`, accountID))
		case r.Method == http.MethodGet && r.URL.Path == "/dashboard/api/pricing":
			_, _ = io.WriteString(w, `{"limits":{"window_5h":12,"window_week":30,"window_month":60}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/dashboard/api/accounts/"+accountID+"/usage":
			_, _ = io.WriteString(w, fmt.Sprintf(`{"account_id":%q,"window_5h":3,"window_week":12,"window_month":15,"resets_in_5h":"2026-08-12T17:00:00Z","resets_in_week":"2026-08-16T23:00:00Z","resets_in_month":"2026-09-10T00:00:00Z"}`, accountID))
		case r.Method == http.MethodPost && r.URL.Path == "/dashboard/api/accounts/"+accountID+"/usage/refresh":
			_, _ = io.WriteString(w, fmt.Sprintf(`{"usage":{"account_id":%q,"window_5h":4,"window_week":13,"window_month":16,"resets_in_5h":"2026-08-12T18:00:00Z","resets_in_week":"2026-08-16T23:00:00Z","resets_in_month":"2026-09-10T00:00:00Z"},"source":"browser_profile_console"}`, accountID))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	provider := newOpenCode(openCodeConfig{
		Label:       "OpenCode",
		BaseURL:     server.URL,
		Username:    "admin",
		Password:    "secret",
		Timeout:     5 * time.Second,
		Concurrency: 2,
	})

	balance := provider.Balance(context.Background())
	if !balance.OK || balance.OpenCode == nil {
		t.Fatalf("balance = %+v", balance)
	}
	if len(balance.OpenCode.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(balance.OpenCode.Accounts))
	}
	account := balance.OpenCode.Accounts[0]
	if !account.CanRefreshUsage {
		t.Fatalf("managed account should support live refresh")
	}
	if len(account.UsageWindows) != 3 {
		t.Fatalf("windows = %d, want 3", len(account.UsageWindows))
	}
	if got := account.UsageWindows[0].RemainingUSD; got != 9 {
		t.Fatalf("5h remaining = %v, want 9", got)
	}
	if got := account.UsageWindows[1].RemainingPercent; got != 60 {
		t.Fatalf("weekly remaining percent = %v, want 60", got)
	}

	live, err := provider.RefreshUsage(context.Background(), accountID)
	if err != nil {
		t.Fatalf("refresh usage: %v", err)
	}
	if live.Source != "browser_profile_console" {
		t.Fatalf("source = %q", live.Source)
	}
	if len(live.Windows) != 3 || live.Windows[0].Source != "live" || live.Windows[0].RemainingUSD != 8 {
		t.Fatalf("live windows = %+v", live.Windows)
	}
}

func TestOpenCodeUsageWindowCapsAtLimit(t *testing.T) {
	window := openCodeUsageWindow("周", "estimated", 35, 30, nil)
	if window.RemainingUSD != 0 || window.UsedPercent != 100 || window.RemainingPercent != 0 {
		t.Fatalf("window = %+v", window)
	}
}
