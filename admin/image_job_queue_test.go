package admin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func TestImageQueueAcceptsThousandWithoutFetchingOrKeySlots(t *testing.T) {
	const jobCount = 1000
	t.Setenv("IMAGE_JOB_WORKERS", "2")
	db := newTestAdminDB(t)
	if err := db.InitImageJobQueue(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := db.InsertAPIKeyWithOptions(context.Background(), database.APIKeyInput{Name: "queue", Key: "sk-queue", Limits: database.APIKeyLimits{MaxConcurrency: 1}})
	if err != nil {
		t.Fatal(err)
	}
	tc := cache.NewMemory(1)
	defer tc.Close()
	store := auth.NewStore(db, tc, nil)
	defer store.Stop()
	h := NewHandler(store, db, tc, nil, "")
	h.imageQueue = &imageJobQueue{intake: make(chan struct{}, 2)}
	p := proxy.NewHandler(store, db, nil, nil)
	r := gin.New()
	h.RegisterExternalImageRoutes(r, p)
	// Saturated key must not reject durable admission or download references.
	release := occupyAPIKeyConcurrency(t, p, "sk-queue")
	defer release()
	var wg sync.WaitGroup
	for i := 0; i < jobCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/v1/images/jobs", strings.NewReader(`{"model":"gpt-image-2","prompt":"queue test","input_images":["https://does-not-exist.invalid/ref.png"]}`))
			req.Header.Set("Authorization", "Bearer sk-queue")
			req.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(w, req)
			if w.Code != http.StatusAccepted {
				t.Errorf("status=%d: %s", w.Code, w.Body.String())
			}
			if w.Body.Len() > 150 || strings.Contains(w.Body.String(), "prompt") {
				t.Errorf("oversized receipt: %s", w.Body.String())
			}
		}()
	}
	wg.Wait()
	page, err := db.ListImageGenerationJobs(context.Background(), 1, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != jobCount {
		t.Fatalf("jobs=%d", page.Total)
	}
	for _, job := range page.Jobs {
		if job.Status != database.ImageJobQueued || !strings.Contains(job.ParamsJSON, "https://does-not-exist.invalid/ref.png") || strings.Contains(job.ParamsJSON, "base64,") {
			t.Fatalf("unexpected queued job %d", job.ID)
		}
	}
	// Reconstructing the handler must no longer fail queued jobs.
	NewHandler(store, db, tc, nil, "")
	ids, err := db.QueuedImageJobIDs(context.Background(), jobCount)
	if err != nil || len(ids) != jobCount {
		t.Fatalf("restart: %d %v", len(ids), err)
	}
}

