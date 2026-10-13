package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Every account answering the plan-gated 400 must surface that reason, not a
// bare no_available_account, whether or not the cooldown is enabled (#785).
func TestChatCompletionsPassesThroughUnsupportedModelAfterFailover(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		name := "cooldown enabled"
		if disabled {
			name = "cooldown disabled"
		}
		t.Run(name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			previousResin := resinCfg.Load()
			t.Cleanup(func() { resinCfg.Store(previousResin) })

			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"message":"The 'gpt-5.5' model is not supported when using Codex with a ChatGPT account.","type":"invalid_request_error"}}`)
			}))
			t.Cleanup(upstream.Close)
			SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "test"})

			store := auth.NewStore(nil, nil, &database.SystemSettings{
				MaxConcurrency: 1, TestConcurrency: 1, TestModel: "gpt-5.5", MaxRetries: 2,
				OAuthUnsupportedModelCooldownDisabled: disabled,
			})
			t.Cleanup(store.Stop)
			accounts := []*auth.Account{
				{DBID: 1, AccessToken: "at-1", PlanType: "free", AccountID: "acct-1"},
				{DBID: 2, AccessToken: "at-2", PlanType: "free", AccountID: "acct-2"},
			}
			for _, account := range accounts {
				store.AddAccount(account)
			}
			handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)

			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hello"}]}`))
			ctx.Request.Header.Set("Content-Type", "application/json")
			handler.ChatCompletions(ctx)

			body := recorder.Body.String()
			if got := calls.Load(); got != 2 {
				t.Fatalf("upstream calls = %d, want one per account; body=%s", got, body)
			}
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", recorder.Code, body)
			}
			if message := gjson.Get(body, "error.message").String(); !strings.Contains(message, "not supported when using Codex") {
				t.Fatalf("message = %q, want the upstream reason", message)
			}
			for _, account := range accounts {
				if got := account.IsModelRateLimited("gpt-5.5"); got == disabled {
					t.Fatalf("account %d cooled=%v with cooldown disabled=%v", account.DBID, got, disabled)
				}
			}
		})
	}
}
