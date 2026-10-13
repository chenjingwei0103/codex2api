package database

import (
	"context"
	"path/filepath"
	"testing"
)

func TestUnsupportedModelCooldownSettingRoundTrip(t *testing.T) {
	ctx := context.Background()
	db, err := New("sqlite", filepath.Join(t.TempDir(), "codex2api.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	defaults, err := db.GetModelCooldownSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if defaults.UnsupportedCooldownSeconds() != DefaultOAuthUnsupportedModelCooldownSeconds {
		t.Fatalf("default = %d, want %d", defaults.UnsupportedCooldownSeconds(), DefaultOAuthUnsupportedModelCooldownSeconds)
	}

	update := func(seconds int) ModelCooldownSettings {
		t.Helper()
		updated, err := db.UpdateModelCooldownSettings(ctx, ModelCooldownSettingsUpdate{OAuthUnsupportedSeconds: &seconds})
		if err != nil {
			t.Fatal(err)
		}
		reloaded, err := db.GetModelCooldownSettings(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if reloaded != updated {
			t.Fatalf("reloaded = %#v, want %#v", reloaded, updated)
		}
		return reloaded
	}

	if got := update(0); !got.OAuthUnsupportedDisabled || got.UnsupportedCooldownSeconds() != 0 {
		t.Fatalf("0 must disable: %#v", got)
	}
	if got := update(600); got.OAuthUnsupportedDisabled || got.UnsupportedCooldownSeconds() != 600 {
		t.Fatalf("600 must re-enable: %#v", got)
	}
	if got := update(-5); got.UnsupportedCooldownSeconds() != 600 {
		t.Fatalf("negative must be ignored: %#v", got)
	}
	if got := update(99999); got.UnsupportedCooldownSeconds() != MaxModelCooldownSeconds {
		t.Fatalf("oversized must clamp: %#v", got)
	}
	// Other cooldown fields are untouched by this update.
	if got, _ := db.GetModelCooldownSettings(ctx); got.OAuthSeconds != defaults.OAuthSeconds || got.OAuthMode != defaults.OAuthMode {
		t.Fatalf("unrelated settings changed: %#v", got)
	}

	if got := NormalizeModelCooldownSettings(ModelCooldownSettings{}).UnsupportedCooldownSeconds(); got != DefaultOAuthUnsupportedModelCooldownSeconds {
		t.Fatalf("normalized zero value = %d, want default (must not disable)", got)
	}
}
