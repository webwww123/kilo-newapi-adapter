package adapter

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNormalizeLingRequestDropsUnsupportedControls(t *testing.T) {
	body := []byte(`{
		"model":"inclusionai/ling-3.0-flash:free",
		"input":"give data",
		"response_format":{"type":"json_object"},
		"reasoning_effort":"high",
		"reasoning":{"effort":"medium","summary":"auto"},
		"text":{"format":{"type":"json_schema"},"verbosity":"medium"},
		"output_config":{"effort":"high","format":{"type":"json_schema"},"other":"keep"},
		"max_output_tokens":4096,
		"stream":true,
		"tools":[{"type":"function","function":{"name":"lookup"}}]
	}`)

	out, stats, rejection, err := normalizeLingRequest(body, "/responses")
	if err != nil {
		t.Fatal(err)
	}
	if rejection != nil {
		t.Fatalf("request was rejected: %#v", rejection)
	}
	if !stats.Changed || !stats.ReasoningDropped || !stats.StructuredOutputDropped {
		t.Fatal("Ling request was not cleaned")
	}

	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatal(err)
	}
	if _, exists := payload["response_format"]; exists {
		t.Fatalf("response_format still present: %s", out)
	}
	if _, exists := payload["reasoning_effort"]; exists {
		t.Fatalf("reasoning_effort still present: %s", out)
	}
	reasoning := payload["reasoning"].(map[string]any)
	if _, exists := reasoning["effort"]; exists || reasoning["summary"] != "auto" {
		t.Fatalf("reasoning cleanup changed unrelated fields: %#v", reasoning)
	}
	text := payload["text"].(map[string]any)
	if _, exists := text["format"]; exists || text["verbosity"] != "medium" {
		t.Fatalf("text cleanup changed unrelated fields: %#v", text)
	}
	outputConfig := payload["output_config"].(map[string]any)
	if _, exists := outputConfig["effort"]; exists {
		t.Fatalf("output_config effort still present: %#v", outputConfig)
	}
	if _, exists := outputConfig["format"]; exists || outputConfig["other"] != "keep" {
		t.Fatalf("output_config cleanup changed unrelated fields: %#v", outputConfig)
	}
	if payload["stream"] != true {
		t.Fatalf("stream changed: %#v", payload)
	}
	if payload["max_output_tokens"] != float64(4096) || len(payload["tools"].([]any)) != 1 {
		t.Fatalf("output budget or tools changed: %#v", payload)
	}
}

func TestNormalizeLingRequestDropsOnlyOversizedOutputLimits(t *testing.T) {
	body := []byte(`{
		"model":"inclusionai/ling-3.0-flash:free",
		"messages":[{"role":"user","content":"hello"}],
		"max_tokens":32768,
		"max_completion_tokens":32769,
		"max_output_tokens":131072,
		"temperature":0.2
	}`)

	out, stats, rejection, err := normalizeLingRequest(body, "/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	if rejection != nil {
		t.Fatalf("request was rejected: %#v", rejection)
	}
	if !stats.Changed || !stats.OversizedOutputDropped {
		t.Fatalf("oversized output limits were not reported as dropped: %#v", stats)
	}

	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["max_tokens"] != float64(32768) {
		t.Fatalf("valid max_tokens changed: %#v", payload)
	}
	for _, key := range []string{"max_completion_tokens", "max_output_tokens"} {
		if _, exists := payload[key]; exists {
			t.Fatalf("oversized %s still present: %#v", key, payload)
		}
	}
	if payload["temperature"] != float64(0.2) {
		t.Fatalf("unrelated field changed: %#v", payload)
	}
}

func TestNormalizeLingRequestRejectsUnsupportedInputsInChinese(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		body    string
		code    string
		message string
	}{
		{
			name:    "image",
			path:    "/chat/completions",
			body:    `{"model":"inclusionai/ling-3.0-flash:free","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AA"}}]}]}`,
			code:    "unsupported_image_input",
			message: "当前模型不支持图片输入，请移除图片后重试",
		},
		{
			name:    "audio",
			path:    "/responses",
			body:    `{"model":"inclusionai/ling-3.0-flash:free","input":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"AA"}}]}]}`,
			code:    "unsupported_audio_input",
			message: "当前模型不支持音频输入，请移除音频后重试",
		},
		{
			name:    "video",
			path:    "/messages",
			body:    `{"model":"inclusionai/ling-3.0-flash:free","messages":[{"role":"user","content":[{"type":"video","source":{"data":"AA"}}]}]}`,
			code:    "unsupported_video_input",
			message: "当前模型不支持视频输入，请移除视频后重试",
		},
		{
			name:    "embeddings",
			path:    "/embeddings",
			body:    `{"model":"inclusionai/ling-3.0-flash:free","input":"hello"}`,
			code:    "unsupported_embeddings_model",
			message: "当前模型不支持向量嵌入，请改用 Embeddings 模型",
		},
		{
			name:    "empty user",
			path:    "/chat/completions",
			body:    `{"model":"inclusionai/ling-3.0-flash:free","messages":[{"role":"user","content":""}]}`,
			code:    "invalid_user_message",
			message: "用户消息不能为空，请输入内容后重试",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, rejection, err := normalizeLingRequest([]byte(tc.body), tc.path)
			if err != nil {
				t.Fatal(err)
			}
			if rejection == nil || rejection.Code != tc.code || rejection.Message != tc.message {
				t.Fatalf("rejection=%#v, want code=%q message=%q", rejection, tc.code, tc.message)
			}
		})
	}
}

func TestNormalizeLingRequestLeavesOtherModelsUnchanged(t *testing.T) {
	body := []byte(`{"model":"nvidia/other:free","messages":[],"reasoning_effort":"high","response_format":{"type":"json_object"},"max_tokens":64000}`)
	out, stats, rejection, err := normalizeLingRequest(body, "/chat/completions")
	if err != nil || rejection != nil || stats.Changed || string(out) != string(body) {
		t.Fatalf("stats=%#v rejection=%#v err=%v out=%s", stats, rejection, err, out)
	}
}

func TestNormalizeLingRequestDoesNotTreatToolTextAsMedia(t *testing.T) {
	body := []byte(`{
		"model":"inclusionai/ling-3.0-flash:free",
		"messages":[
			{"role":"user","content":"summarize the tool result"},
			{"role":"tool","tool_call_id":"call_1","content":"{\"image_url\":\"metadata only\",\"type\":\"record\"}"}
		]
	}`)
	out, stats, rejection, err := normalizeLingRequest(body, "/chat/completions")
	if err != nil || rejection != nil || stats.Changed || string(out) != string(body) {
		t.Fatalf("stats=%#v rejection=%#v err=%v out=%s", stats, rejection, err, out)
	}
}

func TestLingContextOverflowMessageIsChinese(t *testing.T) {
	rejection := lingContextOverflowRejection()
	if rejection.Code != "context_length_exceeded" || !strings.Contains(rejection.Message, "上下文长度已超限") {
		t.Fatalf("unexpected rejection: %#v", rejection)
	}
}
