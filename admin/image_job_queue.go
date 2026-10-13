package admin

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const queueInputPrefix = "queue-input:"
const queueInputBudget = 32 << 20
const queueLeaseDuration = 2 * time.Minute
const queueHeadScanLimit = 64
const queueStaleScanLimit = 32
const defaultMaxQueuedPerKey = 1000
const defaultMaxQueueWait = 24 * time.Hour

type imageJobQueue struct {
	intake          chan struct{}
	wg              sync.WaitGroup
	pipeline        bool
	maxQueuedPerKey int           // 0 disables the per-key backlog cap
	maxQueueWait    time.Duration // 0 disables failing jobs that never started
}

func imageJobQueueLimits() (int, time.Duration, error) {
	maxQueued := defaultMaxQueuedPerKey
	if raw := strings.TrimSpace(os.Getenv("IMAGE_JOB_MAX_QUEUED_PER_KEY")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return 0, 0, fmt.Errorf("IMAGE_JOB_MAX_QUEUED_PER_KEY must be a non-negative integer")
		}
		maxQueued = n
	}
	maxWait := defaultMaxQueueWait
	if raw := strings.TrimSpace(os.Getenv("IMAGE_JOB_MAX_QUEUE_WAIT")); raw != "" {
		d, err := time.ParseDuration(raw)
		if raw == "0" {
			d, err = 0, nil
		}
		if err != nil || d < 0 || (d > 0 && d < time.Minute) {
			return 0, 0, fmt.Errorf("IMAGE_JOB_MAX_QUEUE_WAIT must be 0 or a duration of at least 1m")
		}
		maxWait = d
	}
	return maxQueued, maxWait, nil
}

// ImageJobWorkerCount reads the opt-in worker limit; zero preserves legacy admission.
func ImageJobWorkerCount() (int, error) {
	raw := strings.TrimSpace(os.Getenv("IMAGE_JOB_WORKERS"))
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 || n > 32 {
		return 0, fmt.Errorf("IMAGE_JOB_WORKERS must be 0..32")
	}
	return n, nil
}

// StartImageJobQueue starts a bounded durable dispatcher before HTTP traffic.
func (h *Handler) StartImageJobQueue(ctx context.Context, workers int) error {
	if workers <= 0 {
		return nil
	}
	if err := h.db.InitImageJobQueue(ctx); err != nil {
		return err
	}
	if err := h.db.ExpireImageJobLeases(ctx, time.Now()); err != nil {
		return err
	}
	memoryWorkers := 0
	if raw := strings.TrimSpace(os.Getenv("IMAGE_JOB_MEMORY_WORKERS")); raw != "" {
		var err error
		memoryWorkers, err = strconv.Atoi(raw)
		if err != nil || memoryWorkers < 0 || memoryWorkers > workers {
			return fmt.Errorf("IMAGE_JOB_MEMORY_WORKERS must be 0..IMAGE_JOB_WORKERS")
		}
	}
	maxQueued, maxWait, err := imageJobQueueLimits()
	if err != nil {
		return err
	}
	proxy.ConfigureImagePipeline(memoryWorkers)
	h.imageQueue = &imageJobQueue{intake: make(chan struct{}, 2), pipeline: memoryWorkers > 0, maxQueuedPerKey: maxQueued, maxQueueWait: maxWait}
	log.Printf("[image-pipeline] configured inflight=%d memory_workers=%d", workers, memoryWorkers)
	proxy.ConfigureImageExecutionLimit(workers)
	for i := 0; i < workers; i++ {
		h.imageQueue.wg.Add(1)
		go func() { defer h.imageQueue.wg.Done(); h.imageQueueWorker(ctx) }()
	}
	log.Printf("[image-queue] started workers=%d input_budget_bytes=%d max_queued_per_key=%d max_queue_wait=%s durable=true", workers, queueInputBudget, maxQueued, maxWait)
	return nil
}

// Body decoding is bounded too, including clients uploading data URLs. URL
// submissions wait here without downloading anything and do not hold a worker.
func (h *Handler) imageQueueIntake(c *gin.Context) (func(), bool) {
	if h.imageQueue == nil {
		return func() {}, true
	}
	select {
	case h.imageQueue.intake <- struct{}{}:
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 48<<20)
		return func() { <-h.imageQueue.intake }, true
	case <-c.Request.Context().Done():
		return nil, false
	}
}

