package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

const (
	codexTuiUDSHandshakeURL = "ws://localhost/rpc"
	codexTuiUDSReadLimit    = 128 << 20
	codexTuiUDSDialTimeout  = 10 * time.Second
)

var codexTuiUDSUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

type CodexTuiUDSRuntime struct {
	listenPath  string
	backendPath string
	homePath    string
	server      *http.Server
	listener    net.Listener
	cmd         *exec.Cmd
}

type tuiUDSAuth struct {
	AccessToken string
	AccountID   string
	PlanType    string
}

type tuiUDSTurnResult struct {
	ThreadID string
	TurnID   string
	Text     string
}

type jsonrpcMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *jsonrpcError   `json:"error,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type tuiUDSConn struct {
	conn      *websocket.Conn
	writeMu   sync.Mutex
	pendingMu sync.Mutex
	pending   map[string]chan jsonrpcMessage
	notifs    chan jsonrpcMessage
	closed    chan struct{}
	closeOnce sync.Once
	closeErr  error
	auth      *tuiUDSAuth
}

func (h *Handler) codexTuiUDSEnabled() bool {
	return h != nil && h.cfg != nil && h.cfg.CodexTuiUDSEnabled
}

func StartCodexTuiUDS(cfg *config.Config) (*CodexTuiUDSRuntime, error) {
	if cfg == nil || !cfg.CodexTuiUDSEnabled {
		return nil, nil
	}
	runtime := &CodexTuiUDSRuntime{
		listenPath:  strings.TrimSpace(cfg.CodexTuiUDSListen),
		backendPath: strings.TrimSpace(cfg.CodexTuiUDSBackend),
		homePath:    strings.TrimSpace(cfg.CodexTuiUDSHome),
	}
	if runtime.backendPath == "" {
		return nil, fmt.Errorf("CODEX_TUI_UDS_BACKEND is empty")
	}
	if runtime.listenPath != "" && sameFilePath(runtime.listenPath, runtime.backendPath) {
		return nil, fmt.Errorf("CODEX_TUI_UDS_LISTEN must not equal CODEX_TUI_UDS_BACKEND (%s); that would loop", runtime.listenPath)
	}
	if cfg.CodexTuiUDSSpawn {
		if err := runtime.maybeSpawnBackend(); err != nil {
			return nil, err
		}
	}
	if runtime.listenPath == "" {
		log.Printf("Codex TUI UDS: HTTP /v1/responses -> %s (no listen socket)", runtime.backendPath)
		return runtime, nil
	}
	if err := os.MkdirAll(filepath.Dir(runtime.listenPath), 0o700); err != nil {
		return nil, fmt.Errorf("create UDS listen dir: %w", err)
	}
	_ = os.Remove(runtime.listenPath)
	ln, err := net.Listen("unix", runtime.listenPath)
	if err != nil {
		return nil, fmt.Errorf("listen Codex TUI UDS %s: %w", runtime.listenPath, err)
	}
	runtime.listener = ln
	mux := http.NewServeMux()
	mux.HandleFunc("/rpc", runtime.serveRPC)
	mux.HandleFunc("/", runtime.serveRPC)
	runtime.server = &http.Server{Handler: mux}
	go func() {
		if err := runtime.server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("Codex TUI UDS listener exited: %v", err)
		}
	}()
	log.Printf("Codex TUI UDS listen %s -> backend %s", runtime.listenPath, runtime.backendPath)
	return runtime, nil
}

func (rt *CodexTuiUDSRuntime) Close() error {
	if rt == nil {
		return nil
	}
	var errs []error
	if rt.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		errs = append(errs, rt.server.Shutdown(ctx))
		cancel()
	}
	if rt.listener != nil {
		_ = rt.listener.Close()
	}
	if rt.listenPath != "" {
		_ = os.Remove(rt.listenPath)
	}
	if rt.cmd != nil && rt.cmd.Process != nil {
		errs = append(errs, rt.cmd.Process.Kill())
	}
	return errors.Join(errs...)
}

func (rt *CodexTuiUDSRuntime) maybeSpawnBackend() error {
	if rt.backendPath == "" {
		return fmt.Errorf("backend socket path is empty")
	}
	if udsSocketAlive(rt.backendPath) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(rt.backendPath), 0o700); err != nil {
		return fmt.Errorf("create TUI backend socket dir: %w", err)
	}
	home := strings.TrimSpace(rt.homePath)
	if home == "" {
		home = filepath.Join(filepath.Dir(rt.backendPath), "tui-home")
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return fmt.Errorf("create TUI CODEX_HOME: %w", err)
	}
	codexBin := lookupCodexBinary()
	cmd := exec.Command(codexBin, "app-server", "--listen", "unix://"+rt.backendPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "CODEX_HOME="+home)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn `codex app-server --listen unix://%s`: %w", rt.backendPath, err)
	}
	rt.cmd = cmd
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if udsSocketAlive(rt.backendPath) {
			log.Printf("spawned codex app-server on %s (pid %d, CODEX_HOME=%s)", rt.backendPath, cmd.Process.Pid, home)
			return nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	return fmt.Errorf("spawned codex app-server but %s did not accept connections", rt.backendPath)
}

