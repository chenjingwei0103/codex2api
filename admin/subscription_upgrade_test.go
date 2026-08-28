package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

const subscriptionUpgradeTestWorkspaceID = "288c5d93-a113-4ed3-b6a9-08b6a4d35417"

type fakeSubscriptionUpgradeUpstream struct {
	mu           sync.Mutex
	readResult   *proxy.ChatGPTSubscription
	readErr      error
	readResults  []*proxy.ChatGPTSubscription
	readErrors   []error
	readCount    int
	quoteResult  *proxy.SubscriptionUpgradeQuote
	submitResult *proxy.SubscriptionUpgradeSubmitResult
	submitErr    error
	submitCount  int
	submitDelay  time.Duration
	readCalls    int
	// submitHook 在付费 POST 期间触发，用于模拟「付费途中下游断连」。
	submitHook func()
	// onRead 在每次只读调用后于锁外触发，供并发用例控制交错顺序。
	onRead func(call int)
}

func (f *fakeSubscriptionUpgradeUpstream) paidSubmissionCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.submitCount
}

func (f *fakeSubscriptionUpgradeUpstream) Read(context.Context, proxy.SubscriptionUpgradeCredentials) (*proxy.ChatGPTSubscription, error) {
	f.mu.Lock()
	f.readCalls++
	call, hook := f.readCalls, f.onRead
	var result *proxy.ChatGPTSubscription
	var err error
	if f.readCount < len(f.readResults) || f.readCount < len(f.readErrors) {
		index := f.readCount
		f.readCount++
		if index < len(f.readResults) {
			result = f.readResults[index]
		}
		if index < len(f.readErrors) {
			err = f.readErrors[index]
		}
	} else {
		result, err = f.readResult, f.readErr
	}
	f.mu.Unlock()
	// 钩子在锁外触发，供并发用例控制交错顺序。
	if hook != nil {
		hook(call)
	}
	return result, err
}

func (f *fakeSubscriptionUpgradeUpstream) Quote(context.Context, proxy.SubscriptionUpgradeCredentials, string, string) (*proxy.SubscriptionUpgradeQuote, error) {
	return f.quoteResult, nil
}

func TestSubscriptionUpgradeTokenInvalidationRequiresReauthenticationWithoutSecondPaidPost(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "codex2api.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2})
	account := &auth.Account{DBID: 20, AccessToken: "test-at", AccountID: subscriptionUpgradeTestWorkspaceID, PlanType: "prolite"}
	store.AddAccount(account)
	prolite := &proxy.ChatGPTSubscription{PlanType: "prolite", BillingCurrency: "PHP"}
	fake := &fakeSubscriptionUpgradeUpstream{
		readResults: []*proxy.ChatGPTSubscription{prolite, prolite, nil},
		readErrors:  []error{nil, nil, &proxy.SubscriptionUpstreamHTTPError{StatusCode: http.StatusUnauthorized, Body: "invalidated"}},
		quoteResult: &proxy.SubscriptionUpgradeQuote{
			Currency: "PHP", AmountDueMinor: 345196, RecurringAmountMinor: 999000,
		},
		submitResult: &proxy.SubscriptionUpgradeSubmitResult{Status: "succeeded"},
	}
	handler := &Handler{
		store:                     store,
		db:                        db,
		subscriptionUpgradeQuotes: make(map[string]subscriptionUpgradeQuoteRecord),
		subscriptionUpgradeClientFactory: func(*auth.Account, string) subscriptionUpgradeUpstream {
			return fake
		},
	}
	handler.setSubscriptionUpgradeEnabled(true)

	quoteRecorder := httptest.NewRecorder()
	quoteContext, _ := gin.CreateTestContext(quoteRecorder)
	quoteContext.Params = gin.Params{{Key: "id", Value: "20"}}
	quoteContext.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/20/subscription/upgrade-quotes", strings.NewReader(`{"target_plan":"chatgptpro","currency":"PHP"}`))
	handler.CreateSubscriptionUpgradeQuote(quoteContext)
	if strings.Contains(quoteRecorder.Body.String(), `"silent_reauthorization_available":true`) {
		t.Fatal("quote must not claim silent reauthorization without a Web Session")
	}
	var quoteResponse struct {
		QuoteID string `json:"quote_id"`
	}
	if err := json.Unmarshal(quoteRecorder.Body.Bytes(), &quoteResponse); err != nil || quoteResponse.QuoteID == "" {
		t.Fatalf("quote response = %s, err=%v", quoteRecorder.Body.String(), err)
	}

	body := `{"quote_id":"` + quoteResponse.QuoteID + `","currency":"PHP","max_amount_minor":350000,"confirmed":true}`
	invoke := func() *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Params = gin.Params{{Key: "id", Value: "20"}}
		ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/20/subscription/upgrades", strings.NewReader(body))
		ctx.Request.Header.Set("Idempotency-Key", "upgrade-once")
		handler.CreateSubscriptionUpgrade(ctx)
		return recorder
	}
	first := invoke()
	if first.Code != http.StatusAccepted || !strings.Contains(first.Body.String(), "verification_requires_reauthentication") {
		t.Fatalf("first response status=%d body=%s", first.Code, first.Body.String())
	}
	second := invoke()
	if second.Code != http.StatusOK || !strings.Contains(second.Body.String(), "verification_requires_reauthentication") {
		t.Fatalf("idempotent response status=%d body=%s", second.Code, second.Body.String())
	}
	if fake.paidSubmissionCount() != 1 {
		t.Fatalf("paid POST count = %d, want exactly one", fake.paidSubmissionCount())
	}
}