func queueInputPath(value string) (string, error) {
	id := strings.TrimPrefix(value, queueInputPrefix)
	if !strings.HasPrefix(value, queueInputPrefix) {
		return "", fmt.Errorf("invalid queued image reference")
	}
	if _, err := uuid.Parse(id); err != nil {
		return "", fmt.Errorf("invalid queued image reference")
	}
	return filepath.Join(imageAssetDir(), "queue-inputs", id), nil
}

func cleanupQueueInputs(images []string) {
	for _, value := range images {
		if path, err := queueInputPath(value); err == nil {
			_ = os.Remove(path)
		}
	}
}

func checkQueueImage(data []byte) error {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("reference image header cannot be decoded")
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > 40_000_000 {
		return fmt.Errorf("reference image exceeds 40 megapixels")
	}
	return nil
}

// URLs are validated without fetching. Uploaded data URLs are spooled to disk
// so neither the database nor the queue retains their Base64 payload.
func prepareQueueInputs(req *imageGenerationJobPayload) (err error) {
	if len(req.InputImages) > proxy.MaxImageEditInputCount {
		return fmt.Errorf("too many reference images")
	}
	var prepared []string
	defer func() {
		if err != nil {
			cleanupQueueInputs(prepared)
		}
	}()
	remaining := int64(queueInputBudget)
	for _, raw := range req.InputImages {
		value := strings.TrimSpace(raw)
		if strings.HasPrefix(strings.ToLower(value), "data:image/") {
			comma := strings.Index(value, ",")
			if comma < 0 || !strings.Contains(strings.ToLower(value[:comma]), ";base64") {
				return fmt.Errorf("invalid image data URL")
			}
			data, e := io.ReadAll(io.LimitReader(base64.NewDecoder(base64.StdEncoding, strings.NewReader(value[comma+1:])), remaining+1))
			if e != nil || int64(len(data)) > remaining || len(data) > externalInputImageMaxBytes {
				return fmt.Errorf("invalid or oversized reference image")
			}
			if e = checkQueueImage(data); e != nil {
				return e
			}
			remaining -= int64(len(data))
			token := queueInputPrefix + uuid.NewString()
			path, _ := queueInputPath(token)
			if e = os.MkdirAll(filepath.Dir(path), 0700); e != nil {
				return e
			}
			if e = os.WriteFile(path, data, 0600); e != nil {
				_ = os.Remove(path)
				return e
			}
			prepared = append(prepared, token)
		} else {
			u, e := url.Parse(value)
			if e != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") || strings.EqualFold(u.Hostname(), "localhost") {
				return fmt.Errorf("reference must be an HTTP(S) URL or image data URL")
			}
			prepared = append(prepared, value)
		}
	}
	req.InputImages = prepared
	return nil
}

func loadQueueInputs(ctx context.Context, req *imageGenerationJobPayload) error {
	remaining := queueInputBudget
	for i, value := range req.InputImages {
		var data []byte
		var err error
		if strings.HasPrefix(value, queueInputPrefix) {
			path, e := queueInputPath(value)
			if e != nil {
				return e
			}
			f, e := os.Open(path)
			if e != nil {
				return e
			}
			data, err = io.ReadAll(io.LimitReader(f, int64(remaining)+1))
			f.Close()
		} else if strings.HasPrefix(strings.ToLower(value), "data:image/") {
			comma := strings.Index(value, ",")
			if comma < 0 {
				return fmt.Errorf("invalid legacy reference image")
			}
			data, err = io.ReadAll(io.LimitReader(base64.NewDecoder(base64.StdEncoding, strings.NewReader(value[comma+1:])), int64(remaining)+1))
		} else {
			data, _, err = fetchExternalInputImageBytes(ctx, value)
		}
		if err != nil {
			return err
		}
		remaining -= len(data)
		if remaining < 0 {
			return fmt.Errorf("reference images exceed 32 MiB total")
		}
		if err = checkQueueImage(data); err != nil {
			return err
		}
		req.InputImages[i] = "data:" + http.DetectContentType(data) + ";base64," + base64.StdEncoding.EncodeToString(data)
	}
	return nil
}

