package database

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestOfficialPricingSyncConfigLifecycleSQLite(t *testing.T) {
	db, err := New("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	initial, err := db.GetOfficialPricingSyncConfig(ctx)
	if err != nil {
		t.Fatalf("Get initial config: %v", err)
	}
	if initial.Enabled || initial.IntervalMinutes != 1440 || !initial.IncludeOpenAI || !initial.IncludeGrok {
		t.Fatalf("unexpected initial config: %+v", initial)
	}

	updated, err := db.UpdateOfficialPricingSyncConfig(ctx, OfficialPricingSyncConfig{
		Enabled:         true,
		IntervalMinutes: 360,
		IncludeOpenAI:   true,
		IncludeGrok:     false,
	})
	if err != nil {
		t.Fatalf("Update config: %v", err)
	}
	if !updated.Enabled || updated.IntervalMinutes != 360 || !updated.IncludeOpenAI || updated.IncludeGrok {
		t.Fatalf("unexpected updated config: %+v", updated)
	}

	attempted := time.Now().UTC().Truncate(time.Millisecond)
	if err := db.RecordOfficialPricingSyncResult(ctx, attempted, errors.New("temporary upstream error"), []string{"grok warning"}); err != nil {
		t.Fatalf("Record failed result: %v", err)
	}
	failed, err := db.GetOfficialPricingSyncConfig(ctx)
	if err != nil {
		t.Fatalf("Get failed result: %v", err)
	}
	if !failed.LastAttemptAt.Valid || failed.LastSuccessAt.Valid || failed.LastError == "" || failed.LastWarning != "grok warning" {
		t.Fatalf("unexpected failed state: %+v", failed)
	}

	if err := db.RecordOfficialPricingSyncResult(ctx, attempted.Add(time.Minute), nil, nil); err != nil {
		t.Fatalf("Record successful result: %v", err)
	}
	succeeded, err := db.GetOfficialPricingSyncConfig(ctx)
	if err != nil {
		t.Fatalf("Get successful result: %v", err)
	}
	if !succeeded.LastSuccessAt.Valid || succeeded.LastError != "" || succeeded.LastWarning != "" {
		t.Fatalf("unexpected successful state: %+v", succeeded)
	}
}

// 重启后首次读取不能去抢写锁:慢盘 SQLite 启动时用量回填长时间占着写锁,
// 读取若先跑种子行 INSERT 就会等到超时 (#778)。
func TestOfficialPricingSyncConfigReadDoesNotWaitForWriterSQLite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex2api.db")
	seeded, err := New("sqlite", path)
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	if _, err := seeded.GetOfficialPricingSyncConfig(context.Background()); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	_ = seeded.Close()

	db, err := New("sqlite", path)
	if err != nil {
		t.Fatalf("reopen sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	held := make(chan struct{})
	release := make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- db.withWriteTx(context.Background(), func(tx *sql.Tx) error {
			if _, err := tx.Exec(`UPDATE official_pricing_sync_config SET last_warning = last_warning WHERE singleton_id = 1`); err != nil {
				return err
			}
			close(held)
			select {
			case <-release:
			case <-time.After(5 * time.Second):
			}
			return nil
		})
	}()
	<-held

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cfg, err := db.GetOfficialPricingSyncConfig(ctx)
	close(release)
	if err != nil {
		t.Fatalf("read config while a writer holds the lock: %v", err)
	}
	if cfg.Enabled || cfg.IntervalMinutes != DefaultOfficialPricingSyncIntervalMinutes {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if err := <-writerDone; err != nil {
		t.Fatalf("writer: %v", err)
	}
}

func TestOfficialPricingSyncConfigUpgradesLegacyTableSQLite(t *testing.T) {
	db, err := New("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if _, err := db.conn.ExecContext(ctx, `CREATE TABLE official_pricing_sync_config (
		singleton_id INTEGER PRIMARY KEY CHECK (singleton_id = 1),
		enabled BOOLEAN NOT NULL DEFAULT FALSE,
		interval_minutes INTEGER NOT NULL DEFAULT 1440,
		include_openai BOOLEAN NOT NULL DEFAULT TRUE,
		include_grok BOOLEAN NOT NULL DEFAULT TRUE,
		last_attempt_at TIMESTAMP NULL,
		last_success_at TIMESTAMP NULL,
		last_error TEXT NOT NULL DEFAULT '',
		last_warning TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO official_pricing_sync_config (singleton_id, enabled, interval_minutes)
		VALUES (1, TRUE, 360)`); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}

	cfg, err := db.GetOfficialPricingSyncConfig(ctx)
	if err != nil {
		t.Fatalf("Get legacy config: %v", err)
	}
	if !cfg.Enabled || cfg.IntervalMinutes != 360 || !cfg.IncludeClaude {
		t.Fatalf("legacy config not upgraded: %+v", cfg)
	}
}

func TestOfficialPricingSyncConfigRejectsEmptySources(t *testing.T) {
	db, err := New("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.UpdateOfficialPricingSyncConfig(context.Background(), OfficialPricingSyncConfig{
		IntervalMinutes: 1440,
	})
	if err == nil {
		t.Fatal("expected empty official sources to be rejected")
	}
}
