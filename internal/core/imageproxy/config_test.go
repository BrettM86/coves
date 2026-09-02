package imageproxy

import (
	"errors"
	"testing"
	"time"
)

func TestConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		config  Config
		wantErr error
	}{
		{
			name:    "valid default config",
			config:  DefaultConfig(),
			wantErr: nil,
		},
		{
			name: "valid enabled config",
			config: Config{
				Enabled:                true,
				BaseURL:                "/img",
				CachePath:              "/var/cache/images",
				CacheMaxGB:             10,
				FetchTimeout:           30 * time.Second,
				MaxSourceSizeMB:        10,
				MaxSourceMegapixels:    50,
				MaxConcurrentProcesses: 4,
				ProcessQueueWait:       5 * time.Second,
				MaxInFlightRequests:    64,
			},
			wantErr: nil,
		},
		{
			name: "invalid CacheMaxGB zero",
			config: Config{
				Enabled:                false,
				BaseURL:                "/img",
				CachePath:              "/var/cache/images",
				CacheMaxGB:             0,
				FetchTimeout:           30 * time.Second,
				MaxSourceSizeMB:        10,
				MaxSourceMegapixels:    50,
				MaxConcurrentProcesses: 4,
				ProcessQueueWait:       5 * time.Second,
				MaxInFlightRequests:    64,
			},
			wantErr: ErrInvalidCacheMaxGB,
		},
		{
			name: "invalid CacheMaxGB negative",
			config: Config{
				Enabled:                false,
				BaseURL:                "/img",
				CachePath:              "/var/cache/images",
				CacheMaxGB:             -5,
				FetchTimeout:           30 * time.Second,
				MaxSourceSizeMB:        10,
				MaxSourceMegapixels:    50,
				MaxConcurrentProcesses: 4,
				ProcessQueueWait:       5 * time.Second,
				MaxInFlightRequests:    64,
			},
			wantErr: ErrInvalidCacheMaxGB,
		},
		{
			name: "invalid FetchTimeout zero",
			config: Config{
				Enabled:                false,
				BaseURL:                "/img",
				CachePath:              "/var/cache/images",
				CacheMaxGB:             10,
				FetchTimeout:           0,
				MaxSourceSizeMB:        10,
				MaxSourceMegapixels:    50,
				MaxConcurrentProcesses: 4,
				ProcessQueueWait:       5 * time.Second,
				MaxInFlightRequests:    64,
			},
			wantErr: ErrInvalidFetchTimeout,
		},
		{
			name: "invalid FetchTimeout negative",
			config: Config{
				Enabled:                false,
				BaseURL:                "/img",
				CachePath:              "/var/cache/images",
				CacheMaxGB:             10,
				FetchTimeout:           -1 * time.Second,
				MaxSourceSizeMB:        10,
				MaxSourceMegapixels:    50,
				MaxConcurrentProcesses: 4,
				ProcessQueueWait:       5 * time.Second,
				MaxInFlightRequests:    64,
			},
			wantErr: ErrInvalidFetchTimeout,
		},
		{
			name: "invalid MaxSourceSizeMB zero",
			config: Config{
				Enabled:                false,
				BaseURL:                "/img",
				CachePath:              "/var/cache/images",
				CacheMaxGB:             10,
				FetchTimeout:           30 * time.Second,
				MaxSourceSizeMB:        0,
				MaxSourceMegapixels:    50,
				MaxConcurrentProcesses: 4,
				ProcessQueueWait:       5 * time.Second,
				MaxInFlightRequests:    64,
			},
			wantErr: ErrInvalidMaxSourceSize,
		},
		{
			name: "invalid MaxSourceSizeMB negative",
			config: Config{
				Enabled:                false,
				BaseURL:                "/img",
				CachePath:              "/var/cache/images",
				CacheMaxGB:             10,
				FetchTimeout:           30 * time.Second,
				MaxSourceSizeMB:        -5,
				MaxSourceMegapixels:    50,
				MaxConcurrentProcesses: 4,
				ProcessQueueWait:       5 * time.Second,
				MaxInFlightRequests:    64,
			},
			wantErr: ErrInvalidMaxSourceSize,
		},
		{
			name: "enabled but missing CachePath",
			config: Config{
				Enabled:                true,
				BaseURL:                "/img",
				CachePath:              "",
				CacheMaxGB:             10,
				FetchTimeout:           30 * time.Second,
				MaxSourceSizeMB:        10,
				MaxSourceMegapixels:    50,
				MaxConcurrentProcesses: 4,
				ProcessQueueWait:       5 * time.Second,
				MaxInFlightRequests:    64,
			},
			wantErr: ErrMissingCachePath,
		},
		{
			name: "enabled allows empty BaseURL for relative URLs",
			config: Config{
				Enabled:                true,
				BaseURL:                "",
				CachePath:              "/var/cache/images",
				CacheMaxGB:             10,
				FetchTimeout:           30 * time.Second,
				MaxSourceSizeMB:        10,
				MaxSourceMegapixels:    50,
				MaxConcurrentProcesses: 4,
				ProcessQueueWait:       5 * time.Second,
				MaxInFlightRequests:    64,
			},
			wantErr: nil,
		},
		{
			name: "disabled allows empty CachePath",
			config: Config{
				Enabled:                false,
				BaseURL:                "/img",
				CachePath:              "",
				CacheMaxGB:             10,
				FetchTimeout:           30 * time.Second,
				MaxSourceSizeMB:        10,
				MaxSourceMegapixels:    50,
				MaxConcurrentProcesses: 4,
				ProcessQueueWait:       5 * time.Second,
				MaxInFlightRequests:    64,
			},
			wantErr: nil,
		},
		{
			name: "disabled allows empty BaseURL",
			config: Config{
				Enabled:                false,
				BaseURL:                "",
				CachePath:              "/var/cache/images",
				CacheMaxGB:             10,
				FetchTimeout:           30 * time.Second,
				MaxSourceSizeMB:        10,
				MaxSourceMegapixels:    50,
				MaxConcurrentProcesses: 4,
				ProcessQueueWait:       5 * time.Second,
				MaxInFlightRequests:    64,
			},
			wantErr: nil,
		},
		{
			name: "valid TTL zero (disabled)",
			config: Config{
				Enabled:                true,
				BaseURL:                "",
				CachePath:              "/var/cache/images",
				CacheMaxGB:             10,
				CacheTTLDays:           0,
				FetchTimeout:           30 * time.Second,
				MaxSourceSizeMB:        10,
				MaxSourceMegapixels:    50,
				MaxConcurrentProcesses: 4,
				ProcessQueueWait:       5 * time.Second,
				MaxInFlightRequests:    64,
			},
			wantErr: nil,
		},
		{
			name: "valid TTL positive",
			config: Config{
				Enabled:                true,
				BaseURL:                "",
				CachePath:              "/var/cache/images",
				CacheMaxGB:             10,
				CacheTTLDays:           30,
				FetchTimeout:           30 * time.Second,
				MaxSourceSizeMB:        10,
				MaxSourceMegapixels:    50,
				MaxConcurrentProcesses: 4,
				ProcessQueueWait:       5 * time.Second,
				MaxInFlightRequests:    64,
			},
			wantErr: nil,
		},
		{
			name: "invalid TTL negative",
			config: Config{
				Enabled:                true,
				BaseURL:                "",
				CachePath:              "/var/cache/images",
				CacheMaxGB:             10,
				CacheTTLDays:           -1,
				FetchTimeout:           30 * time.Second,
				MaxSourceSizeMB:        10,
				MaxSourceMegapixels:    50,
				MaxConcurrentProcesses: 4,
				ProcessQueueWait:       5 * time.Second,
				MaxInFlightRequests:    64,
			},
			wantErr: ErrInvalidCacheTTL,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if tt.wantErr == nil {
				if err != nil {
					t.Errorf("expected no error, got: %v", err)
				}
			} else {
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("expected %v, got: %v", tt.wantErr, err)
				}
			}
		})
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	// Verify default values
	if !cfg.Enabled {
		t.Error("expected Enabled to be true by default")
	}
	if cfg.BaseURL != "" {
		t.Errorf("expected empty BaseURL for relative URLs, got %q", cfg.BaseURL)
	}
	if cfg.CachePath != "/var/cache/coves/images" {
		t.Errorf("expected CachePath '/var/cache/coves/images', got %q", cfg.CachePath)
	}
	if cfg.CacheMaxGB != 10 {
		t.Errorf("expected CacheMaxGB 10, got %d", cfg.CacheMaxGB)
	}
	if cfg.CacheTTLDays != 30 {
		t.Errorf("expected CacheTTLDays 30, got %d", cfg.CacheTTLDays)
	}
	if cfg.CleanupInterval != 1*time.Hour {
		t.Errorf("expected CleanupInterval 1h, got %v", cfg.CleanupInterval)
	}
	if cfg.CDNURL != "" {
		t.Errorf("expected empty CDNURL, got %q", cfg.CDNURL)
	}
	if cfg.FetchTimeout != 30*time.Second {
		t.Errorf("expected FetchTimeout 30s, got %v", cfg.FetchTimeout)
	}
	if cfg.MaxSourceSizeMB != 10 {
		t.Errorf("expected MaxSourceSizeMB 10, got %d", cfg.MaxSourceSizeMB)
	}

	// Default config should be valid
	if err := cfg.Validate(); err != nil {
		t.Errorf("default config should be valid, got error: %v", err)
	}
}