func (rt *CodexTuiUDSRuntime) serveRPC(w http.ResponseWriter, r *http.Request) {
	frontend, err := codexTuiUDSUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Codex TUI UDS upgrade failed: %v", err)
		return
	}
	defer frontend.Close()
	backend, err := dialCodexTuiUDS(r.Context(), rt.backendPath)
	if err != nil {
		log.Printf("Codex TUI UDS backend dial failed: %v", err)
		_ = frontend.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseTryAgainLater, "codex tui backend unavailable"))
		return
	}
	defer backend.Close()
	proxyCodexTuiWebSocket(frontend, backend)
}

func proxyCodexTuiWebSocket(a, b *websocket.Conn) {
	var wg sync.WaitGroup
	copyWS := func(dst, src *websocket.Conn) {
		defer wg.Done()
		defer dst.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
		for {
			mt, payload, err := src.ReadMessage()
			if err != nil {
				return
			}
			if err := dst.WriteMessage(mt, payload); err != nil {
				return
			}
		}
	}
	wg.Add(2)
	go copyWS(a, b)
	go copyWS(b, a)
	wg.Wait()
}

func (h *Handler) executeResponsesViaCodexTuiUDS(c *gin.Context, rawBody []byte, model string, isStream bool) {
	start := time.Now()
	if h == nil || h.cfg == nil {
		api.SendError(c, api.NewAPIError(api.ErrCodeServiceUnavailable, "Codex TUI UDS is not configured", api.ErrorTypeServer))
		return
	}
	backend := strings.TrimSpace(h.cfg.CodexTuiUDSBackend)
	if backend == "" {
		api.SendError(c, api.NewAPIError(api.ErrCodeServiceUnavailable, "CODEX_TUI_UDS_BACKEND is empty", api.ErrorTypeServer))
		return
	}
	input := responsesBodyToTuiInput(rawBody)
	if len(input) == 0 {
		api.SendError(c, api.NewAPIError(api.ErrCodeMissingField, "input is required", api.ErrorTypeInvalidRequest))
		return
	}
	account, tuiAuth := h.tuiUDSAccountAndAuth(c)
	if account != nil && h.store != nil {
		defer h.store.Release(account)
	}
	endpoint := inboundEndpointFromRequest(c, "/v1/responses")
	result, err := runCodexTuiUDSTurn(c.Request.Context(), backend, model, input, tuiAuth)
	if err != nil {
		log.Printf("Codex TUI UDS %s failed: %v", endpoint, err)
		h.logCodexTuiUDSUsage(c, account, endpoint, model, start, http.StatusBadGateway, input, nil, err, isStream, false)
		api.SendError(c, api.NewAPIError(api.ErrCodeUpstreamError, err.Error(), api.ErrorTypeUpstream))
		return
	}
	h.logCodexTuiUDSUsage(c, account, endpoint, model, start, http.StatusOK, input, result, nil, isStream, false)
	responseID := "resp_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if isStream {
		writeCodexTuiUDSResponseStream(c, responseID, model, result)
		return
	}
	c.JSON(http.StatusOK, buildCodexTuiUDSResponseJSON(responseID, model, result, "completed"))
}

