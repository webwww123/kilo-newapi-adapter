package adapter

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	lingFlashFreeModel          = "inclusionai/ling-3.0-flash:free"
	lingMaxExplicitOutputTokens = 32768
)

type lingNormalizationStats struct {
	Changed                 bool
	ReasoningDropped        bool
	StructuredOutputDropped bool
	OversizedOutputDropped  bool
}

type lingRequestRejection struct {
	Code    string
	Message string
}

type lingUnsupportedInputKinds struct {
	Image bool
	Audio bool
	Video bool
}

func normalizeLingRequest(body []byte, upstreamPath string) ([]byte, lingNormalizationStats, *lingRequestRejection, error) {
	var stats lingNormalizationStats
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, stats, nil, fmt.Errorf("invalid JSON body: %w", err)
	}
	model, _ := payload["model"].(string)
	if strings.TrimSpace(model) != lingFlashFreeModel {
		return body, stats, nil, nil
	}

	if strings.TrimRight(strings.TrimSpace(upstreamPath), "/") == "/embeddings" {
		return body, stats, &lingRequestRejection{
			Code:    "unsupported_embeddings_model",
			Message: "当前模型不支持向量嵌入，请改用 Embeddings 模型",
		}, nil
	}

	kinds := lingUnsupportedInputKinds{}
	collectLingMessageInputKinds(payload["messages"], &kinds)
	collectLingResponseInputKinds(payload["input"], &kinds)
	switch {
	case kinds.Image:
		return body, stats, &lingRequestRejection{
			Code:    "unsupported_image_input",
			Message: "当前模型不支持图片输入，请移除图片后重试",
		}, nil
	case kinds.Audio:
		return body, stats, &lingRequestRejection{
			Code:    "unsupported_audio_input",
			Message: "当前模型不支持音频输入，请移除音频后重试",
		}, nil
	case kinds.Video:
		return body, stats, &lingRequestRejection{
			Code:    "unsupported_video_input",
			Message: "当前模型不支持视频输入，请移除视频后重试",
		}, nil
	}

	if lingRequestHasEmptyUserInput(payload, upstreamPath) {
		return body, stats, &lingRequestRejection{
			Code:    "invalid_user_message",
			Message: "用户消息不能为空，请输入内容后重试",
		}, nil
	}

	if _, exists := payload["reasoning_effort"]; exists {
		delete(payload, "reasoning_effort")
		stats.ReasoningDropped = true
	}
	if deleteNestedLingField(payload, "reasoning", "effort") {
		stats.ReasoningDropped = true
	}
	if deleteNestedLingField(payload, "output_config", "effort") {
		stats.ReasoningDropped = true
	}

	for _, key := range []string{"response_format", "output_format"} {
		if _, exists := payload[key]; exists {
			delete(payload, key)
			stats.StructuredOutputDropped = true
		}
	}
	if deleteNestedLingField(payload, "text", "format") {
		stats.StructuredOutputDropped = true
	}
	if deleteNestedLingField(payload, "output_config", "format") {
		stats.StructuredOutputDropped = true
	}
	if dropLingOversizedOutputLimits(payload) {
		stats.OversizedOutputDropped = true
	}

	stats.Changed = stats.ReasoningDropped || stats.StructuredOutputDropped || stats.OversizedOutputDropped
	if !stats.Changed {
		return body, stats, nil, nil
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return nil, stats, nil, fmt.Errorf("encode Ling request cleanup: %w", err)
	}
	return out, stats, nil, nil
}

func dropLingOversizedOutputLimits(payload map[string]any) bool {
	dropped := false
	for _, key := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		limit, ok := payload[key].(float64)
		if !ok || limit <= lingMaxExplicitOutputTokens {
			continue
		}
		delete(payload, key)
		dropped = true
	}
	return dropped
}

func deleteNestedLingField(payload map[string]any, containerKey, fieldKey string) bool {
	container, ok := payload[containerKey].(map[string]any)
	if !ok {
		return false
	}
	if _, exists := container[fieldKey]; !exists {
		return false
	}
	delete(container, fieldKey)
	if len(container) == 0 {
		delete(payload, containerKey)
	}
	return true
}