// A key saturated by its own long-lived requests must not park workers on its
// backlog: its jobs stay queued while other keys' jobs keep executing.
func TestImageQueueSaturatedKeyDoesNotStarveOtherKeys(t *testing.T) {
	t.Setenv("IMAGE_JOB_WORKERS", "2")
	t.Setenv("IMAGE_JOB_MEMORY_WORKERS", "")
	_, imageProxy, db := newExternalImageJobRouter(t, database.APIKeyLimits{MaxConcurrency: 1}, "sk-backlog")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	key, err := db.GetAPIKeyByValue(ctx, "sk-backlog")
	if err != nil {
		t.Fatal(err)
	}
	const backlog = 1000
	for i := 0; i < backlog; i++ {
		if _, err := db.InsertImageGenerationJob(ctx, database.ImageGenerationJobInput{APIKeyID: key.ID, ParamsJSON: `{"prompt":"test","n":1,"model":"gpt-image-2"}`}); err != nil {
			t.Fatal(err)
		}
	}
	release := occupyAPIKeyConcurrency(t, imageProxy, "sk-backlog")
	defer release()
	otherKeyID, err := db.InsertAPIKey(ctx, "other", "sk-other")
	if err != nil {
		t.Fatal(err)
	}
	otherJob, err := db.InsertImageGenerationJob(ctx, database.ImageGenerationJobInput{APIKeyID: otherKeyID, ParamsJSON: `{"prompt":"test","n":1,"model":"gpt-image-2"}`})
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{db: db, imageProxy: imageProxy}
	if err := h.StartImageJobQueue(ctx, 2); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		h.imageQueue.wg.Wait()
		proxy.ConfigureImageExecutionLimit(0)
		proxy.ConfigureImagePipeline(0)
	})
	assertBacklogQueued := func() {
		t.Helper()
		n, err := db.CountQueuedImageJobsByKey(ctx, key.ID)
		if err != nil {
			t.Fatal(err)
		}
		if n != backlog {
			t.Fatalf("saturated key had %d jobs claimed", backlog-n)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		assertBacklogQueued()
		job, err := db.GetImageGenerationJob(ctx, otherJob)
		if err != nil {
			t.Fatal(err)
		}
		if job.Status != database.ImageJobQueued {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("other key's job was starved by the saturated key")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Keep observing a few dispatch ticks.
	for end := time.Now().Add(1500 * time.Millisecond); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		assertBacklogQueued()
	}
}

func TestImageQueueCapsQueuedJobsPerKey(t *testing.T) {
	db := newTestAdminDB(t)
	if err := db.InitImageJobQueue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertAPIKey(context.Background(), "capped", "sk-capped"); err != nil {
		t.Fatal(err)
	}
	tc := cache.NewMemory(1)
	defer tc.Close()
	store := auth.NewStore(db, tc, nil)
	defer store.Stop()
	h := NewHandler(store, db, tc, nil, "")
	h.imageQueue = &imageJobQueue{intake: make(chan struct{}, 2), maxQueuedPerKey: 2}
	r := gin.New()
	h.RegisterExternalImageRoutes(r, proxy.NewHandler(store, db, nil, nil))
	for i, want := range []int{http.StatusAccepted, http.StatusAccepted, http.StatusTooManyRequests} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/images/jobs", strings.NewReader(`{"model":"gpt-image-2","prompt":"cap test"}`))
		req.Header.Set("Authorization", "Bearer sk-capped")
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("request %d: status=%d want %d: %s", i, w.Code, want, w.Body.String())
		}
	}
	ids, err := db.QueuedImageJobIDs(context.Background(), 10)
	if err != nil || len(ids) != 2 {
		t.Fatalf("queued=%v err=%v", ids, err)
	}
}

func TestImageQueueFailsJobsThatNeverStarted(t *testing.T) {
	t.Setenv("IMAGE_ASSET_DIR", t.TempDir())
	db := newTestAdminDB(t)
	ctx := context.Background()
	if err := db.InitImageJobQueue(ctx); err != nil {
		t.Fatal(err)
	}
	keyID, err := db.InsertAPIKey(ctx, "stale", "sk-stale")
	if err != nil {
		t.Fatal(err)
	}
	req := imageGenerationJobPayload{Prompt: "test", Model: "gpt-image-2", N: 1, InputImages: []string{"data:image/png;base64," + base64.StdEncoding.EncodeToString(tinyPNG(t))}}
	if err = prepareQueueInputs(&req); err != nil {
		t.Fatal(err)
	}
	path, _ := queueInputPath(req.InputImages[0])
	params, _ := json.Marshal(req)
	id, err := db.InsertImageGenerationJob(ctx, database.ImageGenerationJobInput{Prompt: req.Prompt, ParamsJSON: string(params), APIKeyID: keyID})
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{db: db, imageQueue: &imageJobQueue{maxQueueWait: time.Millisecond}}
	// created_at has second precision.
	time.Sleep(1100 * time.Millisecond)
	h.failStaleQueuedImages(ctx)
	job, err := db.GetImageGenerationJob(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != database.ImageJobFailed || !strings.Contains(job.ErrorMessage, "排队") {
		t.Fatalf("stale job not failed: %+v", job)
	}
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("spooled input retained for expired job")
	}
}

