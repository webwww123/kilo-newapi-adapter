package adapter

import (
	"encoding/json"
	"fmt"
	"strings"
)

func logKiloAutoUpstream400(upstreamPath, publicModel string, body []byte) {
	if upstreamPath != lingJSONRequestPath {
		return
	}
	publicModel = strings.TrimSpace(publicModel)
	if publicModel != kiloAutoFreeModel && publicModel != kiloAutoJSONPublicModel {
		return
	}
	category, parameterClass := classifyKiloAutoUpstream400(body)
	fmt.Printf("kilo_auto_400_class path=/chat/completions category=%s parameter_class=%s response_bytes=%d\n",
		category, parameterClass, len(body))
}

func classifyKiloAutoUpstream400(body []byte) (string, string) {
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return "non_json", "none"
	}
	fields := make(map[string][]string)
	collectKiloAutoErrorFields(payload, fields)
	parameterClass := classifyKiloAutoErrorParameter(strings.Join(fields["param"], " "))
	combined := strings.ToLower(strings.Join([]string{
		strings.Join(fields["code"], " "),
		strings.Join(fields["type"], " "),
		strings.Join(fields["error_type"], " "),
		strings.Join(fields["message"], " "),
	}, " "))
	switch {
	case strings.Contains(combined, "paid_model_auth_required"):
		return "paid_model_auth_required", parameterClass
	case parameterClass == "tool_function" && strings.Contains(combined, "expected object"):
		return "missing_tool_function", parameterClass
	case parameterClass == "message_content" && strings.Contains(combined, "user message must have content"):
		return "empty_user_content", parameterClass
	case parameterClass == "message_content":
		return "invalid_message_content", parameterClass
	case parameterClass == "message_role":
		return "invalid_message_role", parameterClass
	case parameterClass == "tool_arguments":
		return "invalid_tool_arguments", parameterClass
	case parameterClass == "tool_parameters":
		return "invalid_tool_schema", parameterClass
	case parameterClass == "tool_choice":
		return "invalid_tool_choice", parameterClass
	case parameterClass == "response_format":
		return "invalid_response_format", parameterClass
	case parameterClass == "sampling_parameter":
		return "invalid_sampling_parameter", parameterClass
	case parameterClass == "output_limit":
		return "invalid_output_limit", parameterClass
	case parameterClass == "stop":
		return "invalid_stop", parameterClass
	case strings.Contains(combined, "invalid input"):
		return "invalid_input", parameterClass
	case strings.Contains(combined, "invalid_request"):
		return "invalid_request", parameterClass
	default:
		return "unknown", parameterClass
	}
}

func collectKiloAutoErrorFields(value any, fields map[string][]string) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			normalized := strings.ToLower(strings.TrimSpace(key))
			switch normalized {
			case "code", "type", "error_type", "param", "message":
				if text, ok := child.(string); ok {
					fields[normalized] = append(fields[normalized], text)
				}
			}
			collectKiloAutoErrorFields(child, fields)
		}
	case []any:
		for _, child := range typed {
			collectKiloAutoErrorFields(child, fields)
		}
	}
}

func classifyKiloAutoErrorParameter(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch {
	case strings.Contains(value, "tool_calls") && strings.Contains(value, "arguments"):
		return "tool_arguments"
	case strings.Contains(value, "tools") && strings.Contains(value, "parameters"):
		return "tool_parameters"
	case strings.Contains(value, "tools") && strings.Contains(value, "function"):
		return "tool_function"
	case strings.Contains(value, "messages") && strings.Contains(value, "role"):
		return "message_role"
	case strings.Contains(value, "messages") && strings.Contains(value, "content"):
		return "message_content"
	case strings.Contains(value, "tool_choice"):
		return "tool_choice"
	case strings.Contains(value, "response_format") || strings.Contains(value, "json_schema"):
		return "response_format"
	case strings.Contains(value, "temperature") || strings.Contains(value, "top_p"):
		return "sampling_parameter"
	case strings.Contains(value, "max_tokens") || strings.Contains(value, "max_completion_tokens") || strings.Contains(value, "max_output_tokens"):
		return "output_limit"
	case strings.Contains(value, "stop"):
		return "stop"
	case strings.Contains(value, "model"):
		return "model"
	case value == "":
		return "none"
	default:
		return "other"
	}
}