func TestDefaultConfig_ProcessingBudgets(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.MaxSourceMegapixels != 50 {
		t.Errorf("expected MaxSourceMegapixels 50, got %d", cfg.MaxSourceMegapixels)
	}
	if cfg.MaxConcurrentProcesses != 4 {
		t.Errorf("expected MaxConcurrentProcesses 4, got %d", cfg.MaxConcurrentProcesses)
	}
	if cfg.ProcessQueueWait != 5*time.Second {
		t.Errorf("expected ProcessQueueWait 5s, got %v", cfg.ProcessQueueWait)
	}
	if cfg.MaxInFlightRequests != 64 {
		t.Errorf("expected MaxInFlightRequests 64, got %d", cfg.MaxInFlightRequests)
	}
}

// TestConfig_Validate_ProcessingBudgets checks each processing budget on an
// otherwise-valid DefaultConfig so a failure can only come from the field under
// test. A zero budget is not "unlimited": for the pixel cap it would refuse
// every image, for the concurrency cap it would deadlock every request, and
// for the queue wait it would refuse any request that did not find a free slot
// on the first try. All three are misconfigurations the service must not boot with.
func TestConfig_Validate_ProcessingBudgets(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr error
	}{
		{
			name:    "MaxSourceMegapixels zero",
			mutate:  func(c *Config) { c.MaxSourceMegapixels = 0 },
			wantErr: ErrInvalidMaxSourceMegapixels,
		},
		{
			name:    "MaxSourceMegapixels negative",
			mutate:  func(c *Config) { c.MaxSourceMegapixels = -1 },
			wantErr: ErrInvalidMaxSourceMegapixels,
		},
		{
			// The ceiling is enforced at Validate as well as in NewProcessor so
			// a bad env value is refused at boot, not when the first image arrives.
			name:    "MaxSourceMegapixels above the ceiling",
			mutate:  func(c *Config) { c.MaxSourceMegapixels = MaxSourceMegapixelsCeiling + 1 },
			wantErr: ErrInvalidMaxSourceMegapixels,
		},
		{
			name:    "MaxConcurrentProcesses zero",
			mutate:  func(c *Config) { c.MaxConcurrentProcesses = 0 },
			wantErr: ErrInvalidMaxConcurrentProcesses,
		},
		{
			name:    "MaxConcurrentProcesses negative",
			mutate:  func(c *Config) { c.MaxConcurrentProcesses = -1 },
			wantErr: ErrInvalidMaxConcurrentProcesses,
		},
		{
			name:    "ProcessQueueWait zero",
			mutate:  func(c *Config) { c.ProcessQueueWait = 0 },
			wantErr: ErrInvalidProcessQueueWait,
		},
		{
			name:    "ProcessQueueWait negative",
			mutate:  func(c *Config) { c.ProcessQueueWait = -1 * time.Second },
			wantErr: ErrInvalidProcessQueueWait,
		},
		{
			name:    "MaxInFlightRequests zero",
			mutate:  func(c *Config) { c.MaxInFlightRequests = 0 },
			wantErr: ErrInvalidMaxInFlightRequests,
		},
		{
			name:    "MaxInFlightRequests negative",
			mutate:  func(c *Config) { c.MaxInFlightRequests = -1 },
			wantErr: ErrInvalidMaxInFlightRequests,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tt.mutate(&cfg)

			err := cfg.Validate()
			if err == nil {
				t.Fatalf("expected error wrapping %v, got nil", tt.wantErr)
			}
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("expected error wrapping %v, got: %v", tt.wantErr, err)
			}
		})
	}
}

