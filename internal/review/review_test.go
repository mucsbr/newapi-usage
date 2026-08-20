package review

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mucsbr/newapi-usage/internal/audit"
)

func TestReviewJobUsesMessageDeltasAndInheritsRisk(t *testing.T) {
	dir := t.TempDir()
	indexPath := filepath.Join(dir, "audit.db")
	logPath := filepath.Join(dir, "request.jsonl")
	lines := []string{
		`{"time":1000,"path":"/v1/chat/completions","headers":{"authorization":"Bearer sk-test"},"body":{"model":"gpt-test","messages":[{"role":"user","content":"普通问题"}]}}`,
		`{"time":1001,"path":"/v1/chat/completions","headers":{"authorization":"Bearer sk-test"},"body":{"model":"gpt-test","messages":[{"role":"user","content":"普通问题"},{"role":"assistant","content":"普通回答"},{"role":"user","content":"危险请求"}]}}`,
		`{"time":1002,"path":"/v1/chat/completions","headers":{"authorization":"Bearer sk-test"},"body":{"model":"gpt-test","messages":[{"role":"user","content":"普通问题"},{"role":"assistant","content":"普通回答"},{"role":"user","content":"危险请求"},{"role":"assistant","content":"拒绝"},{"role":"user","content":"继续"}]}}`,
		`{"time":1003,"path":"/v1/chat/completions","headers":{"authorization":"Bearer sk-test"},"body":{"model":"gpt-test","messages":[{"role":"user","content":"普通问题"},{"role":"assistant","content":"普通回答"},{"role":"user","content":"危险请求"},{"role":"assistant","content":"拒绝"},{"role":"user","content":"继续"}]}}`,
	}
	if err := os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
		t.Fatalf("write audit log: %v", err)
	}
	idx, err := audit.Open(audit.Config{LogGlob: logPath, IndexDSN: indexPath, TimeZone: "UTC"}, func(key string) (audit.ResolvedToken, error) {
		return audit.ResolvedToken{TokenID: 7, Name: "测试用户", KeyTail: "sk-test"}, nil
	})
	if err != nil {
		t.Fatalf("open audit: %v", err)
	}
	if err := idx.ScanOnce(context.Background()); err != nil {
		t.Fatalf("scan audit: %v", err)
	}
	if err := idx.Close(); err != nil {
		t.Fatalf("close audit: %v", err)
	}

	var mu sync.Mutex
	contents := make([]string, 0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer secret-key" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var request struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &request)
		content := request.Messages[len(request.Messages)-1].Content
		mu.Lock()
		contents = append(contents, content)
		mu.Unlock()
		decision := Decision{Decision: DecisionPass, RiskScore: 3, Categories: []string{}, Reason: "正常", Confidence: 0.98}
		if strings.Contains(content, "危险请求") {
			decision = Decision{Decision: DecisionBlock, RiskScore: 95, Categories: []string{"网络攻击"}, Reason: "存在明确攻击意图", Confidence: 0.97}
		}
		decisionJSON, _ := json.Marshal(decision)
		_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`, string(decisionJSON))
	}))
	defer server.Close()

	manager, err := Open(Config{IndexDSN: indexPath, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("open review: %v", err)
	}
	defer manager.Close()
	settings, err := manager.SaveSettings(context.Background(), SettingsInput{
		BaseURL: server.URL + "/v1",
		APIKey:  "secret-key",
		Model:   "review-model",
		Policy:  DefaultPolicy,
	})
	if err != nil || !settings.KeyConfigured {
		t.Fatalf("save settings: settings=%+v err=%v", settings, err)
	}
	job, err := manager.CreateJob(context.Background(), JobInput{TokenIDs: []int64{7}, Start: 1000, End: 1003, RoleMode: RoleUser})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := manager.runNextJob(context.Background()); err != nil {
		t.Fatalf("run job: %v", err)
	}
	job, err = manager.Job(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("load job: %v", err)
	}
	if job.Status != StatusCompleted || job.TotalEntries != 4 || job.ReviewUnits != 3 || job.ReviewedUnits != 3 {
		t.Fatalf("unexpected job: %+v", job)
	}
	mu.Lock()
	gotContents := append([]string{}, contents...)
	mu.Unlock()
	if len(gotContents) != 3 {
		t.Fatalf("model calls = %d, want 3: %v", len(gotContents), gotContents)
	}
	for _, expected := range []string{"普通问题", "危险请求", "继续"} {
		matched := false
		for _, content := range gotContents {
			if strings.Contains(content, expected) {
				matched = true
			}
		}
		if !matched {
			t.Fatalf("missing delta %q in %v", expected, gotContents)
		}
	}
	results, err := manager.Results(context.Background(), job.ID, ResultFilter{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("results: %v", err)
	}
	if len(results.Items) != 4 {
		t.Fatalf("results = %d, want 4", len(results.Items))
	}
	byEntry := make(map[int64]ResultItem)
	for _, item := range results.Items {
		byEntry[item.AuditEntryID] = item
	}
	if byEntry[2].EffectiveDecision != DecisionBlock || byEntry[2].Inherited {
		t.Fatalf("entry 2 result = %+v", byEntry[2])
	}
	if byEntry[3].EffectiveDecision != DecisionBlock || !byEntry[3].Inherited {
		t.Fatalf("entry 3 should inherit block: %+v", byEntry[3])
	}
	if len(byEntry[3].EffectiveResult.Categories) != 1 || byEntry[3].EffectiveResult.Categories[0] != "网络攻击" {
		t.Fatalf("entry 3 should expose inherited categories: %+v", byEntry[3])
	}
	if byEntry[4].EffectiveDecision != DecisionBlock || !byEntry[4].Inherited {
		t.Fatalf("entry 4 should inherit block without a model call: %+v", byEntry[4])
	}

	db, err := sql.Open("sqlite", indexPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	var cipherText []byte
	if err := db.QueryRow(`SELECT api_key_cipher FROM review_settings WHERE id = 1`).Scan(&cipherText); err != nil {
		t.Fatalf("read encrypted key: %v", err)
	}
	if strings.Contains(string(cipherText), "secret-key") {
		t.Fatalf("api key was stored in plaintext")
	}
}

func TestParseDecisionNormalizesChineseCategories(t *testing.T) {
	decision, err := parseDecision("```json\n{\"decision\":\"复核\",\"risk_score\":70,\"categories\":[\"提示词注入\",\"unknown\"],\"reason\":\"可疑\",\"confidence\":0.8}\n```")
	if err != nil {
		t.Fatalf("parse decision: %v", err)
	}
	if len(decision.Categories) != 2 || decision.Categories[0] != "提示词注入" || decision.Categories[1] != "其他风险" {
		t.Fatalf("categories = %v", decision.Categories)
	}
}

