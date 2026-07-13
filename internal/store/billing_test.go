package store

import "testing"

func TestCalculateLogBillingWithCacheBreakdown(t *testing.T) {
	settings := billingSettings{quotaPerUnit: 500000, usdToCNY: 7.3}
	other := `{
		"model_ratio": 2,
		"group_ratio": 1,
		"completion_ratio": 4,
		"cache_ratio": 0.1,
		"cache_creation_ratio": 1.25,
		"cache_tokens": 30,
		"cache_write_tokens": 20
	}`

	result := calculateLogBilling(476, 100, 40, other, settings)
	if result.cacheReadTokens != 30 || result.cacheWriteTokens != 20 {
		t.Fatalf("unexpected cache tokens: %+v", result)
	}
	if result.inputCostCNY <= 0 || result.outputCostCNY <= 0 || result.cacheReadCostCNY <= 0 || result.cacheWriteCostCNY <= 0 {
		t.Fatalf("expected cost components: %+v", result)
	}
	if result.otherCostCNY != 0 {
		t.Fatalf("unexpected other cost: %+v", result)
	}
}

func TestCalculateLogBillingKeepsFixedPriceAsOther(t *testing.T) {
	settings := billingSettings{quotaPerUnit: 500000, usdToCNY: 7.3}
	result := calculateLogBilling(500000, 100, 20, `{"model_price":1}`, settings)
	if result.quotaCNY != 7.3 || result.otherCostCNY != 7.3 {
		t.Fatalf("unexpected fixed-price result: %+v", result)
	}
}