// TestConfigFromEnv_MaxSourceMegapixels and its sibling below follow the
// parse-or-warn-and-keep-default contract every IMAGE_PROXY_* variable already
// honours: a bad value must never turn a safety limit off, so "0", a negative,
// and garbage all leave the default in place instead of disabling the budget.
func TestConfigFromEnv_MaxSourceMegapixels(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  int
	}{
		{name: "valid value is applied", value: "30", want: 30},
		{name: "zero keeps the default", value: "0", want: DefaultMaxSourceMegapixels},
		{name: "negative keeps the default", value: "-3", want: DefaultMaxSourceMegapixels},
		{name: "non-numeric keeps the default", value: "abc", want: DefaultMaxSourceMegapixels},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("IMAGE_PROXY_MAX_SOURCE_MEGAPIXELS", tt.value)

			cfg := ConfigFromEnv()

			if cfg.MaxSourceMegapixels != tt.want {
				t.Errorf("IMAGE_PROXY_MAX_SOURCE_MEGAPIXELS=%q: expected MaxSourceMegapixels %d, got %d",
					tt.value, tt.want, cfg.MaxSourceMegapixels)
			}
		})
	}
}

func TestConfigFromEnv_MaxConcurrentProcesses(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  int
	}{
		{name: "valid value is applied", value: "8", want: 8},
		{name: "zero keeps the default", value: "0", want: DefaultMaxConcurrentProcesses},
		{name: "negative keeps the default", value: "-3", want: DefaultMaxConcurrentProcesses},
		{name: "non-numeric keeps the default", value: "abc", want: DefaultMaxConcurrentProcesses},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("IMAGE_PROXY_MAX_CONCURRENT_PROCESSES", tt.value)

			cfg := ConfigFromEnv()

			if cfg.MaxConcurrentProcesses != tt.want {
				t.Errorf("IMAGE_PROXY_MAX_CONCURRENT_PROCESSES=%q: expected MaxConcurrentProcesses %d, got %d",
					tt.value, tt.want, cfg.MaxConcurrentProcesses)
			}
		})
	}
}

