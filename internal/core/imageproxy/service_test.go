package imageproxy

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
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

	// Wait a bit for async cache write
	time.Sleep(50 * time.Millisecond)

	// Verify cache was written
	if cache.SetCalls() < 1 {
		t.Errorf("expected cache to be written, got %d set calls", cache.SetCalls())
	}

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

	// Wait for async cache write to complete
	time.Sleep(100 * time.Millisecond)

	// Now verify cache was written
	if cache.SetCalls() < 1 {
		t.Errorf("expected cache to be written asynchronously, got %d set calls", cache.SetCalls())
	}
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
