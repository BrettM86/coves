package media_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"Coves/internal/core/media"
	"Coves/tests/testkit"
)

type nearCapWarning struct {
	did, limit string
	usage, cap int64
}

// Not parallel: these contracts capture the default logger and read process
// counters. Check log output first so a missing counter stub cannot explain RED.
func expectNearCapWarnings(t *testing.T, logged *bytes.Buffer, before int64, want ...nearCapWarning) {
	t.Helper()
	var entries []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logged.String()), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("parse warning log: %v", err)
		}
		entries = append(entries, entry)
	}
	if len(entries) != len(want) {
		t.Fatalf("near-cap WARN lines: got %d, want %d; log %q", len(entries), len(want), logged.String())
	}
	for index, expected := range want {
		entry := entries[index]
		for key, value := range map[string]any{
			"level": "WARN", "did": expected.did, "limit": expected.limit,
			"usage": float64(expected.usage), "cap": float64(expected.cap),
		} {
			if entry[key] != value {
				t.Errorf("warning %d %s: got %v, want %v", index+1, key, entry[key], value)
			}
		}
	}
	if delta := media.UploadNearCapWarningCount() - before; delta != int64(len(want)) {
		t.Errorf("near-cap warning counter delta: got %d, want %d", delta, len(want))
	}
}

// B6, T0: one limiter crosses count first, then bytes for the same DID while
// count remains high. A second DID crosses both independently. All stores
// succeed, distinguishing near-cap events from rate-limit refusal events.
func TestUploadServiceNearCapWarningsAreIndependentPerDIDAndCap(t *testing.T) {
	// This metadata-free JPEG is stored unchanged; fixture bytes supply its size.
	storedSize := int64(len(validUpload().Data))
	h := newLimitHarnessWithConfig(t, pipeline(t, 1), media.UploadLimitConfig{
		UploadsPerTenMinutes: 10, BytesPerDay: 11 * storedSize, WarnFraction: 0.8,
	})
	logged := captureJSONLog(t)
	before := media.UploadNearCapWarningCount()
	refusalsBefore := media.UploadRateLimitRefusalCount()
	countA := nearCapWarning{didA, "uploads_per_10min", 8, 10}
	bytesA := nearCapWarning{didA, "bytes_per_day", 9 * storedSize, 11 * storedSize}
	countB := nearCapWarning{didB, "uploads_per_10min", 8, 10}
	bytesB := nearCapWarning{didB, "bytes_per_day", 9 * storedSize, 11 * storedSize}

	h.store(didA, 7, limitT0)
	expectNearCapWarnings(t, logged, before)
	h.expectStored(didA, "A reaches the count threshold")
	expectNearCapWarnings(t, logged, before, countA)
	h.expectStored(didA, "A reaches the byte threshold while count is still high")
	expectNearCapWarnings(t, logged, before, countA, bytesA)
	h.expectStored(didA, "A remains above both thresholds")
	expectNearCapWarnings(t, logged, before, countA, bytesA)

	h.store(didB, 8, limitT0)
	expectNearCapWarnings(t, logged, before, countA, bytesA, countB)
	h.expectStored(didB, "B reaches its independent byte threshold")
	h.expectStored(didB, "B remains above both thresholds")
	expectNearCapWarnings(t, logged, before, countA, bytesA, countB, bytesB)
	if delta := media.UploadRateLimitRefusalCount() - refusalsBefore; delta != 0 {
		t.Errorf("successful stores incremented refusal counter by %d, want 0", delta)
	}
}

// B6, T0: reservations, a pipeline rejection and a failed PDS store do not
// trigger the warning; only the successful store at the threshold does.
func TestUploadServiceNearCapWarningsUseOnlyStoredUsage(t *testing.T) {
	storedSize := int64(len(validUpload().Data))
	for _, row := range []struct {
		name   string
		config media.UploadLimitConfig
		want   nearCapWarning
	}{
		{"count", media.UploadLimitConfig{UploadsPerTenMinutes: 5, BytesPerDay: 500000000, WarnFraction: 0.8}, nearCapWarning{didA, "uploads_per_10min", 4, 5}},
		{"bytes", media.UploadLimitConfig{UploadsPerTenMinutes: 60, BytesPerDay: 5 * storedSize, WarnFraction: 0.8}, nearCapWarning{didA, "bytes_per_day", 4 * storedSize, 5 * storedSize}},
	} {
		t.Run(row.name, func(t *testing.T) {
			h := newLimitHarnessWithConfig(t, pipeline(t, 1), row.config)
			logged := captureJSONLog(t)
			before := media.UploadNearCapWarningCount()
			h.store(didA, 3, limitT0)
			expectCode(t, h.uploadWithin(10*time.Second, didA, &limitUploader{}, media.Input{
				Data: []byte("not an image"), DeclaredContentType: "image/jpeg",
			}), media.ErrorCodeUnsupportedImageFormat)
			expectNearCapWarnings(t, logged, before)

			slow := h.gatedUploader()
			slow.failure = func(context.Context) error { return errors.New("PDS refused the blob") }
			done := h.start(didA, slow, nil)
			expectEntered(t, slow, "reservation reaches the threshold but is not stored")
			expectNearCapWarnings(t, logged, before)
			slow.Release()
			expectCode(t, receiveUpload(t, done, "failed reserved store"), media.ErrorCodePDSUploadFailed)
			expectNearCapWarnings(t, logged, before)

			h.expectStored(didA, "successful store reaches the threshold after the failed reservation")
			expectNearCapWarnings(t, logged, before, row.want)
			h.expectStored(didA, "further stored usage stays above the threshold")
			expectNearCapWarnings(t, logged, before, row.want)
		})
	}
}

