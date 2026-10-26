package media_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image/color"
	"image/jpeg"
	"strings"
	"testing"
	"time"

	"Coves/internal/core/media"
	"Coves/tests/testkit"
)

// B1, T0: the allowance pays for sanitized/downscaled PDS bytes. The cap is
// literal; captured output is used only to verify fixture/cap relationships.
func TestUploadServiceChargesStoredBytesAfterSanitizingAndDownscaling(t *testing.T) {
	input := media.Input{
		Data: testkit.JPEGWithSegments(testkit.TestJPEGColor(128, 128, color.RGBA{R: 255, A: 255}),
			testkit.JPEGExifGPSegment(1), testkit.JPEGMarkerSegment(0xfe, bytes.Repeat([]byte("x"), 5000))),
		DeclaredContentType: "image/jpeg",
	}
	if len(input.Data) <= 1500 {
		t.Fatal("fixture must send more than the 1500-byte daily cap")
	}
	h := newLimitHarnessWithConfig(t, pipelineWithMaxLongEdge(t, 1, 8), media.UploadLimitConfig{
		UploadsPerTenMinutes: 60, BytesPerDay: 1500, WarnFraction: 0.8,
	})
	stored := &recordingUploader{}
	if err := h.uploadWithin(10*time.Second, didA, stored, input); err != nil {
		t.Fatalf("first downscaled upload: got %v, want stored despite input exceeding the daily cap", err)
	}
	output := append([]byte(nil), stored.data...)
	config, err := jpeg.DecodeConfig(bytes.NewReader(output))
	if err != nil || config.Width != 8 || config.Height != 8 {
		t.Fatalf("PDS bytes: got dimensions %dx%d, error %v, want 8x8 JPEG", config.Width, config.Height, err)
	}
	if bytes.Contains(output, []byte("Exif")) || len(output) >= len(input.Data) {
		t.Fatal("PDS fixture output must have EXIF removed and be smaller than the sent bytes")
	}
	if 2*len(output) > 1500 || 3*len(output) <= 1500 {
		t.Fatalf("captured output is %d bytes: fixture must fit twice, but not three times, in 1500 bytes", len(output))
	}
	if err := h.uploadWithin(10*time.Second, didA, stored, input); err != nil {
		t.Fatalf("second downscaled upload: got %v, want stored using the remaining output-byte allowance", err)
	}
	if !bytes.Equal(stored.data, output) {
		t.Fatal("repeated fixture must store identical bytes")
	}
	refused := &limitUploader{}
	expectCode(t, h.uploadWithin(10*time.Second, didA, refused, input), media.ErrorCodeUploadRateLimited)
	if refused.Calls() != 0 {
		t.Fatal("third output exceeds 1500 stored bytes and must not reach the PDS")
	}
}

// B2, T0: byte refusal retains the existing error, refusal log and counter.
// These service gates are the owning level; handler mapping is already pinned.
func TestUploadServiceEnforcesTheDailyByteCap(t *testing.T) {
	t.Run("a: an output over remaining bytes is refused and a fitting output is stored per DID", func(t *testing.T) {
		small := validUpload()
		large := media.Input{Data: testkit.TestJPEG(64, 64), DeclaredContentType: "image/jpeg"}
		cap := int64(2*len(small.Data) + 1)
		if len(large.Data) <= len(small.Data)+1 {
			t.Fatal("large fixture must exceed the remaining daily bytes after one small upload")
		}
		h := newLimitHarnessWithConfig(t, pipeline(t, 1), media.UploadLimitConfig{
			UploadsPerTenMinutes: 60, BytesPerDay: cap, WarnFraction: 1,
		})
		h.expectStored(didA, "A's first small upload")
		before := media.UploadRateLimitRefusalCount()
		logged := captureJSONLog(t)
		refused := &limitUploader{}
		err := h.uploadWithin(10*time.Second, didA, refused, large)
		expectCode(t, err, media.ErrorCodeUploadRateLimited)
		if refused.Calls() != 0 {
			t.Fatal("output over the remaining byte allowance reached the PDS")
		}
		if delta := media.UploadRateLimitRefusalCount() - before; delta != 1 {
			t.Errorf("byte refusal counter delta: got %d, want 1", delta)
		}
		lines := strings.Split(strings.TrimSpace(logged.String()), "\n")
		if len(lines) != 1 {
			t.Fatalf("byte refusal: got %d log lines, want one WARN; log %q", len(lines), logged.String())
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
			t.Fatalf("parse refusal log: %v", err)
		}
		for key, want := range map[string]any{
			"level": "WARN", "did": didA, "limit": "bytes_per_day",
			"usage": float64(len(small.Data)), "cap": float64(cap),
		} {
			if entry[key] != want {
				t.Errorf("byte refusal log %s: got %v, want %v", key, entry[key], want)
			}
		}
		h.expectStored(didA, "A's second small upload still fits after the byte refusal")
		h.expectRefused(didA, "A's third small upload exceeds the daily byte cap")
		h.expectStored(didB, "B's byte allowance is independent of A's")
	})

	t.Run("b: stored bytes exactly filling the cap refuse before the pipeline", func(t *testing.T) {
		h := newLimitHarnessWithConfig(t, pipeline(t, 1), media.UploadLimitConfig{
			UploadsPerTenMinutes: 60, BytesPerDay: int64(len(validUpload().Data)), WarnFraction: 0.8,
		})
		h.expectStored(didA, "A's output exactly fills the byte cap")
		refused := &limitUploader{}
		err := h.uploadWithin(10*time.Second, didA, refused, media.Input{
			Data: []byte("not an image"), DeclaredContentType: "image/jpeg",
		})
		expectCode(t, err, media.ErrorCodeUploadRateLimited)
		if refused.Calls() != 0 {
			t.Fatal("full byte allowance must refuse before any PDS call")
		}
	})
}

