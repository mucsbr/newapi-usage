package audit

import (
	"encoding/json"
	"strings"
)

func NormalizeMessages(body string) []Message {
	var obj map[string]any
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		return []Message{}
	}
	if messages := normalizeOpenAIMessages(obj); len(messages) > 0 {
		return messages
	}
	if messages := normalizeResponsesInput(obj); len(messages) > 0 {
		return messages
	}
	if messages := normalizeGeminiContents(obj); len(messages) > 0 {
		return messages
	}
	return []Message{}
}

func normalizeOpenAIMessages(obj map[string]any) []Message {
	out := make([]Message, 0)
	if system := contentToText(obj["system"]); system != "" {
		out = append(out, Message{Role: "system", Content: system})
	}
	values, ok := obj["messages"].([]any)
	if !ok {
		return out
	}
	for _, value := range values {
		message, ok := value.(map[string]any)
		if !ok {
			continue
		}
		role := stringValue(message["role"])
		if role == "" {
			role = "message"
		}
		content := contentToText(message["content"])
		if content == "" {
			content = contentToText(message["tool_calls"])
		}
		if content != "" {
			out = append(out, Message{Role: role, Content: content})
		}
	}
	return out
}

func normalizeResponsesInput(obj map[string]any) []Message {
	input := obj["input"]
	switch value := input.(type) {
	case string:
		if strings.TrimSpace(value) == "" {
			return []Message{}
		}
		return []Message{{Role: "user", Content: strings.TrimSpace(value)}}
	case []any:
		out := make([]Message, 0)
		for _, item := range value {
			message, ok := item.(map[string]any)
			if !ok {
				continue
			}
			role := stringValue(message["role"])
			if role == "" {
				role = stringValue(message["type"])
			}
			if role == "" {
				role = "input"
			}
			content := contentToText(message["content"])
			if content != "" {
				out = append(out, Message{Role: role, Content: content})
			}
		}
		return out
	default:
		return []Message{}
	}
}

func normalizeGeminiContents(obj map[string]any) []Message {
	values, ok := obj["contents"].([]any)
	if !ok {
		return []Message{}
	}
	out := make([]Message, 0, len(values))
	for _, value := range values {
		message, ok := value.(map[string]any)
		if !ok {
			continue
		}
		role := stringValue(message["role"])
		if role == "" {
			role = "user"
		}
		content := contentToText(message["parts"])
		if content != "" {
			out = append(out, Message{Role: role, Content: content})
		}
	}
	return out
}

func contentToText(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(typed)
	case []any:
		parts := make([]string, 0, len(typed))
		for _, part := range typed {
			text := contentToText(part)
			if text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	case map[string]any:
		for _, key := range []string{"text", "input_text", "output_text"} {
			if text := stringValue(typed[key]); text != "" {
				return text
			}
		}
		if image, ok := typed["image_url"]; ok {
			if text := contentToText(image); text != "" {
				return "[image] " + text
			}
			return "[image]"
		}
		if typed["inline_data"] != nil {
			return "[inline_data]"
		}
		if typed["tool_use"] != nil || typed["tool_calls"] != nil {
			return compactAny(typed)
		}
		return compactAny(typed)
	default:
		return compactAny(typed)
	}
}

func stringValue(value any) string {
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	return ""
}

func compactAny(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(data)
}
