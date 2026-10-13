package proxy

import (
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"
)

// Anthropic 服务端工具必须转成 Codex hosted tool，而不是 function。
// 转成 function 会缺 parameters（上游 400 code 11129），且会被模型当成需客户端
// 执行的普通函数而吐出空调用（检索静默失效）。
func TestConvertAnthropicTools_ServerToolsBecomeHosted(t *testing.T) {
	cases := []struct {
		name string
		tool anthropicTool
	}{
		{"服务端类型", anthropicTool{Type: "web_search_20250305", Name: "web_search"}},
		{"服务端类型-新版", anthropicTool{Type: "web_search_20260209", Name: "web_search"}},
		{"服务端类型-响应包含控制版", anthropicTool{Type: "web_search_20260318", Name: "web_search"}},
		{"preview 变体", anthropicTool{Type: "web_search_preview"}},
		{"function 形态（无 type、无 schema）", anthropicTool{Name: "web_search"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := convertAnthropicTools([]anthropicTool{tc.tool})
			if len(got) != 1 {
				t.Fatalf("len = %d, want 1", len(got))
			}
			m, ok := got[0].(map[string]any)
			if !ok {
				t.Fatalf("tool is %T, want map", got[0])
			}
			if m["type"] != "web_search" {
				t.Fatalf("type = %v, want web_search", m["type"])
			}
			if _, has := m["name"]; has {
				t.Fatalf("hosted tool must not carry name: %v", m)
			}
		})
	}
}

