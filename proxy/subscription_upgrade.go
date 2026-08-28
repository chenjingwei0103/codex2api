package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"

	"github.com/codex2api/auth"
)

const chatGPTWebBaseURL = "https://chatgpt.com"

// SubscriptionUpgradeCredentials contains only the account-scoped values sent
// to ChatGPT. Callers must never persist this value in an upgrade journal.
type SubscriptionUpgradeCredentials struct {
	AccessToken string
	WorkspaceID string
	DeviceID    string
}

type SubscriptionUpgradeLineItem struct {
	Kind        string `json:"kind"`
	AmountMinor int64  `json:"amount_minor"`
}

type SubscriptionUpgradeQuote struct {
	Currency                    string                        `json:"currency"`
	AmountDueMinor              int64                         `json:"amount_due_minor"`
	RecurringAmountMinor        int64                         `json:"recurring_amount_minor"`
	TaxAmountMinor              int64                         `json:"tax_amount_minor"`
	RenewalDate                 string                        `json:"renewal_date,omitempty"`
	DefaultPaymentMethodPresent bool                          `json:"default_payment_method_present"`
	LineItems                   []SubscriptionUpgradeLineItem `json:"line_items,omitempty"`
	PaymentMethodID             string                        `json:"-"`
}

type SubscriptionUpgradeSubmission struct {
	TargetPlan      string
	FlowID          string
	MutationID      string
	IdempotencyKey  string
	PaymentMethodID string
}

type SubscriptionUpgradeSubmitResult struct {
	Status             string `json:"status,omitempty"`
	PaymentStatus      string `json:"payment_status,omitempty"`
	RequiresUserAction bool   `json:"requires_user_action"`
}

type SubscriptionUpstreamHTTPError struct {
	StatusCode int
	Body       string
}

func (e *SubscriptionUpstreamHTTPError) Error() string {
	return fmt.Sprintf("subscription upstream returned status %d: %s", e.StatusCode, e.Body)
}

// SubscriptionUpgradeClient is the ChatGPT web subscription boundary. The
// injected constructor is useful for tests; NewChatGPTSubscriptionUpgradeClient
// selects the same browser-fingerprint maintenance transport used by reads.
type SubscriptionUpgradeClient struct {
	baseURL string
	client  *http.Client
	account *auth.Account
}

func NewSubscriptionUpgradeClient(baseURL string, client *http.Client) *SubscriptionUpgradeClient {
	if client == nil {
		client = http.DefaultClient
	}
	return &SubscriptionUpgradeClient{baseURL: strings.TrimRight(baseURL, "/"), client: client}
}

func NewChatGPTSubscriptionUpgradeClient(account *auth.Account, proxyURL string) *SubscriptionUpgradeClient {
	return &SubscriptionUpgradeClient{
		baseURL: chatGPTWebBaseURL,
		client:  getMaintenanceClient(account, proxyURL, maintenancePurposeSubscription, true),
		account: account,
	}
}

