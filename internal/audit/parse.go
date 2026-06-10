package audit

import (
	"encoding/json"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"
)

func parseLine(line string, location *time.Location) (parsedRecord, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &obj); err != nil {
		return parsedRecord{}, err
	}

	headers := extractStringMap(obj, "headers", "request_headers", "requestHeaders")
	path := firstString(obj, "path", "uri", "request_uri", "requestUri")
	body := extractBody(obj)
	bodyObj := parseJSONObject(body)
	userAgent := firstString(obj, "user_agent", "userAgent", "ua")
	if userAgent == "" {
		userAgent = firstHeader(headers, "user-agent")
	}
	client := parseClientInfo(userAgent)

	createdAt, hasTimestamp := extractTimestamp(obj, location)
	record := parsedRecord{
		CreatedAt:     createdAt,
		HasTimestamp:  hasTimestamp,
		Method:        strings.ToUpper(firstString(obj, "method", "request_method", "requestMethod")),
		Path:          path,
		Model:         firstBodyString(bodyObj, "model"),
		RequestID:     firstString(obj, "request_id", "requestId", "trace_id", "traceId"),
		APIKey:        extractAPIKey(headers, path),
		UserAgent:     userAgent,
		ClientName:    client.Name,
		ClientVersion: client.Version,
		ClientVariant: client.Variant,
		Body:          body,
	}
	if record.RequestID == "" {
		record.RequestID = firstHeader(headers, "x-oneapi-request-id", "x-request-id", "request-id")
	}
	return record, nil
}

func extractBody(obj map[string]json.RawMessage) string {
	for _, key := range []string{"body", "request_body", "requestBody", "request", "data"} {
		raw, ok := getRaw(obj, key)
		if !ok || len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var text string
		if err := json.Unmarshal(raw, &text); err == nil {
			text = strings.TrimSpace(text)
			if text != "" {
				return compactJSON(text)
			}
			continue
		}
		return compactJSON(string(raw))
	}
	return ""
}

func extractStringMap(obj map[string]json.RawMessage, keys ...string) map[string]string {
	out := make(map[string]string)
	for _, key := range keys {
		raw, ok := getRaw(obj, key)
		if !ok || len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var values map[string]any
		if err := json.Unmarshal(raw, &values); err != nil {
			continue
		}
		for k, v := range values {
			switch value := v.(type) {
			case string:
				out[strings.ToLower(k)] = strings.TrimSpace(value)
			case []any:
				parts := make([]string, 0, len(value))
				for _, part := range value {
					if text, ok := part.(string); ok && text != "" {
						parts = append(parts, text)
					}
				}
				if len(parts) > 0 {
					out[strings.ToLower(k)] = strings.Join(parts, ",")
				}
			default:
				if value != nil {
					out[strings.ToLower(k)] = strings.TrimSpace(String(value))
				}
			}
		}
	}
	return out
}

func extractAPIKey(headers map[string]string, path string) string {
	auth := firstHeader(headers, "authorization")
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		auth = strings.TrimSpace(auth[7:])
	}
	if usableKey(auth) {
		return auth
	}
	for _, header := range []string{"x-api-key", "x-goog-api-key", "api-key"} {
		if value := firstHeader(headers, header); usableKey(value) {
			return value
		}
	}
	if path != "" {
		if parsed, err := url.Parse(path); err == nil {
			if value := parsed.Query().Get("key"); usableKey(value) {
				return value
			}
		}
	}
	return ""
}

type clientInfo struct {
	Name    string
	Version string
	Variant string
}

func parseClientInfo(userAgent string) clientInfo {
	userAgent = strings.TrimSpace(userAgent)
	lower := strings.ToLower(userAgent)
	switch {
	case strings.Contains(lower, "codex desktop/"):
		return clientInfo{Name: "codex", Version: productVersion(userAgent, "Codex Desktop/"), Variant: "desktop"}
	case strings.Contains(lower, "codex-tui/"):
		return clientInfo{Name: "codex", Version: productVersion(userAgent, "codex-tui/"), Variant: "tui"}
	case strings.Contains(lower, "claude-cli/"):
		return clientInfo{Name: "claude", Version: productVersion(userAgent, "claude-cli/"), Variant: claudeVariant(lower)}
	case strings.Contains(lower, "cherrystudio/"):
		return clientInfo{Name: "cherrystudio", Version: productVersion(userAgent, "CherryStudio/"), Variant: "desktop"}
	default:
		return clientInfo{Name: "unknown"}
	}
}