func TestImageJobQueueLimitsFromEnv(t *testing.T) {
	t.Setenv("IMAGE_JOB_MAX_QUEUED_PER_KEY", "")
	t.Setenv("IMAGE_JOB_MAX_QUEUE_WAIT", "")
	if n, d, err := imageJobQueueLimits(); err != nil || n != defaultMaxQueuedPerKey || d != defaultMaxQueueWait {
		t.Fatalf("defaults: %d %s %v", n, d, err)
	}
	t.Setenv("IMAGE_JOB_MAX_QUEUED_PER_KEY", "0")
	t.Setenv("IMAGE_JOB_MAX_QUEUE_WAIT", "0")
	if n, d, err := imageJobQueueLimits(); err != nil || n != 0 || d != 0 {
		t.Fatalf("disabled: %d %s %v", n, d, err)
	}
	for _, bad := range [][2]string{{"-1", "1h"}, {"x", "1h"}, {"10", "30s"}, {"10", "-1h"}, {"10", "soon"}} {
		t.Setenv("IMAGE_JOB_MAX_QUEUED_PER_KEY", bad[0])
		t.Setenv("IMAGE_JOB_MAX_QUEUE_WAIT", bad[1])
		if _, _, err := imageJobQueueLimits(); err == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
}

func TestImageQueueSpoolsDataAndValidatesBeforeExecution(t *testing.T) {
	t.Setenv("IMAGE_ASSET_DIR", t.TempDir())
	const png = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aRZkAAAAASUVORK5CYII="
	req := imageGenerationJobPayload{InputImages: []string{"data:image/png;base64," + png}}
	if err := prepareQueueInputs(&req); err != nil {
		t.Fatal(err)
	}
	inputs := append([]string(nil), req.InputImages...)
	defer cleanupQueueInputs(inputs)
	encoded, _ := json.Marshal(req)
	if strings.Contains(string(encoded), png) {
		t.Fatal("Base64 retained in persisted parameters")
	}
	path, err := queueInputPath(req.InputImages[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if err = loadQueueInputs(context.Background(), &req); err != nil {
		t.Fatal(err)
	}
	if req.InputImages[0] != "data:image/png;base64,"+png {
		t.Fatal("input bytes changed")
	}
	cleanupQueueInputs(inputs)
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("spooled input not removed")
	}
	for _, bad := range []string{"queue-input:../../secret", "file:///etc/passwd", "https://user:pass@example.com/a.png", "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("not an image"))} {
		r := imageGenerationJobPayload{InputImages: []string{bad}}
		if err := prepareQueueInputs(&r); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

// Exercise the real dispatcher, input loading, in-process proxy and fenced
// terminal write without making a billable upstream request (no accounts).
func TestImageQueueWorkerProcessesPersistedInputAndReleasesSlot(t *testing.T) {
	t.Setenv("IMAGE_JOB_WORKERS", "1")
	t.Setenv("IMAGE_ASSET_DIR", t.TempDir())
	db := newTestAdminDB(t)
	if err := db.InitImageJobQueue(context.Background()); err != nil {
		t.Fatal(err)
	}
	tc := cache.NewMemory(1)
	defer tc.Close()
	store := auth.NewStore(db, tc, nil)
	defer store.Stop()
	h := NewHandler(store, db, tc, nil, "")
	h.imageQueue = &imageJobQueue{intake: make(chan struct{}, 2)}
	keyID, err := db.InsertAPIKey(context.Background(), "worker", "sk-worker")
	if err != nil {
		t.Fatal(err)
	}
	req := imageGenerationJobPayload{Prompt: "test", Model: "gpt-image-2", N: 1, InputImages: []string{"data:image/png;base64," + base64.StdEncoding.EncodeToString(tinyPNG(t))}}
	if err = prepareQueueInputs(&req); err != nil {
		t.Fatal(err)
	}
	path, _ := queueInputPath(req.InputImages[0])
	params, _ := json.Marshal(req)
	id, err := db.InsertImageGenerationJob(context.Background(), database.ImageGenerationJobInput{Prompt: req.Prompt, ParamsJSON: string(params), APIKeyID: keyID})
	if err != nil {
		t.Fatal(err)
	}
	proxy.ConfigureImageExecutionLimit(1)
	defer proxy.ConfigureImageExecutionLimit(0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx, release, err := proxy.AcquireImageExecution(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h.runNextQueuedImage(ctx)
	release()
	job, err := db.GetImageGenerationJob(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != database.ImageJobFailed || job.ErrorMessage == "" {
		t.Fatalf("job did not finish: %+v", job)
	}
	if strings.Contains(job.ErrorMessage, "busy") || strings.Contains(job.ErrorMessage, "concurrency") || strings.Contains(job.ErrorMessage, "reference") {
		t.Fatalf("failed before upstream account scheduling: %s", job.ErrorMessage)
	}
	if job.ParamsJSON != string(params) {
		t.Fatal("expanded Base64 replaced persisted parameters")
	}
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("input file retained after completion")
	}
	_, release, err = proxy.AcquireImageExecution(ctx)
	if err != nil {
		t.Fatal("execution slot leaked")
	}
	release()
}
