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

func TestCPARefreshAggregatesRateLimit(t *testing.T) {
	usage := func(usedPercent float64, secondary bool) string {
		body := map[string]any{
			"rate_limit": map[string]any{
				"primary_window": map[string]any{"used_percent": usedPercent},
			},
		}
		if secondary {
			body["rate_limit"].(map[string]any)["secondary_window"] = map[string]any{"used_percent": 1}
		}
		raw, _ := json.Marshal(body)
		envelope, _ := json.Marshal(map[string]any{"status_code": 200, "body": string(raw)})
		return string(envelope)
	}

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
				{"type":"codex","name":"paid","auth_index":3},
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
				_, _ = io.WriteString(w, usage(20, false))
			case 2:
				_, _ = io.WriteString(w, usage(98, false))
			case 3:
				_, _ = io.WriteString(w, usage(5, true))
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
		Label:                "CPA",
		BaseURL:              server.URL,
		Token:                "cpa-token",
		TargetType:           "codex",
		UserAgent:            "test-ua",
		UsedPercentThreshold: 95,
		Concurrency:          4,
		ProbeTimeout:         5 * time.Second,
		RefreshInterval:      time.Minute,
	})

	balance := provider.refresh(context.Background())
	if !balance.OK {
		t.Fatalf("expected ok, got %q", balance.Error)
	}
	pool := balance.Pool
	if pool == nil {
		t.Fatalf("expected pool summary")
	}
	if pool.Total != 5 {
		t.Fatalf("total = %d, want 5 (codex candidates only)", pool.Total)
	}
	if pool.Healthy != 1 {
		t.Fatalf("healthy = %d, want 1", pool.Healthy)
	}
	if pool.Exhausted != 1 {
		t.Fatalf("exhausted = %d, want 1", pool.Exhausted)
	}
	if pool.Paid != 1 {
		t.Fatalf("paid = %d, want 1", pool.Paid)
	}
	if pool.Errors != 2 {
		t.Fatalf("errors = %d, want 2 (401 + missing auth_index)", pool.Errors)
	}
	if pool.Probed != 3 {
		t.Fatalf("probed = %d, want 3", pool.Probed)
	}
	// remaining: account1 = 80, account2 = 2 -> avg 41.0
	if pool.AvgRemaining < 40.9 || pool.AvgRemaining > 41.1 {
		t.Fatalf("avg remaining = %.2f, want ~41.0", pool.AvgRemaining)
	}
	if len(pool.Accounts) != 5 {
		t.Fatalf("accounts = %d, want 5", len(pool.Accounts))
	}
	// the missing-auth_index account should report the expected error
	var missing *PoolAccount
	for i := range pool.Accounts {
		if pool.Accounts[i].Name == "noidx" {
			missing = &pool.Accounts[i]
		}
	}
	if missing == nil || !strings.Contains(missing.Error, "auth_index") {
		t.Fatalf("expected missing auth_index error, got %+v", missing)
	}
}

func TestCPAFetchAuthFilesError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer server.Close()

	provider := newCPA(cpaConfig{
		BaseURL:              server.URL,
		Token:                "cpa-token",
		TargetType:           "codex",
		UsedPercentThreshold: 95,
	})
	balance := provider.refresh(context.Background())
	if balance.OK {
		t.Fatalf("expected failure when auth-files endpoint errors")
	}
	if balance.Error == "" {
		t.Fatalf("expected error message")
	}
}