func runCodexTuiUDSTurn(ctx context.Context, socketPath, model string, input []map[string]any, auth *tuiUDSAuth) (*tuiUDSTurnResult, error) {
	result, err := runCodexTuiUDSTurnOnce(ctx, socketPath, model, input, auth)
	if err != nil && shouldFallbackCodexTuiUDSModel(model, err) {
		return runCodexTuiUDSTurnOnce(ctx, socketPath, "gpt-5.5", input, auth)
	}
	return result, err
}

func shouldFallbackCodexTuiUDSModel(model string, err error) bool {
	if err == nil {
		return false
	}
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" || model == "gpt-5.5" || strings.HasPrefix(model, "gpt-5.5-") {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not supported when using codex with a chatgpt account")
}

func runCodexTuiUDSTurnOnce(ctx context.Context, socketPath, model string, input []map[string]any, auth *tuiUDSAuth) (*tuiUDSTurnResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	conn, err := dialCodexTuiUDS(ctx, socketPath)
	if err != nil {
		return nil, err
	}
	client := newTuiUDSConn(conn, auth)
	defer client.Close()
	go func() {
		<-ctx.Done()
		client.Close()
	}()

	if _, err := client.call(ctx, "initialize", map[string]any{
		"clientInfo": map[string]any{
			"name":    "codex2api",
			"title":   "Codex2API",
			"version": "dev",
		},
		"capabilities": map[string]any{
			"experimentalApi": true,
		},
	}); err != nil {
		return nil, fmt.Errorf("tui initialize: %w", err)
	}
	if err := client.notify("initialized", nil); err != nil {
		return nil, fmt.Errorf("tui initialized: %w", err)
	}
	if err := client.loginWithAccount(ctx); err != nil {
		return nil, err
	}

	threadParams := map[string]any{
		"ephemeral":      true,
		"approvalPolicy": "never",
		"sandbox":        "read-only",
	}
	if strings.TrimSpace(model) != "" {
		threadParams["model"] = model
	}
	threadResult, err := client.call(ctx, "thread/start", threadParams)
	if err != nil {
		return nil, fmt.Errorf("tui thread/start: %w", err)
	}
	threadID := strings.TrimSpace(gjson.GetBytes(threadResult, "thread.id").String())
	if threadID == "" {
		return nil, fmt.Errorf("tui thread/start missing thread.id")
	}

	turnResult, err := client.call(ctx, "turn/start", map[string]any{
		"threadId": threadID,
		"input":    input,
	})
	if err != nil {
		return nil, fmt.Errorf("tui turn/start: %w", err)
	}
	turnID := strings.TrimSpace(gjson.GetBytes(turnResult, "turn.id").String())
	text, completedTurnID, err := client.waitTurnCompleted(ctx, threadID, turnID)
	if err != nil {
		return nil, err
	}
	if turnID == "" {
		turnID = completedTurnID
	}
	return &tuiUDSTurnResult{ThreadID: threadID, TurnID: turnID, Text: text}, nil
}

func newTuiUDSConn(conn *websocket.Conn, auth *tuiUDSAuth) *tuiUDSConn {
	client := &tuiUDSConn{
		conn:    conn,
		pending: make(map[string]chan jsonrpcMessage),
		notifs:  make(chan jsonrpcMessage, 256),
		closed:  make(chan struct{}),
		auth:    auth,
	}
	conn.SetReadLimit(codexTuiUDSReadLimit)
	go client.readLoop()
	return client
}

func (c *tuiUDSConn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		err = c.conn.Close()
		c.closeErr = err
		close(c.closed)
	})
	return err
}

func (c *tuiUDSConn) loginWithAccount(ctx context.Context) error {
	if c.auth == nil || strings.TrimSpace(c.auth.AccessToken) == "" {
		return nil
	}
	params := map[string]any{
		"type":             "chatgptAuthTokens",
		"accessToken":      c.auth.AccessToken,
		"chatgptAccountId": c.auth.AccountID,
	}
	if strings.TrimSpace(c.auth.PlanType) != "" {
		params["chatgptPlanType"] = c.auth.PlanType
	}
	if _, err := c.call(ctx, "account/login/start", params); err != nil {
		return fmt.Errorf("tui account/login/start: %w", err)
	}
	return nil
}

