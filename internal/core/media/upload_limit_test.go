package media_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"Coves/internal/core/blobs"
	"Coves/internal/core/media"
	"Coves/tests/testkit"
)

const (
	didA = "did:plc:uploadera"
	didB = "did:plc:uploaderb"
	didC = "did:plc:uploaderc"
)

var limitT0 = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

// limitClock is the limiter's clock; tests move it with Set.
type limitClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *limitClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *limitClock) Set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = at
}

// limitUploader counts UploadBlob calls. A gated uploader signals entered on
// each call and then waits until Release or until the request context ends.
// failure, when set, decides the call's error.
type limitUploader struct {
	mu      sync.Mutex
	calls   int
	failure func(ctx context.Context) error
	entered chan struct{}
	gate    chan struct{}
	release sync.Once
}

func (u *limitUploader) UploadBlob(ctx context.Context, data []byte, mimeType string) (*blobs.BlobRef, error) {
	u.mu.Lock()
	u.calls++
	u.mu.Unlock()
	if u.gate != nil {
		u.entered <- struct{}{}
		select {
		case <-u.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if u.failure != nil {
		if err := u.failure(ctx); err != nil {
			return nil, err
		}
	}
	return &blobs.BlobRef{Type: "blob", Ref: map[string]string{"$link": "bafkreilimituploader"}, MimeType: mimeType, Size: len(data)}, nil
}

func (u *limitUploader) Calls() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls
}

func (u *limitUploader) Release() {
	u.release.Do(func() { close(u.gate) })
}

// limitHarness is an UploadService with a cap of 60 per ten minutes over p,
// whose limiter reads clock. Cleanup releases every gated uploader and joins
// every started upload.
type limitHarness struct {
	t        *testing.T
	clock    *limitClock
	limiter  *media.UploadLimiter
	service  *media.UploadService
	workers  sync.WaitGroup
	finished atomic.Int32
	gated    []*limitUploader
}

func newLimitHarness(t *testing.T, p *media.Pipeline) *limitHarness {
	t.Helper()
	return newLimitHarnessWithConfig(t, p, media.UploadLimitConfig{UploadsPerTenMinutes: 60, BytesPerDay: 500000000, WarnFraction: 0.8})
}

func newLimitHarnessWithConfig(t *testing.T, p *media.Pipeline, config media.UploadLimitConfig) *limitHarness {
	t.Helper()
	clock := &limitClock{now: limitT0}
	limiter, err := media.NewUploadLimiterWithClock(config, clock.Now)
	if err != nil {
		t.Fatalf("NewUploadLimiterWithClock: %v", err)
	}
	h := &limitHarness{t: t, clock: clock, limiter: limiter, service: media.NewUploadService(p, limiter)}
	t.Cleanup(func() {
		for _, u := range h.gated {
			u.Release()
		}
		h.workers.Wait()
	})
	return h
}

func validUpload() media.Input {
	return media.Input{Data: testkit.TestJPEG(8, 8), DeclaredContentType: "image/jpeg"}
}

func (h *limitHarness) gatedUploader() *limitUploader {
	u := &limitUploader{entered: make(chan struct{}, 16), gate: make(chan struct{})}
	h.gated = append(h.gated, u)
	return u
}

func (h *limitHarness) uploadWithin(timeout time.Duration, did string, uploader media.BlobUploader, in media.Input) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, err := h.service.Upload(ctx, did, uploader, in)
	return err
}

// store sets the clock to at and stores n uploads for did.
func (h *limitHarness) store(did string, n int, at time.Time) {
	h.t.Helper()
	h.clock.Set(at)
	uploader := &limitUploader{}
	for i := range n {
		if err := h.uploadWithin(10*time.Second, did, uploader, validUpload()); err != nil {
			h.t.Fatalf("storing upload %d of %d for %s: %v", i+1, n, did, err)
		}
	}
}

func (h *limitHarness) expectStored(did, what string) {
	h.t.Helper()
	uploader := &limitUploader{}
	if err := h.uploadWithin(10*time.Second, did, uploader, validUpload()); err != nil {
		h.t.Fatalf("%s: got %v, want the upload stored", what, err)
	}
	if uploader.Calls() != 1 {
		h.t.Fatalf("%s: uploader calls got %d, want 1", what, uploader.Calls())
	}
}

func (h *limitHarness) expectRefused(did, what string) {
	h.t.Helper()
	uploader := &limitUploader{}
	err := h.uploadWithin(5*time.Second, did, uploader, validUpload())
	if err == nil {
		h.t.Fatalf("%s: want UploadRateLimited, got nil", what)
	}
	expectCode(h.t, err, media.ErrorCodeUploadRateLimited)
	if uploader.Calls() != 0 {
		h.t.Fatalf("%s: uploader calls got %d, want 0", what, uploader.Calls())
	}
}

