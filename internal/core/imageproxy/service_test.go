package imageproxy

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"Coves/tests/testkit"
)

// MockCache implements Cache for testing
type MockCache struct {
	mu       sync.Mutex
	data     map[string][]byte
	getCalls int
	setCalls int
	setData  map[string][]byte // Track what was set
}

func NewMockCache() *MockCache {
	return &MockCache{
		data:    make(map[string][]byte),
		setData: make(map[string][]byte),
	}
}

func (m *MockCache) cacheKey(preset, did, cid string) string {
	return preset + ":" + did + ":" + cid
}

func (m *MockCache) Get(preset, did, cid string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.getCalls++
	key := m.cacheKey(preset, did, cid)
	data, found := m.data[key]
	return data, found, nil
}

func (m *MockCache) Set(preset, did, cid string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.setCalls++
	key := m.cacheKey(preset, did, cid)
	m.data[key] = data
	m.setData[key] = data
	return nil
}

func (m *MockCache) Delete(preset, did, cid string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := m.cacheKey(preset, did, cid)
	delete(m.data, key)
	return nil
}

func (m *MockCache) Cleanup() (int, error) {
	// Mock implementation - no-op for tests
	return 0, nil
}

func (m *MockCache) SetCacheData(preset, did, cid string, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := m.cacheKey(preset, did, cid)
	m.data[key] = data
}

func (m *MockCache) GetCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.getCalls
}

func (m *MockCache) SetCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.setCalls
}

func (m *MockCache) GetSetData(preset, did, cid string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := m.cacheKey(preset, did, cid)
	data, found := m.setData[key]
	return data, found
}

// MockProcessor implements Processor for testing
type MockProcessor struct {
	returnData []byte
	returnErr  error
	calls      int
	mu         sync.Mutex
}

func NewMockProcessor(returnData []byte, returnErr error) *MockProcessor {
	return &MockProcessor{
		returnData: returnData,
		returnErr:  returnErr,
	}
}

func (m *MockProcessor) Process(data []byte, preset Preset) ([]byte, error) {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	if m.returnErr != nil {
		return nil, m.returnErr
	}
	return m.returnData, nil
}

func (m *MockProcessor) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// MockFetcher implements Fetcher for testing
type MockFetcher struct {
	returnData []byte
	returnErr  error
	calls      int
	mu         sync.Mutex
}

func NewMockFetcher(returnData []byte, returnErr error) *MockFetcher {
	return &MockFetcher{
		returnData: returnData,
		returnErr:  returnErr,
	}
}

func (m *MockFetcher) Fetch(ctx context.Context, pdsURL, did, cid string) ([]byte, error) {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	if m.returnErr != nil {
		return nil, m.returnErr
	}
	return m.returnData, nil
}

func (m *MockFetcher) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// mustNewService is a test helper that creates a service or fails the test
func mustNewService(t *testing.T, cache Cache, processor Processor, fetcher Fetcher, config Config) *ImageProxyService {
	t.Helper()
	service, err := NewService(cache, processor, fetcher, config)
	if err != nil {
		t.Fatalf("NewService failed: %v", err)
	}
	return service
}

func TestImageProxyService_GetImage_CacheHit(t *testing.T) {
	cache := NewMockCache()
	processor := NewMockProcessor(nil, nil)
	fetcher := NewMockFetcher(nil, nil)
	config := DefaultConfig()

	// Pre-populate the cache
	cachedData := []byte("cached image data")
	cache.SetCacheData("avatar", "did:plc:test123", "bafyreicid123", cachedData)

	service := mustNewService(t, cache, processor, fetcher, config)
	ctx := context.Background()

	data, err := service.GetImage(ctx, "avatar", "did:plc:test123", "bafyreicid123", "https://pds.example.com")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if string(data) != string(cachedData) {
		t.Errorf("expected cached data %q, got %q", cachedData, data)
	}

	// Verify fetcher was not called
	if fetcher.Calls() != 0 {
		t.Errorf("expected fetcher to not be called on cache hit, got %d calls", fetcher.Calls())
	}

	// Verify processor was not called
	if processor.Calls() != 0 {
		t.Errorf("expected processor to not be called on cache hit, got %d calls", processor.Calls())
	}
}

