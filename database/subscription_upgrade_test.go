package database

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestSubscriptionUpgradeOperationRejectsDuplicateIdempotencyKey(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "codex2api.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	first := SubscriptionUpgradeOperation{
		ID:                 "operation-1",
		AccountID:          20,
		IdempotencyKeyHash: "sha256:first-key",
		SourcePlan:         "prolite",
		TargetPlan:         "chatgptpro",
		Currency:           "PHP",
		AmountDueMinor:     345196,
		Status:             "submitting",
	}
	if err := db.CreateSubscriptionUpgradeOperation(ctx, first); err != nil {
		t.Fatalf("Create first: %v", err)
	}
	duplicate := first
	duplicate.ID = "operation-2"
	if err := db.CreateSubscriptionUpgradeOperation(ctx, duplicate); !errors.Is(err, ErrSubscriptionUpgradeIdempotencyConflict) {
		t.Fatalf("Create duplicate error = %v, want idempotency conflict", err)
	}
	got, err := db.GetSubscriptionUpgradeOperationByIdempotencyHash(ctx, 20, first.IdempotencyKeyHash)
	if err != nil {
		t.Fatalf("Get by idempotency hash: %v", err)
	}
	if got.ID != first.ID || got.AmountDueMinor != 345196 {
		t.Fatalf("operation = %#v, want original operation", got)
	}
}

// 进程内互斥挡不住多实例，部分唯一索引才是真正原子的双重扣款闸门：
// 同一账号只允许存在一行 submitting。
func TestSubscriptionUpgradeOperationBlocksSecondInFlightSubmission(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "codex2api.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	first := SubscriptionUpgradeOperation{
		ID: "inflight-1", AccountID: 42, IdempotencyKeyHash: "sha256:key-a",
		SourcePlan: "prolite", TargetPlan: "chatgptpro", Currency: "PHP",
		AmountDueMinor: 345196, Status: SubscriptionUpgradeStatusSubmitting,
	}
	if err := db.CreateSubscriptionUpgradeOperation(ctx, first); err != nil {
		t.Fatalf("Create first: %v", err)
	}
	// 不同幂等键，所以幂等约束放行；必须被在途索引挡住。
	second := first
	second.ID, second.IdempotencyKeyHash = "inflight-2", "sha256:key-b"
	if err := db.CreateSubscriptionUpgradeOperation(ctx, second); !errors.Is(err, ErrSubscriptionUpgradeInFlight) {
		t.Fatalf("Create second error = %v, want in-flight conflict", err)
	}
	// 前一次落定后，闸门必须放行下一次。
	if err := db.UpdateSubscriptionUpgradeOperation(ctx, first.ID, "succeeded", "verified", 200); err != nil {
		t.Fatalf("Update first: %v", err)
	}
	if err := db.CreateSubscriptionUpgradeOperation(ctx, second); err != nil {
		t.Fatalf("Create second after settlement: %v", err)
	}
	// 另一个账号不受影响。
	other := first
	other.ID, other.AccountID, other.IdempotencyKeyHash = "inflight-3", 43, "sha256:key-c"
	if err := db.CreateSubscriptionUpgradeOperation(ctx, other); err != nil {
		t.Fatalf("Create for a different account: %v", err)
	}
}
