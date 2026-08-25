package proxy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/codex2api/config"
)

func TestExtractAppServerTurnInputFromResponsesBody(t *testing.T) {
	body := []byte(`{
		"model": "gpt-5.4",
		"instructions": "Be brief.",
		"input": [
			{"role":"user","content":[{"type":"input_text","text":"ping"}]}
		]
	}`)
	model, input, err := extractAppServerTurnInput(body)
	if err != nil {
		t.Fatalf("extractAppServerTurnInput() error = %v", err)
	}
	if model != "gpt-5.4" {
		t.Fatalf("model = %q, want gpt-5.4", model)
	}
	if len(input) != 1 || input[0]["type"] != "text" {
		t.Fatalf("input = %#v", input)
	}
	if !strings.Contains(input[0]["text"], "Be brief.") || !strings.Contains(input[0]["text"], "ping") {
		t.Fatalf("text = %q", input[0]["text"])
	}
}

func TestExtractAppServerTurnInputFromStringInput(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","input":"hello uds"}`)
	_, input, err := extractAppServerTurnInput(body)
	if err != nil {
		t.Fatalf("extractAppServerTurnInput() error = %v", err)
	}
	if input[0]["text"] != "hello uds" {
		t.Fatalf("text = %q", input[0]["text"])
	}
}

func TestBuildAppServerHandshakeJSON(t *testing.T) {
	initReq := buildAppServerInitializeRequest(1, "codex2api", "dev")
	raw, err := json.Marshal(initReq)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"jsonrpc"`) {
		t.Fatalf("initialize should omit jsonrpc field: %s", raw)
	}
	if !strings.Contains(string(raw), `"clientInfo"`) {
		t.Fatalf("initialize missing clientInfo: %s", raw)
	}

	initialized, err := json.Marshal(buildAppServerInitializedNotification())
	if err != nil {
		t.Fatal(err)
	}
	if string(initialized) != `{"method":"initialized"}` {
		t.Fatalf("initialized = %s", initialized)
	}

	threadParams, err := json.Marshal(buildAppServerThreadStartParams("gpt-5.4", "/opt/codex-workspace"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"approvalPolicy":"never"`, `"sandbox":"read-only"`, `"ephemeral":true`, `"cwd":"/opt/codex-workspace"`} {
		if !strings.Contains(string(threadParams), want) {
			t.Fatalf("thread/start params missing %s: %s", want, threadParams)
		}
	}

	turnParams, err := json.Marshal(buildAppServerTurnStartParams("thread-1", "hello"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(turnParams), `"threadId":"thread-1"`) || !strings.Contains(string(turnParams), `"type":"text"`) {
		t.Fatalf("turn/start params = %s", turnParams)
	}
}

func TestWriteResponsesSSEFraming(t *testing.T) {
	var b strings.Builder
	if err := writeResponsesSSE(&b, map[string]any{
		"type":  "response.output_text.delta",
		"delta": "hi",
	}); err != nil {
		t.Fatal(err)
	}
	got := b.String()
	if got != "data: {\"delta\":\"hi\",\"type\":\"response.output_text.delta\"}\n\n" && !strings.HasPrefix(got, "data: {") {
		t.Fatalf("sse = %q", got)
	}
	if !strings.Contains(got, `"type":"response.output_text.delta"`) || !strings.Contains(got, `"delta":"hi"`) || !strings.HasSuffix(got, "\n\n") {
		t.Fatalf("sse framing mismatch: %q", got)
	}

	b.Reset()
	if err := writeResponsesCompletedSSE(&b, "resp_1", "gpt-5.4", "hello", "completed"); err != nil {
		t.Fatal(err)
	}
	completed := b.String()
	if !strings.Contains(completed, `"type":"response.completed"`) || !strings.Contains(completed, `"usage"`) {
		t.Fatalf("completed sse = %q", completed)
	}
}

func TestHandleAppServerStreamMessageDeltaAndCompleted(t *testing.T) {
	_, delta, completed, failed, err := handleAppServerStreamMessage(nil, []byte(`{"method":"item/agentMessage/delta","params":{"delta":"abc"}}`))
	if err != nil || !completed && failed {
		t.Fatalf("delta handler failed=%v completed=%v err=%v", failed, completed, err)
	}
	if delta != "abc" {
		t.Fatalf("delta = %q", delta)
	}

	_, _, completed, failed, err = handleAppServerStreamMessage(nil, []byte(`{"method":"turn/completed","params":{"turn":{"status":"completed"}}}`))
	if err != nil || !completed || failed {
		t.Fatalf("completed handler failed=%v completed=%v err=%v", failed, completed, err)
	}

	_, _, _, failed, err = handleAppServerStreamMessage(nil, []byte(`{"method":"turn/completed","params":{"turn":{"status":"failed","error":{"message":"boom"}}}}`))
	if !failed || err == nil || err.Error() != "boom" {
		t.Fatalf("failed handler failed=%v err=%v", failed, err)
	}
}

func TestShouldUseWebsocketForHTTPIgnoresUDS(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	ApplyRuntimeSettings(DefaultRuntimeSettings())

	handler := NewHandler(nil, nil, &config.Config{CodexUpstreamTransport: "uds"}, nil)
	if handler.shouldUseWebsocketForHTTP() {
		t.Fatal("uds transport should not use websocket")
	}
	if !handler.shouldUseAppServerUDS() {
		t.Fatal("uds transport should enable app-server UDS")
	}
}
