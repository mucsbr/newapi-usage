package channels

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestZhipuQuotaParsing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer zhipu-test" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/monitor/usage/quota/limit":
			_, _ = io.WriteString(w, `{"code":200,"msg":"操作成功","success":true,"data":{"level":"max","limits":[{"type":"TIME_LIMIT","unit":5,"number":1,"usage":4000,"currentValue":1,"remaining":3999,"percentage":1,"nextResetTime":1789611664999,"usageDetails":[{"modelCode":"search-prime","usage":1}]},{"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":1,"nextResetTime":1787771961795},{"type":"TOKENS_LIMIT","unit":6,"number":1,"percentage":23,"nextResetTime":1788142864978}]}}`)
		case "/api/biz/customer-package-reset/list":
			_, _ = io.WriteString(w, `{"code":200,"msg":"操作成功","success":true,"data":{"fiveHourResets":[],"weekResets":[{"recordId":99,"expireTime":"2026-10-01 23:59:59","available":true}],"lastFiveHourResetTime":null,"lastWeekResetTime":null}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	accountsPath := filepath.Join(t.TempDir(), "zhipu-accounts.json")
	provider := newZhipu(zhipuConfig{Label: "智谱 GLM", BaseURL: server.URL, LegacyAPIKey: "zhipu-test", AccountsPath: accountsPath, Timeout: 5 * time.Second})
	balance := provider.Balance(context.Background())
	if !balance.OK || balance.Kind != KindZhipu || balance.Zhipu == nil {
		t.Fatalf("unexpected balance: %+v", balance)
	}
	if balance.Zhipu.Total != 1 || len(balance.Zhipu.Accounts) != 1 {
		t.Fatalf("unexpected summary: %+v", balance.Zhipu)
	}
	account := balance.Zhipu.Accounts[0]
	if account.Name != "默认账号" || account.KeyTail != "ipu-test" || account.Level != "max" || len(account.Limits) != 3 {
		t.Fatalf("unexpected account: %+v", account)
	}
	if account.Resets == nil || account.Resets.FiveHour.Available != 0 || account.Resets.Week.Available != 1 || account.Resets.Week.NextExpiresAt != "2026-10-01 23:59:59" {
		t.Fatalf("unexpected reset cards: %+v", account.Resets)
	}
	fiveHour := account.Limits[0]
	if fiveHour.Name != "5小时" || fiveHour.RemainingPercent != 99 || fiveHour.NextResetAt != 1787771961 {
		t.Fatalf("unexpected 5-hour limit: %+v", fiveHour)
	}
	weekly := account.Limits[1]
	if weekly.Name != "周" || weekly.RemainingPercent != 77 {
		t.Fatalf("unexpected weekly limit: %+v", weekly)
	}
	tools := account.Limits[2]
	if tools.Name != "工具调用（月）" || tools.Total != 4000 || tools.Used != 1 || tools.Remaining != 3999 || len(tools.Details) != 1 {
		t.Fatalf("unexpected tool limit: %+v", tools)
	}
	stored, err := os.ReadFile(accountsPath)
	if err != nil || !strings.Contains(string(stored), "zhipu-test") {
		t.Fatalf("legacy key was not persisted: body=%s err=%v", stored, err)
	}
	info, err := os.Stat(accountsPath)
	if err != nil {
		t.Fatalf("stat accounts file: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("accounts file mode = %v, want 0600", info.Mode().Perm())
	}
	encoded, err := json.Marshal(balance)
	if err != nil {
		t.Fatalf("marshal balance: %v", err)
	}
	if strings.Contains(string(encoded), "zhipu-test") {
		t.Fatalf("balance leaked full api key: %s", encoded)
	}
}

func TestZhipuAccountManagement(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/monitor/usage/quota/limit" {
			http.NotFound(w, r)
			return
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer key-") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"code":200,"msg":"ok","success":true,"data":{"level":"pro","limits":[{"type":"TOKENS_LIMIT","unit":6,"number":1,"percentage":25}]}}`)
	}))
	defer server.Close()

	accountsPath := filepath.Join(t.TempDir(), "zhipu-accounts.json")
	provider := newZhipu(zhipuConfig{Label: "智谱 GLM", BaseURL: server.URL, AccountsPath: accountsPath, Timeout: 5 * time.Second})
	first, err := provider.AddAccount(context.Background(), "主账号", "Bearer key-first")
	if err != nil || first.Status != "ok" || first.KeyTail != "ey-first" {
		t.Fatalf("add first account: account=%+v err=%v", first, err)
	}
	second, err := provider.AddAccount(context.Background(), "备用", "key-second")
	if err != nil || second.ID == first.ID {
		t.Fatalf("add second account: account=%+v err=%v", second, err)
	}
	updatedKey := "key-updated"
	updated, err := provider.UpdateAccount(context.Background(), first.ID, nil, &updatedKey)
	if err != nil || updated.KeyTail != "-updated" || updated.Status != "ok" {
		t.Fatalf("update account: account=%+v err=%v", updated, err)
	}
	if err := provider.DeleteAccount(second.ID); err != nil {
		t.Fatalf("delete account: %v", err)
	}
	balance := provider.Refresh(context.Background())
	if balance.Zhipu == nil || balance.Zhipu.Total != 1 || len(balance.Zhipu.Accounts) != 1 || balance.Zhipu.Accounts[0].ID != first.ID {
		t.Fatalf("unexpected managed balance: %+v", balance)
	}
	stored, err := os.ReadFile(accountsPath)
	if err != nil {
		t.Fatalf("read accounts file: %v", err)
	}
	if !strings.Contains(string(stored), "key-updated") || strings.Contains(string(stored), "key-second") {
		t.Fatalf("unexpected stored accounts: %s", stored)
	}
}