// B7, T0 lifecycle cycle: implement separately in GREEN. Expiry to exactly
// the threshold keeps the latch set; expiry below it re-arms the next crossing.
func TestUploadServiceNearCapWarningsRearmOnlyBelowThreshold(t *testing.T) {
	storedSize := int64(len(validUpload().Data))
	for _, row := range []struct {
		name   string
		window time.Duration
		config media.UploadLimitConfig
		want   nearCapWarning
	}{
		{"count", 10 * time.Minute, media.UploadLimitConfig{UploadsPerTenMinutes: 10, BytesPerDay: 500000000, WarnFraction: 0.8}, nearCapWarning{didA, "uploads_per_10min", 8, 10}},
		{"bytes", 24 * time.Hour, media.UploadLimitConfig{UploadsPerTenMinutes: 1000, BytesPerDay: 10 * storedSize, WarnFraction: 0.8}, nearCapWarning{didA, "bytes_per_day", 8 * storedSize, 10 * storedSize}},
	} {
		t.Run(row.name, func(t *testing.T) {
			h := newLimitHarnessWithConfig(t, pipeline(t, 1), row.config)
			logged := captureJSONLog(t)
			before := media.UploadNearCapWarningCount()
			h.store(didA, 1, limitT0)
			h.store(didA, 8, limitT0.Add(time.Minute))
			expectNearCapWarnings(t, logged, before, row.want)

			// Only the oldest store expires: eight remain, exactly 80%.
			h.clock.Set(limitT0.Add(row.window + time.Nanosecond))
			h.expectStored(didA, "expiry to exactly the threshold must not rearm the warning")
			expectNearCapWarnings(t, logged, before, row.want)

			// The eight-store cohort now expires, leaving one below 80%.
			h.store(didA, 7, limitT0.Add(row.window+time.Minute+time.Nanosecond))
			expectNearCapWarnings(t, logged, before, row.want, row.want)
		})
	}
}

// C1 regression, T0: 50*0.14 rounds above 7. The unchanged 1024-byte
// fixture gives the same boundary for bytes: 51200*0.14 rounds above 7168.
func TestUploadServiceNearCapWarningsHonorFractionAtEquality(t *testing.T) {
	input := validUpload()
	padding := 1024 - len(input.Data) - len(jpegICCProfileSegment(nil))
	input.Data = testkit.JPEGWithSegments(input.Data, jpegICCProfileSegment(make([]byte, padding)))
	for _, row := range []struct {
		name   string
		window time.Duration
		config media.UploadLimitConfig
		want   nearCapWarning
	}{
		{"count", 10 * time.Minute, media.UploadLimitConfig{UploadsPerTenMinutes: 50, BytesPerDay: 500000000, WarnFraction: 0.14}, nearCapWarning{didA, "uploads_per_10min", 7, 50}},
		{"bytes", 24 * time.Hour, media.UploadLimitConfig{UploadsPerTenMinutes: 1000, BytesPerDay: 51200, WarnFraction: 0.14}, nearCapWarning{didA, "bytes_per_day", 7168, 51200}},
	} {
		t.Run(row.name, func(t *testing.T) {
			for _, scenario := range []string{"threshold", "rearm"} {
				t.Run(scenario, func(t *testing.T) {
					h := newLimitHarnessWithConfig(t, pipeline(t, 1), row.config)
					logged := captureJSONLog(t)
					before := media.UploadNearCapWarningCount()
					store := func(count int, at time.Time) {
						t.Helper()
						h.clock.Set(at)
						for range count {
							uploader := &recordingUploader{}
							if err := h.uploadWithin(10*time.Second, didA, uploader, input); err != nil {
								t.Fatalf("store at %s: got %v, want stored", at, err)
							}
							if len(uploader.data) != 1024 {
								t.Fatalf("stored fixture: got %d bytes, want 1024", len(uploader.data))
							}
						}
					}
					if scenario == "threshold" {
						store(6, limitT0)
						expectNearCapWarnings(t, logged, before)
						store(1, limitT0)
						expectNearCapWarnings(t, logged, before, row.want)
						store(1, limitT0)
						expectNearCapWarnings(t, logged, before, row.want)
						return
					}

					// Eight stores establish a latch even when the threshold is
					// rounded late. Isolate equality from the threshold assertion.
					store(1, limitT0)
					store(2, limitT0.Add(time.Minute))
					store(5, limitT0.Add(2*time.Minute))
					logged.Reset()
					before = media.UploadNearCapWarningCount()

					// One store expires, leaving seven exactly at 14%; adding
					// another must keep the warning latched.
					store(1, limitT0.Add(row.window+time.Nanosecond))
					expectNearCapWarnings(t, logged, before)

					// Two more expire, leaving six below 14%; the next store
					// reaches seven and must warn again.
					store(1, limitT0.Add(row.window+time.Minute+time.Nanosecond))
					expectNearCapWarnings(t, logged, before, row.want)
				})
			}
		})
	}
}
