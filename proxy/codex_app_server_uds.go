package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/internal/version"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

const (
	defaultAppServerSocket = "/run/codex/app-server.sock"
	defaultAppServerCwd    = "/opt/codex-workspace"
	appServerUDSAccountID  = int64(-1001)
)

type appServerUDSSettings struct {
	enabled    bool
	socketPath string
	cwd        string
}

var (
	appServerUDSMu            sync.RWMutex
	appServerUDSSettingsValue appServerUDSSettings
)

func configureAppServerUDS(cfg *config.Config) {
	settings := appServerUDSSettings{
		socketPath: strings.TrimSpace(os.Getenv("CODEX_APP_SERVER_SOCKET")),
		cwd:        strings.TrimSpace(os.Getenv("CODEX_APP_SERVER_CWD")),
	}
	if cfg != nil {
		if strings.EqualFold(strings.TrimSpace(cfg.CodexUpstreamTransport), "uds") {
			settings.enabled = true
		}
		if path := strings.TrimSpace(cfg.CodexAppServerSocket); path != "" {
			settings.socketPath = path
		}
		if cwd := strings.TrimSpace(cfg.CodexAppServerCwd); cwd != "" {
			settings.cwd = cwd
		}
	}
	if !settings.enabled {
		settings.enabled = strings.EqualFold(strings.TrimSpace(os.Getenv("CODEX_UPSTREAM_TRANSPORT")), "uds")
	}
	if settings.socketPath == "" {
		settings.socketPath = defaultAppServerSocket
	}
	if settings.cwd == "" {
		settings.cwd = defaultAppServerCwd
	}
	appServerUDSMu.Lock()
	appServerUDSSettingsValue = settings
	appServerUDSMu.Unlock()
}

func currentAppServerUDSSettings() appServerUDSSettings {
	appServerUDSMu.RLock()
	settings := appServerUDSSettingsValue
	appServerUDSMu.RUnlock()
	if settings.socketPath == "" {
		settings.socketPath = strings.TrimSpace(os.Getenv("CODEX_APP_SERVER_SOCKET"))
		if settings.socketPath == "" {
			settings.socketPath = defaultAppServerSocket
		}
	}
	if settings.cwd == "" {
		settings.cwd = strings.TrimSpace(os.Getenv("CODEX_APP_SERVER_CWD"))
		if settings.cwd == "" {
			settings.cwd = defaultAppServerCwd
		}
	}
	if !settings.enabled {
		settings.enabled = strings.EqualFold(strings.TrimSpace(os.Getenv("CODEX_UPSTREAM_TRANSPORT")), "uds")
	}
	return settings
}

func appServerUDSEnabled() bool {
	return currentAppServerUDSSettings().enabled
}

func appServerUDSAccount() *auth.Account {
	return &auth.Account{
		DBID:     appServerUDSAccountID,
		Email:    "codex-app-server-uds",
		PlanType: "uds",
		Status:   auth.StatusReady,
	}
}

func (h *Handler) shouldUseAppServerUDS() bool {
	if h != nil && h.cfg != nil && strings.EqualFold(strings.TrimSpace(h.cfg.CodexUpstreamTransport), "uds") {
		return true
	}
	return appServerUDSEnabled()
}

func executeCodexAppServerUDS(ctx context.Context, requestBody []byte) (*http.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	settings := currentAppServerUDSSettings()
	model, userInput, err := extractAppServerTurnInput(requestBody)
	if err != nil {
		return nil, ErrUpstream(http.StatusBadRequest, "codex app-server 输入为空", err)
	}

	conn, err := dialCodexAppServerUDS(ctx, settings.socketPath)
	if err != nil {
		return nil, ErrUpstream(0, "连接 Codex app-server Unix socket 失败", err)
	}

	session := &appServerUDSSession{conn: conn, nextID: 1}
	if err := session.handshake(ctx); err != nil {
		conn.Close()
		return nil, ErrUpstream(0, "Codex app-server initialize 失败", err)
	}
	threadID, err := session.startThread(ctx, model, settings.cwd)
	if err != nil {
		conn.Close()
		return nil, ErrUpstream(0, "Codex app-server thread/start 失败", err)
	}
	if err := session.startTurn(ctx, threadID, userInput); err != nil {
		conn.Close()
		return nil, ErrUpstream(0, "Codex app-server turn/start 失败", err)
	}

	pr, pw := io.Pipe()
	responseID := "resp_" + uuid.NewString()
	go session.streamTurnToSSE(ctx, pw, responseID, model)

	header := make(http.Header)
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	return &http.Response{
		StatusCode:    http.StatusOK,
		Status:        "200 OK",
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        header,
		Body:          pr,
		ContentLength: -1,
	}, nil
}