func (c *tuiUDSConn) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := uuid.NewString()
	idRaw, _ := json.Marshal(id)
	ch := make(chan jsonrpcMessage, 1)
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()
	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
	}()
	msg := jsonrpcMessage{ID: idRaw, Method: method}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		msg.Params = raw
	}
	if err := c.write(msg); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.closed:
		if c.closeErr != nil {
			return nil, c.closeErr
		}
		return nil, io.EOF
	case resp := <-ch:
		if resp.Error != nil {
			return nil, fmt.Errorf("%s: %s", method, resp.Error.Message)
		}
		return resp.Result, nil
	}
}

func (c *tuiUDSConn) notify(method string, params any) error {
	msg := jsonrpcMessage{Method: method}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return err
		}
		msg.Params = raw
	}
	return c.write(msg)
}

func (c *tuiUDSConn) waitTurnCompleted(ctx context.Context, threadID, turnID string) (string, string, error) {
	var b strings.Builder
	gotDelta := false
	for {
		select {
		case <-ctx.Done():
			return b.String(), turnID, ctx.Err()
		case <-c.closed:
			if b.Len() > 0 {
				return b.String(), turnID, nil
			}
			if c.closeErr != nil {
				return "", turnID, c.closeErr
			}
			return "", turnID, io.EOF
		case msg := <-c.notifs:
			switch msg.Method {
			case "item/agentMessage/delta":
				delta := gjson.GetBytes(msg.Params, "delta").String()
				if delta != "" {
					b.WriteString(delta)
					gotDelta = true
				}
			case "item/completed":
				itemType := gjson.GetBytes(msg.Params, "item.type").String()
				if !gotDelta && (itemType == "agentMessage" || itemType == "agent_message") {
					if text := strings.TrimSpace(firstNonEmpty(
						gjson.GetBytes(msg.Params, "item.text").String(),
						gjson.GetBytes(msg.Params, "item.content.0.text").String(),
					)); text != "" {
						b.WriteString(text)
					}
				}
			case "turn/completed":
				completedTurn := strings.TrimSpace(gjson.GetBytes(msg.Params, "turn.id").String())
				if turnID == "" || completedTurn == "" || completedTurn == turnID {
					if completedTurn != "" {
						turnID = completedTurn
					}
					if errMsg := strings.TrimSpace(firstNonEmpty(
						gjson.GetBytes(msg.Params, "turn.error.message").String(),
						gjson.GetBytes(msg.Params, "error.message").String(),
					)); errMsg != "" && b.Len() == 0 {
						return "", turnID, fmt.Errorf("tui turn failed: %s", errMsg)
					}
					if !gotDelta {
						gjson.GetBytes(msg.Params, "turn.items").ForEach(func(_, item gjson.Result) bool {
							if item.Get("type").String() == "agentMessage" {
								if text := strings.TrimSpace(item.Get("text").String()); text != "" {
									b.WriteString(text)
								}
							}
							return true
						})
					}
					return b.String(), turnID, nil
				}
			}
		}
	}
}

func (c *tuiUDSConn) write(msg jsonrpcMessage) error {
	payload, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.conn.WriteMessage(websocket.TextMessage, payload)
}

func (c *tuiUDSConn) readLoop() {
	defer c.Close()
	for {
		_, payload, err := c.conn.ReadMessage()
		if err != nil {
			c.closeErr = err
			return
		}
		var msg jsonrpcMessage
		if err := json.Unmarshal(payload, &msg); err != nil {
			continue
		}
		id := jsonrpcIDKey(msg.ID)
		if msg.Method != "" && id != "" {
			c.handleServerRequest(msg)
			continue
		}
		if msg.Method != "" {
			select {
			case c.notifs <- msg:
			default:
			}
			continue
		}
		if id == "" {
			continue
		}
		c.pendingMu.Lock()
		ch := c.pending[id]
		c.pendingMu.Unlock()
		if ch == nil {
			continue
		}
		select {
		case ch <- msg:
		default:
		}
	}
}