func TestSubscriptionUpgradeUsesStoredWebSessionOnlyForReadOnlyRecovery(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "codex2api.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2})
	account := &auth.Account{
		DBID: 20, AccessToken: "old-at", SessionToken: "independent-web-session",
		AccountID: subscriptionUpgradeTestWorkspaceID, PlanType: "prolite",
	}
	store.AddAccount(account)
	prolite := &proxy.ChatGPTSubscription{PlanType: "prolite", BillingCurrency: "PHP"}
	fake := &fakeSubscriptionUpgradeUpstream{
		readResults:  []*proxy.ChatGPTSubscription{prolite, prolite, nil, {PlanType: "pro"}},
		readErrors:   []error{nil, nil, &proxy.SubscriptionUpstreamHTTPError{StatusCode: http.StatusUnauthorized}, nil},
		quoteResult:  &proxy.SubscriptionUpgradeQuote{Currency: "PHP", AmountDueMinor: 345196, RecurringAmountMinor: 999000},
		submitResult: &proxy.SubscriptionUpgradeSubmitResult{Status: "succeeded"},
	}
	refreshCount := 0
	handler := &Handler{
		store:                     store,
		db:                        db,
		subscriptionUpgradeQuotes: make(map[string]subscriptionUpgradeQuoteRecord),
		subscriptionUpgradeClientFactory: func(*auth.Account, string) subscriptionUpgradeUpstream {
			return fake
		},
		refreshAccount: func(context.Context, int64) error {
			refreshCount++
			account.AccessToken = "new-at"
			return nil
		},
	}
	handler.setSubscriptionUpgradeEnabled(true)

	quoteRecorder := httptest.NewRecorder()
	quoteContext, _ := gin.CreateTestContext(quoteRecorder)
	quoteContext.Params = gin.Params{{Key: "id", Value: "20"}}
	quoteContext.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/20/subscription/upgrade-quotes", strings.NewReader(`{"target_plan":"chatgptpro","currency":"PHP"}`))
	handler.CreateSubscriptionUpgradeQuote(quoteContext)
	if !strings.Contains(quoteRecorder.Body.String(), `"silent_reauthorization_available":true`) {
		t.Fatalf("quote does not report stored Web Session readiness: %s", quoteRecorder.Body.String())
	}
	var quoteResponse struct {
		QuoteID string `json:"quote_id"`
	}
	_ = json.Unmarshal(quoteRecorder.Body.Bytes(), &quoteResponse)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: "20"}}
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/20/subscription/upgrades", strings.NewReader(`{"quote_id":"`+quoteResponse.QuoteID+`","currency":"PHP","max_amount_minor":350000,"confirmed":true}`))
	ctx.Request.Header.Set("Idempotency-Key", "upgrade-with-web-session")
	handler.CreateSubscriptionUpgrade(ctx)

	if recorder.Code != http.StatusAccepted || !strings.Contains(recorder.Body.String(), `"status":"succeeded"`) {
		t.Fatalf("response status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if refreshCount != 1 || fake.paidSubmissionCount() != 1 {
		t.Fatalf("refresh count=%d submit count=%d, want 1 and 1", refreshCount, fake.paidSubmissionCount())
	}
}