func dialCodexAppServerUDS(ctx context.Context, socketPath string) (*websocket.Conn, error) {
	socketPath = strings.TrimSpace(socketPath)
	if socketPath == "" {
		return nil, fmt.Errorf("empty unix socket path")
	}
	dialer := websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
		NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var nd net.Dialer
			return nd.DialContext(ctx, "unix", socketPath)
		},
	}
	header := http.Header{}
	header.Set("Origin", "http://localhost")
	conn, _, err := dialer.DialContext(ctx, "ws://localhost/app-server", header)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

type appServerUDSSession struct {
	conn   *websocket.Conn
	nextID int64
}

func (s *appServerUDSSession) handshake(ctx context.Context) error {
	_, err := s.call(ctx, "initialize", map[string]any{
		"clientInfo": map[string]any{
			"name":    "codex2api",
			"title":   "Codex2API",
			"version": version.Current(),
		},
	})
	if err != nil {
		return err
	}
	return s.notify("initialized", nil)
}

func (s *appServerUDSSession) startThread(ctx context.Context, model, cwd string) (string, error) {
	params := map[string]any{
		"cwd":            cwd,
		"approvalPolicy": "never",
		"sandbox":        "read-only",
		"ephemeral":      true,
	}
	if strings.TrimSpace(model) != "" {
		params["model"] = model
	}
	result, err := s.call(ctx, "thread/start", params)
	if err != nil {
		return "", err
	}
	threadID := strings.TrimSpace(gjson.GetBytes(result, "thread.id").String())
	if threadID == "" {
		return "", fmt.Errorf("thread/start missing thread.id: %s", truncateAppServerPayload(result))
	}
	return threadID, nil
}

func (s *appServerUDSSession) startTurn(ctx context.Context, threadID string, input []map[string]string) error {
	_, err := s.call(ctx, "turn/start", map[string]any{
		"threadId": threadID,
		"input":    input,
	})
	return err
}

func (s *appServerUDSSession) streamTurnToSSE(ctx context.Context, w *io.PipeWriter, responseID, model string) {
	defer s.conn.Close()
	defer w.Close()

	var textBuilder strings.Builder
	if err := writeResponsesSSE(w, map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id":     responseID,
			"object": "response",
			"status": "in_progress",
			"model":  model,
			"output": []any{},
		},
	}); err != nil {
		_ = w.CloseWithError(err)
		return
	}

	for {
		if err := ctx.Err(); err != nil {
			_ = w.CloseWithError(err)
			return
		}
		_ = s.conn.SetReadDeadline(time.Now().Add(5 * time.Minute))
		_, payload, err := s.conn.ReadMessage()
		if err != nil {
			if textBuilder.Len() == 0 {
				_ = w.CloseWithError(fmt.Errorf("codex app-server stream closed: %w", err))
				return
			}
			if writeErr := writeResponsesCompletedSSE(w, responseID, model, textBuilder.String(), "incomplete"); writeErr != nil {
				_ = w.CloseWithError(writeErr)
			}
			return
		}
		if handled, delta, completed, failed, failErr := handleAppServerStreamMessage(s, payload); handled {
			if failErr != nil {
				_ = writeResponsesFailedSSE(w, responseID, failErr.Error())
				return
			}
			if delta != "" {
				textBuilder.WriteString(delta)
				if err := writeResponsesSSE(w, map[string]any{
					"type":  "response.output_text.delta",
					"delta": delta,
				}); err != nil {
					_ = w.CloseWithError(err)
					return
				}
			}
			if failed {
				msg := "codex app-server turn failed"
				if failErr != nil {
					msg = failErr.Error()
				}
				_ = writeResponsesFailedSSE(w, responseID, msg)
				return
			}
			if completed {
				if err := writeResponsesCompletedSSE(w, responseID, model, textBuilder.String(), "completed"); err != nil {
					_ = w.CloseWithError(err)
				}
				return
			}
		}
	}
}