func TestImageProxyService_GetImage_CacheMiss(t *testing.T) {
	cache := NewMockCache()
	rawImageData := []byte("raw image from PDS")
	processedData := []byte("processed image")
	processor := NewMockProcessor(processedData, nil)
	fetcher := NewMockFetcher(rawImageData, nil)
	config := DefaultConfig()

	service := mustNewService(t, cache, processor, fetcher, config)
	ctx := context.Background()

	data, err := service.GetImage(ctx, "avatar", "did:plc:test123", "bafyreicid123", "https://pds.example.com")
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if string(data) != string(processedData) {
		t.Errorf("expected processed data %q, got %q", processedData, data)
	}

	// Verify fetcher was called
	if fetcher.Calls() != 1 {
		t.Errorf("expected fetcher to be called once, got %d calls", fetcher.Calls())
	}

	// Verify processor was called
	if processor.Calls() != 1 {
		t.Errorf("expected processor to be called once, got %d calls", processor.Calls())
	}

	// The cache write happens on its own goroutine, so wait for the write
	// itself rather than for a duration guessed to contain it.
	testkit.WaitFor(t, 5*time.Second, func() (bool, error) {
		return cache.SetCalls() >= 1, nil
	}, testkit.WithDescription("the asynchronous cache write to land"),
		testkit.WithDiagnostics(func() string {
			return fmt.Sprintf("cache Set calls: %d", cache.SetCalls())
		}))

	// Verify the correct data was cached
	setData, found := cache.GetSetData("avatar", "did:plc:test123", "bafyreicid123")
	if !found {
		t.Error("expected data to be set in cache")
	}
	if string(setData) != string(processedData) {
		t.Errorf("expected cached data %q, got %q", processedData, setData)
	}
}

func TestImageProxyService_GetImage_InvalidPreset(t *testing.T) {
	cache := NewMockCache()
	processor := NewMockProcessor(nil, nil)
	fetcher := NewMockFetcher(nil, nil)
	config := DefaultConfig()

	service := mustNewService(t, cache, processor, fetcher, config)
	ctx := context.Background()

	_, err := service.GetImage(ctx, "invalid_preset", "did:plc:test123", "bafyreicid123", "https://pds.example.com")
	if !errors.Is(err, ErrInvalidPreset) {
		t.Errorf("expected ErrInvalidPreset, got: %v", err)
	}
}

func TestImageProxyService_GetImage_PDSFetchError(t *testing.T) {
	cache := NewMockCache()
	processor := NewMockProcessor(nil, nil)
	fetcher := NewMockFetcher(nil, ErrPDSNotFound)
	config := DefaultConfig()

	service := mustNewService(t, cache, processor, fetcher, config)
	ctx := context.Background()

	_, err := service.GetImage(ctx, "avatar", "did:plc:test123", "bafyreicid123", "https://pds.example.com")
	if !errors.Is(err, ErrPDSNotFound) {
		t.Errorf("expected ErrPDSNotFound, got: %v", err)
	}
}

func TestImageProxyService_GetImage_ProcessingError(t *testing.T) {
	cache := NewMockCache()
	processor := NewMockProcessor(nil, ErrProcessingFailed)
	fetcher := NewMockFetcher([]byte("raw data"), nil)
	config := DefaultConfig()

	service := mustNewService(t, cache, processor, fetcher, config)
	ctx := context.Background()

	_, err := service.GetImage(ctx, "avatar", "did:plc:test123", "bafyreicid123", "https://pds.example.com")
	if !errors.Is(err, ErrProcessingFailed) {
		t.Errorf("expected ErrProcessingFailed, got: %v", err)
	}
}

func TestImageProxyService_GetImage_CacheWriteIsAsync(t *testing.T) {
	cache := NewMockCache()
	rawImageData := []byte("raw image from PDS")
	processedData := []byte("processed image")
	processor := NewMockProcessor(processedData, nil)
	fetcher := NewMockFetcher(rawImageData, nil)
	config := DefaultConfig()

	service := mustNewService(t, cache, processor, fetcher, config)
	ctx := context.Background()

	// Call GetImage
	startTime := time.Now()
	data, err := service.GetImage(ctx, "avatar", "did:plc:test123", "bafyreicid123", "https://pds.example.com")
	elapsed := time.Since(startTime)

	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if string(data) != string(processedData) {
		t.Errorf("expected processed data %q, got %q", processedData, data)
	}

	// The response should come back quickly, not blocked by cache write
	// (This is a soft assertion - just ensures we're not blocking)
	if elapsed > 100*time.Millisecond {
		t.Logf("warning: GetImage took %v, expected faster response", elapsed)
	}

	// The write still has to happen — asynchronous must not mean dropped.
	testkit.WaitFor(t, 5*time.Second, func() (bool, error) {
		return cache.SetCalls() >= 1, nil
	}, testkit.WithDescription("the cache write to complete after GetImage returned"),
		testkit.WithDiagnostics(func() string {
			return fmt.Sprintf("cache Set calls: %d", cache.SetCalls())
		}))
}

func TestImageProxyService_GetImage_EmptyPreset(t *testing.T) {
	cache := NewMockCache()
	processor := NewMockProcessor(nil, nil)
	fetcher := NewMockFetcher(nil, nil)
	config := DefaultConfig()

	service := mustNewService(t, cache, processor, fetcher, config)
	ctx := context.Background()

	_, err := service.GetImage(ctx, "", "did:plc:test123", "bafyreicid123", "https://pds.example.com")
	if !errors.Is(err, ErrInvalidPreset) {
		t.Errorf("expected ErrInvalidPreset for empty preset, got: %v", err)
	}
}