func (h *limitHarness) expectTracked(want int, what string) {
	h.t.Helper()
	if got := h.limiter.TrackedDIDs(); got != want {
		h.t.Fatalf("%s: TrackedDIDs got %d, want %d", what, got, want)
	}
}

// start runs a valid upload for did in a worker once barrier is closed (nil
// starts it at once) and returns its result channel.
func (h *limitHarness) start(did string, uploader media.BlobUploader, barrier <-chan struct{}) <-chan error {
	done := make(chan error, 1)
	h.workers.Add(1)
	go func() {
		defer h.workers.Done()
		if barrier != nil {
			<-barrier
		}
		err := h.uploadWithin(10*time.Second, did, uploader, validUpload())
		done <- err
		h.finished.Add(1)
	}()
	return done
}

func receiveUpload(t *testing.T, done <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: upload did not return within 10s", what)
		return nil
	}
}

func expectEntered(t *testing.T, u *limitUploader, what string) {
	t.Helper()
	select {
	case <-u.entered:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: upload did not reach the uploader within 10s", what)
	}
}

func TestUploadServiceRefusesOverCapBeforeThePipeline(t *testing.T) {
	p := pipeline(t, 2)
	h := newLimitHarness(t, p)
	h.store(didA, 60, limitT0)

	var releases []func()
	var releaseOnce sync.Once
	releaseSlots := func() {
		releaseOnce.Do(func() {
			for _, release := range releases {
				release()
			}
		})
	}
	t.Cleanup(releaseSlots)
	for range 2 {
		release, err := media.HoldDecodeSlot(p, context.Background())
		if err != nil {
			t.Fatalf("hold decode slot: %v", err)
		}
		releases = append(releases, release)
	}

	uploader := &limitUploader{}
	err := h.uploadWithin(2*time.Second, didA, uploader, validUpload())
	if err == nil {
		t.Fatal("A's 61st upload with every decode slot held: want UploadRateLimited, got nil")
	}
	expectCode(t, err, media.ErrorCodeUploadRateLimited)
	if uploader.Calls() != 0 {
		t.Fatalf("A's refused upload: uploader calls got %d, want 0", uploader.Calls())
	}

	releaseSlots()
	h.expectStored(didB, "B's upload after the slots are released")
}

func TestUploadServiceCountsUploadsInARollingWindowFromStoreTime(t *testing.T) {
	t.Run("a: uploads stop counting once more than ten minutes old", func(t *testing.T) {
		h := newLimitHarness(t, pipeline(t, 1))
		h.store(didA, 1, limitT0)
		h.store(didA, 59, limitT0.Add(time.Minute))

		h.clock.Set(limitT0.Add(10 * time.Minute))
		h.expectRefused(didA, "at t0+10m, with the t0 upload exactly ten minutes old")

		h.clock.Set(limitT0.Add(10*time.Minute + time.Second))
		h.expectStored(didA, "at t0+10m+1s, after the t0 upload left the window")
		h.expectRefused(didA, "the next upload at t0+10m+1s")
	})

	t.Run("b: an upload counts from when it was stored, not when it started", func(t *testing.T) {
		h := newLimitHarness(t, pipeline(t, 1))
		slow := h.gatedUploader()
		done := h.start(didA, slow, nil)
		expectEntered(t, slow, "the upload started at t0")
		h.clock.Set(limitT0.Add(5 * time.Minute))
		slow.Release()
		if err := receiveUpload(t, done, "the upload started at t0"); err != nil {
			t.Fatalf("the upload started at t0 and stored at t0+5m: got %v, want stored", err)
		}
		h.store(didA, 59, limitT0.Add(6*time.Minute))

		h.clock.Set(limitT0.Add(14 * time.Minute))
		h.expectRefused(didA, "at t0+14m, nine minutes after the slow upload was stored")

		h.clock.Set(limitT0.Add(15*time.Minute + time.Second))
		h.expectStored(didA, "at t0+15m+1s, after the slow upload left the window")
		h.expectRefused(didA, "the next upload at t0+15m+1s")
	})
}