func (c *tuiUDSConn) handleServerRequest(msg jsonrpcMessage) {
	resp := jsonrpcMessage{ID: msg.ID}
	switch msg.Method {
	case "account/chatgptAuthTokens/refresh":
		if c.auth == nil || strings.TrimSpace(c.auth.AccessToken) == "" {
			resp.Error = &jsonrpcError{Code: -32000, Message: "no chatgpt auth tokens available"}
			break
		}
		raw, _ := json.Marshal(map[string]any{
			"accessToken":      c.auth.AccessToken,
			"chatgptAccountId": c.auth.AccountID,
			"chatgptPlanType":  c.auth.PlanType,
		})
		resp.Result = raw
	default:
		if strings.Contains(strings.ToLower(msg.Method), "approval") {
			raw, _ := json.Marshal(map[string]any{"decision": "accept"})
			resp.Result = raw
			break
		}
		resp.Error = &jsonrpcError{Code: -32601, Message: "unsupported remote app-server request " + msg.Method}
	}
	_ = c.write(resp)
}

func jsonrpcIDKey(id json.RawMessage) string {
	if len(id) == 0 || string(id) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(id, &s); err == nil {
		return s
	}
	return strings.TrimSpace(string(id))
}

func dialCodexTuiUDS(ctx context.Context, socketPath string) (*websocket.Conn, error) {
	socketPath = strings.TrimSpace(socketPath)
	if socketPath == "" {
		return nil, fmt.Errorf("codex tui unix socket path is empty")
	}
	dialer := websocket.Dialer{
		HandshakeTimeout: codexTuiUDSDialTimeout,
		NetDialContext: func(dialCtx context.Context, _, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: codexTuiUDSDialTimeout}
			return d.DialContext(dialCtx, "unix", socketPath)
		},
	}
	conn, _, err := dialer.DialContext(ctx, codexTuiUDSHandshakeURL, http.Header{
		"Originator": []string{"codex_cli_rs"},
	})
	if err != nil {
		return nil, fmt.Errorf("dial codex tui unix socket %s: %w", socketPath, err)
	}
	conn.SetReadLimit(codexTuiUDSReadLimit)
	return conn, nil
}

func lookupCodexBinary() string {
	if path, err := exec.LookPath("codex"); err == nil && strings.TrimSpace(path) != "" {
		return path
	}
	if path, err := exec.LookPath("codex.exe"); err == nil && strings.TrimSpace(path) != "" {
		return path
	}
	home, err := os.UserHomeDir()
	if err == nil {
		candidate := filepath.Join(home, "AppData", "Local", "OpenAI", "Codex", "bin", "codex.exe")
		if _, statErr := os.Stat(candidate); statErr == nil {
			return candidate
		}
	}
	return "codex"
}

