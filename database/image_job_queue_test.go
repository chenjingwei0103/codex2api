package database

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestImageJobQueueMigrationClaimAndRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	db, err := New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if db != nil {
			db.Close()
		}
	}()
	ctx := context.Background()
	queued, err := db.InsertImageGenerationJob(ctx, ImageGenerationJobInput{Prompt: "preserve me", ParamsJSON: `{"input_images":["https://example.com/a.png"]}`})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err = db.InitImageJobQueue(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, e := db.ClaimImageJob(ctx, queued, "owner", time.Now().Add(time.Minute))
			if e != nil {
				t.Error(e)
			}
			if ok {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("claim winners=%d", wins.Load())
	}
	if ok, e := db.RenewImageJobLease(ctx, queued, "wrong-owner", time.Now().Add(time.Minute)); e != nil || ok {
		t.Fatalf("wrong owner renewed: %v %v", ok, e)
	}
	waiting, err := db.InsertImageGenerationJob(ctx, ImageGenerationJobInput{Prompt: "still queued"})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.ExpireImageJobLeases(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	job, _ := db.GetImageGenerationJob(ctx, queued)
	if job.Status != ImageJobRunning {
		t.Fatal("live lease was expired")
	}
	if err = db.ExpireImageJobLeases(ctx, time.Now().Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err = db.FinishLeasedImageJob(ctx, queued, "owner", ImageJobSucceeded, "", 0); err != nil {
		t.Fatal(err)
	}
	job, _ = db.GetImageGenerationJob(ctx, queued)
	if job.Status != ImageJobFailed {
		t.Fatal("late worker overwrote recovery")
	}
	job, _ = db.GetImageGenerationJob(ctx, waiting)
	if job.Status != ImageJobQueued {
		t.Fatal("queued job lost during recovery")
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db = nil
	db2, err := New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	if err = db2.InitImageJobQueue(ctx); err != nil {
		t.Fatal(err)
	}
	ids, err := db2.QueuedImageJobIDs(ctx, 2)
	if err != nil || len(ids) != 1 || ids[0] != waiting {
		t.Fatalf("restart lost queue: %v %v", ids, err)
	}
}

func TestQueuedImageJobHeadsOnePerKeyAndStaleScan(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "heads.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err = db.InitImageJobQueue(ctx); err != nil {
		t.Fatal(err)
	}
	insert := func(keyID int64) int64 {
		id, err := db.InsertImageGenerationJob(ctx, ImageGenerationJobInput{Prompt: "p", APIKeyID: keyID})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	var flood []int64
	for i := 0; i < 50; i++ {
		flood = append(flood, insert(7))
	}
	other := insert(9)
	noKey := insert(0)
	if _, err = db.conn.ExecContext(ctx, `UPDATE image_generation_jobs SET api_key_id=NULL WHERE id=$1`, noKey); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.ClaimImageJob(ctx, flood[0], "owner", time.Now().Add(time.Minute)); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}

	heads, err := db.QueuedImageJobHeads(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	want := []QueuedImageJobHead{{ID: flood[1], APIKeyID: 7}, {ID: other, APIKeyID: 9}}
	if len(heads) != 2 || heads[0] != want[0] || heads[1] != want[1] {
		t.Fatalf("heads=%v want %v", heads, want)
	}
	heads, err = db.QueuedImageJobHeads(ctx, 10)
	if err != nil || len(heads) != 3 || heads[2] != (QueuedImageJobHead{ID: noKey, APIKeyID: 0}) {
		t.Fatalf("heads=%v err=%v", heads, err)
	}

	if n, err := db.CountQueuedImageJobsByKey(ctx, 7); err != nil || n != 49 {
		t.Fatalf("queued for key 7: %d %v", n, err)
	}

	if _, err = db.conn.ExecContext(ctx, `UPDATE image_generation_jobs SET created_at=$1 WHERE id IN ($2,$3)`, db.imageRetentionCutoff(time.Now().Add(-48*time.Hour)), flood[0], other); err != nil {
		t.Fatal(err)
	}
	stale, err := db.StaleQueuedImageJobIDs(ctx, time.Now().Add(-24*time.Hour), 10)
	if err != nil || len(stale) != 1 || stale[0] != other {
		t.Fatalf("stale=%v err=%v (running jobs must be excluded)", stale, err)
	}
}
