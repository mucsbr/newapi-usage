package channels

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestZhipuQuotaParsing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/monitor/usage/quota/limit" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer zhipu-test" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"code":200,"msg":"操作成功","success":true,"data":{"level":"max","limits":[{"type":"TIME_LIMIT","unit":5,"number":1,"usage":4000,"currentValue":1,"remaining":3999,"percentage":1,"nextResetTime":1789611664999,"usageDetails":[{"modelCode":"search-prime","usage":1}]},{"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":1,"nextResetTime":1787771961795},{"type":"TOKENS_LIMIT","unit":6,"number":1,"percentage":23,"nextResetTime":1788142864978}]}}`)
	}))
	defer server.Close()

	provider := newZhipu(zhipuConfig{Label: "智谱 GLM", BaseURL: server.URL, APIKey: "zhipu-test", Timeout: 5 * time.Second})
	balance := provider.Balance(context.Background())
	if !balance.OK || balance.Kind != KindZhipu || balance.Zhipu == nil {
		t.Fatalf("unexpected balance: %+v", balance)
	}
	if balance.Zhipu.Level != "max" || len(balance.Zhipu.Limits) != 3 {
		t.Fatalf("unexpected summary: %+v", balance.Zhipu)
	}
	fiveHour := balance.Zhipu.Limits[0]
	if fiveHour.Name != "5小时" || fiveHour.RemainingPercent != 99 || fiveHour.NextResetAt != 1787771961 {
		t.Fatalf("unexpected 5-hour limit: %+v", fiveHour)
	}
	weekly := balance.Zhipu.Limits[1]
	if weekly.Name != "周" || weekly.RemainingPercent != 77 {
		t.Fatalf("unexpected weekly limit: %+v", weekly)
	}
	tools := balance.Zhipu.Limits[2]
	if tools.Name != "工具调用（月）" || tools.Total != 4000 || tools.Used != 1 || tools.Remaining != 3999 || len(tools.Details) != 1 {
		t.Fatalf("unexpected tool limit: %+v", tools)
	}
}
