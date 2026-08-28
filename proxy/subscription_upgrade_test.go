package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSubscriptionUpgradeClientQuoteParsesPHPMinorUnits(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-at" {
			t.Fatalf("Authorization = %q, want Bearer test-at", got)
		}
		switch r.URL.Path {
		case "/backend-api/subscriptions/update/preview":
			if got := r.URL.Query().Get("account_id"); got != testWorkspaceUUID {
				t.Fatalf("account_id = %q, want %q", got, testWorkspaceUUID)
			}
			if got := r.URL.Query().Get("updated_plan"); got != "chatgptpro" {
				t.Fatalf("updated_plan = %q, want chatgptpro", got)
			}
			_, _ = w.Write([]byte(`{
				"currency":"php",
				"amount_due":{"amount":345196,"currency":"php"},
				"tax_amount":0,
				"renewal_date":"2026-09-25T14:45:25Z",
				"default_payment_method":null,
				"line_items":[
					{"kind":"new_plan","amount":891964},
					{"kind":"unused_time_credit","amount":-547004}
				]
			}`))
		case "/backend-api/checkout_pricing_config/configs/PHP":
			_, _ = w.Write([]byte(`{"currency_config":{"pro":{"month":{"amount":9990}}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewSubscriptionUpgradeClient(server.URL, server.Client())
	quote, err := client.Quote(context.Background(), SubscriptionUpgradeCredentials{
		AccessToken: "test-at",
		WorkspaceID: testWorkspaceUUID,
	}, "chatgptpro", "PHP")
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if quote.Currency != "PHP" || quote.AmountDueMinor != 345196 {
		t.Fatalf("amount due = %s %d, want PHP 345196", quote.Currency, quote.AmountDueMinor)
	}
	if quote.RecurringAmountMinor != 999000 {
		t.Fatalf("recurring amount = %d, want 999000", quote.RecurringAmountMinor)
	}
	if quote.TaxAmountMinor != 0 {
		t.Fatalf("tax amount = %d, want 0", quote.TaxAmountMinor)
	}
	if quote.DefaultPaymentMethodPresent {
		t.Fatal("default payment method present = true, want false")
	}
	if len(quote.LineItems) != 2 || quote.LineItems[1].AmountMinor != -547004 {
		t.Fatalf("line items = %#v, want unused-time credit -547004", quote.LineItems)
	}
}

func TestSubscriptionUpgradeClientReadReportsUnauthorizedForReauthentication(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"token family invalidated"}`))
	}))
	defer server.Close()

	client := NewSubscriptionUpgradeClient(server.URL, server.Client())
	_, err := client.Read(context.Background(), SubscriptionUpgradeCredentials{
		AccessToken: "invalidated-at",
		WorkspaceID: testWorkspaceUUID,
	})
	var upstreamErr *SubscriptionUpstreamHTTPError
	if !errors.As(err, &upstreamErr) || upstreamErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("Read error = %v, want typed 401", err)
	}
}

func TestSubscriptionUpgradeClientSubmitOmitsEmptyPaymentMethodAndStopsFor3DS(t *testing.T) {
	var posted map[string]any
	var postCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/subscriptions/update" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		postCount++
		if got := r.Header.Get("Idempotency-Key"); got != "upgrade-once" {
			t.Fatalf("Idempotency-Key = %q, want upgrade-once", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		_, _ = w.Write([]byte(`{"status":"requires_action","next_action":{"type":"use_stripe_sdk"}}`))
	}))
	defer server.Close()

	client := NewSubscriptionUpgradeClient(server.URL, server.Client())
	result, err := client.Submit(context.Background(), SubscriptionUpgradeCredentials{
		AccessToken: "test-at",
		WorkspaceID: testWorkspaceUUID,
	}, SubscriptionUpgradeSubmission{
		TargetPlan:     "chatgptpro",
		FlowID:         "flow-1",
		MutationID:     "mutation-1",
		IdempotencyKey: "upgrade-once",
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if !result.RequiresUserAction {
		t.Fatal("RequiresUserAction = false, want true")
	}
	if _, exists := posted["payment_method_id"]; exists {
		t.Fatal("empty payment_method_id must be omitted")
	}
	if postCount != 1 {
		t.Fatalf("POST count = %d, want exactly one", postCount)
	}
}

// 上游定价配置给的是主单位金额，硬编码 ×100 会让无小数币种的展示价差 100 倍。
func TestSubscriptionUpgradeClientQuoteHandlesZeroDecimalCurrency(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/subscriptions/update/preview":
			_, _ = w.Write([]byte(`{"currency":"jpy","amount_due":{"amount":4500,"currency":"jpy"}}`))
		case "/backend-api/checkout_pricing_config/configs/JPY":
			_, _ = w.Write([]byte(`{"currency_config":{"pro":{"month":{"amount":30000}}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewSubscriptionUpgradeClient(server.URL, server.Client())
	quote, err := client.Quote(context.Background(), SubscriptionUpgradeCredentials{
		AccessToken: "test-at", WorkspaceID: testWorkspaceUUID,
	}, "chatgptpro", "JPY")
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	// JPY 无小数位：¥30,000/月的最小单位就是 30000，不是 3000000。
	if quote.RecurringAmountMinor != 30000 {
		t.Fatalf("recurring amount = %d, want 30000", quote.RecurringAmountMinor)
	}
	if quote.AmountDueMinor != 4500 || quote.Currency != "JPY" {
		t.Fatalf("amount due = %s %d, want JPY 4500", quote.Currency, quote.AmountDueMinor)
	}
}
