package media

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// ErrorCodeUploadRateLimited reports that a per-DID upload cap was reached.
const ErrorCodeUploadRateLimited ErrorCode = "UploadRateLimited"

// DefaultUploadsPerTenMinutes is the per-DID upload cap when none is configured.
const DefaultUploadsPerTenMinutes = 60

// DefaultUploadBytesPerDay is the stored-byte cap per DID, in decimal bytes.
const DefaultUploadBytesPerDay int64 = 500_000_000

// DefaultUploadWarnFraction is the stored usage fraction that triggers a warning.
const DefaultUploadWarnFraction = 0.8

// uploadWindow is the rolling window the upload cap applies to. An upload
// stored at T counts while now-T <= uploadWindow.
const uploadWindow = 10 * time.Minute

const uploadByteWindow = 24 * time.Hour

// UploadLimitConfig bounds stored uploads and bytes per DID.
type UploadLimitConfig struct {
	// UploadsPerTenMinutes is the most uploads one DID may have stored in the
	// last ten minutes plus in flight. It must be at least 1.
	UploadsPerTenMinutes int
	// BytesPerDay includes stored bytes in the last 24 hours plus reserved
	// output bytes. It must be positive.
	BytesPerDay int64
	// WarnFraction must be finite and in (0, 1].
	WarnFraction float64
}

// UploadLimiter counts each DID's stored and in-flight uploads.
type UploadLimiter struct {
	limit        int
	byteLimit    int64
	warnFraction float64
	now          func() time.Time

	mu   sync.Mutex
	dids map[string]*didUploads
}

// didUploads is one DID's state: the store times of its uploads, oldest first,
// and how many of its uploads hold a reservation without having stored yet.
type didUploads struct {
	stored        []time.Time
	storedBytes   []storedUploadBytes
	byteUsage     int64
	inFlight      int
	inFlightBytes int64
	countWarned   bool
	bytesWarned   bool
}

type storedUploadBytes struct {
	at   time.Time
	size int64
}

// uploadNearCapWarnings counts stored-usage threshold crossings.
var uploadNearCapWarnings atomic.Int64

// uploadRateLimitRefusals counts uploads refused for being over the per-DID cap.
var uploadRateLimitRefusals atomic.Int64

// NewUploadLimiter returns a limiter enforcing cfg, or an error wrapping
// ErrInvalidConfig when a cap or warning fraction is invalid.
func NewUploadLimiter(cfg UploadLimitConfig) (*UploadLimiter, error) {
	return newUploadLimiterWithClock(cfg, time.Now)
}

func newUploadLimiterWithClock(cfg UploadLimitConfig, now func() time.Time) (*UploadLimiter, error) {
	if cfg.UploadsPerTenMinutes < 1 {
		return nil, fmt.Errorf("%w: uploads per ten minutes must be positive: %d", ErrInvalidConfig, cfg.UploadsPerTenMinutes)
	}
	if cfg.BytesPerDay <= 0 {
		return nil, fmt.Errorf("%w: stored bytes per day must be positive: %d", ErrInvalidConfig, cfg.BytesPerDay)
	}
	if math.IsNaN(cfg.WarnFraction) || math.IsInf(cfg.WarnFraction, 0) || cfg.WarnFraction <= 0 || cfg.WarnFraction > 1 {
		return nil, fmt.Errorf("%w: upload warning fraction must be in (0, 1]", ErrInvalidConfig)
	}
	if now == nil {
		panic("media.NewUploadLimiter: clock cannot be nil")
	}
	return &UploadLimiter{
		limit: cfg.UploadsPerTenMinutes, byteLimit: cfg.BytesPerDay,
		warnFraction: cfg.WarnFraction, now: now, dids: make(map[string]*didUploads),
	}, nil
}

// TrackedDIDs is the number of DIDs the limiter holds state for. It only
// reads; idle DIDs are dropped when the limiter is consulted for an upload.
func (l *UploadLimiter) TrackedDIDs() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.dids)
}

// reserve takes a slot for an upload by did. It refuses, returning false and
// did's usage (stored in the window plus in flight), when that usage has
// reached the cap. It first drops did's expired store times and every other
// DID with nothing in the window and nothing in flight.
func (l *UploadLimiter) reserve(did string) (limit string, usage int64, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for key, entry := range l.dids {
		if entry.idle(now) {
			delete(l.dids, key)
		}
	}
	entry := l.dids[did]
	if entry == nil {
		entry = &didUploads{}
		l.dids[did] = entry
	}
	entry.dropExpired(now)
	l.rearmWarnings(entry)
	usage = int64(len(entry.stored) + entry.inFlight)
	if usage >= int64(l.limit) {
		return "uploads_per_10min", usage, false
	}
	if entry.byteUsage >= l.byteLimit {
		return "bytes_per_day", entry.byteUsage, false
	}
	entry.inFlight++
	return "", usage, true
}

// reserveBytes atomically admits the sanitized output before the PDS call.
func (l *UploadLimiter) reserveBytes(did string, size int64) (usage int64, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := l.dids[did]
	entry.dropExpired(l.now())
	l.rearmWarnings(entry)
	usage = entry.byteUsage + entry.inFlightBytes
	if size > l.byteLimit-usage {
		return usage, false
	}
	entry.inFlightBytes += size
	return usage, true
}