func TestConvertAnthropicTools_WebSearchFunctionWithSchemaStaysFunction(t *testing.T) {
	got := convertAnthropicTools([]anthropicTool{{
		Name:        "web_search",
		Description: "custom client-side web search",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`),
	}})
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	m := got[0].(map[string]any)
	if m["type"] != "function" || m["name"] != "web_search" {
		t.Fatalf("custom same-name tool converted incorrectly: %v", m)
	}
	if _, ok := m["parameters"].(map[string]any)["properties"].(map[string]any)["query"]; !ok {
		t.Fatalf("custom schema not preserved: %v", m["parameters"])
	}
}

func TestConvertAnthropicTools_ServerToolPreservesSupportedConfig(t *testing.T) {
	got := convertAnthropicTools([]anthropicTool{{
		Type:           "web_search_20250305",
		Name:           "web_search",
		AllowedDomains: []string{"example.com"},
		BlockedDomains: []string{"spam.example"},
		UserLocation: map[string]any{
			"type":    "approximate",
			"country": "US",
		},
	}})
	m := got[0].(map[string]any)
	filters, ok := m["filters"].(map[string]any)
	if !ok {
		t.Fatalf("filters missing: %v", m)
	}
	if filters["allowed_domains"].([]string)[0] != "example.com" {
		t.Fatalf("allowed_domains not preserved: %v", filters)
	}
	if filters["blocked_domains"].([]string)[0] != "spam.example" {
		t.Fatalf("blocked_domains not preserved: %v", filters)
	}
	loc := m["user_location"].(map[string]any)
	if loc["country"] != "US" {
		t.Fatalf("user_location not preserved: %v", loc)
	}
}

// 普通 function 工具必须补上 parameters，否则上游 400 code 11129。
func TestConvertAnthropicTools_FunctionGetsParameters(t *testing.T) {
	// 不带 input_schema 的普通工具（Anthropic 客户端允许）
	got := convertAnthropicTools([]anthropicTool{{Name: "get_time", Description: "get time"}})
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	m := got[0].(map[string]any)
	if m["type"] != "function" || m["name"] != "get_time" {
		t.Fatalf("unexpected tool: %v", m)
	}
	params, ok := m["parameters"].(map[string]any)
	if !ok || params["type"] != "object" {
		t.Fatalf("parameters = %v, want object schema", m["parameters"])
	}

	// 带 input_schema 的工具保留原 schema
	got = convertAnthropicTools([]anthropicTool{{
		Name:        "lookup",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
	}})
	params = got[0].(map[string]any)["parameters"].(map[string]any)
	if _, ok := params["properties"].(map[string]any)["q"]; !ok {
		t.Fatalf("input_schema not preserved: %v", params)
	}
}

// Responses 的 function tool_choice 是扁平形态；嵌套形态会被上游以
// "Missing required parameter: 'tool_choice.name'" 拒绝。
func TestConvertAnthropicToolChoice_FlatFunctionShape(t *testing.T) {
	got := convertAnthropicToolChoice(json.RawMessage(`{"type":"tool","name":"lookup"}`))
	m, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("choice is %T, want map", got)
	}
	if m["type"] != "function" || m["name"] != "lookup" {
		t.Fatalf("choice = %v, want flat {type:function,name:lookup}", m)
	}
	if _, has := m["function"]; has {
		t.Fatalf("nested function object must not be emitted: %v", m)
	}

	// 字符串形态保持透传
	for raw, want := range map[string]string{
		`{"type":"auto"}`: "auto",
		`{"type":"any"}`:  "required",
		`{"type":"none"}`: "none",
	} {
		if got := convertAnthropicToolChoice(json.RawMessage(raw)); got != want {
			t.Fatalf("%s -> %v, want %q", raw, got, want)
		}
	}
}

func TestConvertAnthropicToolChoice_HostedWebSearchRestrictsAllowedTools(t *testing.T) {
	got := convertAnthropicToolChoiceForTools(
		json.RawMessage(`{"type":"tool","name":"web_search"}`),
		[]anthropicTool{{Type: "web_search_20250305", Name: "web_search"}},
	)
	m, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("hosted web_search tool_choice = %v, want allowed_tools object", got)
	}
	// 字符串 "required" 只保证「调用某个工具」；工具表里还有普通 function 时
	// 模型可能改选它（实测会选中别的 function），违反命名选择语义。
	if m["type"] != "allowed_tools" || m["mode"] != "required" {
		t.Fatalf("choice = %v, want allowed_tools + required", m)
	}
	tools, ok := m["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("allowed tools = %v, want exactly web_search", m["tools"])
	}
	if tool := tools[0].(map[string]any); tool["type"] != "web_search" {
		t.Fatalf("allowed tool = %v, want web_search", tool)
	}

	got = convertAnthropicToolChoiceForTools(
		json.RawMessage(`{"type":"tool","name":"lookup"}`),
		[]anthropicTool{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	)
	fm, ok := got.(map[string]any)
	if !ok || fm["type"] != "function" || fm["name"] != "lookup" {
		t.Fatalf("function tool_choice = %v, want flat function lookup", got)
	}
}

// web_search_call 必须转成 server_tool_use + web_search_tool_result 对，
// 否则检索结果会被静默丢弃。
func TestAnthropicServerToolBlocks(t *testing.T) {
	item := gjson.Parse(`{
		"id":"ws_abc","type":"web_search_call","status":"completed",
		"action":{"type":"search","queries":["北京时间","beijing time"]}
	}`)
	blocks := anthropicServerToolBlocks(item)
	if len(blocks) != 2 {
		t.Fatalf("len = %d, want 2 (server_tool_use + web_search_tool_result)", len(blocks))
	}
	if blocks[0].Type != "server_tool_use" || blocks[0].Name != "web_search" {
		t.Fatalf("first block = %+v", blocks[0])
	}
	if blocks[0].ID != "srvtoolu_ws_abc" {
		t.Fatalf("id = %q", blocks[0].ID)
	}
	var input map[string]any
	if err := json.Unmarshal(blocks[0].Input, &input); err != nil {
		t.Fatalf("input not JSON: %v", err)
	}
	if input["query"] != "北京时间 beijing time" {
		t.Fatalf("query = %v, want joined queries", input["query"])
	}
	if blocks[1].Type != "web_search_tool_result" || blocks[1].ToolUseID != blocks[0].ID {
		t.Fatalf("result block = %+v", blocks[1])
	}

	// 未完成的调用不下发
	if got := anthropicServerToolBlocks(gjson.Parse(`{"id":"ws_x","status":"in_progress"}`)); got != nil {
		t.Fatalf("in-progress call should emit nothing, got %v", got)
	}
	if got := anthropicServerToolBlocks(gjson.Parse(`{"id":"ws_x","status":"searching"}`)); got != nil {
		t.Fatalf("searching call should emit nothing, got %v", got)
	}
}

// 失败的检索必须下发配对的 web_search_tool_result_error，否则 server_tool_use
// 会悬空、客户端拿不到终态。空 content 数组只对「完成但无命中」合法。
func TestAnthropicServerToolBlocks_FailedSearchEmitsErrorResult(t *testing.T) {
	for _, status := range []string{"failed", "incomplete"} {
		t.Run(status, func(t *testing.T) {
			item := gjson.Parse(`{"id":"ws_bad","type":"web_search_call","status":"` + status + `","action":{"type":"search","queries":["北京时间"]}}`)
			blocks := anthropicServerToolBlocks(item)
			if len(blocks) != 2 {
				t.Fatalf("len = %d, want paired server_tool_use + result", len(blocks))
			}
			if blocks[0].Type != "server_tool_use" || blocks[0].ID != "srvtoolu_ws_bad" {
				t.Fatalf("first block = %+v", blocks[0])
			}
			if blocks[1].Type != "web_search_tool_result" || blocks[1].ToolUseID != blocks[0].ID {
				t.Fatalf("result not linked: %+v", blocks[1])
			}
			var content map[string]any
			if err := json.Unmarshal(blocks[1].Content, &content); err != nil {
				t.Fatalf("content not JSON: %v", err)
			}
			if content["type"] != "web_search_tool_result_error" {
				t.Fatalf("content = %v, want error object not empty array", content)
			}
			// 上游没给已知错误码时收敛到 unavailable，不编造内部原因
			if content["error_code"] != "unavailable" {
				t.Fatalf("error_code = %v, want unavailable", content["error_code"])
			}
		})
	}
}

func TestAnthropicServerToolBlocks_KnownErrorCodePassesThrough(t *testing.T) {
	item := gjson.Parse(`{"id":"ws_q","status":"failed","action":{"query":"x"},"error":{"code":"query_too_long"}}`)
	blocks := anthropicServerToolBlocks(item)
	if len(blocks) != 2 {
		t.Fatalf("len = %d, want 2", len(blocks))
	}
	var content map[string]any
	if err := json.Unmarshal(blocks[1].Content, &content); err != nil {
		t.Fatalf("content not JSON: %v", err)
	}
	if content["error_code"] != "query_too_long" {
		t.Fatalf("error_code = %v, want passthrough query_too_long", content["error_code"])
	}
}

// 完成但无命中仍是空数组，不能误判成错误。
func TestAnthropicServerToolBlocks_CompletedWithoutMatchesIsEmptyArray(t *testing.T) {
	item := gjson.Parse(`{"id":"ws_ok","status":"completed","action":{"query":"x"}}`)
	blocks := anthropicServerToolBlocks(item)
	if len(blocks) != 2 {
		t.Fatalf("len = %d, want 2", len(blocks))
	}
	if string(blocks[1].Content) != "[]" {
		t.Fatalf("content = %s, want []", blocks[1].Content)
	}
}

// 非流式响应构建：web_search_call 必须出现在 content 里。
func TestBuildAnthropicResponseFromCompleted_WebSearch(t *testing.T) {
	completed := []byte(`{"response":{
		"status":"completed",
		"output":[
			{"type":"web_search_call","id":"ws_1","status":"completed",
			 "action":{"type":"search","queries":["北京时间"]}},
			{"type":"message","content":[{"type":"output_text","text":"现在是 00:20"}]}
		],
		"usage":{"input_tokens":10,"output_tokens":5}
	}}`)
	resp := buildAnthropicResponseFromCompleted(completed, "gpt-6-luna")
	var types []string
	for _, b := range resp.Content {
		types = append(types, b.Type)
	}
	want := []string{"server_tool_use", "web_search_tool_result", "text"}
	if len(types) != len(want) {
		t.Fatalf("block types = %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("block types = %v, want %v", types, want)
		}
	}
	if resp.Content[2].Text != "现在是 00:20" {
		t.Fatalf("text = %q", resp.Content[2].Text)
	}
	if anthropicStopReasonValue(resp.StopReason) != "end_turn" {
		t.Fatalf("stop_reason = %q, want end_turn", anthropicStopReasonValue(resp.StopReason))
	}
}

func TestCompactAnthropicContent_PreservesWebSearchBlocks(t *testing.T) {
	content := compactAnthropicContent([]anthropicContentBlock{
		{Type: "server_tool_use", ID: "srvtoolu_ws_1", Name: "web_search", Input: json.RawMessage(`{"query":"北京时间"}`)},
		{Type: "web_search_tool_result", ToolUseID: "srvtoolu_ws_1", Content: json.RawMessage(`[]`)},
	})
	if len(content) != 2 {
		t.Fatalf("len = %d, want 2: %+v", len(content), content)
	}
	if content[0].Type != "server_tool_use" || content[1].Type != "web_search_tool_result" {
		t.Fatalf("content = %+v", content)
	}
}

func TestAnthropicStreamTranslator_WebSearchCallEndTurnAndQueryDelta(t *testing.T) {
	tr := newAnthropicStreamTranslator("gpt-6-luna")
	var all []anthropicStreamEvent
	for _, ev := range []string{
		`{"type":"response.created"}`,
		`{"type":"response.output_item.added","item":{"type":"web_search_call","id":"ws_1","status":"in_progress"}}`,
		`{"type":"response.output_item.done","item":{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","queries":["北京时间"]}}}`,
		`{"type":"response.output_item.added","item":{"type":"message"}}`,
		`{"type":"response.output_text.delta","delta":"现在是 00:20"}`,
		`{"type":"response.output_text.done"}`,
		`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":5}}}`,
	} {
		all = append(all, tr.translateEvent([]byte(ev))...)
	}

	var sawServer, sawResult, sawQuery bool
	stopReason := ""
	for _, evt := range all {
		switch evt.Type {
		case "content_block_start":
			if evt.ContentBlock != nil && evt.ContentBlock.Type == "server_tool_use" {
				sawServer = true
			}
			if evt.ContentBlock != nil && evt.ContentBlock.Type == "web_search_tool_result" {
				sawResult = true
			}
		case "content_block_delta":
			if evt.Delta != nil && evt.Delta.Type == "input_json_delta" && evt.Delta.PartialJSON == `{"query":"北京时间"}` {
				sawQuery = true
			}
		case "message_delta":
			if evt.Delta != nil && evt.Delta.StopReason != nil {
				stopReason = anthropicStopReasonValue(evt.Delta.StopReason)
			}
		}
	}
	if !sawServer || !sawResult || !sawQuery {
		t.Fatalf("missing stream web-search events: server=%v result=%v query=%v events=%+v", sawServer, sawResult, sawQuery, all)
	}
	if stopReason != "end_turn" {
		t.Fatalf("stop_reason = %q, want end_turn", stopReason)
	}
}