func TestReviewClientFallsBackToPlainJSON(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		if payload["response_format"] != nil {
			http.Error(w, "response_format unsupported", http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"decision\":\"通过\",\"risk_score\":1,\"categories\":[],\"reason\":\"正常\",\"confidence\":0.99}"}}],"usage":{}}`)
	}))
	defer server.Close()
	manager := &Manager{client: server.Client()}
	decision, _, mode, effort, err := manager.callReview(context.Background(), storedSettings{
		Settings: Settings{BaseURL: server.URL + "/v1", Model: "test", Policy: DefaultPolicy},
		APIKey:   "key",
	}, "普通内容", "auto")
	if err != nil {
		t.Fatalf("call review: %v", err)
	}
	if mode != "plain" || effort != ReasoningOmit || decision.Decision != DecisionPass || calls != 3 {
		t.Fatalf("mode=%q effort=%q decision=%+v calls=%d", mode, effort, decision, calls)
	}
}

func TestReviewClientAutoDetectsRequiredReasoningEffort(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		if payload["reasoning_effort"] != ReasoningNoThink {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"reasoning_effort must be one of: no_think, low, high"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"decision\":\"通过\",\"risk_score\":1,\"categories\":[],\"reason\":\"正常\",\"confidence\":0.99}"}}],"usage":{}}`)
	}))
	defer server.Close()
	manager := &Manager{client: server.Client()}
	decision, _, mode, effort, err := manager.callReview(context.Background(), storedSettings{
		Settings: Settings{BaseURL: server.URL + "/v1", Model: "test", Policy: DefaultPolicy, ReasoningEffort: ReasoningAuto},
		APIKey:   "key",
	}, "普通内容", "auto")
	if err != nil {
		t.Fatalf("call review: %v", err)
	}
	if mode != "json_schema" || effort != ReasoningNoThink || decision.Decision != DecisionPass || calls != 2 {
		t.Fatalf("mode=%q effort=%q decision=%+v calls=%d", mode, effort, decision, calls)
	}
}