func (c *SubscriptionUpgradeClient) Quote(ctx context.Context, credentials SubscriptionUpgradeCredentials, targetPlan, currency string) (*SubscriptionUpgradeQuote, error) {
	previewPath := "/backend-api/subscriptions/update/preview?" + url.Values{
		"account_id":   {credentials.WorkspaceID},
		"updated_plan": {targetPlan},
	}.Encode()
	var preview struct {
		Currency  string `json:"currency"`
		AmountDue struct {
			Amount   int64  `json:"amount"`
			Currency string `json:"currency"`
		} `json:"amount_due"`
		TotalAmount          int64  `json:"total_amount"`
		TaxAmount            int64  `json:"tax_amount"`
		RenewalDate          string `json:"renewal_date"`
		DefaultPaymentMethod any    `json:"default_payment_method"`
		LineItems            []struct {
			Kind   string `json:"kind"`
			Amount int64  `json:"amount"`
		} `json:"line_items"`
	}
	if err := c.doJSON(ctx, http.MethodGet, previewPath, credentials, "", nil, &preview); err != nil {
		return nil, fmt.Errorf("preview subscription upgrade: %w", err)
	}

	currency = strings.ToUpper(strings.TrimSpace(currency))
	var pricing struct {
		CurrencyConfig map[string]map[string]struct {
			Amount float64 `json:"amount"`
		} `json:"currency_config"`
	}
	pricingPath := "/backend-api/checkout_pricing_config/configs/" + url.PathEscape(currency)
	if err := c.doJSON(ctx, http.MethodGet, pricingPath, credentials, "", nil, &pricing); err != nil {
		return nil, fmt.Errorf("read subscription pricing: %w", err)
	}

	planKey := targetPricingPlanKey(targetPlan)
	monthly, ok := pricing.CurrencyConfig[planKey]["month"]
	if !ok || monthly.Amount <= 0 {
		return nil, fmt.Errorf("monthly price for target plan %q is unavailable", targetPlan)
	}
	amountDue := preview.AmountDue.Amount
	if amountDue == 0 {
		amountDue = preview.TotalAmount
	}
	quoteCurrency := strings.ToUpper(strings.TrimSpace(preview.Currency))
	if quoteCurrency == "" {
		quoteCurrency = strings.ToUpper(strings.TrimSpace(preview.AmountDue.Currency))
	}
	quote := &SubscriptionUpgradeQuote{
		Currency:             quoteCurrency,
		AmountDueMinor:       amountDue,
		RecurringAmountMinor: int64(math.Round(monthly.Amount * float64(currencyMinorUnitFactor(quoteCurrency)))),
		TaxAmountMinor:       preview.TaxAmount,
		RenewalDate:          preview.RenewalDate,
		LineItems:            make([]SubscriptionUpgradeLineItem, 0, len(preview.LineItems)),
	}
	for _, item := range preview.LineItems {
		quote.LineItems = append(quote.LineItems, SubscriptionUpgradeLineItem{Kind: item.Kind, AmountMinor: item.Amount})
	}
	quote.PaymentMethodID = paymentMethodID(preview.DefaultPaymentMethod)
	quote.DefaultPaymentMethodPresent = quote.PaymentMethodID != ""
	return quote, nil
}

func (c *SubscriptionUpgradeClient) Read(ctx context.Context, credentials SubscriptionUpgradeCredentials) (*ChatGPTSubscription, error) {
	path := "/backend-api/subscriptions?" + url.Values{"account_id": {credentials.WorkspaceID}}.Encode()
	var subscription ChatGPTSubscription
	if err := c.doJSON(ctx, http.MethodGet, path, credentials, "", nil, &subscription); err != nil {
		return nil, fmt.Errorf("read subscription: %w", err)
	}
	return &subscription, nil
}

// Submit performs exactly one paid mutation. It deliberately contains no
// retry loop: after bytes may have reached the upstream, transport errors are
// ambiguous and must be reconciled through read-only subscription checks.
func (c *SubscriptionUpgradeClient) Submit(ctx context.Context, credentials SubscriptionUpgradeCredentials, submission SubscriptionUpgradeSubmission) (*SubscriptionUpgradeSubmitResult, error) {
	body := struct {
		AccountID             string  `json:"account_id"`
		FlowID                string  `json:"flow_id"`
		UpdatedPlan           string  `json:"updated_plan"`
		UpdatedSeats          any     `json:"updated_seats"`
		UpdatedSeatQuantities any     `json:"updated_seat_quantities"`
		UpdatedPriceInterval  any     `json:"updated_price_interval"`
		UpdatedPromoID        any     `json:"updated_promo_id"`
		UpdatedPromoCode      any     `json:"updated_promo_code"`
		UpdatedPromoCampaign  any     `json:"updated_promo_campaign"`
		PaymentMethodID       *string `json:"payment_method_id,omitempty"`
		SetupIntentID         any     `json:"setup_intent_id"`
		SeatOperation         any     `json:"seat_operation"`
		ProrationQuoteID      any     `json:"proration_quote_id"`
		MutationAttemptID     string  `json:"mutation_attempt_id"`
	}{
		AccountID:         credentials.WorkspaceID,
		FlowID:            submission.FlowID,
		UpdatedPlan:       submission.TargetPlan,
		MutationAttemptID: submission.MutationID,
	}
	if paymentMethodID := strings.TrimSpace(submission.PaymentMethodID); paymentMethodID != "" {
		body.PaymentMethodID = &paymentMethodID
	}
	var payload map[string]any
	if err := c.doJSON(ctx, http.MethodPost, "/backend-api/subscriptions/update", credentials, submission.IdempotencyKey, body, &payload); err != nil {
		return nil, err
	}
	result := &SubscriptionUpgradeSubmitResult{
		Status:             scalarString(payload["status"]),
		PaymentStatus:      scalarString(payload["payment_status"]),
		RequiresUserAction: responseRequiresUserAction(payload, 0),
	}
	return result, nil
}

