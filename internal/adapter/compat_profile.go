package adapter

import (
	"encoding/json"
	"fmt"
	"strings"
)

const lingFlashFreeModel = "inclusionai/ling-3.0-flash:free"

// dropLingResponseFormat removes response_format from Ling Chat Completions
// requests because the upstream model rejects structured-output parameters.
func dropLingResponseFormat(body []byte, upstreamPath string) ([]byte, bool, error) {
	if upstreamPath != "/chat/completions" {
		return body, false, nil
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, false, fmt.Errorf("invalid JSON body: %w", err)
	}
	model, _ := payload["model"].(string)
	if strings.TrimSpace(model) != lingFlashFreeModel {
		return body, false, nil
	}
	if _, exists := payload["response_format"]; !exists {
		return body, false, nil
	}

	delete(payload, "response_format")
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, false, fmt.Errorf("encode response_format cleanup: %w", err)
	}
	return encoded, true, nil
}