func handleAppServerStreamMessage(s *appServerUDSSession, payload []byte) (handled bool, delta string, completed bool, failed bool, failErr error) {
	msg, ok := parseAppServerRPCMessage(payload)
	if !ok {
		return false, "", false, false, nil
	}
	if msg.Method != "" && msg.ID != nil {
		_ = s.replyServerRequest(msg)
		return true, "", false, false, nil
	}
	if msg.Method == "" {
		return false, "", false, false, nil
	}
	switch msg.Method {
	case "item/agentMessage/delta":
		return true, gjson.GetBytes(payload, "params.delta").String(), false, false, nil
	case "turn/completed":
		status := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "params.turn.status").String()))
		if status == "failed" || status == "interrupted" {
			errMsg := strings.TrimSpace(gjson.GetBytes(payload, "params.turn.error.message").String())
			if errMsg == "" {
				errMsg = "codex app-server turn " + status
			}
			return true, "", false, true, fmt.Errorf("%s", errMsg)
		}
		return true, "", true, false, nil
	case "turn/failed":
		errMsg := strings.TrimSpace(gjson.GetBytes(payload, "params.error.message").String())
		if errMsg == "" {
			errMsg = "codex app-server turn failed"
		}
		return true, "", false, true, fmt.Errorf("%s", errMsg)
	default:
		return true, "", false, false, nil
	}
}

type appServerRPCMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func parseAppServerRPCMessage(payload []byte) (appServerRPCMessage, bool) {
	var msg appServerRPCMessage
	if err := json.Unmarshal(payload, &msg); err != nil {
		return appServerRPCMessage{}, false
	}
	return msg, true
}

func (s *appServerUDSSession) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := s.nextID
	s.nextID++
	request := map[string]any{
		"id":     id,
		"method": method,
	}
	if params != nil {
		request["params"] = params
	}
	if err := s.conn.WriteJSON(request); err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		_ = s.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		_, payload, err := s.conn.ReadMessage()
		if err != nil {
			return nil, err
		}
		msg, ok := parseAppServerRPCMessage(payload)
		if !ok {
			continue
		}
		if msg.Method != "" && msg.ID != nil {
			_ = s.replyServerRequest(msg)
			continue
		}
		if msg.Method != "" {
			continue
		}
		if !appServerRPCIDEquals(msg.ID, id) {
			continue
		}
		if msg.Error != nil {
			return nil, fmt.Errorf("%s: %s", method, msg.Error.Message)
		}
		return msg.Result, nil
	}
}

func (s *appServerUDSSession) notify(method string, params any) error {
	message := map[string]any{"method": method}
	if params != nil {
		message["params"] = params
	}
	return s.conn.WriteJSON(message)
}

func (s *appServerUDSSession) replyServerRequest(msg appServerRPCMessage) error {
	method := strings.TrimSpace(msg.Method)
	var result any
	switch method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		result = map[string]any{"decision": "decline"}
	default:
		return s.conn.WriteJSON(map[string]any{
			"id": msg.ID,
			"error": map[string]any{
				"code":    -32601,
				"message": "unsupported server request: " + method,
			},
		})
	}
	return s.conn.WriteJSON(map[string]any{
		"id":     msg.ID,
		"result": result,
	})
}

func appServerRPCIDEquals(raw json.RawMessage, id int64) bool {
	if len(raw) == 0 {
		return false
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		return n == id
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s == fmt.Sprintf("%d", id)
	}
	return false
}

func extractAppServerTurnInput(requestBody []byte) (string, []map[string]string, error) {
	model := strings.TrimSpace(gjson.GetBytes(requestBody, "model").String())
	var parts []string
	if instructions := strings.TrimSpace(gjson.GetBytes(requestBody, "instructions").String()); instructions != "" {
		parts = append(parts, instructions)
	}
	parts = append(parts, collectResponsesInputText(gjson.GetBytes(requestBody, "input"))...)
	text := strings.TrimSpace(strings.Join(parts, "\n\n"))
	if text == "" {
		return model, nil, fmt.Errorf("no text input in /v1/responses body")
	}
	return model, []map[string]string{{
		"type": "text",
		"text": text,
	}}, nil
}