func productVersion(userAgent string, marker string) string {
	idx := strings.Index(strings.ToLower(userAgent), strings.ToLower(marker))
	if idx < 0 {
		return ""
	}
	start := idx + len(marker)
	end := start
	for end < len(userAgent) {
		ch := userAgent[end]
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '.' || ch == '-' || ch == '_' {
			end++
			continue
		}
		break
	}
	return strings.TrimSpace(userAgent[start:end])
}

func claudeVariant(lowerUserAgent string) string {
	switch {
	case strings.Contains(lowerUserAgent, "claude-vscode"):
		return "vscode"
	case strings.Contains(lowerUserAgent, "claude-desktop"):
		return "desktop"
	case strings.Contains(lowerUserAgent, "sdk-cli"):
		return "sdk-cli"
	case strings.Contains(lowerUserAgent, "sdk-ts"):
		return "sdk-ts"
	case strings.Contains(lowerUserAgent, "agent-sdk/"):
		return "cli-agent"
	case strings.Contains(lowerUserAgent, "cli"):
		return "cli"
	default:
		return ""
	}
}

func usableKey(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	lower := strings.ToLower(value)
	return lower != "[redacted]" && lower != "redacted" && lower != "***" && lower != "-"
}

func extractTimestamp(obj map[string]json.RawMessage, location *time.Location) (int64, bool) {
	for _, key := range []string{"created_at", "createdAt", "timestamp", "time", "ts", "@timestamp"} {
		raw, ok := getRaw(obj, key)
		if !ok || len(raw) == 0 || string(raw) == "null" {
			continue
		}
		if ts := parseTimestamp(raw, location); ts > 0 {
			return ts, true
		}
	}
	return 0, false
}

func parseTimestamp(raw json.RawMessage, location *time.Location) int64 {
	var number json.Number
	if err := json.Unmarshal(raw, &number); err == nil {
		if value, err := number.Int64(); err == nil {
			if value > 1_000_000_000_000 {
				return value / 1000
			}
			return value
		}
		if value, err := strconv.ParseFloat(number.String(), 64); err == nil {
			if value > 1_000_000_000_000 {
				return int64(value / 1000)
			}
			return int64(value)
		}
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return 0
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return 0
	}
	if value, err := strconv.ParseInt(text, 10, 64); err == nil {
		if value > 1_000_000_000_000 {
			return value / 1000
		}
		return value
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
	} {
		if parsed, err := time.Parse(layout, text); err == nil {
			return parsed.Unix()
		}
	}
	if location == nil {
		location = timestampLocation("")
	}
	for _, layout := range []string{
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006/01/02 15:04:05",
	} {
		if parsed, err := time.ParseInLocation(layout, text, location); err == nil {
			return parsed.Unix()
		}
	}
	return 0
}

func loadTimeLocation(tz string) (*time.Location, error) {
	tz = strings.TrimSpace(tz)
	if tz == "" {
		return timestampLocation(""), nil
	}
	location, err := time.LoadLocation(tz)
	if err != nil {
		return nil, err
	}
	return location, nil
}

func timestampLocation(tz string) *time.Location {
	tz = strings.TrimSpace(tz)
	if tz == "" {
		tz = strings.TrimSpace(os.Getenv("AUDIT_TIMEZONE"))
	}
	if tz == "" {
		tz = strings.TrimSpace(os.Getenv("TZ"))
	}
	if tz != "" {
		if location, err := time.LoadLocation(tz); err == nil {
			return location
		}
	}
	return time.Local
}

func firstString(obj map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		raw, ok := getRaw(obj, key)
		if !ok || len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var text string
		if err := json.Unmarshal(raw, &text); err == nil {
			if strings.TrimSpace(text) != "" {
				return strings.TrimSpace(text)
			}
		}
	}
	return ""
}

func firstHeader(headers map[string]string, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(headers[strings.ToLower(key)]); value != "" {
			return value
		}
	}
	return ""
}

func firstBodyString(obj map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := obj[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func getRaw(obj map[string]json.RawMessage, key string) (json.RawMessage, bool) {
	if raw, ok := obj[key]; ok {
		return raw, true
	}
	lowerKey := strings.ToLower(key)
	for k, raw := range obj {
		if strings.ToLower(k) == lowerKey {
			return raw, true
		}
	}
	return nil, false
}

func parseJSONObject(text string) map[string]any {
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return map[string]any{}
	}
	return out
}

func compactJSON(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	var value any
	if err := json.Unmarshal([]byte(text), &value); err != nil {
		return text
	}
	data, err := json.Marshal(value)
	if err != nil {
		return text
	}
	return string(data)
}

func String(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(data)
}