func udsSocketAlive(path string) bool {
	if _, err := os.Stat(path); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	conn, err := dialCodexTuiUDS(ctx, path)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func sameFilePath(a, b string) bool {
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return strings.EqualFold(absA, absB)
}

func responsesBodyToTuiInput(rawBody []byte) []map[string]any {
	if prompt := strings.TrimSpace(gjson.GetBytes(rawBody, "prompt").String()); prompt != "" && !gjson.GetBytes(rawBody, "input").Exists() && !gjson.GetBytes(rawBody, "messages").Exists() {
		return []map[string]any{{"type": "text", "text": prompt}}
	}
	var out []map[string]any
	input := gjson.GetBytes(rawBody, "input")
	if input.Type == gjson.String {
		if text := strings.TrimSpace(input.String()); text != "" {
			out = append(out, map[string]any{"type": "text", "text": text})
		}
	} else if input.IsArray() {
		input.ForEach(func(_, item gjson.Result) bool {
			out = append(out, tuiInputFromResponsesItem(item)...)
			return true
		})
	}
	if len(out) == 0 {
		messages := gjson.GetBytes(rawBody, "messages")
		if messages.IsArray() {
			messages.ForEach(func(_, item gjson.Result) bool {
				out = append(out, tuiInputFromResponsesItem(item)...)
				return true
			})
		}
	}
	return out
}

func tuiInputFromResponsesItem(item gjson.Result) []map[string]any {
	if item.Type == gjson.String {
		text := strings.TrimSpace(item.String())
		if text == "" {
			return nil
		}
		return []map[string]any{{"type": "text", "text": text}}
	}
	if !item.IsObject() {
		return nil
	}
	var out []map[string]any
	if text := strings.TrimSpace(item.Get("text").String()); text != "" && !item.Get("content").Exists() {
		out = append(out, map[string]any{"type": "text", "text": text})
	}
	content := item.Get("content")
	if content.Type == gjson.String {
		if text := strings.TrimSpace(content.String()); text != "" {
			out = append(out, map[string]any{"type": "text", "text": text})
		}
	}
	if content.IsArray() {
		content.ForEach(func(_, part gjson.Result) bool {
			if text := strings.TrimSpace(part.Get("text").String()); text != "" {
				out = append(out, map[string]any{"type": "text", "text": text})
			}
			url := strings.TrimSpace(firstNonEmpty(part.Get("image_url.url").String(), part.Get("image_url").String(), part.Get("url").String()))
			if url != "" && (part.Get("type").String() == "input_image" || part.Get("type").String() == "image_url" || part.Get("image_url").Exists()) {
				out = append(out, map[string]any{"type": "image", "url": url})
			}
			return true
		})
	}
	if url := strings.TrimSpace(firstNonEmpty(item.Get("image_url.url").String(), item.Get("url").String())); url != "" && len(out) == 0 {
		out = append(out, map[string]any{"type": "image", "url": url})
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func writeCodexTuiUDSResponseStream(c *gin.Context, responseID, model string, result *tuiUDSTurnResult) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Status(http.StatusOK)
	flusher, _ := c.Writer.(http.Flusher)
	write := func(event string, payload any) {
		body, _ := json.Marshal(payload)
		fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", event, body)
		if flusher != nil {
			flusher.Flush()
		}
	}
	write("response.created", map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id":     responseID,
			"object": "response",
			"status": "in_progress",
			"model":  model,
		},
	})
	if result != nil && result.Text != "" {
		write("response.output_text.delta", map[string]any{
			"type":          "response.output_text.delta",
			"delta":         result.Text,
			"output_index":  0,
			"content_index": 0,
		})
	}
	write("response.completed", map[string]any{
		"type":     "response.completed",
		"response": buildCodexTuiUDSResponseJSON(responseID, model, result, "completed"),
	})
}

func buildCodexTuiUDSResponseJSON(responseID, model string, result *tuiUDSTurnResult, status string) map[string]any {
	text := ""
	if result != nil {
		text = result.Text
	}
	return map[string]any{
		"id":     responseID,
		"object": "response",
		"status": status,
		"model":  model,
		"output": []map[string]any{
			{
				"type": "message",
				"role": "assistant",
				"content": []map[string]any{
					{"type": "output_text", "text": text},
				},
			},
		},
		"output_text": text,
	}
}

func (h *Handler) tuiUDSAuthFromRequest(c *gin.Context) *tuiUDSAuth {
	_, auth := h.tuiUDSAccountAndAuth(c)
	return auth
}

func (h *Handler) tuiUDSAccountAndAuth(c *gin.Context) (*auth.Account, *tuiUDSAuth) {
	if h == nil || h.store == nil || c == nil {
		return nil, nil
	}
	account, _ := h.nextAccountForSession("", requestAPIKeyID(c), nil)
	if account == nil {
		return nil, nil
	}
	token := strings.TrimSpace(account.GetAccessToken())
	if token == "" {
		return account, nil
	}
	return account, &tuiUDSAuth{
		AccessToken: token,
		AccountID:   account.EffectiveAccountID(),
		PlanType:    account.GetPlanType(),
	}
}

func inboundEndpointFromRequest(c *gin.Context, fallback string) string {
	if c == nil || c.Request == nil {
		return fallback
	}
	path := strings.TrimSpace(c.Request.URL.Path)
	if path == "" {
		return fallback
	}
	return path
}