// TestConfigFromEnv_ProcessQueueWaitIsNotConfigurable documents a deliberate
// decision rather than an omission. The queue wait is coupled to the handler's
// fixed Retry-After and to the upstream proxy's own timeouts; letting an
// operator stretch it would let queued requests pile up holding connections
// for longer than any of those layers expect. It stays a compile-time constant
// until there is a concrete reason to tune it, and this test is where that
// reason will have to be argued.
func TestConfigFromEnv_ProcessQueueWaitIsNotConfigurable(t *testing.T) {
	t.Setenv("IMAGE_PROXY_PROCESS_QUEUE_WAIT_SECONDS", "1")

	cfg := ConfigFromEnv()

	if cfg.ProcessQueueWait != DefaultProcessQueueWait {
		t.Errorf("ProcessQueueWait must not be env-configurable: expected %v, got %v",
			DefaultProcessQueueWait, cfg.ProcessQueueWait)
	}
}

func TestConfigFromEnv_MaxInFlightRequests(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  int
	}{
		{name: "valid value is applied", value: "16", want: 16},
		{name: "zero keeps the default", value: "0", want: DefaultMaxInFlightRequests},
		{name: "negative keeps the default", value: "-1", want: DefaultMaxInFlightRequests},
		{name: "non-numeric keeps the default", value: "x", want: DefaultMaxInFlightRequests},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("IMAGE_PROXY_MAX_IN_FLIGHT_REQUESTS", tt.value)

			cfg := ConfigFromEnv()

			if cfg.MaxInFlightRequests != tt.want {
				t.Errorf("IMAGE_PROXY_MAX_IN_FLIGHT_REQUESTS=%q: expected MaxInFlightRequests %d, got %d",
					tt.value, tt.want, cfg.MaxInFlightRequests)
			}
		})
	}
}
