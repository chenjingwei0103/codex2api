package proxy

import (
	"bytes"
	"encoding/json"
	"strings"
)

// appendGrokContinueForRepeatedToolCall 在 Responses 请求里，同一轮出现两次
// 完全相同且已完成的工具调用、中间又没有新的用户消息时，给上游补一条
// {"type":"message","role":"user","content":"continue"}。
// 只改发给上游的请求体。当前轮已经有 continue 时不再追加。
func appendGrokContinueForRepeatedToolCall(body []byte) ([]byte, bool) {
	var root map[string]json.RawMessage
	if len(body) == 0 || json.Unmarshal(body, &root) != nil {
		return body, false
	}
	rawInput, ok := root["input"]
	if !ok {
		return body, false
	}
	var items []json.RawMessage
	if json.Unmarshal(rawInput, &items) != nil {
		return body, false
	}
	if !grokHasRepeatedCompletedToolCall(items) {
		return body, false
	}
	if grokHasContinueUserMessage(items) {
		return body, false
	}
	items = append(items, json.RawMessage(`{"type":"message","role":"user","content":"continue"}`))
	encodedInput, err := json.Marshal(items)
	if err != nil {
		return body, false
	}
	root["input"] = encodedInput
	encoded, err := json.Marshal(root)
	if err != nil {
		return body, false
	}
	return encoded, true
}

type grokRepeatedToolCallRecord struct {
	name      string
	arguments string
}

func grokHasRepeatedCompletedToolCall(items []json.RawMessage) bool {
	pending := make(map[string]grokRepeatedToolCallRecord)
	completed := make(map[string]struct{})
	for _, raw := range items {
		item := grokDecodeRawObject(raw)
		if item == nil {
			continue
		}
		if grokIsMeaningfulUserInput(item) {
			pending = make(map[string]grokRepeatedToolCallRecord)
			completed = make(map[string]struct{})
			continue
		}
		typeName := strings.TrimSpace(grokRawString(item["type"]))
		switch typeName {
		case "function_call", "custom_tool_call":
			callID := strings.TrimSpace(grokRawString(item["call_id"]))
			name := strings.TrimSpace(grokRawString(item["name"]))
			if callID == "" || name == "" {
				continue
			}
			pending[callID] = grokRepeatedToolCallRecord{
				name:      name,
				arguments: grokCanonicalToolJSON(grokToolCallInput(item, typeName)),
			}
		case "function_call_output", "custom_tool_call_output":
			callID := strings.TrimSpace(grokRawString(item["call_id"]))
			call, ok := pending[callID]
			if !ok || call.arguments == "" {
				continue
			}
			key := call.name + "\x00" + call.arguments + "\x00" + grokCanonicalToolJSON(item["output"])
			if _, exists := completed[key]; exists {
				return true
			}
			completed[key] = struct{}{}
		}
	}
	return false
}

func grokHasContinueUserMessage(items []json.RawMessage) bool {
	markerInCurrentTurn := false
	for _, raw := range items {
		item := grokDecodeRawObject(raw)
		if item == nil || !grokIsMeaningfulUserInput(item) {
			continue
		}
		if strings.TrimSpace(grokUserInputText(item)) == "continue" {
			markerInCurrentTurn = true
			continue
		}
		markerInCurrentTurn = false
	}
	return markerInCurrentTurn
}

func grokIsMeaningfulUserInput(item map[string]json.RawMessage) bool {
	role := strings.ToLower(strings.TrimSpace(grokRawString(item["role"])))
	if role != "user" {
		return false
	}
	rawContent := bytes.TrimSpace(item["content"])
	if len(rawContent) == 0 || bytes.Equal(rawContent, []byte("null")) {
		return false
	}
	var text string
	if json.Unmarshal(rawContent, &text) == nil {
		return strings.TrimSpace(text) != ""
	}
	var parts []json.RawMessage
	if json.Unmarshal(rawContent, &parts) == nil {
		return len(parts) > 0
	}
	return true
}

func grokToolCallInput(item map[string]json.RawMessage, typeName string) json.RawMessage {
	if typeName == "custom_tool_call" {
		return item["input"]
	}
	return item["arguments"]
}

func grokUserInputText(item map[string]json.RawMessage) string {
	raw := bytes.TrimSpace(item["content"])
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var builder strings.Builder
	for _, part := range parts {
		partType := strings.TrimSpace(grokRawString(part["type"]))
		switch partType {
		case "input_text", "text", "output_text":
			builder.WriteString(grokRawString(part["text"]))
		case "refusal":
			builder.WriteString(grokRawString(part["refusal"]))
		}
	}
	return builder.String()
}

func grokDecodeRawObject(raw json.RawMessage) map[string]json.RawMessage {
	var item map[string]json.RawMessage
	if json.Unmarshal(raw, &item) != nil {
		return nil
	}
	return item
}

func grokRawString(raw json.RawMessage) string {
	var value string
	if len(bytes.TrimSpace(raw)) == 0 || json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}

func grokCanonicalToolJSON(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	var text string
	if json.Unmarshal(trimmed, &text) == nil {
		text = strings.TrimSpace(text)
		var nested any
		if json.Unmarshal([]byte(text), &nested) == nil {
			if encoded, err := json.Marshal(nested); err == nil {
				return string(encoded)
			}
		}
		return text
	}
	var value any
	if json.Unmarshal(trimmed, &value) == nil {
		encoded, err := json.Marshal(value)
		if err == nil {
			return string(encoded)
		}
	}
	return string(trimmed)
}