func estimateTuiUDSTokens(input []map[string]any, text string) (prompt, completion int) {
	raw, _ := json.Marshal(input)
	prompt = (len(raw) + 3) / 4
	if prompt < 1 {
		prompt = 1
	}
	completion = (len(text) + 3) / 4
	if text != "" && completion < 1 {
		completion = 1
	}
	return prompt, completion
}

func (h *Handler) logCodexTuiUDSUsage(c *gin.Context, account *auth.Account, endpoint, model string, start time.Time, status int, input []map[string]any, result *tuiUDSTurnResult, turnErr error, stream, viaWS bool) {
	if h == nil {
		return
	}
	if endpoint == "" {
		endpoint = "/v1/responses"
	}
	durationMs := int(time.Since(start).Milliseconds())
	text := ""
	if result != nil {
		text = result.Text
	}
	promptTokens, completionTokens := estimateTuiUDSTokens(input, text)
	logInput := &database.UsageLogInput{
		Endpoint:         endpoint,
		Model:            model,
		EffectiveModel:   model,
		StatusCode:       status,
		DurationMs:       durationMs,
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      promptTokens + completionTokens,
		InputTokens:      promptTokens,
		OutputTokens:     completionTokens,
		InboundEndpoint:  endpoint,
		UpstreamEndpoint: "codex-tui-uds",
		Stream:           stream,
		ViaWebsocket:     viaWS,
		AttemptIndex:     1,
		Channel:          database.UpstreamChannelCodex,
	}
	if account != nil {
		logInput.AccountID = account.ID()
	}
	if turnErr != nil {
		logInput.UpstreamErrorKind = "tui_uds"
		logInput.ErrorMessage = turnErr.Error()
	}
	h.logUsageForRequest(c, logInput)
	if h.db != nil {
		h.db.FlushUsageLogs()
	}
	if account != nil && h.store != nil && turnErr == nil && status == http.StatusOK {
		h.store.ReportRequestSuccess(account, time.Duration(durationMs)*time.Millisecond)
	}
	log.Printf("Codex TUI UDS %s status=%d account=%d model=%s duration_ms=%d tokens=%d/%d", endpoint, status, logInput.AccountID, model, durationMs, promptTokens, completionTokens)
}

func (h *Handler) executeResponsesWSViaCodexTuiUDS(c *gin.Context, conn *websocket.Conn, rawBody []byte, model string) error {
	if h == nil || h.cfg == nil {
		apiErr := api.NewAPIError(api.ErrCodeServiceUnavailable, "Codex TUI UDS is not configured", api.ErrorTypeServer)
		_ = writeResponsesWSError(conn, apiErr)
		return newResponsesWSCloseError(websocket.CloseTryAgainLater, apiErr.Message, apiErr)
	}
	backend := strings.TrimSpace(h.cfg.CodexTuiUDSBackend)
	if backend == "" {
		apiErr := api.NewAPIError(api.ErrCodeServiceUnavailable, "CODEX_TUI_UDS_BACKEND is empty", api.ErrorTypeServer)
		_ = writeResponsesWSError(conn, apiErr)
		return newResponsesWSCloseError(websocket.CloseTryAgainLater, apiErr.Message, apiErr)
	}
	input := responsesBodyToTuiInput(rawBody)
	if len(input) == 0 {
		apiErr := api.NewAPIError(api.ErrCodeMissingField, "input is required", api.ErrorTypeInvalidRequest)
		_ = writeResponsesWSError(conn, apiErr)
		return newResponsesWSCloseError(websocket.ClosePolicyViolation, apiErr.Message, apiErr)
	}
	account, tuiAuth := h.tuiUDSAccountAndAuth(c)
	if account != nil && h.store != nil {
		defer h.store.Release(account)
	}
	endpoint := inboundEndpointFromRequest(c, "/v1/responses")
	start := time.Now()
	result, err := runCodexTuiUDSTurn(c.Request.Context(), backend, model, input, tuiAuth)
	if err != nil {
		h.logCodexTuiUDSUsage(c, account, endpoint, model, start, http.StatusBadGateway, input, nil, err, true, true)
		apiErr := api.NewAPIError(api.ErrCodeUpstreamError, err.Error(), api.ErrorTypeUpstream)
		_ = writeResponsesWSError(conn, apiErr)
		return newResponsesWSCloseError(websocket.CloseTryAgainLater, apiErr.Message, apiErr)
	}
	h.logCodexTuiUDSUsage(c, account, endpoint, model, start, http.StatusOK, input, result, nil, true, true)
	responseID := "resp_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	events := []any{
		map[string]any{
			"type":     "response.created",
			"response": map[string]any{"id": responseID, "object": "response", "status": "in_progress", "model": model},
		},
	}
	if result != nil && result.Text != "" {
		events = append(events, map[string]any{
			"type":          "response.output_text.delta",
			"delta":         result.Text,
			"output_index":  0,
			"content_index": 0,
		})
	}
	events = append(events, map[string]any{
		"type":     "response.completed",
		"response": buildCodexTuiUDSResponseJSON(responseID, model, result, "completed"),
	})
	for _, event := range events {
		payload, marshalErr := json.Marshal(event)
		if marshalErr != nil {
			return marshalErr
		}
		if writeErr := writeResponsesWSMessage(conn, payload); writeErr != nil {
			return writeErr
		}
	}
	return nil
}

