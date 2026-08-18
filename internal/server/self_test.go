package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSelfLogDoesNotExposeRequestDetails(t *testing.T) {
	data, err := json.Marshal(selfLog{
		ID:           1,
		ModelName:    "gpt-5",
		InputTokens:  100,
		OutputTokens: 20,
	})
	if err != nil {
		t.Fatalf("marshal self log: %v", err)
	}
	text := string(data)
	for _, forbidden := range []string{"content", "other", "request_id", "ip", "user_agent", "key_name", "token_id"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("self log leaked %q: %s", forbidden, text)
		}
	}
}