func TestSubscriptionUpgradeFeatureIsDisabledByDefault(t *testing.T) {
	t.Setenv("CODEX2API_SUBSCRIPTION_UPGRADES_ENABLED", "")
	if subscriptionUpgradeFeatureEnabled() {
		t.Fatal("subscription upgrade feature must default to disabled")
	}
}

func (f *fakeSubscriptionUpgradeUpstream) Submit(context.Context, proxy.SubscriptionUpgradeCredentials, proxy.SubscriptionUpgradeSubmission) (*proxy.SubscriptionUpgradeSubmitResult, error) {
	f.mu.Lock()
	f.submitCount++
	hook, delay, result, err := f.submitHook, f.submitDelay, f.submitResult, f.submitErr
	f.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	if hook != nil {
		hook()
	}
	return result, err
}

func TestSubscriptionUpgradeRejectsFreshQuoteAboveConfirmedCapWithoutSubmitting(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "codex2api.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2})
	account := &auth.Account{DBID: 20, AccessToken: "test-at", AccountID: subscriptionUpgradeTestWorkspaceID, PlanType: "prolite"}
	store.AddAccount(account)
	fake := &fakeSubscriptionUpgradeUpstream{
		readResult: &proxy.ChatGPTSubscription{PlanType: "prolite", BillingCurrency: "PHP"},
		quoteResult: &proxy.SubscriptionUpgradeQuote{
			Currency: "PHP", AmountDueMinor: 345196, RecurringAmountMinor: 999000,
		},
	}
	handler := &Handler{
		store:                     store,
		db:                        db,
		subscriptionUpgradeQuotes: make(map[string]subscriptionUpgradeQuoteRecord),
		subscriptionUpgradeClientFactory: func(*auth.Account, string) subscriptionUpgradeUpstream {
			return fake
		},
	}
	handler.setSubscriptionUpgradeEnabled(true)

	quoteRecorder := httptest.NewRecorder()
	quoteContext, _ := gin.CreateTestContext(quoteRecorder)
	quoteContext.Params = gin.Params{{Key: "id", Value: "20"}}
	quoteContext.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/20/subscription/upgrade-quotes", strings.NewReader(`{"target_plan":"chatgptpro","currency":"PHP"}`))
	handler.CreateSubscriptionUpgradeQuote(quoteContext)
	if quoteRecorder.Code != http.StatusOK {
		t.Fatalf("quote status = %d, body=%s", quoteRecorder.Code, quoteRecorder.Body.String())
	}
	var quoteResponse struct {
		QuoteID string `json:"quote_id"`
	}
	if err := json.Unmarshal(quoteRecorder.Body.Bytes(), &quoteResponse); err != nil || quoteResponse.QuoteID == "" {
		t.Fatalf("decode quote response: %v, body=%s", err, quoteRecorder.Body.String())
	}

	upgradeRecorder := httptest.NewRecorder()
	upgradeContext, _ := gin.CreateTestContext(upgradeRecorder)
	upgradeContext.Params = gin.Params{{Key: "id", Value: "20"}}
	upgradeContext.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/20/subscription/upgrades", strings.NewReader(`{
		"quote_id":"`+quoteResponse.QuoteID+`",
		"currency":"PHP",
		"max_amount_minor":300000,
		"confirmed":true
	}`))
	upgradeContext.Request.Header.Set("Idempotency-Key", "upgrade-once")
	handler.CreateSubscriptionUpgrade(upgradeContext)

	if upgradeRecorder.Code != http.StatusConflict {
		t.Fatalf("upgrade status = %d, body=%s", upgradeRecorder.Code, upgradeRecorder.Body.String())
	}
	if fake.paidSubmissionCount() != 0 {
		t.Fatalf("submit count = %d, want 0", fake.paidSubmissionCount())
	}
}

// 付费开关必须任何时候都能从管理后台关掉：数据库里存过的显式值是唯一权威，
// 环境变量只在管理员从未设置过时提供初值。
func TestSubscriptionUpgradeGatePrefersStoredSettingOverEnv(t *testing.T) {
	newHandlerWithDB := func(t *testing.T) (*Handler, *database.DB) {
		t.Helper()
		db, err := database.New("sqlite", filepath.Join(t.TempDir(), "codex2api.db"))
		if err != nil {
			t.Fatalf("database.New: %v", err)
		}
		t.Cleanup(func() { db.Close() })
		return &Handler{db: db}, db
	}

	t.Run("env default applies when never configured", func(t *testing.T) {
		t.Setenv("CODEX2API_SUBSCRIPTION_UPGRADES_ENABLED", "true")
		handler, _ := newHandlerWithDB(t)
		handler.initSubscriptionUpgradeGate()
		if !handler.subscriptionUpgradesEnabled() {
			t.Fatal("gate should fall back to the env default when unset in the database")
		}
	})

	t.Run("stored false overrides env true", func(t *testing.T) {
		t.Setenv("CODEX2API_SUBSCRIPTION_UPGRADES_ENABLED", "true")
		handler, db := newHandlerWithDB(t)
		if err := db.SaveSubscriptionUpgradesEnabled(context.Background(), false); err != nil {
			t.Fatalf("SaveSubscriptionUpgradesEnabled: %v", err)
		}
		handler.initSubscriptionUpgradeGate()
		if handler.subscriptionUpgradesEnabled() {
			t.Fatal("an explicitly disabled paid feature must not be re-enabled by the environment")
		}
	})

	t.Run("stored true applies without env", func(t *testing.T) {
		t.Setenv("CODEX2API_SUBSCRIPTION_UPGRADES_ENABLED", "")
		handler, db := newHandlerWithDB(t)
		if err := db.SaveSubscriptionUpgradesEnabled(context.Background(), true); err != nil {
			t.Fatalf("SaveSubscriptionUpgradesEnabled: %v", err)
		}
		handler.initSubscriptionUpgradeGate()
		if !handler.subscriptionUpgradesEnabled() {
			t.Fatal("gate should honor the stored enabled setting")
		}
	})
}

// newSubscriptionUpgradeTestHandler 组装一个已开启开关、带真实 SQLite 日志的处理器。
func newSubscriptionUpgradeTestHandler(t *testing.T, account *auth.Account, fake *fakeSubscriptionUpgradeUpstream) (*Handler, *database.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "codex2api.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2})
	store.AddAccount(account)
	handler := &Handler{
		store:                     store,
		db:                        db,
		subscriptionUpgradeQuotes: make(map[string]subscriptionUpgradeQuoteRecord),
		subscriptionUpgradeClientFactory: func(*auth.Account, string) subscriptionUpgradeUpstream {
			return fake
		},
	}
	handler.setSubscriptionUpgradeEnabled(true)
	return handler, db
}

func subscriptionUpgradeTestQuoteID(t *testing.T, handler *Handler) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: "20"}}
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/20/subscription/upgrade-quotes",
		strings.NewReader(`{"target_plan":"chatgptpro","currency":"PHP"}`))
	handler.CreateSubscriptionUpgradeQuote(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("quote status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		QuoteID string `json:"quote_id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || response.QuoteID == "" {
		t.Fatalf("decode quote: %v body=%s", err, recorder.Body.String())
	}
	return response.QuoteID
}

