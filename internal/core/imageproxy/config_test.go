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
				Enabled:         true,
				BaseURL:         "/img",
				CachePath:       "/var/cache/images",
				CacheMaxGB:      10,
				FetchTimeout:    30 * time.Second,
				MaxSourceSizeMB: 10,
			},
			wantErr: nil,
		},
		{
			name: "invalid CacheMaxGB zero",
			config: Config{
				Enabled:         false,
				BaseURL:         "/img",
				CachePath:       "/var/cache/images",
				CacheMaxGB:      0,
				FetchTimeout:    30 * time.Second,
				MaxSourceSizeMB: 10,
			},
			wantErr: ErrInvalidCacheMaxGB,
		},
		{
			name: "invalid CacheMaxGB negative",
			config: Config{
				Enabled:         false,
				BaseURL:         "/img",
				CachePath:       "/var/cache/images",
				CacheMaxGB:      -5,
				FetchTimeout:    30 * time.Second,
				MaxSourceSizeMB: 10,
			},
			wantErr: ErrInvalidCacheMaxGB,
		},
		{
			name: "invalid FetchTimeout zero",
			config: Config{
				Enabled:         false,
				BaseURL:         "/img",
				CachePath:       "/var/cache/images",
				CacheMaxGB:      10,
				FetchTimeout:    0,
				MaxSourceSizeMB: 10,
			},
			wantErr: ErrInvalidFetchTimeout,
		},
		{
			name: "invalid FetchTimeout negative",
			config: Config{
				Enabled:         false,
				BaseURL:         "/img",
				CachePath:       "/var/cache/images",
				CacheMaxGB:      10,
				FetchTimeout:    -1 * time.Second,
				MaxSourceSizeMB: 10,
			},
			wantErr: ErrInvalidFetchTimeout,
		},
		{
			name: "invalid MaxSourceSizeMB zero",
			config: Config{
				Enabled:         false,
				BaseURL:         "/img",
				CachePath:       "/var/cache/images",
				CacheMaxGB:      10,
				FetchTimeout:    30 * time.Second,
				MaxSourceSizeMB: 0,
			},
			wantErr: ErrInvalidMaxSourceSize,
		},
		{
			name: "invalid MaxSourceSizeMB negative",
			config: Config{
				Enabled:         false,
				BaseURL:         "/img",
				CachePath:       "/var/cache/images",
				CacheMaxGB:      10,
				FetchTimeout:    30 * time.Second,
				MaxSourceSizeMB: -5,
			},
			wantErr: ErrInvalidMaxSourceSize,
		},
		{
			name: "enabled but missing CachePath",
			config: Config{
				Enabled:         true,
				BaseURL:         "/img",
				CachePath:       "",
				CacheMaxGB:      10,
				FetchTimeout:    30 * time.Second,
				MaxSourceSizeMB: 10,
			},
			wantErr: ErrMissingCachePath,
		},
		{
			name: "enabled allows empty BaseURL for relative URLs",
			config: Config{
				Enabled:         true,
				BaseURL:         "",
				CachePath:       "/var/cache/images",
				CacheMaxGB:      10,
				FetchTimeout:    30 * time.Second,
				MaxSourceSizeMB: 10,
			},
			wantErr: nil,
		},
		{
			name: "disabled allows empty CachePath",
			config: Config{
				Enabled:         false,
				BaseURL:         "/img",
				CachePath:       "",
				CacheMaxGB:      10,
				FetchTimeout:    30 * time.Second,
				MaxSourceSizeMB: 10,
			},
			wantErr: nil,
		},
		{
			name: "disabled allows empty BaseURL",
			config: Config{
				Enabled:         false,
				BaseURL:         "",
				CachePath:       "/var/cache/images",
				CacheMaxGB:      10,
				FetchTimeout:    30 * time.Second,
				MaxSourceSizeMB: 10,
			},
			wantErr: nil,
		},
		{
			name: "valid TTL zero (disabled)",
			config: Config{
				Enabled:         true,
				BaseURL:         "",
				CachePath:       "/var/cache/images",
				CacheMaxGB:      10,
				CacheTTLDays:    0,
				FetchTimeout:    30 * time.Second,
				MaxSourceSizeMB: 10,
			},
			wantErr: nil,
		},
		{
			name: "valid TTL positive",
			config: Config{
				Enabled:         true,
				BaseURL:         "",
				CachePath:       "/var/cache/images",
				CacheMaxGB:      10,
				CacheTTLDays:    30,
				FetchTimeout:    30 * time.Second,
				MaxSourceSizeMB: 10,
			},
			wantErr: nil,
		},
		{
			name: "invalid TTL negative",
			config: Config{
				Enabled:         true,
				BaseURL:         "",
				CachePath:       "/var/cache/images",
				CacheMaxGB:      10,
				CacheTTLDays:    -1,
				FetchTimeout:    30 * time.Second,
				MaxSourceSizeMB: 10,
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