func scalarString(value any) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func responseRequiresUserAction(value any, depth int) bool {
	if depth > 6 {
		return false
	}
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			normalizedKey := strings.ToLower(strings.TrimSpace(key))
			if normalizedKey == "next_action" && child != nil {
				return true
			}
			if normalizedKey == "status" || normalizedKey == "payment_status" {
				switch strings.ToLower(scalarString(child)) {
				case "requires_action", "requires_confirmation", "requires_payment_method":
					return true
				}
			}
			if responseRequiresUserAction(child, depth+1) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if responseRequiresUserAction(child, depth+1) {
				return true
			}
		}
	}
	return false
}

// currencyMinorUnitFactor 返回主单位换算成最小单位的倍数。上游定价配置给的是
// 主单位金额，硬编码 ×100 会让 JPY/KRW 这类无小数币种的展示价差 100 倍。
// 注意这只影响展示用的 recurring 价格：金额上限校验用的是上游 preview 的原始
// 最小单位数值，不经过这里。
func currencyMinorUnitFactor(currency string) int64 {
	switch strings.ToUpper(strings.TrimSpace(currency)) {
	case "BIF", "CLP", "DJF", "GNF", "JPY", "KMF", "KRW", "MGA", "PYG",
		"RWF", "UGX", "VND", "VUV", "XAF", "XOF", "XPF":
		return 1
	case "BHD", "IQD", "JOD", "KWD", "LYD", "OMR", "TND":
		return 1000
	default:
		return 100
	}
}

func targetPricingPlanKey(targetPlan string) string {
	switch strings.ToLower(strings.TrimSpace(targetPlan)) {
	case "chatgptpro":
		return "pro"
	case "chatgptprolite":
		return "prolite"
	default:
		return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(targetPlan)), "chatgpt")
	}
}

func paymentMethodID(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case map[string]any:
		for _, key := range []string{"id", "payment_method_id"} {
			if raw, ok := typed[key].(string); ok && strings.TrimSpace(raw) != "" {
				return strings.TrimSpace(raw)
			}
		}
	}
	return ""
}

func (c *SubscriptionUpgradeClient) doJSON(ctx context.Context, method, path string, credentials SubscriptionUpgradeCredentials, idempotencyKey string, input, output any) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	endpoint := c.baseURL + path
	client := c.client
	viaResin := false
	if c.account != nil && strings.HasPrefix(endpoint, chatGPTWebBaseURL) {
		if finalURL, resinClient, routed := resinMaintenanceTarget(c.account, endpoint); routed {
			endpoint, client, viaResin = finalURL, resinClient, true
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+credentials.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", chatGPTWebBaseURL)
	req.Header.Set("Referer", chatGPTWebBaseURL+"/")
	req.Header.Set("User-Agent", subscriptionsBrowserUserAgent)
	if credentials.DeviceID != "" {
		req.Header.Set("oai-device-id", credentials.DeviceID)
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if viaResin {
		req.Header.Set("X-Resin-Account", ResinAccountID(c.account))
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 128<<10))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return &SubscriptionUpstreamHTTPError{StatusCode: resp.StatusCode, Body: truncateForLog(responseBody, 300)}
	}
	if output != nil && len(bytes.TrimSpace(responseBody)) > 0 {
		if err := json.Unmarshal(responseBody, output); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}