// 两个并发请求带不同幂等键、引用同一份报价时，只允许发出一次付费 POST。
// 修复前报价查验在账号锁之外，两者都能通过校验，幂等唯一约束也拦不住。
func TestSubscriptionUpgradeConcurrentRequestsSharingQuoteChargeOnce(t *testing.T) {
	account := &auth.Account{DBID: 20, AccessToken: "test-at", AccountID: subscriptionUpgradeTestWorkspaceID, PlanType: "prolite"}
	fake := &fakeSubscriptionUpgradeUpstream{
		readResult:   &proxy.ChatGPTSubscription{PlanType: "prolite", BillingCurrency: "PHP"},
		quoteResult:  &proxy.SubscriptionUpgradeQuote{Currency: "PHP", AmountDueMinor: 345196, RecurringAmountMinor: 999000},
		submitResult: &proxy.SubscriptionUpgradeSubmitResult{Status: "succeeded"},
	}
	// 第 1 次只读是建报价；第 2 次是 A 进入付费段后的复读。在那里挂住 A，让 B
	// 有充足时间走到「报价查验」这一步——竞态窗口正是在这里。
	firstRequestInPaidSection := make(chan struct{})
	fake.onRead = func(call int) {
		if call == 2 {
			close(firstRequestInPaidSection)
			time.Sleep(200 * time.Millisecond)
		}
	}
	handler, _ := newSubscriptionUpgradeTestHandler(t, account, fake)
	quoteID := subscriptionUpgradeTestQuoteID(t, handler)

	body := `{"quote_id":"` + quoteID + `","currency":"PHP","max_amount_minor":350000,"confirmed":true}`
	codes := make([]int, 2)
	invoke := func(index int, idempotencyKey string) {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Params = gin.Params{{Key: "id", Value: "20"}}
		ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/20/subscription/upgrades", strings.NewReader(body))
		ctx.Request.Header.Set("Idempotency-Key", idempotencyKey)
		handler.CreateSubscriptionUpgrade(ctx)
		codes[index] = recorder.Code
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		invoke(0, "concurrent-key-a")
	}()
	<-firstRequestInPaidSection
	wg.Add(1)
	go func() {
		defer wg.Done()
		invoke(1, "concurrent-key-b")
	}()
	wg.Wait()

	if got := fake.paidSubmissionCount(); got != 1 {
		t.Fatalf("paid POST count = %d, want exactly one (concurrent double charge)", got)
	}
	accepted, conflicted := 0, 0
	for _, code := range codes {
		switch code {
		case http.StatusAccepted:
			accepted++
		case http.StatusConflict:
			conflicted++
		}
	}
	if accepted != 1 || conflicted != 1 {
		t.Fatalf("responses = %v, want one 202 and one 409", codes)
	}
}