// B3, T0 lifecycle cycle: implement separately in GREEN. Byte history survives
// the shorter count window, uses strict 24h expiry and timestamps completion.
func TestUploadServiceCountsStoredBytesForTwentyFourHours(t *testing.T) {
	t.Run("a: count expiry and another DID sweep retain bytes through the exact 24 hour boundary", func(t *testing.T) {
		h := newLimitHarnessWithConfig(t, pipeline(t, 1), media.UploadLimitConfig{
			UploadsPerTenMinutes: 60, BytesPerDay: int64(len(validUpload().Data)), WarnFraction: 0.8,
		})
		h.store(didA, 1, limitT0)
		h.clock.Set(limitT0.Add(10*time.Minute + time.Nanosecond))
		h.expectStored(didB, "B triggers an idle-DID sweep after A's count window expires")
		h.expectRefused(didA, "A's stored bytes still count after ten minutes")
		h.clock.Set(limitT0.Add(24 * time.Hour))
		h.expectRefused(didA, "A's stored bytes still count when exactly 24 hours old")
		h.clock.Set(limitT0.Add(24*time.Hour + time.Nanosecond))
		h.expectStored(didA, "A's bytes free only once more than 24 hours old")
	})

	t.Run("b: a delayed store starts its byte window at completion", func(t *testing.T) {
		h := newLimitHarnessWithConfig(t, pipeline(t, 1), media.UploadLimitConfig{
			UploadsPerTenMinutes: 60, BytesPerDay: int64(len(validUpload().Data)), WarnFraction: 0.8,
		})
		slow := h.gatedUploader()
		done := h.start(didA, slow, nil)
		expectEntered(t, slow, "A's upload admitted at t0")
		h.clock.Set(limitT0.Add(5 * time.Hour))
		slow.Release()
		if err := receiveUpload(t, done, "A's store completed at t0+5h"); err != nil {
			t.Fatalf("delayed store: got %v, want stored", err)
		}
		h.clock.Set(limitT0.Add(24*time.Hour + time.Nanosecond))
		h.expectRefused(didA, "admission is over 24 hours old but completion is only 19 hours old")
		h.clock.Set(limitT0.Add(29 * time.Hour))
		h.expectRefused(didA, "completion is exactly 24 hours old")
		h.clock.Set(limitT0.Add(29*time.Hour + time.Nanosecond))
		h.expectStored(didA, "completion is more than 24 hours old")
	})
}

// B4, T0 guard: A1 bite proves shared/count failure release with cap one.
// A2 must also bite byte-reservation leakage once that mechanism exists.
func TestUploadServiceGuardReleasesByteAndCountReservationsAfterFailures(t *testing.T) {
	h := newLimitHarnessWithConfig(t, pipeline(t, 1), media.UploadLimitConfig{
		UploadsPerTenMinutes: 1, BytesPerDay: int64(len(validUpload().Data)), WarnFraction: 0.8,
	})
	failed := &limitUploader{failure: func(_ context.Context) error { return errors.New("PDS refused the blob") }}
	expectCode(t, h.uploadWithin(10*time.Second, didA, failed, validUpload()), media.ErrorCodePDSUploadFailed)
	err := h.uploadWithin(10*time.Second, didA, &limitUploader{}, media.Input{
		Data: []byte("not an image"), DeclaredContentType: "image/jpeg",
	})
	expectCode(t, err, media.ErrorCodeUnsupportedImageFormat)
	h.expectStored(didA, "the successful retry uses the count slot and all bytes released by both failures")
}

// B5, T0 concurrency cycle: implement separately in GREEN. The held PDS call
// owns its output-byte reservation while the second output reaches admission.
func TestUploadServiceReservesInFlightBytesBeforeThePDSCall(t *testing.T) {
	h := newLimitHarnessWithConfig(t, pipeline(t, 1), media.UploadLimitConfig{
		UploadsPerTenMinutes: 60, BytesPerDay: int64(2*len(validUpload().Data) - 1), WarnFraction: 0.8,
	})
	slow := h.gatedUploader()
	done := h.start(didA, slow, nil)
	expectEntered(t, slow, "A's first output held inside the PDS")
	h.expectRefused(didA, "A's second output fits alone but exceeds the cap together with the first reservation")
	slow.Release()
	if err := receiveUpload(t, done, "A's first upload"); err != nil {
		t.Fatalf("first upload after releasing PDS: got %v, want stored", err)
	}
	h.expectRefused(didA, "the first stored output leaves too few bytes for another output")
}