// finish ends a reservation taken by reserve. A stored upload counts from now;
// an upload that did not store leaves no trace, and a DID left idle is dropped.
func (l *UploadLimiter) finish(did string, size int64, stored bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := l.dids[did]
	entry.inFlight--
	entry.inFlightBytes -= size
	now := l.now()
	entry.dropExpired(now)
	l.rearmWarnings(entry)
	if stored {
		entry.stored = append(entry.stored, now)
		entry.storedBytes = append(entry.storedBytes, storedUploadBytes{at: now, size: size})
		entry.byteUsage += size
		l.warnNearCaps(did, entry)
		return
	}
	if entry.idle(now) {
		delete(l.dids, did)
	}
}

// rearmWarnings observes only stored usage, after expiry and before a store.
// Reaching the threshold exactly leaves an existing warning latched.
func (l *UploadLimiter) rearmWarnings(entry *didUploads) {
	if float64(len(entry.stored))/float64(l.limit) < l.warnFraction {
		entry.countWarned = false
	}
	if float64(entry.byteUsage)/float64(l.byteLimit) < l.warnFraction {
		entry.bytesWarned = false
	}
}

// warnNearCaps is called under the limiter lock only after a successful store.
func (l *UploadLimiter) warnNearCaps(did string, entry *didUploads) {
	count := int64(len(entry.stored))
	if !entry.countWarned && float64(count)/float64(l.limit) >= l.warnFraction {
		entry.countWarned = true
		warnUploadNearCap(did, "uploads_per_10min", count, int64(l.limit))
	}
	if !entry.bytesWarned && float64(entry.byteUsage)/float64(l.byteLimit) >= l.warnFraction {
		entry.bytesWarned = true
		warnUploadNearCap(did, "bytes_per_day", entry.byteUsage, l.byteLimit)
	}
}

func warnUploadNearCap(did, limit string, usage, cap int64) {
	uploadNearCapWarnings.Add(1)
	slog.Warn("media upload nearing limit",
		"did", did, "limit", limit, "usage", usage, "cap", cap)
}

// idle reports whether both stored windows and all reservations are empty.
func (d *didUploads) idle(now time.Time) bool {
	d.dropExpired(now)
	return d.inFlight == 0 && d.inFlightBytes == 0 && len(d.stored) == 0 && len(d.storedBytes) == 0
}

// dropExpired removes stored uploads strictly older than their respective windows.
func (d *didUploads) dropExpired(now time.Time) {
	expired := 0
	for expired < len(d.stored) && now.Sub(d.stored[expired]) > uploadWindow {
		expired++
	}
	d.stored = d.stored[expired:]
	expired = 0
	for expired < len(d.storedBytes) && now.Sub(d.storedBytes[expired].at) > uploadByteWindow {
		d.byteUsage -= d.storedBytes[expired].size
		expired++
	}
	d.storedBytes = d.storedBytes[expired:]
}

// UploadService runs uploads through the pipeline under the per-DID limit.
type UploadService struct {
	pipeline *Pipeline
	limiter  *UploadLimiter
}

// NewUploadService returns an UploadService over pipeline and limiter. It
// panics when either is nil.
func NewUploadService(pipeline *Pipeline, limiter *UploadLimiter) *UploadService {
	if pipeline == nil {
		panic("media.NewUploadService: pipeline cannot be nil")
	}
	if limiter == nil {
		panic("media.NewUploadService: limiter cannot be nil")
	}
	return &UploadService{pipeline: pipeline, limiter: limiter}
}

// Upload reserves a count slot before ingest and output bytes before the PDS
// call. Full stored byte usage also refuses before ingest. Only successful
// stores count, timestamped at completion; failures release both reservations.
func (s *UploadService) Upload(ctx context.Context, did string, uploader BlobUploader, in Input) (UploadedImage, error) {
	limit, usage, ok := s.limiter.reserve(did)
	if !ok {
		return UploadedImage{}, s.refuse(did, limit, usage)
	}
	stored := false
	var reservedBytes int64
	defer func() { s.limiter.finish(did, reservedBytes, stored) }()
	in.Accept = uploadFormats
	result, err := s.pipeline.ingest(ctx, in, s.pipeline.maxLongEdge)
	if err != nil {
		return UploadedImage{}, err
	}
	size := int64(len(result.Data))
	usage, ok = s.limiter.reserveBytes(did, size)
	if !ok {
		return UploadedImage{}, s.refuse(did, "bytes_per_day", usage)
	}
	reservedBytes = size
	image, err := storeUpload(ctx, uploader, result)
	if err != nil {
		return image, err
	}
	stored = true
	return image, nil
}

func (s *UploadService) refuse(did, limit string, usage int64) error {
	cap := s.limiter.byteLimit
	if limit == "uploads_per_10min" {
		cap = int64(s.limiter.limit)
	}
	uploadRateLimitRefusals.Add(1)
	slog.Warn("media upload refused: upload limit reached",
		"did", did, "limit", limit, "usage", usage, "cap", cap)
	return &Error{Code: ErrorCodeUploadRateLimited, Err: fmt.Errorf("%s cap %d reached", limit, cap)}
}

// UploadNearCapWarningCount returns the near-cap warning total since process start.
func UploadNearCapWarningCount() int64 {
	return uploadNearCapWarnings.Load()
}

// UploadRateLimitRefusalCount returns the total number of uploads refused for
// being over the per-DID upload cap since process start. It stands in for a
// metric until proper metrics exist.
func UploadRateLimitRefusalCount() int64 {
	return uploadRateLimitRefusals.Load()
}