func TestImageProxyService_GetImage_AllPresets(t *testing.T) {
	// Test that all predefined presets work
	presets := []string{"avatar", "avatar_small", "banner", "content_preview", "content_full", "embed_thumbnail"}

	for _, presetName := range presets {
		t.Run(presetName, func(t *testing.T) {
			cache := NewMockCache()
			processedData := []byte("processed image")
			processor := NewMockProcessor(processedData, nil)
			fetcher := NewMockFetcher([]byte("raw data"), nil)
			config := DefaultConfig()

			service := mustNewService(t, cache, processor, fetcher, config)
			ctx := context.Background()

			data, err := service.GetImage(ctx, presetName, "did:plc:test123", "bafyreicid123", "https://pds.example.com")
			if err != nil {
				t.Errorf("expected no error for preset %s, got: %v", presetName, err)
			}
			if string(data) != string(processedData) {
				t.Errorf("expected processed data for preset %s", presetName)
			}
		})
	}
}

func TestNewService_NilDependencies(t *testing.T) {
	config := DefaultConfig()
	cache := NewMockCache()
	processor := NewMockProcessor(nil, nil)
	fetcher := NewMockFetcher(nil, nil)

	t.Run("nil cache", func(t *testing.T) {
		_, err := NewService(nil, processor, fetcher, config)
		if !errors.Is(err, ErrNilDependency) {
			t.Errorf("expected ErrNilDependency, got: %v", err)
		}
	})

	t.Run("nil processor", func(t *testing.T) {
		_, err := NewService(cache, nil, fetcher, config)
		if !errors.Is(err, ErrNilDependency) {
			t.Errorf("expected ErrNilDependency, got: %v", err)
		}
	})

	t.Run("nil fetcher", func(t *testing.T) {
		_, err := NewService(cache, processor, nil, config)
		if !errors.Is(err, ErrNilDependency) {
			t.Errorf("expected ErrNilDependency, got: %v", err)
		}
	})

	t.Run("all valid", func(t *testing.T) {
		service, err := NewService(cache, processor, fetcher, config)
		if err != nil {
			t.Errorf("expected no error with valid dependencies, got: %v", err)
		}
		if service == nil {
			t.Error("expected non-nil service")
		}
	})
}

// blockingProcessor is a Processor whose Process call parks until released.
// It reports each entry on a channel so a test can KNOW how many calls are
// inside rather than guessing with a sleep, and it records the high-water mark
// of concurrent calls, which is the number the semaphore exists to bound.
type blockingProcessor struct {
	entered     chan struct{}
	release     chan struct{}
	releaseOnce sync.Once
	returnData  []byte

	mu          sync.Mutex
	calls       int
	inFlight    int
	maxInFlight int
}

func newBlockingProcessor(returnData []byte) *blockingProcessor {
	return &blockingProcessor{
		// Buffered well past any test's call count so a Process that the
		// service SHOULD have refused still signals instead of deadlocking;
		// the test then sees the extra entry and fails with a clear message.
		entered:    make(chan struct{}, 16),
		release:    make(chan struct{}),
		returnData: returnData,
	}
}

func (b *blockingProcessor) Process(data []byte, preset Preset) ([]byte, error) {
	b.mu.Lock()
	b.calls++
	b.inFlight++
	if b.inFlight > b.maxInFlight {
		b.maxInFlight = b.inFlight
	}
	b.mu.Unlock()

	b.entered <- struct{}{}
	<-b.release

	b.mu.Lock()
	b.inFlight--
	b.mu.Unlock()
	return b.returnData, nil
}

// Release lets every parked and future Process call through. Safe to call
// more than once so a failing test can unwind its goroutines from Cleanup.
func (b *blockingProcessor) Release() {
	b.releaseOnce.Do(func() { close(b.release) })
}

func (b *blockingProcessor) Calls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

func (b *blockingProcessor) MaxInFlight() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.maxInFlight
}

// waitForEntries blocks until the processor has admitted `count` calls or the
// timeout elapses, failing the test with the number actually seen.
func (b *blockingProcessor) waitForEntries(t *testing.T, count int, timeout time.Duration) {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for seen := 0; seen < count; seen++ {
		select {
		case <-b.entered:
		case <-timer.C:
			t.Fatalf("only %d of %d Process calls entered within %v (total calls %d)", seen, count, timeout, b.Calls())
		}
	}
}

// getImageResult carries one GetImage return across a goroutine boundary.
type getImageResult struct {
	data []byte
	err  error
}