func TestZhipuResetAccount(t *testing.T) {
	var used atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer key-reset" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/monitor/usage/quota/limit":
			percentage := 80
			if used.Load() {
				percentage = 0
			}
			_, _ = fmt.Fprintf(w, `{"code":200,"msg":"ok","success":true,"data":{"level":"max","limits":[{"type":"TOKENS_LIMIT","unit":6,"number":1,"percentage":%d}]}}`, percentage)
		case "/api/biz/customer-package-reset/list":
			available := !used.Load()
			lastReset := "null"
			if used.Load() {
				lastReset = `"2026-09-18 12:00:00"`
			}
			_, _ = fmt.Fprintf(w, `{"code":200,"msg":"ok","success":true,"data":{"fiveHourResets":[],"weekResets":[{"recordId":99,"expireTime":"2026-10-01 23:59:59","available":%t}],"lastFiveHourResetTime":null,"lastWeekResetTime":%s}}`, available, lastReset)
		case "/api/biz/customer-package-reset/use":
			if r.Method != http.MethodPost {
				http.Error(w, "method", http.StatusMethodNotAllowed)
				return
			}
			var body struct {
				TargetType string `json:"targetType"`
				ResetType  string `json:"resetType"`
				RecordID   int64  `json:"recordId"`
				RequestID  string `json:"requestId"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if body.TargetType != "PERSONAL" || body.ResetType != "WEEK" || body.RecordID != 99 || body.RequestID == "" {
				http.Error(w, "invalid reset body", http.StatusBadRequest)
				return
			}
			used.Store(true)
			_, _ = io.WriteString(w, `{"code":200,"msg":"ok","success":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	provider := newZhipu(zhipuConfig{Label: "智谱 GLM", BaseURL: server.URL, AccountsPath: filepath.Join(t.TempDir(), "zhipu.json"), Timeout: 5 * time.Second})
	account, err := provider.AddAccount(context.Background(), "可重置", "key-reset")
	if err != nil || account.Resets == nil || account.Resets.Week.Available != 1 {
		t.Fatalf("add resettable account: account=%+v err=%v", account, err)
	}
	account, err = provider.ResetAccount(context.Background(), account.ID, "WEEK")
	if err != nil {
		t.Fatalf("reset account: %v", err)
	}
	if !used.Load() || account.Resets == nil || account.Resets.Week.Available != 0 || account.Resets.LastWeekResetAt != "2026-09-18 12:00:00" {
		t.Fatalf("unexpected account after reset: %+v", account)
	}
	if len(account.Limits) != 1 || account.Limits[0].UsedPercent != 0 {
		t.Fatalf("quota was not refreshed after reset: %+v", account.Limits)
	}
}
