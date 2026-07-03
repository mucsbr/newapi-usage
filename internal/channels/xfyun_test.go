package channels

import "testing"

func TestXFYunUsageFromLimit(t *testing.T) {
	limit := float64(90000)
	usage := float64(4258)
	left := float64(85742)
	win := xfyunUsage(&limit, &usage, &left)
	if win == nil {
		t.Fatalf("expected usage window")
	}
	if win.Left != 85742 || win.Usage != 4258 || win.Limit != 90000 {
		t.Fatalf("unexpected usage: %+v", win)
	}
	if win.RemainingPercent < 95.2 || win.RemainingPercent > 95.3 {
		t.Fatalf("remaining percent = %v", win.RemainingPercent)
	}
}

func TestXFYunSessionExpiry(t *testing.T) {
	if got := xfyunSessionExpiry(nil); got != "" {
		t.Fatalf("empty expiry = %q", got)
	}
}

func TestNormalizeXFYunSSO(t *testing.T) {
	got := normalizeXFYunSSO("foo=bar; ssoSessionId=abc-123; tenantToken=ignored")
	if got != "abc-123" {
		t.Fatalf("sso = %q", got)
	}
}