func collectLingMessageInputKinds(value any, kinds *lingUnsupportedInputKinds) {
	messages, _ := value.([]any)
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		collectLingUnsupportedInputKinds(message["content"], kinds)
	}
}

func collectLingResponseInputKinds(value any, kinds *lingUnsupportedInputKinds) {
	items, ok := value.([]any)
	if !ok {
		return
	}
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if content, exists := item["content"]; exists {
			collectLingUnsupportedInputKinds(content, kinds)
			continue
		}
		// Responses also permits media content blocks directly in input.
		typeName := strings.ToLower(strings.TrimSpace(fmt.Sprint(item["type"])))
		if typeName == "image" || typeName == "image_url" || typeName == "input_image" ||
			typeName == "audio" || typeName == "audio_url" || typeName == "input_audio" ||
			typeName == "video" || typeName == "video_url" || typeName == "input_video" {
			collectLingUnsupportedInputKinds(item, kinds)
		}
	}
}

func collectLingUnsupportedInputKinds(value any, kinds *lingUnsupportedInputKinds) {
	if kinds == nil || value == nil {
		return
	}
	switch item := value.(type) {
	case []any:
		for _, child := range item {
			collectLingUnsupportedInputKinds(child, kinds)
		}
	case map[string]any:
		typeName := strings.ToLower(strings.TrimSpace(fmt.Sprint(item["type"])))
		switch typeName {
		case "image", "image_url", "input_image":
			kinds.Image = true
		case "audio", "audio_url", "input_audio":
			kinds.Audio = true
		case "video", "video_url", "input_video":
			kinds.Video = true
		}
		for _, key := range []string{"media_type", "mime_type"} {
			mediaType := strings.ToLower(strings.TrimSpace(fmt.Sprint(item[key])))
			switch {
			case strings.HasPrefix(mediaType, "image/"):
				kinds.Image = true
			case strings.HasPrefix(mediaType, "audio/"):
				kinds.Audio = true
			case strings.HasPrefix(mediaType, "video/"):
				kinds.Video = true
			}
		}
		for _, child := range item {
			collectLingUnsupportedInputKinds(child, kinds)
		}
	}
}

func lingRequestHasEmptyUserInput(payload map[string]any, upstreamPath string) bool {
	path := strings.TrimRight(strings.TrimSpace(upstreamPath), "/")
	if path == "/responses" {
		switch input := payload["input"].(type) {
		case nil:
			return true
		case string:
			return strings.TrimSpace(input) == ""
		case []any:
			if len(input) == 0 {
				return true
			}
			return lingItemsHaveEmptyUserMessage(input)
		}
	}
	messages, _ := payload["messages"].([]any)
	return lingItemsHaveEmptyUserMessage(messages)
}

func lingItemsHaveEmptyUserMessage(items []any) bool {
	for _, raw := range items {
		message, ok := raw.(map[string]any)
		if !ok || strings.TrimSpace(fmt.Sprint(message["role"])) != "user" {
			continue
		}
		if lingUserContentIsEmpty(message["content"]) {
			return true
		}
	}
	return false
}

func lingUserContentIsEmpty(value any) bool {
	switch content := value.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(content) == ""
	case []any:
		if len(content) == 0 {
			return true
		}
		for _, raw := range content {
			switch block := raw.(type) {
			case string:
				if strings.TrimSpace(block) != "" {
					return false
				}
			case map[string]any:
				typeName := strings.TrimSpace(fmt.Sprint(block["type"]))
				if text, exists := block["text"]; exists {
					if strings.TrimSpace(fmt.Sprint(text)) != "" {
						return false
					}
					continue
				}
				if typeName != "" && typeName != "text" {
					return false
				}
			default:
				return false
			}
		}
		return true
	default:
		return false
	}
}

func lingContextOverflowRejection() lingRequestRejection {
	return lingRequestRejection{
		Code:    "context_length_exceeded",
		Message: "当前模型上下文长度已超限，且本次请求无法安全裁剪，请缩短上下文后重试",
	}
}