func TestUploadServiceDoesNotCountPipelineRejections(t *testing.T) {
	rows := []struct {
		name     string
		data     []byte
		declared string
		code     media.ErrorCode
	}{
		{"unsupported format", []byte("not an image at all"), "image/jpeg", media.ErrorCodeUnsupportedImageFormat},
		{"invalid image", jpegHeader(64, 0), "image/jpeg", media.ErrorCodeInvalidImage},
		{"dimensions too large", pngHeader(10001, 5000), "image/png", media.ErrorCodeImageDimensionsTooLarge},
		{"output over 10 MB", testkit.JPEGThatGrowsPastTenMegabytesWhenDownscaled(), "image/jpeg", media.ErrorCodeImageTooLarge},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			h := newLimitHarness(t, pipelineWithMaxLongEdge(t, 1, 1000))
			h.store(didA, 59, limitT0)

			err := h.uploadWithin(10*time.Second, didA, &limitUploader{}, media.Input{Data: row.data, DeclaredContentType: row.declared})
			expectCode(t, err, row.code)

			h.expectStored(didA, "the upload after the rejection")
			h.expectRefused(didA, "the upload after that")
		})
	}
}

func TestUploadServiceDoesNotCountFailedPDSStores(t *testing.T) {
	rows := []struct {
		name    string
		failure func(cancel context.CancelFunc) func(ctx context.Context) error
		check   func(t *testing.T, err error)
	}{
		{
			name: "uploader returns an error",
			failure: func(context.CancelFunc) func(context.Context) error {
				return func(context.Context) error { return errors.New("PDS refused the blob") }
			},
			check: func(t *testing.T, err error) { expectCode(t, err, media.ErrorCodePDSUploadFailed) },
		},
		{
			name: "request context cancelled during the store",
			failure: func(cancel context.CancelFunc) func(context.Context) error {
				return func(ctx context.Context) error {
					cancel()
					return ctx.Err()
				}
			},
			check: func(t *testing.T, err error) {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled store: got %v, want context.Canceled", err)
				}
			},
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			h := newLimitHarness(t, pipeline(t, 1))
			h.store(didA, 59, limitT0)

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err := h.service.Upload(ctx, didA, &limitUploader{failure: row.failure(cancel)}, validUpload())
			row.check(t, err)

			h.expectStored(didA, "the upload after the failed store")
			h.expectRefused(didA, "the upload after that")
		})
	}
}

func TestUploadServiceCountsInFlightUploads(t *testing.T) {
	t.Run("a: an upload in flight holds the last slot", func(t *testing.T) {
		h := newLimitHarness(t, pipeline(t, 1))
		h.store(didA, 59, limitT0)
		slow := h.gatedUploader()
		done := h.start(didA, slow, nil)
		expectEntered(t, slow, "upload 1")

		h.expectRefused(didA, "upload 2 while upload 1 is in flight")

		slow.Release()
		if err := receiveUpload(t, done, "upload 1"); err != nil {
			t.Fatalf("upload 1: got %v, want stored", err)
		}
		h.expectRefused(didA, "the upload after upload 1 was stored")
	})

	t.Run("b: one of eight simultaneous uploads gets the last slot", func(t *testing.T) {
		h := newLimitHarness(t, pipeline(t, 1))
		h.store(didA, 59, limitT0)
		slow := h.gatedUploader()
		barrier := make(chan struct{})
		dones := make([]<-chan error, 0, 8)
		for range 8 {
			dones = append(dones, h.start(didA, slow, barrier))
		}
		close(barrier)

		testkit.WaitFor(t, 10*time.Second, func() (bool, error) {
			return slow.Calls()+int(h.finished.Load()) == 8, nil
		}, testkit.WithDescription("every upload either refused or at the uploader"))
		if slow.Calls() != 1 {
			t.Fatalf("eight simultaneous uploads with one slot left: uploader calls got %d, want 1", slow.Calls())
		}
		var pending <-chan error
		refused := 0
		for _, done := range dones {
			select {
			case err := <-done:
				expectCode(t, err, media.ErrorCodeUploadRateLimited)
				refused++
			default:
				pending = done
			}
		}
		if refused != 7 {
			t.Fatalf("refused uploads: got %d, want 7", refused)
		}

		slow.Release()
		if err := receiveUpload(t, pending, "the upload that got the last slot"); err != nil {
			t.Fatalf("the upload that got the last slot: got %v, want stored", err)
		}
		h.expectRefused(didA, "the upload after the eight")
	})
}