// 下游在付费途中断连时，ambiguous_transport 必须仍然落库。修复前收尾写入沿用
// 已取消的请求 context，最该留痕的记录反而丢失，操作永远停在 submitting。
func TestSubscriptionUpgradeJournalsAmbiguousTransportAfterClientDisconnect(t *testing.T) {
	account := &auth.Account{DBID: 20, AccessToken: "test-at", AccountID: subscriptionUpgradeTestWorkspaceID, PlanType: "prolite"}
	fake := &fakeSubscriptionUpgradeUpstream{
		readResult:  &proxy.ChatGPTSubscription{PlanType: "prolite", BillingCurrency: "PHP"},
		quoteResult: &proxy.SubscriptionUpgradeQuote{Currency: "PHP", AmountDueMinor: 345196, RecurringAmountMinor: 999000},
		submitErr:   errors.New("connection reset by peer"),
	}
	handler, db := newSubscriptionUpgradeTestHandler(t, account, fake)
	quoteID := subscriptionUpgradeTestQuoteID(t, handler)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: "20"}}
	request := httptest.NewRequest(http.MethodPost, "/api/admin/accounts/20/subscription/upgrades",
		strings.NewReader(`{"quote_id":"`+quoteID+`","currency":"PHP","max_amount_minor":350000,"confirmed":true}`))
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	ctx.Request = request.WithContext(requestCtx)
	ctx.Request.Header.Set("Idempotency-Key", "disconnect-mid-payment")
	// 模拟付费 POST 发出后下游立刻断连。
	fake.mu.Lock()
	fake.submitHook = cancelRequest
	fake.mu.Unlock()
	handler.CreateSubscriptionUpgrade(ctx)
	cancelRequest()

	operation, err := db.GetSubscriptionUpgradeOperationByIdempotencyHash(context.Background(), 20,
		subscriptionUpgradeIdempotencyHash("disconnect-mid-payment"))
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	if operation.Status != "ambiguous_transport" {
		t.Fatalf("status = %q, want ambiguous_transport (post-payment write lost)", operation.Status)
	}
}

