package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/config"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestResponsesBodyToTuiInput(t *testing.T) {
	got := responsesBodyToTuiInput([]byte(`{"model":"gpt-5.4","input":"hello"}`))
	if len(got) != 1 || got[0]["type"] != "text" || got[0]["text"] != "hello" {
		t.Fatalf("string input = %#v", got)
	}
	got = responsesBodyToTuiInput([]byte(`{"input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`))
	if len(got) != 1 || got[0]["text"] != "hi" {
		t.Fatalf("array input = %#v", got)
	}
	got = responsesBodyToTuiInput([]byte(`{"messages":[{"role":"user","content":"from chat"}]}`))
	if len(got) != 1 || got[0]["text"] != "from chat" {
		t.Fatalf("messages input = %#v", got)
	}
}

func TestShouldFallbackCodexTuiUDSModel(t *testing.T) {
	err := fmt.Errorf("tui turn failed: gpt-5.4 is not supported when using Codex with a ChatGPT account")
	if !shouldFallbackCodexTuiUDSModel("gpt-5.4", err) {
		t.Fatal("expected fallback for gpt-5.4")
	}
	if shouldFallbackCodexTuiUDSModel("gpt-5.5", err) {
		t.Fatal("did not expect fallback for gpt-5.5")
	}
}

func TestCodexTuiUDSClientTurnRoundtrip(t *testing.T) {
	backend := startFakeCodexTuiAppServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := runCodexTuiUDSTurn(ctx, backend, "gpt-5.4", []map[string]any{{"type": "text", "text": "hello"}}, nil)
	if err != nil {
		t.Fatalf("runCodexTuiUDSTurn: %v", err)
	}
	if result.Text != "hello from tui" {
		t.Fatalf("text = %q", result.Text)
	}
	if result.ThreadID != "thread-1" {
		t.Fatalf("thread id = %q", result.ThreadID)
	}
}

func TestResponsesViaCodexTuiUDS(t *testing.T) {
	gin.SetMode(gin.TestMode)
	backend := startFakeCodexTuiAppServer(t)
	handler := NewHandler(nil, nil, &config.Config{
		AllowAnonymousV1:   true,
		CodexTuiUDSEnabled: true,
		CodexTuiUDSBackend: backend,
	}, nil)
	router := gin.New()
	handler.RegisterRoutes(router)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"gpt-5.4","input":"hello","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "response.output_text.delta") || !strings.Contains(body, "hello from tui") {
		t.Fatalf("stream body = %s", body)
	}
}

func TestChatCompletionsViaCodexTuiUDS(t *testing.T) {
	gin.SetMode(gin.TestMode)
	backend := startFakeCodexTuiAppServer(t)
	handler := NewHandler(nil, nil, &config.Config{
		AllowAnonymousV1:   true,
		CodexTuiUDSEnabled: true,
		CodexTuiUDSBackend: backend,
	}, nil)
	router := gin.New()
	handler.RegisterRoutes(router)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "hello from tui") {
		t.Fatalf("chat body = %s", rec.Body.String())
	}
}

func TestResponsesCompactViaCodexTuiUDS(t *testing.T) {
	gin.SetMode(gin.TestMode)
	backend := startFakeCodexTuiAppServer(t)
	handler := NewHandler(nil, nil, &config.Config{
		AllowAnonymousV1:   true,
		CodexTuiUDSEnabled: true,
		CodexTuiUDSBackend: backend,
	}, nil)
	router := gin.New()
	handler.RegisterRoutes(router)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewBufferString(`{"model":"gpt-5.5","input":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "hello from tui") {
		t.Fatalf("compact body = %s", rec.Body.String())
	}
}

func TestBackendAPICodexResponsesViaCodexTuiUDS(t *testing.T) {
	gin.SetMode(gin.TestMode)
	backend := startFakeCodexTuiAppServer(t)
	handler := NewHandler(nil, nil, &config.Config{
		AllowAnonymousV1:   true,
		CodexTuiUDSEnabled: true,
		CodexTuiUDSBackend: backend,
	}, nil)
	router := gin.New()
	handler.RegisterRoutes(router)
	req := httptest.NewRequest(http.MethodPost, "/backend-api/codex/responses", bytes.NewBufferString(`{"model":"gpt-5.5","input":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "hello from tui") {
		t.Fatalf("backend-api body = %s", rec.Body.String())
	}
}

func TestCodexTuiUDSProxyForwardsJSONRPC(t *testing.T) {

	backend := startFakeCodexTuiAppServer(t)
	listen := filepath.Join(t.TempDir(), "listen.sock")
	runtime, err := StartCodexTuiUDS(&config.Config{
		CodexTuiUDSEnabled: true,
		CodexTuiUDSListen:  listen,
		CodexTuiUDSBackend: backend,
	})
	if err != nil {
		t.Fatalf("StartCodexTuiUDS: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(listen); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := runCodexTuiUDSTurn(ctx, listen, "gpt-5.4", []map[string]any{{"type": "text", "text": "hello"}}, nil)
	if err != nil {
		t.Fatalf("proxied turn: %v", err)
	}
	if result.Text != "hello from tui" {
		t.Fatalf("proxied text = %q", result.Text)
	}
}

func startFakeCodexTuiAppServer(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tui.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc("/rpc", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			method := gjson.GetBytes(payload, "method").String()
			id := gjson.GetBytes(payload, "id")
			switch method {
			case "initialize":
				writeFakeJSONRPC(conn, map[string]any{
					"id": rawJSON(id.Raw),
					"result": map[string]any{
						"userAgent": "codex-tui/0.0.0",
						"codexHome": t.TempDir(),
					},
				})
			case "initialized":
			case "account/login/start":
				writeFakeJSONRPC(conn, map[string]any{
					"id":     rawJSON(id.Raw),
					"result": map[string]any{"type": "chatgptAuthTokens"},
				})
			case "thread/start":
				writeFakeJSONRPC(conn, map[string]any{
					"id": rawJSON(id.Raw),
					"result": map[string]any{
						"thread": map[string]any{"id": "thread-1", "sessionId": "sess-1", "preview": "", "ephemeral": true},
					},
				})
			case "turn/start":
				writeFakeJSONRPC(conn, map[string]any{
					"id":     rawJSON(id.Raw),
					"result": map[string]any{"turn": map[string]any{"id": "turn-1"}},
				})
				writeFakeJSONRPC(conn, map[string]any{
					"method": "item/agentMessage/delta",
					"params": map[string]any{"threadId": "thread-1", "turnId": "turn-1", "itemId": "item-1", "delta": "hello from tui"},
				})
				writeFakeJSONRPC(conn, map[string]any{
					"method": "turn/completed",
					"params": map[string]any{"threadId": "thread-1", "turn": map[string]any{"id": "turn-1"}},
				})
			default:
				if id.Exists() {
					writeFakeJSONRPC(conn, map[string]any{
						"id":    rawJSON(id.Raw),
						"error": map[string]any{"code": -32601, "message": "unknown " + method},
					})
				}
			}
		}
	})

	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(ln) }()
	t.Cleanup(func() {
		_ = server.Close()
		_ = ln.Close()
		_ = os.Remove(path)
	})
	return path
}

func writeFakeJSONRPC(conn *websocket.Conn, payload map[string]any) {
	body, _ := json.Marshal(payload)
	_ = conn.WriteMessage(websocket.TextMessage, body)
}

func rawJSON(raw string) any {
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err == nil {
		return value
	}
	return raw
}
