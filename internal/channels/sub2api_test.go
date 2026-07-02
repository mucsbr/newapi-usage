package channels

import "testing"

func TestSub2APIAccountUsageNormalization(t *testing.T) {
	account := accountFromSub2Raw(sub2AccountRaw{
		ID:                  776,
		Name:                "pz pro",
		Platform:            "anthropic",
		Type:                "oauth",
		Status:              "active",
		SessionWindowEnd:    "2026-07-02T18:29:59+08:00",
		SessionWindowStatus: "allowed_warning",
		Extra: map[string]any{
			"session_window_utilization":   0.03,
			"passive_usage_7d_utilization": 0,
			"passive_usage_7d_reset":       float64(1783573199),
			"email_address":                "searchurmom@gmail.com",
		},
	})

	if !account.CanRefreshUsage {
		t.Fatalf("oauth account should allow live refresh")
	}
	if account.Email != "searchurmom@gmail.com" {
		t.Fatalf("email = %q", account.Email)
	}
	if len(account.UsageWindows) != 2 {
		t.Fatalf("usage windows = %d", len(account.UsageWindows))
	}
	if account.UsageWindows[0].UsedPercent != 3 {
		t.Fatalf("session usage = %f", account.UsageWindows[0].UsedPercent)
	}
	if account.UsageWindows[0].RemainingPercent != 97 {
		t.Fatalf("session remaining = %f", account.UsageWindows[0].RemainingPercent)
	}
}

func TestSub2APILiveUsage(t *testing.T) {
	data := sub2UsageData{
		UpdatedAt: "2026-07-02T16:15:53+08:00",
		FiveHour: sub2UsageWindowRaw{
			Utilization:      float64(3),
			ResetsAt:         "2026-07-02T10:29:59Z",
			RemainingSeconds: 8046,
		},
		SevenDay: sub2UsageWindowRaw{
			Utilization:      float64(0),
			ResetsAt:         "2026-07-09T04:59:59Z",
			RemainingSeconds: 593046,
		},
	}
	data.FiveHour.WindowStats.Requests = 119
	data.FiveHour.WindowStats.Tokens = 17278452
	data.FiveHour.WindowStats.Cost = 23.1523094

	windows := usageWindowsFromLive(data)
	if len(windows) != 2 {
		t.Fatalf("windows = %d", len(windows))
	}
	if windows[0].UsedPercent != 3 || windows[0].Requests != 119 || windows[0].Tokens != 17278452 {
		t.Fatalf("unexpected five-hour usage: %+v", windows[0])
	}
	if windows[0].RemainingPercent != 97 {
		t.Fatalf("unexpected five-hour remaining: %+v", windows[0])
	}
	if windows[1].UsedPercent != 0 || windows[1].RemainingPercent != 100 || windows[1].RemainingSeconds != 593046 {
		t.Fatalf("unexpected seven-day usage: %+v", windows[1])
	}
}

func TestIkunQuotaToCNY(t *testing.T) {
	if got := quotaToCNY(245047897); got != 490.1 {
		t.Fatalf("quota cny = %v", got)
	}
	if got := quotaToCNY(1204898553); got != 2409.8 {
		t.Fatalf("used cny = %v", got)
	}
}

func TestIkunMatchesSub2Account(t *testing.T) {
	provider := newIkun(ikunConfig{
		Label:       "Ikun",
		BaseURL:     "https://api.ikuncode.cc",
		AccessToken: "token",
		UserID:      20378,
	}, 0)

	if !provider.matchesSub2Account(sub2AccountRaw{
		ID:   275,
		Name: "ikun",
	}) {
		t.Fatalf("expected name match")
	}
	if !provider.matchesSub2Account(sub2AccountRaw{
		ID:          123,
		Name:        "other",
		Credentials: map[string]any{"base_url": "https://api.ikuncode.cc/"},
	}) {
		t.Fatalf("expected base_url match")
	}
}