// chatgpt.com 的 403 多是 Cloudflare 盾误伤，不能报成「付费已被接受且凭据被作废」。
func TestSubscriptionUpgradeForbiddenIsNotReportedAsCredentialInvalidation(t *testing.T) {
	account := &auth.Account{DBID: 20, AccessToken: "test-at", AccountID: subscriptionUpgradeTestWorkspaceID, PlanType: "prolite"}
	prolite := &proxy.ChatGPTSubscription{PlanType: "prolite", BillingCurrency: "PHP"}
	fake := &fakeSubscriptionUpgradeUpstream{
		readResults:  []*proxy.ChatGPTSubscription{prolite, prolite, nil},
		readErrors:   []error{nil, nil, &proxy.SubscriptionUpstreamHTTPError{StatusCode: http.StatusForbidden, Body: "cloudflare"}},
		quoteResult:  &proxy.SubscriptionUpgradeQuote{Currency: "PHP", AmountDueMinor: 345196, RecurringAmountMinor: 999000},
		submitResult: &proxy.SubscriptionUpgradeSubmitResult{Status: "succeeded"},
	}
	handler, _ := newSubscriptionUpgradeTestHandler(t, account, fake)
	quoteID := subscriptionUpgradeTestQuoteID(t, handler)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: "20"}}
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/20/subscription/upgrades",
		strings.NewReader(`{"quote_id":"`+quoteID+`","currency":"PHP","max_amount_minor":350000,"confirmed":true}`))
	ctx.Request.Header.Set("Idempotency-Key", "forbidden-probe")
	handler.CreateSubscriptionUpgrade(ctx)

	if strings.Contains(recorder.Body.String(), "verification_requires_reauthentication") {
		t.Fatalf("403 must not be reported as credential invalidation: %s", recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "verification_pending") {
		t.Fatalf("403 should leave the operation pending reconciliation: %s", recorder.Body.String())
	}
}

// 对账端点是只读出口：把 pending 推进到终态，且绝不重发付费 POST。
func TestSubscriptionUpgradeVerifyReconcilesWithoutRepeatingPayment(t *testing.T) {
	account := &auth.Account{DBID: 20, AccessToken: "test-at", AccountID: subscriptionUpgradeTestWorkspaceID, PlanType: "prolite"}
	fake := &fakeSubscriptionUpgradeUpstream{readResult: &proxy.ChatGPTSubscription{PlanType: "pro"}}
	handler, db := newSubscriptionUpgradeTestHandler(t, account, fake)

	operation := database.SubscriptionUpgradeOperation{
		ID: "operation-verify", AccountID: 20, IdempotencyKeyHash: "sha256:verify",
		SourcePlan: "prolite", TargetPlan: "chatgptpro", Currency: "PHP",
		AmountDueMinor: 345196, Status: "verification_pending",
	}
	if err := db.CreateSubscriptionUpgradeOperation(context.Background(), operation); err != nil {
		t.Fatalf("seed operation: %v", err)
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "operation_id", Value: "operation-verify"}}
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/subscription-upgrades/operation-verify/verify", nil)
	handler.VerifySubscriptionUpgradeOperation(ctx)

	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"status":"succeeded"`) {
		t.Fatalf("verify status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := fake.paidSubmissionCount(); got != 0 {
		t.Fatalf("reconciliation sent %d paid POSTs, want 0", got)
	}
}
