package auth

import (
	"testing"
	"time"

	"github.com/codex2api/database"
)

func newUnsupportedCooldownTestStore(t *testing.T, accounts ...*Account) *Store {
	t.Helper()
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 4, TestConcurrency: 1, TestModel: "gpt-5.4"})
	t.Cleanup(store.Stop)
	for _, acc := range accounts {
		store.AddAccount(acc)
	}
	return store
}

func TestUnsupportedModelCooldownSettingDefaultsAndDisable(t *testing.T) {
	store := newUnsupportedCooldownTestStore(t)
	if got := store.UnsupportedModelCooldown(); got != 30*time.Minute {
		t.Fatalf("default = %s, want 30m (partial SystemSettings must not disable it)", got)
	}
	store.SetModelCooldownSettings(database.ModelCooldownSettings{OAuthUnsupportedSeconds: 120})
	if got := store.UnsupportedModelCooldown(); got != 2*time.Minute {
		t.Fatalf("configured = %s, want 2m", got)
	}
	store.SetModelCooldownSettings(database.ModelCooldownSettings{OAuthUnsupportedDisabled: true, OAuthUnsupportedSeconds: 120})
	if got := store.UnsupportedModelCooldown(); got != 0 {
		t.Fatalf("disabled = %s, want 0", got)
	}
}

func TestMarkModelUnsupportedKeepsAccountHealth(t *testing.T) {
	acc := &Account{DBID: 1, AccessToken: "at-1", PlanType: "plus", HealthTier: HealthTierHealthy}
	store := newUnsupportedCooldownTestStore(t, acc)

	cooldown := store.MarkModelUnsupported(acc, "GPT-5.6-Sol", 10*time.Minute)
	if cooldown.Reason != ModelCooldownReasonUnsupported || !acc.IsModelRateLimited("gpt-5.6-sol") {
		t.Fatalf("cooldown = %#v, want model_not_supported on gpt-5.6-sol", cooldown)
	}
	acc.mu.RLock()
	streak, tier := acc.FailureStreak, acc.HealthTier
	acc.mu.RUnlock()
	if streak != 0 || tier != HealthTierHealthy {
		t.Fatalf("health changed: streak=%d tier=%v", streak, tier)
	}
	if acc.IsModelRateLimited("gpt-5.5") {
		t.Fatal("other models must stay schedulable")
	}

	// A longer existing cooldown is not shortened.
	store.MarkModelCooldownUntil(acc, "gpt-5.5", "credits_required", time.Now().Add(25*time.Minute))
	store.MarkModelUnsupported(acc, "gpt-5.5", time.Minute)
	if remaining := acc.ModelCooldownRemaining("gpt-5.5"); remaining < 24*time.Minute {
		t.Fatalf("existing cooldown shortened to %s", remaining)
	}
}

func TestClearModelCooldownsByReasonHonorsKeepWithin(t *testing.T) {
	a := &Account{DBID: 1, AccessToken: "at-1", PlanType: "plus"}
	b := &Account{DBID: 2, AccessToken: "at-2", PlanType: "plus"}
	store := newUnsupportedCooldownTestStore(t, a, b)
	store.MarkModelUnsupported(a, "gpt-5.6-sol", 30*time.Minute)
	store.MarkModelUnsupported(b, "gpt-5.6-sol", 2*time.Minute)
	store.MarkModelCooldownUntil(b, "gpt-5.5", "rate_limited", time.Now().Add(20*time.Minute))

	if cleared := store.ClearModelCooldownsByReason(ModelCooldownReasonUnsupported, 5*time.Minute); cleared != 1 {
		t.Fatalf("cleared = %d, want only the cooldown longer than 5m", cleared)
	}
	if a.IsModelRateLimited("gpt-5.6-sol") || !b.IsModelRateLimited("gpt-5.6-sol") {
		t.Fatal("keepWithin must keep cooldowns already inside the new duration")
	}
	if cleared := store.ClearModelCooldownsByReason(ModelCooldownReasonUnsupported, 0); cleared != 1 {
		t.Fatalf("cleared = %d, want the remaining unsupported cooldown", cleared)
	}
	if !b.IsModelRateLimited("gpt-5.5") {
		t.Fatal("cooldowns with other reasons must be kept")
	}
}

func TestUnsupportedModelCandidateSummary(t *testing.T) {
	a := &Account{DBID: 1, AccessToken: "at-1", PlanType: "free"}
	b := &Account{DBID: 2, AccessToken: "at-2", PlanType: "free"}
	store := newUnsupportedCooldownTestStore(t, a, b)

	if store.UnsupportedModelCandidateSummary(0, nil, "gpt-5.6-sol", DispatchPolicyStandard).Found {
		t.Fatal("no cooldown yet")
	}
	store.MarkModelUnsupported(a, "gpt-5.6-sol", 30*time.Minute)
	store.MarkModelUnsupported(b, "gpt-5.6-sol", 10*time.Minute)
	summary := store.UnsupportedModelCandidateSummary(0, nil, "gpt-5.6-sol", DispatchPolicyStandard)
	if !summary.Found || time.Until(summary.ResetAt) > 11*time.Minute {
		t.Fatalf("summary = %#v, want earliest reset ~10m", summary)
	}
	if store.UnsupportedModelCandidateSummary(0, map[int64]bool{1: true, 2: true}, "gpt-5.6-sol", DispatchPolicyStandard).Found {
		t.Fatal("excluded accounts must not count")
	}
	if store.UnsupportedModelCandidateSummary(0, nil, "gpt-5.5", DispatchPolicyStandard).Found {
		t.Fatal("other models are unaffected")
	}
}
