package adapter

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	stepfunFlashFreeModel = "stepfun/step-3.7-flash:free"
	jsonObjectInstruction = "Return exactly one valid JSON object. Do not wrap it in markdown or add any text outside the JSON object."
)

// downgradeFreeModelJSONMode preserves the intent of OpenAI json_object mode
// for Kilo backends that inconsistently reject response_format. The upstream
// field is removed only for the affected profiled model and replaced with a
// system instruction that the plain chat model can follow reliably.
func downgradeFreeModelJSONMode(body []byte, upstreamPath string) ([]byte, bool, error) {
	if upstreamPath != "/chat/completions" {
		return body, false, nil
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, false, fmt.Errorf("invalid JSON body: %w", err)
	}
	model, _ := payload["model"].(string)
	if strings.TrimSpace(model) != stepfunFlashFreeModel {
		return body, false, nil
	}
	responseFormat, ok := payload["response_format"].(map[string]any)
	if !ok || strings.TrimSpace(fmt.Sprint(responseFormat["type"])) != "json_object" {
		return body, false, nil
	}

	delete(payload, "response_format")
	messages, _ := payload["messages"].([]any)
	if !appendJSONInstructionToSystemMessage(messages) {
		messages = append([]any{map[string]any{
			"role":    "system",
			"content": jsonObjectInstruction,
		}}, messages...)
	}
	payload["messages"] = messages

	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, false, fmt.Errorf("encode JSON mode downgrade: %w", err)
	}
	return encoded, true, nil
}

func appendJSONInstructionToSystemMessage(messages []any) bool {
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := message["role"].(string)
		if role != "system" && role != "developer" {
			continue
		}
		switch content := message["content"].(type) {
		case string:
			content = strings.TrimSpace(content)
			if content == "" {
				message["content"] = jsonObjectInstruction
			} else {
				message["content"] = content + "\n\n" + jsonObjectInstruction
			}
			return true
		case []any:
			message["content"] = append(content, map[string]any{"type": "text", "text": jsonObjectInstruction})
			return true
		}
	}
	return false
}
