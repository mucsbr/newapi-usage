package channels

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDeepSeekBalanceParsing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/balance" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("authorization = %q, want Bearer sk-test", got)
		}
		_, _ = io.WriteString(w, `{
			"is_available": true,
			"balance_infos": [
				{"currency":"CNY","total_balance":"110.00","granted_balance":"10.00","topped_up_balance":"100.00"}
			]
		}`)
	}))
	defer server.Close()

	provider := newDeepSeek("DeepSeek", "sk-test", server.URL, 5*time.Second)
	balance := provider.Balance(context.Background())

	if !balance.OK {
		t.Fatalf("expected ok, got error %q", balance.Error)
	}
	if balance.Kind != KindCurrency {
		t.Fatalf("kind = %q, want %q", balance.Kind, KindCurrency)
	}
	if balance.IsAvailable == nil || !*balance.IsAvailable {
		t.Fatalf("expected is_available true")
	}
	if len(balance.Currencies) != 1 {
		t.Fatalf("currencies = %d, want 1", len(balance.Currencies))
	}
	cur := balance.Currencies[0]
	if cur.Currency != "CNY" || cur.TotalBalance != "110.00" || cur.GrantedBalance != "10.00" || cur.ToppedUpBalance != "100.00" {
		t.Fatalf("unexpected currency balance: %+v", cur)
	}
}

func TestDeepSeekBalanceHTTPErrorFallsBack(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	provider := newDeepSeek("DeepSeek", "sk-test", server.URL, 5*time.Second)
	balance := provider.Balance(context.Background())
	if balance.OK {
		t.Fatalf("expected failure on 500")
	}
	if balance.Error == "" {
		t.Fatalf("expected an error message")
	}
}

func TestCPARefreshPerAccountWindows(t *testing.T) {
	// usage builds an api-call envelope with optional primary (5h) and
	// secondary (weekly) window used-percents.
	usage := func(primary, secondary *float64) string {
		rl := map[string]any{}
		if primary != nil {
			rl["primary_window"] = map[string]any{"used_percent": *primary}
		}
		if secondary != nil {
			rl["secondary_window"] = map[string]any{"used_percent": *secondary}
		}
		raw, _ := json.Marshal(map[string]any{"rate_limit": rl})
		envelope, _ := json.Marshal(map[string]any{"status_code": 200, "body": string(raw)})
		return string(envelope)
	}
	pct := func(v float64) *float64 { return &v }

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer cpa-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/management/auth-files":
			_, _ = io.WriteString(w, `{"files":[
				{"type":"codex","name":"a@example.com.json","auth_index":1,"email":"a@example.com"},
				{"type":"codex","name":"b","auth_index":2,"account":"b@example.com"},
				{"type":"codex","name":"broken","auth_index":4},
				{"type":"codex","name":"noidx"},
				{"type":"sub2api","name":"ignored","auth_index":9}
			]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v0/management/api-call":
			payload, _ := io.ReadAll(r.Body)
			var parsed struct {
				AuthIndex float64 `json:"authIndex"`
			}
			_ = json.Unmarshal(payload, &parsed)
			switch int(parsed.AuthIndex) {
			case 1:
				_, _ = io.WriteString(w, usage(pct(20), pct(40))) // both windows
			case 2:
				_, _ = io.WriteString(w, usage(pct(98), nil)) // 5h only
			case 4:
				envelope, _ := json.Marshal(map[string]any{"status_code": 401, "body": ""})
				_, _ = w.Write(envelope)
			default:
				t.Errorf("unexpected authIndex %v", parsed.AuthIndex)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	provider := newCPA(cpaConfig{
		Label:           "CPA",
		BaseURL:         server.URL,
		Token:           "cpa-token",
		TargetType:      "codex",
		UserAgent:       "test-ua",
		Concurrency:     4,
		ProbeTimeout:    5 * time.Second,
		RefreshInterval: time.Minute,
	})

	balance := provider.refresh(context.Background())
	if !balance.OK {
		t.Fatalf("expected ok, got %q", balance.Error)
	}
	pool := balance.Pool
	if pool == nil {
		t.Fatalf("expected pool summary")
	}
	if pool.Total != 4 {
		t.Fatalf("total = %d, want 4 (codex candidates only)", pool.Total)
	}
	if len(pool.Accounts) != 4 {
		t.Fatalf("accounts = %d, want 4", len(pool.Accounts))
	}

	byName := make(map[string]PoolAccount)
	for _, a := range pool.Accounts {
		byName[a.Name] = a
	}

	// account 1: both windows present.
	a1 := byName["a@example.com.json"]
	if a1.Primary == nil || a1.Primary.Remaining != 80 {
		t.Fatalf("a1 primary remaining = %+v, want 80", a1.Primary)
	}
	if a1.Secondary == nil || a1.Secondary.Remaining != 60 {
		t.Fatalf("a1 secondary remaining = %+v, want 60", a1.Secondary)
	}
	if a1.Email != "a@example.com" {
		t.Fatalf("a1 email = %q", a1.Email)
	}

	// account 2: 5h only, no weekly window.
	a2 := byName["b"]
	if a2.Primary == nil || a2.Primary.Remaining != 2 {
		t.Fatalf("a2 primary remaining = %+v, want 2", a2.Primary)
	}
	if a2.Secondary != nil {
		t.Fatalf("a2 secondary = %+v, want nil", a2.Secondary)
	}

	// account 4: 401 -> error, no windows.
	if got := byName["broken"]; got.Error == "" || got.Primary != nil {
		t.Fatalf("broken account = %+v, want error and no window", got)
	}

	// missing auth_index -> error.
	if got := byName["noidx"]; !strings.Contains(got.Error, "auth_index") {
		t.Fatalf("expected missing auth_index error, got %+v", got)
	}
}

func TestCPAFetchAuthFilesError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer server.Close()

	provider := newCPA(cpaConfig{
		BaseURL:    server.URL,
		Token:      "cpa-token",
		TargetType: "codex",
	})
	balance := provider.refresh(context.Background())
	if balance.OK {
		t.Fatalf("expected failure when auth-files endpoint errors")
	}
	if balance.Error == "" {
		t.Fatalf("expected error message")
	}
}