// callGetImageAsync runs GetImage in a goroutine and returns the channel its
// result lands on, so a test can bound the wait instead of blocking forever.
func callGetImageAsync(ctx context.Context, service *ImageProxyService, cid string) <-chan getImageResult {
	done := make(chan getImageResult, 1)
	go func() {
		data, err := service.GetImage(ctx, "avatar", "did:plc:test123", cid, "https://pds.example.com")
		done <- getImageResult{data: data, err: err}
	}()
	return done
}

// sequenceProcessor returns the scripted results in order, one per call, and
// never blocks. It exists to show that a Process that FAILS still gives its
// slot back.
type sequenceProcessor struct {
	mu      sync.Mutex
	results []getImageResult
	calls   int
}

func (s *sequenceProcessor) Process(data []byte, preset Preset) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := s.calls
	s.calls++
	if index >= len(s.results) {
		return nil, fmt.Errorf("sequenceProcessor: unscripted call %d", index+1)
	}
	return s.results[index].data, s.results[index].err
}

func (s *sequenceProcessor) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// semaphoreTestConfig is DefaultConfig with a tiny queue wait so a refused
// request comes back in milliseconds rather than the production five seconds.
func semaphoreTestConfig(maxConcurrent int) Config {
	cfg := DefaultConfig()
	cfg.MaxConcurrentProcesses = maxConcurrent
	cfg.ProcessQueueWait = 50 * time.Millisecond
	return cfg
}

// TestNewService_RejectsInvalidProcessingBudgets: the semaphore is sized from
// Config at construction, so a config that Validate would refuse must be
// refused here too. A zero slot count would park every request forever and a
// zero queue wait would refuse any request that did not win a slot instantly.
func TestNewService_RejectsInvalidProcessingBudgets(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr error
	}{
		{
			name:    "MaxConcurrentProcesses zero",
			mutate:  func(c *Config) { c.MaxConcurrentProcesses = 0 },
			wantErr: ErrInvalidMaxConcurrentProcesses,
		},
		{
			name:    "ProcessQueueWait zero",
			mutate:  func(c *Config) { c.ProcessQueueWait = 0 },
			wantErr: ErrInvalidProcessQueueWait,
		},
		{
			name:    "MaxInFlightRequests zero",
			mutate:  func(c *Config) { c.MaxInFlightRequests = 0 },
			wantErr: ErrInvalidMaxInFlightRequests,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tt.mutate(&cfg)

			service, err := NewService(NewMockCache(), NewMockProcessor(nil, nil), NewMockFetcher(nil, nil), cfg)

			if err == nil {
				t.Fatalf("expected NewService to refuse the config with %v, got a service", tt.wantErr)
			}
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("expected error wrapping %v, got: %v", tt.wantErr, err)
			}
			if service != nil {
				t.Errorf("expected nil service alongside the error, got %v", service)
			}
		})
	}
}

// TestImageProxyService_GetImage_ShedsLoadWhenProcessorSlotsAreFull is the
// service half of SECURITY_AUDIT_2026-09-01 §1.1. The per-image pixel budget
// caps what ONE request can cost; this caps how many such requests decode at
// once, so total processing memory is budget × slots rather than budget ×
// however many connections an attacker opens. A request that cannot get a slot
// within ProcessQueueWait is refused with ErrProcessorBusy and never reaches
// the processor.
func TestImageProxyService_GetImage_ShedsLoadWhenProcessorSlotsAreFull(t *testing.T) {
	processed := []byte("processed image")
	processor := newBlockingProcessor(processed)
	t.Cleanup(processor.Release)
	service := mustNewService(t, NewMockCache(), processor, NewMockFetcher([]byte("raw"), nil), semaphoreTestConfig(2))

	// Fill both slots. Distinct CIDs keep the calls from sharing a cache key.
	held := []<-chan getImageResult{
		callGetImageAsync(context.Background(), service, "bafyreicid001"),
		callGetImageAsync(context.Background(), service, "bafyreicid002"),
	}
	processor.waitForEntries(t, 2, 5*time.Second)

	// The third request must be turned away, but only after it has genuinely
	// waited for a slot: a slot may free up within the queue wait, and a
	// refusal that comes back instantly (a TryAcquire) would shed load the
	// service could have served. The elapsed check is a lower bound only, so
	// it cannot flake on a slow machine.
	refusalsBefore := ProcessorBusyRefusalCount()
	startedAt := time.Now()
	third := callGetImageAsync(context.Background(), service, "bafyreicid003")
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case result := <-third:
		elapsed := time.Since(startedAt)
		if result.err == nil {
			t.Fatalf("expected the third GetImage to be refused, got %d bytes and no error", len(result.data))
		}
		if !errors.Is(result.err, ErrProcessorBusy) {
			t.Errorf("expected error wrapping ErrProcessorBusy, got: %v", result.err)
		}
		if !errors.Is(result.err, context.DeadlineExceeded) {
			t.Errorf("expected the refusal to wrap context.DeadlineExceeded (the queue wait ran out), got: %v", result.err)
		}
		if result.data != nil {
			t.Errorf("expected no data alongside the refusal, got %d bytes", len(result.data))
		}
		if elapsed < service.config.ProcessQueueWait {
			t.Errorf("refusal arrived after %v, before the %v queue wait elapsed: the request did not actually wait for a slot",
				elapsed, service.config.ProcessQueueWait)
		}
		if got := ProcessorBusyRefusalCount() - refusalsBefore; got != 1 {
			t.Errorf("expected ProcessorBusyRefusalCount to grow by exactly 1 for one busy refusal, grew by %d", got)
		}
	case <-processor.entered:
		t.Fatalf("the third GetImage entered the processor while both slots were held (total Process calls %d, max in flight %d): no concurrency bound is enforced",
			processor.Calls(), processor.MaxInFlight())
	case <-timer.C:
		t.Fatalf("the third GetImage neither returned nor entered the processor within 5s (total Process calls %d)", processor.Calls())
	}

	if got := processor.Calls(); got != 2 {
		t.Errorf("expected exactly 2 Process calls after the refusal, got %d", got)
	}
	if got := processor.MaxInFlight(); got != 2 {
		t.Errorf("expected max in-flight Process calls of 2, got %d", got)
	}

	// Shedding the third must not have disturbed the two that were admitted.
	processor.Release()
	for i, done := range held {
		select {
		case result := <-done:
			if result.err != nil {
				t.Errorf("held request %d: expected success after release, got: %v", i+1, result.err)
			}
			if string(result.data) != string(processed) {
				t.Errorf("held request %d: expected %q, got %q", i+1, processed, result.data)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("held request %d did not return within 5s of release", i+1)
		}
	}
}

