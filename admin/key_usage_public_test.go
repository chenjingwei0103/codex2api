package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestParsePublicAPIKeyUsageLogFilter(t *testing.T) {
	filter, err := parsePublicAPIKeyUsageLogFilter(" gpt-5.5 ", "/v1/responses", "ERROR", "Stream", "Claude")
	if err != nil {
		t.Fatalf("parse valid filter: %v", err)
	}
	if filter.Model != "gpt-5.5" || filter.Endpoint != "/v1/responses" || filter.Status != "error" || filter.Stream != "stream" || filter.Channel != "claude" {
		t.Fatalf("filter = %+v", filter)
	}

	if _, err := parsePublicAPIKeyUsageLogFilter("", "", "", "", ""); err != nil {
		t.Fatalf("empty filter should be valid: %v", err)
	}
	invalid := []struct{ model, endpoint, status, stream, channel string }{
		{status: "200"},
		{stream: "ws"},
		{channel: "openai"},
		{model: strings.Repeat("m", publicAPIKeyUsageMaxFilterLen+1)},
	}
	for _, tc := range invalid {
		if _, err := parsePublicAPIKeyUsageLogFilter(tc.model, tc.endpoint, tc.status, tc.stream, tc.channel); err == nil {
			t.Fatalf("expected error for %+v", tc)
		}
	}
}

// 停用的 Key 与代理链路一致被拒绝；过期 Key 仍可读并如实标注 expired。
func TestPublicAPIKeyUsageSummaryKeyStatusGate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	h := &Handler{db: db}
	r := gin.New()
	r.GET("/api/key-usage/summary", h.GetPublicAPIKeyUsageSummary)

	ctx := context.Background()
	insert := func(name, key string, update database.APIKeyUpdate) {
		t.Helper()
		id, err := db.InsertAPIKey(ctx, name, key)
		if err != nil {
			t.Fatalf("InsertAPIKey(%s): %v", name, err)
		}
		if err := db.UpdateAPIKey(ctx, id, update); err != nil {
			t.Fatalf("UpdateAPIKey(%s): %v", name, err)
		}
	}
	insert("disabled", "sk-usage-disabled-000000000000", database.APIKeyUpdate{Enabled: false, EnabledSet: true})
	insert("expired", "sk-usage-expired-0000000000000", database.APIKeyUpdate{
		ExpiresAt:    sql.NullTime{Time: time.Now().Add(-time.Hour), Valid: true},
		ExpiresAtSet: true,
	})

	summary := func(key string) (int, publicAPIKeyUsageResponse) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/key-usage/summary", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var resp publicAPIKeyUsageResponse
		if w.Code == http.StatusOK {
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode summary: %v", err)
			}
		}
		return w.Code, resp
	}

	if code, resp := summary("sk-usage-disabled-000000000000"); code != http.StatusUnauthorized {
		t.Fatalf("disabled key: status=%d key=%+v, want 401", code, resp.Key)
	}
	code, resp := summary("sk-usage-expired-0000000000000")
	if code != http.StatusOK || resp.Key.Status != "expired" {
		t.Fatalf("expired key: status=%d key.status=%q, want 200 expired", code, resp.Key.Status)
	}
}
