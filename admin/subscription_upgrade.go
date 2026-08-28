package admin

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const subscriptionUpgradeQuoteTTL = 2 * time.Minute

type subscriptionUpgradeUpstream interface {
	Read(context.Context, proxy.SubscriptionUpgradeCredentials) (*proxy.ChatGPTSubscription, error)
	Quote(context.Context, proxy.SubscriptionUpgradeCredentials, string, string) (*proxy.SubscriptionUpgradeQuote, error)
	Submit(context.Context, proxy.SubscriptionUpgradeCredentials, proxy.SubscriptionUpgradeSubmission) (*proxy.SubscriptionUpgradeSubmitResult, error)
}

type subscriptionUpgradeQuoteRecord struct {
	ID             string
	AccountID      int64
	SourcePlan     string
	TargetPlan     string
	ExpectedPlan   string
	Currency       string
	AmountDueMinor int64
	ExpiresAt      time.Time
}

// subscriptionUpgradeFeatureEnabled 读取环境变量默认值。它只在管理员从未在后台
// 显式设置过开关时生效；一旦后台保存过，数据库值就是唯一权威。
func subscriptionUpgradeFeatureEnabled() bool {
	enabled, err := strconv.ParseBool(strings.TrimSpace(os.Getenv("CODEX2API_SUBSCRIPTION_UPGRADES_ENABLED")))
	return err == nil && enabled
}

// initSubscriptionUpgradeGate 以环境变量为初值，再用数据库里管理员保存过的值覆盖。
func (h *Handler) initSubscriptionUpgradeGate() {
	if h == nil {
		return
	}
	h.subscriptionUpgradeEnvDefault = subscriptionUpgradeFeatureEnabled()
	h.subscriptionUpgradeEnabled.Store(h.subscriptionUpgradeEnvDefault)
	if h.db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stored, err := h.db.LoadSubscriptionUpgradesEnabled(ctx)
	if err != nil {
		log.Printf("加载订阅升级开关失败，暂用环境变量默认值 %t: %v", h.subscriptionUpgradeEnvDefault, err)
		return
	}
	if stored != nil {
		h.subscriptionUpgradeEnabled.Store(*stored)
	}
}

// setSubscriptionUpgradeEnabled 热更新本进程的开关（跨实例不同步，与其余设置一致）。
func (h *Handler) setSubscriptionUpgradeEnabled(enabled bool) {
	if h == nil {
		return
	}
	h.subscriptionUpgradeEnabled.Store(enabled)
}

func (h *Handler) subscriptionUpgradesEnabled() bool {
	return h != nil && h.subscriptionUpgradeEnabled.Load()
}

func (h *Handler) subscriptionUpgradeReady(c *gin.Context) bool {
	if !h.subscriptionUpgradesEnabled() {
		c.JSON(http.StatusNotFound, gin.H{"error": "subscription upgrade feature is disabled"})
		return false
	}
	if h.store == nil || h.db == nil || h.subscriptionUpgradeClientFactory == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "subscription upgrade service is unavailable"})
		return false
	}
	return true
}

func (h *Handler) subscriptionUpgradeAccount(c *gin.Context) (*auth.Account, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid account id"})
		return nil, false
	}
	account := h.store.FindByID(id)
	if account == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "account not found"})
		return nil, false
	}
	credentials := subscriptionUpgradeCredentials(account)
	if credentials.AccessToken == "" || credentials.WorkspaceID == "" {
		c.JSON(http.StatusConflict, gin.H{"error": "account has no usable ChatGPT OAuth workspace credentials"})
		return nil, false
	}
	return account, true
}

func subscriptionUpgradeCredentials(account *auth.Account) proxy.SubscriptionUpgradeCredentials {
	if account == nil {
		return proxy.SubscriptionUpgradeCredentials{}
	}
	return proxy.SubscriptionUpgradeCredentials{
		AccessToken: account.GetAccessToken(),
		WorkspaceID: account.EffectiveAccountID(),
		DeviceID:    uuid.NewSHA1(uuid.NameSpaceURL, []byte(fmt.Sprintf("codex2api:subscription-upgrade:%d", account.DBID))).String(),
	}
}