// TestImageProxyService_GetImage_CancelledContextDoesNotWaitForSlot: a caller
// that has already gone away must not consume a slot, and the failure must not
// be reported as the processor being busy. ErrProcessorBusy means "the slots
// were full for the whole queue wait", which the handler turns into a 503 and
// the operator reads as a capacity signal; a client hanging up says nothing
// about capacity. The fetcher maps a dead context to ErrPDSTimeout, and the
// slot wait must use the same sentinel so the handler treats caller
// abandonment identically wherever in the pipeline it is noticed. The fetcher
// fake ignores ctx on purpose: the check under test is the service's own.
func TestImageProxyService_GetImage_CancelledContextDoesNotWaitForSlot(t *testing.T) {
	processor := newBlockingProcessor([]byte("processed image"))
	t.Cleanup(processor.Release)
	service := mustNewService(t, NewMockCache(), processor, NewMockFetcher([]byte("raw"), nil), semaphoreTestConfig(1))

	held := callGetImageAsync(context.Background(), service, "bafyreicid001")
	processor.waitForEntries(t, 1, 5*time.Second)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	refusalsBefore := ProcessorBusyRefusalCount()
	second := callGetImageAsync(cancelled, service, "bafyreicid002")
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case result := <-second:
		if result.err == nil {
			t.Fatalf("expected GetImage with a cancelled context to fail, got %d bytes and no error", len(result.data))
		}
		assertCallerAbandonmentError(t, result.err, context.Canceled)
	case <-processor.entered:
		t.Fatalf("GetImage with a cancelled context entered the processor while the only slot was held (total Process calls %d)", processor.Calls())
	case <-timer.C:
		t.Fatalf("GetImage with a cancelled context neither returned nor entered the processor within 5s (total Process calls %d)", processor.Calls())
	}

	if got := processor.Calls(); got != 1 {
		t.Errorf("expected exactly 1 Process call, got %d", got)
	}
	if got := ProcessorBusyRefusalCount() - refusalsBefore; got != 0 {
		t.Errorf("a cancelled caller is not a busy refusal; ProcessorBusyRefusalCount grew by %d", got)
	}

	processor.Release()
	select {
	case result := <-held:
		if result.err != nil {
			t.Errorf("held request: expected success after release, got: %v", result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("held request did not return within 5s of release")
	}
}

// TestImageProxyService_GetImage_ReleasesSlotWhenProcessingFails guards the
// release-on-error path. With a single slot, a failed Process that kept its
// slot would make the very next request wait out ProcessQueueWait and be
// refused, and the service would be one bad image away from serving nothing.
func TestImageProxyService_GetImage_ReleasesSlotWhenProcessingFails(t *testing.T) {
	processingErr := fmt.Errorf("%w: scripted failure", ErrProcessingFailed)
	processed := []byte("processed image")
	processor := &sequenceProcessor{results: []getImageResult{
		{err: processingErr},
		{data: processed},
	}}
	service := mustNewService(t, NewMockCache(), processor, NewMockFetcher([]byte("raw"), nil), semaphoreTestConfig(1))

	_, err := service.GetImage(context.Background(), "avatar", "did:plc:test123", "bafyreicid001", "https://pds.example.com")
	if !errors.Is(err, ErrProcessingFailed) {
		t.Fatalf("first GetImage: expected the scripted processing error, got: %v", err)
	}

	data, err := service.GetImage(context.Background(), "avatar", "did:plc:test123", "bafyreicid002", "https://pds.example.com")
	if err != nil {
		t.Fatalf("second GetImage: expected success once the failed call released its slot, got: %v", err)
	}
	if string(data) != string(processed) {
		t.Errorf("second GetImage: expected %q, got %q", processed, data)
	}
	if got := processor.Calls(); got != 2 {
		t.Errorf("expected 2 Process calls, got %d", got)
	}
}

// assertCallerAbandonmentError checks the shape every "the caller's context
// ended while we waited for a slot" error must have: the fetcher's timeout
// sentinel, the context's own error, and NOT the busy sentinel.
func assertCallerAbandonmentError(t *testing.T, err error, wantContextErr error) {
	t.Helper()
	if !errors.Is(err, ErrPDSTimeout) {
		t.Errorf("expected error wrapping ErrPDSTimeout (the sentinel the fetcher uses for a dead context), got: %v", err)
	}
	if !errors.Is(err, wantContextErr) {
		t.Errorf("expected error wrapping %v, got: %v", wantContextErr, err)
	}
	if errors.Is(err, ErrProcessorBusy) {
		t.Errorf("caller abandonment must not be reported as ErrProcessorBusy (that is a capacity signal), got: %v", err)
	}
}

// TestImageProxyService_GetImage_CallerDeadlineWhileWaitingIsNotBusy: the
// caller's own deadline running out mid-wait is the same condition as a
// cancel, noticed later. With a 5s queue wait and a 10ms caller deadline the
// caller gives up first, and that must surface as the caller's timeout, not as
// the processor being busy.
func TestImageProxyService_GetImage_CallerDeadlineWhileWaitingIsNotBusy(t *testing.T) {
	processor := newBlockingProcessor([]byte("processed image"))
	t.Cleanup(processor.Release)
	cfg := DefaultConfig()
	cfg.MaxConcurrentProcesses = 1
	cfg.ProcessQueueWait = 5 * time.Second
	service := mustNewService(t, NewMockCache(), processor, NewMockFetcher([]byte("raw"), nil), cfg)

	held := callGetImageAsync(context.Background(), service, "bafyreicid001")
	processor.waitForEntries(t, 1, 5*time.Second)

	shortLived, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	refusalsBefore := ProcessorBusyRefusalCount()
	second := callGetImageAsync(shortLived, service, "bafyreicid002")
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case result := <-second:
		if result.err == nil {
			t.Fatalf("expected GetImage to fail once the caller's deadline passed, got %d bytes and no error", len(result.data))
		}
		assertCallerAbandonmentError(t, result.err, context.DeadlineExceeded)
	case <-processor.entered:
		t.Fatalf("GetImage entered the processor while the only slot was held (total Process calls %d)", processor.Calls())
	case <-timer.C:
		t.Fatalf("GetImage with a 10ms deadline neither returned nor entered the processor within 5s; it waited on the queue instead of the caller")
	}

	if got := processor.Calls(); got != 1 {
		t.Errorf("expected exactly 1 Process call, got %d", got)
	}
	if got := ProcessorBusyRefusalCount() - refusalsBefore; got != 0 {
		t.Errorf("a caller deadline is not a busy refusal; ProcessorBusyRefusalCount grew by %d", got)
	}

	processor.Release()
	select {
	case result := <-held:
		if result.err != nil {
			t.Errorf("held request: expected success after release, got: %v", result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("held request did not return within 5s of release")
	}
}

// TestImageProxyService_GetImage_CancelledContextWithFreeSlotDoesNotProcess
// closes the gap semaphore.Acquire leaves open: when a slot is free it can
// succeed without ever looking at the context, so a request whose caller has
// already hung up would go on to decode an image nobody will receive. The
// service has to check ctx.Err() itself before taking the slot.
func TestImageProxyService_GetImage_CancelledContextWithFreeSlotDoesNotProcess(t *testing.T) {
	processor := newBlockingProcessor([]byte("processed image"))
	t.Cleanup(processor.Release)
	service := mustNewService(t, NewMockCache(), processor, NewMockFetcher([]byte("raw"), nil), semaphoreTestConfig(1))

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	refusalsBefore := ProcessorBusyRefusalCount()
	result := callGetImageAsync(cancelled, service, "bafyreicid001")
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case got := <-result:
		if got.err == nil {
			t.Fatalf("expected GetImage with a cancelled context to fail, got %d bytes and no error", len(got.data))
		}
		assertCallerAbandonmentError(t, got.err, context.Canceled)
	case <-processor.entered:
		t.Fatalf("GetImage with a cancelled context entered the processor because a slot happened to be free (total Process calls %d)", processor.Calls())
	case <-timer.C:
		t.Fatalf("GetImage with a cancelled context neither returned nor entered the processor within 5s")
	}

	if got := processor.Calls(); got != 0 {
		t.Errorf("expected no Process call for a caller that had already gone away, got %d", got)
	}
	if got := ProcessorBusyRefusalCount() - refusalsBefore; got != 0 {
		t.Errorf("a cancelled caller is not a busy refusal; ProcessorBusyRefusalCount grew by %d", got)
	}
}

// TestImageProxyService_GetImage_ReleasesSlotAfterSuccess is the success-side
// twin of ReleasesSlotWhenProcessingFails: with a single slot, a second
// sequential request can only succeed if the first gave its slot back.
func TestImageProxyService_GetImage_ReleasesSlotAfterSuccess(t *testing.T) {
	processed := []byte("processed image")
	processor := NewMockProcessor(processed, nil)
	service := mustNewService(t, NewMockCache(), processor, NewMockFetcher([]byte("raw"), nil), semaphoreTestConfig(1))

	for i, cid := range []string{"bafyreicid001", "bafyreicid002"} {
		data, err := service.GetImage(context.Background(), "avatar", "did:plc:test123", cid, "https://pds.example.com")
		if err != nil {
			t.Fatalf("GetImage %d: expected success, got: %v", i+1, err)
		}
		if string(data) != string(processed) {
			t.Errorf("GetImage %d: expected %q, got %q", i+1, processed, data)
		}
	}
	if got := processor.Calls(); got != 2 {
		t.Errorf("expected 2 Process calls, got %d", got)
	}
}

// blockingFetcher is a Fetcher whose Fetch call parks until released, the
// fetch-side twin of blockingProcessor. It lets a test hold requests at the
// point where they own a fetched blob but no decode slot yet, which is exactly
// the memory the in-flight admission cap exists to bound.
type blockingFetcher struct {
	entered     chan struct{}
	release     chan struct{}
	releaseOnce sync.Once
	returnData  []byte

	mu    sync.Mutex
	calls int
}

func newBlockingFetcher(returnData []byte) *blockingFetcher {
	return &blockingFetcher{
		entered:    make(chan struct{}, 16),
		release:    make(chan struct{}),
		returnData: returnData,
	}
}

func (b *blockingFetcher) Fetch(_ context.Context, _, _, _ string) ([]byte, error) {
	b.mu.Lock()
	b.calls++
	b.mu.Unlock()

	b.entered <- struct{}{}
	<-b.release
	return b.returnData, nil
}

func (b *blockingFetcher) Release() {
	b.releaseOnce.Do(func() { close(b.release) })
}

func (b *blockingFetcher) Calls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

func (b *blockingFetcher) waitForEntries(t *testing.T, count int, timeout time.Duration) {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for seen := 0; seen < count; seen++ {
		select {
		case <-b.entered:
		case <-timer.C:
			t.Fatalf("only %d of %d Fetch calls entered within %v (total calls %d)", seen, count, timeout, b.Calls())
		}
	}
}

// admissionTestConfig is DefaultConfig with a small in-flight cap and a short
// queue wait. The decode semaphore keeps its default of 4 slots so it cannot
// be the thing refusing anything in these tests.
func admissionTestConfig(maxInFlight int) Config {
	cfg := DefaultConfig()
	cfg.MaxInFlightRequests = maxInFlight
	cfg.ProcessQueueWait = 50 * time.Millisecond
	return cfg
}

// TestImageProxyService_GetImage_ShedsLoadWhenInFlightCapIsFull: the decode
// semaphore bounds memory DURING decode, but a request that has fetched its
// blob and is waiting for a slot is already holding up to MaxSourceSizeMB, and
// the semaphore's waiters are unbounded. The admission cap sits in front of the
// fetch so the number of blobs held anywhere in the pipeline is bounded too.
// A request refused here never reaches the fetcher, waits the full queue wait
// first (a slot may free up), and counts as a busy refusal.
func TestImageProxyService_GetImage_ShedsLoadWhenInFlightCapIsFull(t *testing.T) {
	processed := []byte("processed image")
	fetcher := newBlockingFetcher([]byte("raw"))
	t.Cleanup(fetcher.Release)
	service := mustNewService(t, NewMockCache(), NewMockProcessor(processed, nil), fetcher, admissionTestConfig(2))

	held := []<-chan getImageResult{
		callGetImageAsync(context.Background(), service, "bafyreicid001"),
		callGetImageAsync(context.Background(), service, "bafyreicid002"),
	}
	fetcher.waitForEntries(t, 2, 5*time.Second)

	refusalsBefore := ProcessorBusyRefusalCount()
	startedAt := time.Now()
	third := callGetImageAsync(context.Background(), service, "bafyreicid003")
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case result := <-third:
		elapsed := time.Since(startedAt)
		if result.err == nil {
			t.Fatalf("expected the third GetImage to be refused, got %d bytes and no error", len(result.data))
		}
		if !errors.Is(result.err, ErrProcessorBusy) {
			t.Errorf("expected error wrapping ErrProcessorBusy, got: %v", result.err)
		}
		if !errors.Is(result.err, context.DeadlineExceeded) {
			t.Errorf("expected the refusal to wrap context.DeadlineExceeded, got: %v", result.err)
		}
		if elapsed < service.config.ProcessQueueWait {
			t.Errorf("refusal arrived after %v, before the %v queue wait elapsed: the request did not wait for admission",
				elapsed, service.config.ProcessQueueWait)
		}
		if got := ProcessorBusyRefusalCount() - refusalsBefore; got != 1 {
			t.Errorf("expected ProcessorBusyRefusalCount to grow by exactly 1, grew by %d", got)
		}
	case <-fetcher.entered:
		t.Fatalf("the third GetImage entered the fetcher while %d requests were already in flight (total Fetch calls %d): no admission cap is enforced",
			service.config.MaxInFlightRequests, fetcher.Calls())
	case <-timer.C:
		t.Fatalf("the third GetImage neither returned nor entered the fetcher within 5s (total Fetch calls %d)", fetcher.Calls())
	}

	if got := fetcher.Calls(); got != 2 {
		t.Errorf("expected exactly 2 Fetch calls; the refused request must never fetch, got %d", got)
	}

	fetcher.Release()
	for i, done := range held {
		select {
		case result := <-done:
			if result.err != nil {
				t.Errorf("held request %d: expected success after release, got: %v", i+1, result.err)
			}
			if string(result.data) != string(processed) {
				t.Errorf("held request %d: expected %q, got %q", i+1, processed, result.data)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("held request %d did not return within 5s of release", i+1)
		}
	}
}

// TestImageProxyService_GetImage_CacheHitBypassesAdmission: a cache hit costs
// a file read and holds no blob, so it must be served even when every
// admission slot is taken. Refusing cached images under load would turn a
// burst of cold requests into an outage for the warm ones too.
func TestImageProxyService_GetImage_CacheHitBypassesAdmission(t *testing.T) {
	cache := NewMockCache()
	cachedData := []byte("cached image data")
	cache.SetCacheData("avatar", "did:plc:test123", "bafyreicid002", cachedData)

	fetcher := newBlockingFetcher([]byte("raw"))
	t.Cleanup(fetcher.Release)
	service := mustNewService(t, cache, NewMockProcessor([]byte("processed"), nil), fetcher, admissionTestConfig(1))

	held := callGetImageAsync(context.Background(), service, "bafyreicid001")
	fetcher.waitForEntries(t, 1, 5*time.Second)

	warm := callGetImageAsync(context.Background(), service, "bafyreicid002")
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case result := <-warm:
		if result.err != nil {
			t.Fatalf("expected the cached request to succeed while admission is full, got: %v", result.err)
		}
		if string(result.data) != string(cachedData) {
			t.Errorf("expected cached data %q, got %q", cachedData, result.data)
		}
	case <-fetcher.entered:
		t.Fatalf("the cached request entered the fetcher; a cache hit must never fetch (total Fetch calls %d)", fetcher.Calls())
	case <-timer.C:
		t.Fatal("the cached request did not return within 5s; it queued behind admission instead of bypassing it")
	}

	if got := fetcher.Calls(); got != 1 {
		t.Errorf("expected exactly 1 Fetch call, got %d", got)
	}

	fetcher.Release()
	select {
	case result := <-held:
		if result.err != nil {
			t.Errorf("held request: expected success after release, got: %v", result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("held request did not return within 5s of release")
	}
}

// TestImageProxyService_GetImage_ReleasesAdmissionAfterSuccess: with a single
// admission slot, the second sequential request can only succeed if the first
// handed its slot back when it finished.
func TestImageProxyService_GetImage_ReleasesAdmissionAfterSuccess(t *testing.T) {
	processed := []byte("processed image")
	fetcher := NewMockFetcher([]byte("raw"), nil)
	service := mustNewService(t, NewMockCache(), NewMockProcessor(processed, nil), fetcher, admissionTestConfig(1))

	for i, cid := range []string{"bafyreicid001", "bafyreicid002"} {
		data, err := service.GetImage(context.Background(), "avatar", "did:plc:test123", cid, "https://pds.example.com")
		if err != nil {
			t.Fatalf("GetImage %d: expected success, got: %v", i+1, err)
		}
		if string(data) != string(processed) {
			t.Errorf("GetImage %d: expected %q, got %q", i+1, processed, data)
		}
	}
	if got := fetcher.Calls(); got != 2 {
		t.Errorf("expected 2 Fetch calls, got %d", got)
	}
}