func (h *Handler) persistQueuedImageJob(c *gin.Context, req imageGenerationJobPayload, key *database.APIKeyRow, status int, external bool) {
	if h.imageQueue != nil && h.imageQueue.maxQueuedPerKey > 0 && key != nil && key.ID > 0 {
		queued, err := h.db.CountQueuedImageJobsByKey(c.Request.Context(), key.ID)
		if err != nil {
			writeInternalError(c, err)
			return
		}
		if queued >= h.imageQueue.maxQueuedPerKey {
			c.Header("Retry-After", "30")
			writeError(c, http.StatusTooManyRequests, fmt.Sprintf("too many queued image jobs for this API key (max %d)", h.imageQueue.maxQueuedPerKey))
			return
		}
	}
	if err := prepareQueueInputs(&req); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	params, err := json.Marshal(req)
	if err != nil {
		cleanupQueueInputs(req.InputImages)
		writeInternalError(c, err)
		return
	}
	keyID, keyName, keyMasked := imageJobAPIKeyMeta(key)
	id, err := h.db.InsertImageGenerationJob(c.Request.Context(), database.ImageGenerationJobInput{Prompt: req.Prompt, ParamsJSON: string(params), APIKeyID: keyID, APIKeyName: keyName, APIKeyMasked: keyMasked})
	if err != nil {
		cleanupQueueInputs(req.InputImages)
		writeInternalError(c, err)
		return
	}
	if req.TemplateID > 0 {
		_ = h.db.IncrementImagePromptTemplateUsage(c.Request.Context(), req.TemplateID)
	}
	log.Printf("[image-queue] accepted job=%d model=%s references=%d params_bytes=%d", id, req.Model, len(req.InputImages), len(params))
	if external {
		// Creation receipts are intentionally small; polling/detail endpoints
		// retain their existing contracts.
		c.JSON(status, gin.H{"job": gin.H{"id": id, "status": database.ImageJobQueued}})
	} else {
		job, err := h.db.GetImageGenerationJob(c.Request.Context(), id)
		if err != nil {
			writeInternalError(c, err)
			return
		}
		c.JSON(status, imageJobResponse{Job: job})
	}
}

func (h *Handler) imageQueueWorker(parent context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-parent.Done():
			return
		case <-ticker.C:
		}
		ctx, release, err := proxy.AcquireImageExecution(parent)
		if err != nil {
			return
		}
		h.runNextQueuedImage(ctx)
		release()
	}
}

func (h *Handler) runNextQueuedImage(ctx context.Context) {
	if err := h.db.ExpireImageJobLeases(ctx, time.Now()); err != nil {
		log.Printf("[image-queue] recovery failed: %v", err)
		return
	}
	h.failStaleQueuedImages(ctx)
	heads, err := h.db.QueuedImageJobHeads(ctx, queueHeadScanLimit)
	if err != nil {
		log.Printf("[image-queue] scan failed: %v", err)
		return
	}
	// Take the key slot before claiming: a key saturated by its own requests
	// must leave its jobs queued instead of parking a worker on them.
	for _, head := range heads {
		key, err := h.db.GetAPIKeyByID(ctx, head.APIKeyID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			log.Printf("[image-queue] key lookup failed job=%d: %v", head.ID, err)
			continue
		}
		var release func()
		if imageJobKeyUsable(key) {
			var ok bool
			if release, ok = h.imageProxy.TryAcquireImageJobKey(key); !ok {
				continue
			}
		}
		owner := uuid.NewString()
		ok, err := h.db.ClaimImageJob(ctx, head.ID, owner, time.Now().Add(queueLeaseDuration))
		if err != nil || !ok {
			if release != nil {
				release()
			}
			if err != nil {
				log.Printf("[image-queue] claim failed job=%d: %v", head.ID, err)
			}
			continue
		}
		h.runClaimedImageJob(ctx, head.ID, owner, key, release)
		return
	}
}