func subscriptionUpgradeTransition(sourcePlan, targetPlan string) (string, bool) {
	sourcePlan = strings.ToLower(strings.TrimSpace(sourcePlan))
	switch strings.ToLower(strings.TrimSpace(targetPlan)) {
	case "chatgptprolite":
		return "prolite", sourcePlan == "plus"
	case "chatgptpro":
		return "pro", sourcePlan == "prolite"
	default:
		return "", false
	}
}

func (h *Handler) GetAccountSubscription(c *gin.Context) {
	if !h.subscriptionUpgradeReady(c) {
		return
	}
	account, ok := h.subscriptionUpgradeAccount(c)
	if !ok {
		return
	}
	client := h.subscriptionUpgradeClientFactory(account, account.GetProxyURL())
	subscription, err := client.Read(c.Request.Context(), subscriptionUpgradeCredentials(account))
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to read upstream subscription"})
		return
	}
	c.JSON(http.StatusOK, subscription)
}

func (h *Handler) CreateSubscriptionUpgradeQuote(c *gin.Context) {
	if !h.subscriptionUpgradeReady(c) {
		return
	}
	account, ok := h.subscriptionUpgradeAccount(c)
	if !ok {
		return
	}
	var request struct {
		TargetPlan string `json:"target_plan" binding:"required"`
		Currency   string `json:"currency" binding:"required"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "target_plan and currency are required"})
		return
	}
	credentials := subscriptionUpgradeCredentials(account)
	client := h.subscriptionUpgradeClientFactory(account, account.GetProxyURL())
	subscription, err := client.Read(c.Request.Context(), credentials)
	if err != nil || subscription == nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to read upstream subscription"})
		return
	}
	expectedPlan, allowed := subscriptionUpgradeTransition(subscription.PlanType, request.TargetPlan)
	if !allowed || subscription.IsDelinquent {
		c.JSON(http.StatusConflict, gin.H{"error": "requested subscription transition is not allowed"})
		return
	}
	requestedCurrency := strings.ToUpper(strings.TrimSpace(request.Currency))
	if observedCurrency := strings.ToUpper(strings.TrimSpace(subscription.BillingCurrency)); observedCurrency != "" && observedCurrency != requestedCurrency {
		c.JSON(http.StatusConflict, gin.H{"error": "requested currency does not match the current subscription"})
		return
	}
	quote, err := client.Quote(c.Request.Context(), credentials, request.TargetPlan, requestedCurrency)
	if err != nil || quote == nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to preview subscription upgrade"})
		return
	}
	now := time.Now().UTC()
	record := subscriptionUpgradeQuoteRecord{
		ID: uuid.NewString(), AccountID: account.DBID, SourcePlan: strings.ToLower(subscription.PlanType),
		TargetPlan: strings.ToLower(request.TargetPlan), ExpectedPlan: expectedPlan,
		Currency: strings.ToUpper(quote.Currency), AmountDueMinor: quote.AmountDueMinor,
		ExpiresAt: now.Add(subscriptionUpgradeQuoteTTL),
	}
	h.subscriptionUpgradeQuoteMu.Lock()
	h.subscriptionUpgradeQuotes[record.ID] = record
	h.subscriptionUpgradeQuoteMu.Unlock()
	c.JSON(http.StatusOK, gin.H{
		"quote_id": record.ID, "source_plan": record.SourcePlan, "target_plan": record.TargetPlan,
		"currency": record.Currency, "amount_due_minor": record.AmountDueMinor,
		"recurring_amount_minor": quote.RecurringAmountMinor, "tax_amount_minor": quote.TaxAmountMinor,
		"renewal_date": quote.RenewalDate, "default_payment_method_present": quote.DefaultPaymentMethodPresent,
		"silent_reauthorization_available": account.HasSessionToken(),
		"line_items":                       quote.LineItems, "expires_at": record.ExpiresAt,
	})
}

func (h *Handler) CreateSubscriptionUpgrade(c *gin.Context) {
	if !h.subscriptionUpgradeReady(c) {
		return
	}
	account, ok := h.subscriptionUpgradeAccount(c)
	if !ok {
		return
	}
	var request struct {
		QuoteID        string `json:"quote_id" binding:"required"`
		Currency       string `json:"currency" binding:"required"`
		MaxAmountMinor int64  `json:"max_amount_minor" binding:"required"`
		Confirmed      bool   `json:"confirmed"`
	}
	if err := c.ShouldBindJSON(&request); err != nil || !request.Confirmed || request.MaxAmountMinor <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "explicit confirmation, currency, quote_id, and a positive max_amount_minor are required"})
		return
	}
	idempotencyKey := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if idempotencyKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Idempotency-Key header is required"})
		return
	}
	keyHash := subscriptionUpgradeIdempotencyHash(idempotencyKey)
	// Resolve a completed/in-flight operation before reading the ephemeral
	// quote. This keeps retries idempotent across process restarts, where the
	// short-lived in-memory quote is intentionally lost.
	if existing, err := h.db.GetSubscriptionUpgradeOperationByIdempotencyHash(c.Request.Context(), account.DBID, keyHash); err == nil {
		c.JSON(http.StatusOK, existing)
		return
	} else if !errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read upgrade operation journal"})
		return
	}
	// 账号级互斥必须先于报价查验。若放在查验之后，两个带不同幂等键、引用同一份
	// 报价的并发请求会各自拿到一份报价拷贝，然后串行进入付费段各发一次付费 POST：
	// 幂等键不同，数据库唯一约束拦不住，结果是真实双重扣款。
	lockValue, _ := h.subscriptionUpgradeLocks.LoadOrStore(account.DBID, &sync.Mutex{})
	lock := lockValue.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()

	h.subscriptionUpgradeQuoteMu.Lock()
	quote, exists := h.subscriptionUpgradeQuotes[request.QuoteID]
	if exists && time.Now().After(quote.ExpiresAt) {
		delete(h.subscriptionUpgradeQuotes, request.QuoteID)
		exists = false
	}
	h.subscriptionUpgradeQuoteMu.Unlock()
	if !exists || quote.AccountID != account.DBID {
		c.JSON(http.StatusConflict, gin.H{"error": "upgrade quote is missing or expired"})
		return
	}

	credentials := subscriptionUpgradeCredentials(account)
	client := h.subscriptionUpgradeClientFactory(account, account.GetProxyURL())
	subscription, err := client.Read(c.Request.Context(), credentials)
	if err != nil || subscription == nil || strings.ToLower(subscription.PlanType) != quote.SourcePlan || subscription.IsDelinquent {
		c.JSON(http.StatusConflict, gin.H{"error": "subscription changed since quote"})
		return
	}
	fresh, err := client.Quote(c.Request.Context(), credentials, quote.TargetPlan, quote.Currency)
	if err != nil || fresh == nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to refresh subscription quote"})
		return
	}
	confirmedCurrency := strings.ToUpper(strings.TrimSpace(request.Currency))
	if confirmedCurrency != quote.Currency || strings.ToUpper(fresh.Currency) != confirmedCurrency || fresh.AmountDueMinor <= 0 || fresh.AmountDueMinor > request.MaxAmountMinor {
		c.JSON(http.StatusConflict, gin.H{"error": "fresh quote exceeds the confirmed currency or amount cap", "fresh_amount_due_minor": fresh.AmountDueMinor, "currency": fresh.Currency})
		return
	}
	operation := database.SubscriptionUpgradeOperation{
		ID: uuid.NewString(), AccountID: account.DBID, IdempotencyKeyHash: keyHash,
		SourcePlan: quote.SourcePlan, TargetPlan: quote.TargetPlan, Currency: confirmedCurrency,
		AmountDueMinor: fresh.AmountDueMinor, Status: database.SubscriptionUpgradeStatusSubmitting,
	}
	if err := h.db.CreateSubscriptionUpgradeOperation(c.Request.Context(), operation); err != nil {
		if errors.Is(err, database.ErrSubscriptionUpgradeIdempotencyConflict) {
			existing, getErr := h.db.GetSubscriptionUpgradeOperationByIdempotencyHash(c.Request.Context(), account.DBID, keyHash)
			if getErr == nil {
				c.JSON(http.StatusOK, existing)
				return
			}
		}
		// 跨实例闸门：另一个实例已为该账号写下在途行，这里不能再发付费 POST。
		if errors.Is(err, database.ErrSubscriptionUpgradeInFlight) {
			c.JSON(http.StatusConflict, gin.H{"error": "another paid upgrade for this account is still in flight; reconcile that operation before starting a new one"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create upgrade operation journal"})
		return
	}
	if h.recordAccountEvent != nil {
		h.recordAccountEvent(account.DBID, "subscription_upgrade_confirmed", fmt.Sprintf("target=%s amount_minor=%d currency=%s operation=%s", quote.TargetPlan, fresh.AmountDueMinor, confirmedCurrency, operation.ID))
	}
	// The payment method identifier is intentionally memory-only and is no
	// longer needed after the durable pre-submit journal exists.
	h.subscriptionUpgradeQuoteMu.Lock()
	delete(h.subscriptionUpgradeQuotes, request.QuoteID)
	h.subscriptionUpgradeQuoteMu.Unlock()
	result, submitErr := client.Submit(c.Request.Context(), credentials, proxy.SubscriptionUpgradeSubmission{
		TargetPlan: quote.TargetPlan, FlowID: uuid.NewString(), MutationID: uuid.NewString(),
		IdempotencyKey: idempotencyKey, PaymentMethodID: fresh.PaymentMethodID,
	})
	// 付费 POST 一旦发出，后续所有落库和只读校验都必须脱离请求 context：
	// ambiguous_transport 恰恰就是连接中断的场景，继续用已取消的请求 context
	// 会让最需要留痕的那条记录静默丢失，操作永远停在 submitting。
	postCtx, cancelPostCtx := subscriptionUpgradePostPaymentContext()
	defer cancelPostCtx()
	if submitErr != nil {
		h.updateSubscriptionUpgradeOperation(postCtx, operation.ID, "ambiguous_transport", "paid submission outcome is ambiguous; do not retry", 0)
		h.respondSubscriptionUpgradeOperation(c, postCtx, operation.ID, http.StatusAccepted)
		return
	}
	if result != nil && result.RequiresUserAction {
		h.updateSubscriptionUpgradeOperation(postCtx, operation.ID, "requires_user_action", "complete upstream payment authentication before verification", http.StatusOK)
		h.respondSubscriptionUpgradeOperation(c, postCtx, operation.ID, http.StatusAccepted)
		return
	}
	verified, verifyErr := client.Read(postCtx, credentials)
	status, detail := "verification_pending", "paid submission accepted; upstream entitlement not yet observed"
	if verifyErr == nil && verified != nil && strings.EqualFold(verified.PlanType, quote.ExpectedPlan) {
		status, detail = "succeeded", "upstream entitlement verified"
	} else if subscriptionUpgradeUnauthorized(verifyErr) {
		status, detail = "verification_requires_reauthentication", "upstream invalidated the OAuth credential family after accepting the upgrade; reauthorize without repeating payment"
		// A separately stored ChatGPT Web Session can mint a new OAuth family.
		// Re-saving the invalidated AT/RT cannot. Refresh at most once and only
		// re-run the read-only verification; the paid POST is never repeated.
		if account.HasSessionToken() && h.refreshAccount != nil {
			if refreshErr := h.refreshAccount(postCtx, account.DBID); refreshErr == nil {
				refreshedAccount := h.store.FindByID(account.DBID)
				if refreshedAccount != nil {
					refreshedClient := h.subscriptionUpgradeClientFactory(refreshedAccount, refreshedAccount.GetProxyURL())
					refreshedSubscription, refreshedErr := refreshedClient.Read(postCtx, subscriptionUpgradeCredentials(refreshedAccount))
					if refreshedErr == nil && refreshedSubscription != nil && strings.EqualFold(refreshedSubscription.PlanType, quote.ExpectedPlan) {
						status, detail = "succeeded", "upstream entitlement verified after silent reauthorization from the stored web session"
					}
				}
			}
		}
	}
	h.updateSubscriptionUpgradeOperation(postCtx, operation.ID, status, detail, http.StatusOK)
	h.respondSubscriptionUpgradeOperation(c, postCtx, operation.ID, http.StatusAccepted)
}

func subscriptionUpgradeIdempotencyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// subscriptionUpgradePostPaymentContext 返回脱离请求生命周期的 context，供付费
// POST 之后的落库与只读校验使用。
func subscriptionUpgradePostPaymentContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

// subscriptionUpgradeUnauthorized 只认 401。chatgpt.com 的 403 多数是 Cloudflare
// 盾误伤而非凭据被作废，把它报成「上游已接受付费并作废了凭据族」会误导对账；
// 403 落到 verification_pending 更符合实际，管理员可用对账端点复核。
func subscriptionUpgradeUnauthorized(err error) bool {
	var upstreamErr *proxy.SubscriptionUpstreamHTTPError
	return errors.As(err, &upstreamErr) && upstreamErr.StatusCode == http.StatusUnauthorized
}

func (h *Handler) updateSubscriptionUpgradeOperation(ctx context.Context, id, status, detail string, httpStatus int) {
	if err := h.db.UpdateSubscriptionUpgradeOperation(ctx, id, status, detail, httpStatus); err != nil {
		// 付费已经发生，日志是唯一剩下的线索，绝不能静默吞掉。
		log.Printf("订阅升级操作 %s 落库失败(status=%s detail=%q): %v", id, status, detail, err)
	}
}

func (h *Handler) respondSubscriptionUpgradeOperation(c *gin.Context, ctx context.Context, id string, status int) {
	operation, err := h.db.GetSubscriptionUpgradeOperation(ctx, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read upgrade operation journal"})
		return
	}
	c.JSON(status, operation)
}

// VerifySubscriptionUpgradeOperation 是只读对账出口：重新读取上游订阅，把已经
// 生效的升级从 submitting/ambiguous_transport/verification_* 推进到终态。它永远
// 不会重发付费 POST，也是把卡在 submitting 的账号从在途闸门里解出来的唯一途径。
func (h *Handler) VerifySubscriptionUpgradeOperation(c *gin.Context) {
	if !h.subscriptionUpgradeReady(c) {
		return
	}
	operationID := strings.TrimSpace(c.Param("operation_id"))
	operation, err := h.db.GetSubscriptionUpgradeOperation(c.Request.Context(), operationID)
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "subscription upgrade operation not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read upgrade operation journal"})
		return
	}
	if operation.Status == "succeeded" {
		c.JSON(http.StatusOK, operation)
		return
	}
	account := h.store.FindByID(operation.AccountID)
	if account == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "account for this operation no longer exists"})
		return
	}
	expectedPlan, _ := subscriptionUpgradeTransition(operation.SourcePlan, operation.TargetPlan)
	if expectedPlan == "" {
		c.JSON(http.StatusConflict, gin.H{"error": "operation records an unsupported subscription transition"})
		return
	}
	client := h.subscriptionUpgradeClientFactory(account, account.GetProxyURL())
	subscription, readErr := client.Read(c.Request.Context(), subscriptionUpgradeCredentials(account))
	switch {
	case readErr == nil && subscription != nil && strings.EqualFold(subscription.PlanType, expectedPlan):
		h.updateSubscriptionUpgradeOperation(c.Request.Context(), operation.ID, "succeeded",
			"upstream entitlement verified by admin reconciliation", operation.SubmitHTTPStatus)
	case subscriptionUpgradeUnauthorized(readErr):
		h.updateSubscriptionUpgradeOperation(c.Request.Context(), operation.ID, "verification_requires_reauthentication",
			"upstream still rejects the OAuth credential family; reauthorize the account, then reconcile again without repeating payment",
			operation.SubmitHTTPStatus)
	case readErr != nil:
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to read upstream subscription for reconciliation"})
		return
	default:
		h.updateSubscriptionUpgradeOperation(c.Request.Context(), operation.ID, "verification_pending",
			"reconciliation did not observe the target entitlement yet; do not repeat payment", operation.SubmitHTTPStatus)
	}
	h.respondSubscriptionUpgradeOperation(c, c.Request.Context(), operation.ID, http.StatusOK)
}

func (h *Handler) GetSubscriptionUpgradeOperation(c *gin.Context) {
	if !h.subscriptionUpgradeReady(c) {
		return
	}
	operation, err := h.db.GetSubscriptionUpgradeOperation(c.Request.Context(), strings.TrimSpace(c.Param("operation_id")))
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "subscription upgrade operation not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read upgrade operation journal"})
		return
	}
	c.JSON(http.StatusOK, operation)
}
