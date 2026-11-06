package config

import (
	"strings"
	"testing"

	"Coves/internal/core/blobs"
)

func TestBlueskyProfileImagePreviewSourceConfig(t *testing.T) {
	for _, row := range []struct {
		name, value string
		want        blobs.BlueskyProfileImagePreviewSource
		wantError   bool
	}{
		{name: "unset defaults to the Bluesky CDN", value: "", want: blobs.BlueskyProfileImagePreviewSourceBlueskyCDN},
		{name: "bluesky_cdn", value: "bluesky_cdn", want: blobs.BlueskyProfileImagePreviewSourceBlueskyCDN},
		{name: "pds", value: "pds", want: blobs.BlueskyProfileImagePreviewSourcePDS},
		{name: "pds with trailing space", value: "pds ", want: blobs.BlueskyProfileImagePreviewSourcePDS},
		{name: "unknown value", value: "proxy", wantError: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("IS_DEV_ENV", "true")
			t.Setenv("BLUESKY_PROFILE_IMAGE_PREVIEW_SOURCE", row.value)
			cfg, err := Load()
			if row.wantError {
				if err == nil {
					t.Fatalf("Load(BLUESKY_PROFILE_IMAGE_PREVIEW_SOURCE=%q): want error", row.value)
				}
				if !strings.Contains(err.Error(), "BLUESKY_PROFILE_IMAGE_PREVIEW_SOURCE") {
					t.Fatalf("error %q must name BLUESKY_PROFILE_IMAGE_PREVIEW_SOURCE", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Media.BlueskyProfileImagePreviewSource != row.want {
				t.Fatalf("Media.BlueskyProfileImagePreviewSource = %q, want %q",
					cfg.Media.BlueskyProfileImagePreviewSource, row.want)
			}
		})
	}
}