func collectResponsesInputText(node gjson.Result) []string {
	if !node.Exists() {
		return nil
	}
	if node.Type == gjson.String {
		if text := strings.TrimSpace(node.String()); text != "" {
			return []string{text}
		}
		return nil
	}
	if !node.IsArray() {
		return collectResponsesContentText(node)
	}
	var parts []string
	for _, item := range node.Array() {
		parts = append(parts, collectResponsesContentText(item)...)
	}
	return parts
}

func collectResponsesContentText(node gjson.Result) []string {
	if !node.Exists() {
		return nil
	}
	if node.Type == gjson.String {
		if text := strings.TrimSpace(node.String()); text != "" {
			return []string{text}
		}
		return nil
	}
	if text := strings.TrimSpace(node.Get("text").String()); text != "" && !node.Get("content").Exists() {
		return []string{text}
	}
	role := strings.ToLower(strings.TrimSpace(node.Get("role").String()))
	content := node.Get("content")
	if !content.Exists() {
		return nil
	}
	if content.Type == gjson.String {
		text := strings.TrimSpace(content.String())
		if text == "" {
			return nil
		}
		if role != "" && role != "user" {
			return []string{role + ": " + text}
		}
		return []string{text}
	}
	if !content.IsArray() {
		return nil
	}
	var parts []string
	for _, part := range content.Array() {
		partType := strings.ToLower(strings.TrimSpace(part.Get("type").String()))
		text := strings.TrimSpace(part.Get("text").String())
		if text == "" {
			continue
		}
		if partType != "" && partType != "input_text" && partType != "text" && partType != "output_text" {
			continue
		}
		if role != "" && role != "user" {
			parts = append(parts, role+": "+text)
			continue
		}
		parts = append(parts, text)
	}
	return parts
}

func writeResponsesSSE(w io.Writer, payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", raw)
	return err
}

func writeResponsesCompletedSSE(w io.Writer, responseID, model, text, status string) error {
	if status == "" {
		status = "completed"
	}
	usage := estimateAppServerUsage(text)
	return writeResponsesSSE(w, map[string]any{
		"type": "response." + status,
		"response": map[string]any{
			"id":     responseID,
			"object": "response",
			"status": status,
			"model":  model,
			"output": []any{
				map[string]any{
					"type": "message",
					"role": "assistant",
					"content": []any{
						map[string]any{
							"type": "output_text",
							"text": text,
						},
					},
				},
			},
			"usage": usage,
		},
	})
}

func writeResponsesFailedSSE(w io.Writer, responseID, message string) error {
	return writeResponsesSSE(w, map[string]any{
		"type": "response.failed",
		"response": map[string]any{
			"id":     responseID,
			"object": "response",
			"status": "failed",
			"error": map[string]any{
				"code":    "app_server_error",
				"message": message,
			},
		},
	})
}

func estimateAppServerUsage(text string) map[string]any {
	outputTokens := len([]rune(text)) / 4
	if outputTokens < 1 && text != "" {
		outputTokens = 1
	}
	return map[string]any{
		"input_tokens":  0,
		"output_tokens": outputTokens,
		"total_tokens":  outputTokens,
	}
}

func truncateAppServerPayload(raw []byte) string {
	const maxLen = 512
	text := strings.TrimSpace(string(raw))
	if len(text) <= maxLen {
		return text
	}
	return text[:maxLen] + "..."
}

func buildAppServerInitializeRequest(id int64, name, versionName string) map[string]any {
	return map[string]any{
		"id":     id,
		"method": "initialize",
		"params": map[string]any{
			"clientInfo": map[string]any{
				"name":    name,
				"version": versionName,
			},
		},
	}
}

func buildAppServerInitializedNotification() map[string]any {
	return map[string]any{"method": "initialized"}
}

func buildAppServerThreadStartParams(model, cwd string) map[string]any {
	params := map[string]any{
		"cwd":            cwd,
		"approvalPolicy": "never",
		"sandbox":        "read-only",
		"ephemeral":      true,
	}
	if strings.TrimSpace(model) != "" {
		params["model"] = model
	}
	return params
}

func buildAppServerTurnStartParams(threadID, text string) map[string]any {
	return map[string]any{
		"threadId": threadID,
		"input": []map[string]string{{
			"type": "text",
			"text": text,
		}},
	}
}
