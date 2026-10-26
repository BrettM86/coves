package config

import (
	"os"
	"testing"
)

func TestMediaUploadMaxConcurrentConfig(t *testing.T) {
	for _, row := range []struct {
		name, value string
		set         bool
		want        int
		wantError   bool
	}{
		{"unset", "", false, 4, false},
		{"two", "2", true, 2, false},
		{"non-integer", "abc", true, 0, true},
		{"zero", "0", true, 0, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("IS_DEV_ENV", "true")
			t.Setenv("MEDIA_UPLOAD_MAX_CONCURRENT", "")
			if !row.set {
				if err := os.Unsetenv("MEDIA_UPLOAD_MAX_CONCURRENT"); err != nil {
					t.Fatal(err)
				}
			} else {
				t.Setenv("MEDIA_UPLOAD_MAX_CONCURRENT", row.value)
			}
			cfg, err := Load()
			if row.wantError {
				if err == nil {
					t.Fatalf("Load(MEDIA_UPLOAD_MAX_CONCURRENT=%q): want error", row.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Media.Upload.MaxConcurrent != row.want {
				t.Fatalf("Media.Upload.MaxConcurrent = %d, want %d", cfg.Media.Upload.MaxConcurrent, row.want)
			}
		})
	}
}

func TestMediaUploadMaxLongEdgeConfig(t *testing.T) {
	for _, row := range []struct {
		name, value string
		set         bool
		want        int
		wantError   bool
	}{
		{"unset", "", false, 4096, false},
		{"2048", "2048", true, 2048, false},
		{"zero", "0", true, 0, true},
		{"negative", "-1", true, 0, true},
		{"non-integer", "abc", true, 0, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("IS_DEV_ENV", "true")
			t.Setenv("MEDIA_UPLOAD_MAX_LONG_EDGE", "")
			if !row.set {
				if err := os.Unsetenv("MEDIA_UPLOAD_MAX_LONG_EDGE"); err != nil {
					t.Fatal(err)
				}
			} else {
				t.Setenv("MEDIA_UPLOAD_MAX_LONG_EDGE", row.value)
			}
			cfg, err := Load()
			if row.wantError {
				if err == nil {
					t.Fatalf("Load(MEDIA_UPLOAD_MAX_LONG_EDGE=%q): want error", row.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Media.Upload.MaxLongEdge != row.want {
				t.Fatalf("Media.Upload.MaxLongEdge = %d, want %d", cfg.Media.Upload.MaxLongEdge, row.want)
			}
		})
	}
}

func TestMediaUploadRateLimitConfig(t *testing.T) {
	for _, row := range []struct {
		name, value string
		set         bool
		want        int
		wantError   bool
	}{
		{"unset", "", false, 60, false},
		{"thirty", "30", true, 30, false},
		{"zero", "0", true, 0, true},
		{"negative", "-1", true, 0, true},
		{"non-integer", "abc", true, 0, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("IS_DEV_ENV", "true")
			t.Setenv("MEDIA_UPLOAD_RATE_PER_10MIN", "")
			if !row.set {
				if err := os.Unsetenv("MEDIA_UPLOAD_RATE_PER_10MIN"); err != nil {
					t.Fatal(err)
				}
			} else {
				t.Setenv("MEDIA_UPLOAD_RATE_PER_10MIN", row.value)
			}
			cfg, err := Load()
			if row.wantError {
				if err == nil {
					t.Fatalf("Load(MEDIA_UPLOAD_RATE_PER_10MIN=%q): want error", row.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Media.UploadLimit.UploadsPerTenMinutes != row.want {
				t.Fatalf("Media.UploadLimit.UploadsPerTenMinutes = %d, want %d", cfg.Media.UploadLimit.UploadsPerTenMinutes, row.want)
			}
		})
	}
}

// B8, T0: load the operator byte cap and near-cap fraction at the env boundary.
func TestMediaUploadByteLimitAndWarningConfig(t *testing.T) {
	for _, row := range []struct {
		name, bytes, fraction string
		wantBytes             int64
		wantFraction          float64
		wantError             bool
	}{
		{"defaults", "", "", 500000000, 0.8, false},
		{"overrides", "750000000", "0.5", 750000000, 0.5, false},
		{"fraction one", "1", "1", 1, 1, false},
		{"zero bytes", "0", "0.8", 0, 0, true},
		{"negative bytes", "-1", "0.8", 0, 0, true},
		{"invalid bytes", "abc", "0.8", 0, 0, true},
		{"overflow bytes", "9223372036854775808", "0.8", 0, 0, true},
		{"zero fraction", "500000000", "0", 0, 0, true},
		{"negative fraction", "500000000", "-0.1", 0, 0, true},
		{"fraction above one", "500000000", "1.01", 0, 0, true},
		{"invalid fraction", "500000000", "abc", 0, 0, true},
		{"NaN fraction", "500000000", "NaN", 0, 0, true},
		{"infinite fraction", "500000000", "+Inf", 0, 0, true},
	} {
		t.Run(row.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("IS_DEV_ENV", "true")
			// Set after clearEnv: it must not erase the override under test.
			t.Setenv("MEDIA_UPLOAD_BYTES_PER_DAY", row.bytes)
			t.Setenv("MEDIA_UPLOAD_WARN_FRACTION", row.fraction)
			if row.name == "defaults" {
				for _, name := range []string{"MEDIA_UPLOAD_BYTES_PER_DAY", "MEDIA_UPLOAD_WARN_FRACTION"} {
					if err := os.Unsetenv(name); err != nil {
						t.Fatal(err)
					}
				}
			}
			cfg, err := Load()
			if row.wantError {
				if err == nil {
					t.Fatal("Load accepted an invalid upload byte limit or warning fraction")
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Media.UploadLimit.BytesPerDay != row.wantBytes || cfg.Media.UploadLimit.WarnFraction != row.wantFraction {
				t.Fatalf("upload byte limit/fraction: got %d/%g, want %d/%g", cfg.Media.UploadLimit.BytesPerDay, cfg.Media.UploadLimit.WarnFraction, row.wantBytes, row.wantFraction)
			}
		})
	}
}

func TestClearEnvForTestClearsUploadByteLimitAndWarning(t *testing.T) {
	t.Setenv("MEDIA_UPLOAD_BYTES_PER_DAY", "750000000")
	t.Setenv("MEDIA_UPLOAD_WARN_FRACTION", "0.5")
	ClearEnvForTest(t)
	for _, name := range []string{"MEDIA_UPLOAD_BYTES_PER_DAY", "MEDIA_UPLOAD_WARN_FRACTION"} {
		if value := os.Getenv(name); value != "" {
			t.Errorf("ClearEnvForTest left %s=%q, want empty", name, value)
		}
	}
}