func (h *Handler) executeChatCompletionsViaCodexTuiUDS(c *gin.Context, rawBody []byte, model string, isStream bool) {
	if h == nil || h.cfg == nil {
		api.SendError(c, api.NewAPIError(api.ErrCodeServiceUnavailable, "Codex TUI UDS is not configured", api.ErrorTypeServer))
		return
	}
	backend := strings.TrimSpace(h.cfg.CodexTuiUDSBackend)
	if backend == "" {
		api.SendError(c, api.NewAPIError(api.ErrCodeServiceUnavailable, "CODEX_TUI_UDS_BACKEND is empty", api.ErrorTypeServer))
		return
	}
	input := responsesBodyToTuiInput(rawBody)
	if len(input) == 0 {
		api.SendError(c, api.NewAPIError(api.ErrCodeMissingField, "messages is required", api.ErrorTypeInvalidRequest))
		return
	}
	account, tuiAuth := h.tuiUDSAccountAndAuth(c)
	if account != nil && h.store != nil {
		defer h.store.Release(account)
	}
	endpoint := inboundEndpointFromRequest(c, "/v1/chat/completions")
	start := time.Now()
	result, err := runCodexTuiUDSTurn(c.Request.Context(), backend, model, input, tuiAuth)
	if err != nil {
		h.logCodexTuiUDSUsage(c, account, endpoint, model, start, http.StatusBadGateway, input, nil, err, isStream, false)
		api.SendError(c, api.NewAPIError(api.ErrCodeUpstreamError, err.Error(), api.ErrorTypeUpstream))
		return
	}
	h.logCodexTuiUDSUsage(c, account, endpoint, model, start, http.StatusOK, input, result, nil, isStream, false)
	text := ""
	if result != nil {
		text = result.Text
	}
	completionID := "chatcmpl_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if isStream {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Status(http.StatusOK)
		flusher, _ := c.Writer.(http.Flusher)
		write := func(payload any) {
			body, _ := json.Marshal(payload)
			fmt.Fprintf(c.Writer, "data: %s\n\n", body)
			if flusher != nil {
				flusher.Flush()
			}
		}
		write(map[string]any{
			"id":     completionID,
			"object": "chat.completion.chunk",
			"model":  model,
			"choices": []map[string]any{{
				"index": 0,
				"delta": map[string]any{"role": "assistant", "content": text},
			}},
		})
		write(map[string]any{
			"id":     completionID,
			"object": "chat.completion.chunk",
			"model":  model,
			"choices": []map[string]any{{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": "stop",
			}},
		})
		fmt.Fprintf(c.Writer, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	c.JSON(http.StatusOK, map[string]any{
		"id":     completionID,
		"object": "chat.completion",
		"model":  model,
		"choices": []map[string]any{{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": text,
			},
			"finish_reason": "stop",
		}},
	})
}
