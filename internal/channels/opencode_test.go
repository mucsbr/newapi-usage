package channels

import (
	"context"
	"encoding/json"
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
		case r.Method == http.MethodGet && r.URL.Path == "/dashboard/api/v3/accounts":
			_, _ = io.WriteString(w, fmt.Sprintf(`{"revision":7,"processGeneration":99,"accounts":[{"id":%q,"name":"managed","username":"user","enabled":true,"accountType":"managed","setupStep":"ready","expiresOn":"2026-09-10","providerId":"opencode","offeringId":"go"},{"id":"zen-free","name":"Zen Free","enabled":true,"accountType":"key","setupStep":"ready","expiresOn":"2026-09-10","providerId":"opencode-zen-free","offeringId":"anonymous-free"}]}`, accountID))
		case r.Method == http.MethodGet && r.URL.Path == "/dashboard/api/v3/providers/opencode/go/pricing":
			_, _ = io.WriteString(w, `{"availability":"available","providerId":"opencode","offeringId":"go","revision":7,"processGeneration":99,"pricingRevision":"pricing-1","providerPricingRevision":"pricing-1","providerSnapshot":null,"snapshot":{"limits":{"window5h":12,"windowWeek":30,"windowMonth":60}}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/dashboard/api/v3/accounts/"+accountID+"/usage":
			_, _ = io.WriteString(w, fmt.Sprintf(`{"accountId":%q,"window5h":3,"windowWeek":12,"windowMonth":15,"resetsIn5h":"2026-09-01T17:00:00Z","resetsInWeek":"2026-09-07T23:00:00Z","resetsInMonth":"2026-09-10T00:00:00Z","revision":7,"processGeneration":99,"pricingRevision":"pricing-1"}`, accountID))
		case r.Method == http.MethodPost && r.URL.Path == "/dashboard/api/v3/accounts/"+accountID+"/usage/refresh":
			var request openCodeRefreshRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				http.Error(w, "invalid json", http.StatusBadRequest)
				return
			}
			if request.ExpectedRevision != 7 || request.ProcessGeneration != 99 {
				http.Error(w, "invalid cas", http.StatusConflict)
				return
			}
			_, _ = io.WriteString(w, fmt.Sprintf(`{"usage":{"accountId":%q,"window5h":4,"windowWeek":13,"windowMonth":16,"resetsIn5h":"2026-09-01T18:00:00Z","resetsInWeek":"2026-09-07T23:00:00Z","resetsInMonth":"2026-09-10T00:00:00Z"},"source":"official_go_usage","lastSuccessAt":"2026-08-31T07:00:00Z","nextAllowedAt":"2026-08-31T07:00:15Z","revision":7,"processGeneration":99}`, accountID))
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
	if live.Source != "official_go_usage" {
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