func TestUploadLimiterForgetsIdleDIDs(t *testing.T) {
	// B3 lifecycle cycle: count expiry must not evict the retained byte window.
	t.Run("a: a DID is retained until its stored bytes leave the 24 hour window", func(t *testing.T) {
		h := newLimitHarness(t, pipeline(t, 1))
		h.store(didA, 60, limitT0)

		h.clock.Set(limitT0.Add(10*time.Minute + time.Second))
		h.expectTracked(1, "A before any further upload")
		h.expectStored(didB, "B's upload after A's count window expired")
		h.expectTracked(2, "A's bytes retained after B's upload swept idle DIDs")

		h.clock.Set(limitT0.Add(24 * time.Hour))
		h.expectStored(didB, "B's upload when A's bytes are exactly 24 hours old")
		h.expectTracked(2, "A retained at the exact byte expiry boundary")

		h.clock.Set(limitT0.Add(24*time.Hour + time.Nanosecond))
		h.expectStored(didB, "B's upload once A's bytes are more than 24 hours old")
		h.expectTracked(1, "only B remains after A's byte window emptied")
	})

	t.Run("b: a pipeline rejection leaves no state", func(t *testing.T) {
		h := newLimitHarness(t, pipeline(t, 1))
		h.store(didA, 1, limitT0)

		err := h.uploadWithin(10*time.Second, didC, &limitUploader{}, media.Input{Data: []byte("not an image at all"), DeclaredContentType: "image/jpeg"})
		expectCode(t, err, media.ErrorCodeUnsupportedImageFormat)

		h.expectTracked(1, "A stored and C rejected")
	})

	t.Run("c: an in-flight upload survives pruning", func(t *testing.T) {
		h := newLimitHarness(t, pipeline(t, 1))
		h.store(didA, 59, limitT0)
		slow := h.gatedUploader()
		done := h.start(didA, slow, nil)
		expectEntered(t, slow, "A's in-flight upload")

		h.clock.Set(limitT0.Add(11 * time.Minute))
		h.expectStored(didB, "B's upload at t0+11m")
		h.expectTracked(2, "A in flight and B stored")

		slow.Release()
		if err := receiveUpload(t, done, "A's in-flight upload"); err != nil {
			t.Fatalf("A's in-flight upload: got %v, want stored", err)
		}
	})
}

// captureJSONLog routes slog's default logger into a JSON buffer until the
// test ends.
func captureJSONLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })
	var captured bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&captured, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return &captured
}

// Not parallel: it reads the process-wide refusal counter and swaps the
// default logger.
func TestUploadServiceRefusalLogAndCounter(t *testing.T) {
	before := media.UploadRateLimitRefusalCount()

	h := newLimitHarness(t, pipeline(t, 1))
	h.store(didA, 59, limitT0)
	err := h.uploadWithin(10*time.Second, didA, &limitUploader{}, media.Input{Data: []byte("not an image at all"), DeclaredContentType: "image/jpeg"})
	expectCode(t, err, media.ErrorCodeUnsupportedImageFormat)
	slow := h.gatedUploader()
	h.start(didA, slow, nil)
	expectEntered(t, slow, "A's in-flight upload")

	logged := captureJSONLog(t)
	refusedUploader := &limitUploader{}
	refusal := h.uploadWithin(5*time.Second, didA, refusedUploader, validUpload())

	if delta := media.UploadRateLimitRefusalCount() - before; delta != 1 {
		t.Errorf("refusal counter delta after 59 stores, 1 rejection, 1 in flight and 1 refusal: got %d, want 1", delta)
	}

	var lines []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logged.String()), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("parsing log line %q: %v", line, err)
		}
		lines = append(lines, entry)
	}
	if len(lines) != 1 {
		t.Errorf("log lines: got %d, want exactly 1 (the refusal); log %q", len(lines), logged.String())
	} else {
		entry := lines[0]
		keys := slices.Sorted(maps.Keys(entry))
		wantKeys := []string{"cap", "did", "level", "limit", "msg", "time", "usage"}
		if !slices.Equal(keys, wantKeys) {
			t.Errorf("refusal log attribute keys: got %v, want %v", keys, wantKeys)
		}
		want := map[string]any{"level": "WARN", "did": didA, "limit": "uploads_per_10min", "usage": float64(60), "cap": float64(60)}
		for key, value := range want {
			if entry[key] != value {
				t.Errorf("refusal log %s: got %v (%T), want %v", key, entry[key], entry[key], value)
			}
		}
	}

	if refusal == nil {
		t.Fatal("A's upload with 59 stored and 1 in flight: want UploadRateLimited, got nil")
	}
	expectCode(t, refusal, media.ErrorCodeUploadRateLimited)
	if refusedUploader.Calls() != 0 {
		t.Fatalf("refused upload: uploader calls got %d, want 0", refusedUploader.Calls())
	}
}

func TestNewUploadLimiterRejectsInvalidConfig(t *testing.T) {
	limiter, err := media.NewUploadLimiter(media.UploadLimitConfig{UploadsPerTenMinutes: 0, BytesPerDay: 500000000, WarnFraction: 0.8})
	if !errors.Is(err, media.ErrInvalidConfig) {
		t.Fatalf("NewUploadLimiter(UploadsPerTenMinutes: 0): got limiter %v, error %v; want ErrInvalidConfig", limiter, err)
	}
}