func imageJobKeyUsable(key *database.APIKeyRow) bool {
	return key != nil && key.Enabled && !(key.ExpiresAt.Valid && key.ExpiresAt.Time.Before(time.Now()))
}

// failStaleQueuedImages fails jobs that never started within maxQueueWait.
// They never reached upstream, so retrying them is safe.
func (h *Handler) failStaleQueuedImages(ctx context.Context) {
	if h.imageQueue == nil || h.imageQueue.maxQueueWait <= 0 {
		return
	}
	ids, err := h.db.StaleQueuedImageJobIDs(ctx, time.Now().Add(-h.imageQueue.maxQueueWait), queueStaleScanLimit)
	if err != nil {
		log.Printf("[image-queue] stale scan failed: %v", err)
		return
	}
	for _, id := range ids {
		owner := uuid.NewString()
		ok, err := h.db.ClaimImageJob(ctx, id, owner, time.Now().Add(queueLeaseDuration))
		if err != nil || !ok {
			continue
		}
		if job, err := h.db.GetImageGenerationJob(ctx, id); err == nil {
			var req imageGenerationJobPayload
			if json.Unmarshal([]byte(job.ParamsJSON), &req) == nil {
				cleanupQueueInputs(req.InputImages)
			}
		}
		statusCtx, stop := imageJobStatusContext()
		_ = h.db.FinishLeasedImageJob(statusCtx, id, owner, database.ImageJobFailed, fmt.Sprintf("图片任务排队超过 %s 仍未开始，已取消；未请求上游，可直接重试", h.imageQueue.maxQueueWait), 0)
		stop()
		log.Printf("[image-queue] expired stale queued job=%d", id)
	}
}

// release is the API-key slot taken before the claim; nil when the key is
// unusable and the job is about to fail.
func (h *Handler) runClaimedImageJob(ctx context.Context, id int64, owner string, key *database.APIKeyRow, release func()) {
	if release != nil {
		defer release()
	}
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(20 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-jobCtx.Done():
				return
			case <-t.C:
				leaseCtx, stop := context.WithTimeout(jobCtx, 10*time.Second)
				ok, e := h.db.RenewImageJobLease(leaseCtx, id, owner, time.Now().Add(queueLeaseDuration))
				stop()
				if e != nil || !ok {
					cancel()
					return
				}
			}
		}
	}()
	start := time.Now()
	fail := func(err error) {
		statusCtx, stop := imageJobStatusContext()
		defer stop()
		_ = h.db.FinishLeasedImageJob(statusCtx, id, owner, database.ImageJobFailed, err.Error(), int(time.Since(start).Milliseconds()))
	}
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[image-queue] worker panic job=%d", id)
			fail(fmt.Errorf("image worker interrupted; upstream result may be unknown"))
		}
	}()
	job, err := h.db.GetImageGenerationJob(jobCtx, id)
	if err != nil {
		fail(err)
		return
	}
	var req imageGenerationJobPayload
	if err = json.Unmarshal([]byte(job.ParamsJSON), &req); err != nil {
		fail(fmt.Errorf("invalid persisted image job parameters"))
		return
	}
	inputs := append([]string(nil), req.InputImages...)
	defer cleanupQueueInputs(inputs)
	req.External = true
	if key == nil {
		fail(fmt.Errorf("image job API key is unavailable"))
		return
	}
	if !imageJobKeyUsable(key) {
		fail(fmt.Errorf("image job API key is disabled or deleted"))
		return
	}
	if h.imageQueue != nil && h.imageQueue.pipeline {
		h.runPipelinedImageJob(jobCtx, id, owner, req, key)
		return
	}
	if err = loadQueueInputs(jobCtx, &req); err != nil {
		fail(err)
		return
	}
	log.Printf("[image-queue] executing job=%d model=%s references=%d", id, req.Model, len(req.InputImages))
	opts := imageJobRunOptions{sharedAPIKeyConcurrency: true, queueContext: jobCtx, queueOwner: owner}
	if len(req.InputImages) > 0 {
		h.runImageEditJob(id, req, key, opts)
	} else {
		h.runImageGenerationJob(id, req, key, opts)
	}
}
