package config

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const cdnPurgeTestToken = "cf-purge-token-SENTINEL-7f3a"

func TestLoadCDNPurge(t *testing.T) {
	for _, mode := range []string{"dev", "production"} {
		for _, test := range []struct {
			name, zone, token, base, cdn, proxy, missing string
			baseURLs                                     []string
			devOnly                                      bool
		}{
			{name: "unset", base: "https://img.example.test"},
			{name: "zone only", zone: "zone-abc", base: "https://img.example.test", missing: "CLOUDFLARE_CACHE_PURGE_API_TOKEN"},
			{name: "token only", token: cdnPurgeTestToken, base: "https://img.example.test", missing: "CLOUDFLARE_CACHE_PURGE_ZONE_ID"},
			{name: "proxy disabled", zone: "zone-abc", token: cdnPurgeTestToken, base: "https://img.example.test", proxy: "false", missing: "IMAGE_PROXY_ENABLED"},
			{name: "relative base", zone: "zone-abc", token: cdnPurgeTestToken, base: "/media", cdn: "https://cdn.example.test", missing: "IMAGE_PROXY_BASE_URL"},
			{name: "empty base", zone: "zone-abc", token: cdnPurgeTestToken, base: "", cdn: "https://cdn.example.test", missing: "IMAGE_PROXY_BASE_URL"},
			{name: "relative CDN", zone: "zone-abc", token: cdnPurgeTestToken, base: "https://img.example.test", cdn: "/media", missing: "IMAGE_PROXY_CDN_URL", devOnly: true},
			{name: "base only", zone: "zone-abc", token: cdnPurgeTestToken, base: "https://img.example.test", baseURLs: []string{"https://img.example.test"}},
			{name: "distinct CDN", zone: "zone-abc", token: cdnPurgeTestToken, base: "https://img.example.test", cdn: "https://cdn.example.test", baseURLs: []string{"https://img.example.test", "https://cdn.example.test"}},
			{name: "same CDN", zone: "zone-abc", token: cdnPurgeTestToken, base: "https://img.example.test", cdn: "https://img.example.test", baseURLs: []string{"https://img.example.test"}},
			{name: "unset empty base", base: "", devOnly: true},
			{name: "unset proxy disabled", base: "https://img.example.test", proxy: "false"},
		} {
			if test.devOnly && mode == "production" {
				continue
			}
			t.Run(mode+"/"+test.name, func(t *testing.T) {
				clearEnv(t)
				if mode == "production" {
					prodEnv(t)
					t.Setenv("ALLOW_UNPROXIED_MEDIA", "true")
				} else {
					t.Setenv("IS_DEV_ENV", "true")
				}
				t.Setenv("IMAGE_PROXY_ENABLED", test.proxy)
				if test.proxy == "" {
					t.Setenv("IMAGE_PROXY_ENABLED", "true")
				}
				t.Setenv("IMAGE_PROXY_BASE_URL", test.base)
				t.Setenv("IMAGE_PROXY_CDN_URL", test.cdn)
				t.Setenv("CLOUDFLARE_CACHE_PURGE_ZONE_ID", "")
				t.Setenv("CLOUDFLARE_CACHE_PURGE_API_TOKEN", "")
				// Control: the otherwise identical environment must load without purge.
				if test.missing != "" {
					_, err := Load()
					require.NoError(t, err, "control without purge")
				}
				t.Setenv("CLOUDFLARE_CACHE_PURGE_ZONE_ID", test.zone)
				t.Setenv("CLOUDFLARE_CACHE_PURGE_API_TOKEN", test.token)
				cfg, err := Load()
				if test.missing != "" {
					require.Error(t, err)
					assert.ErrorContains(t, err, test.missing)
					assert.NotContains(t, err.Error(), cdnPurgeTestToken)
					return
				}
				require.NoError(t, err)
				assert.Equal(t, test.zone != "", cfg.Media.CDNPurge.Enabled())
				if test.zone != "" {
					assert.Equal(t, "zone-abc", cfg.Media.CDNPurge.ZoneID)
					assert.Equal(t, cdnPurgeTestToken, cfg.Media.CDNPurge.APIToken)
					assert.Equal(t, test.baseURLs, cfg.Media.CDNPurge.BaseURLs)
				}
				assert.NotContains(t, fmt.Sprintf("%v", cfg), cdnPurgeTestToken)
				assert.NotContains(t, fmt.Sprintf("%+v", cfg), cdnPurgeTestToken)
				assert.NotContains(t, fmt.Sprintf("%#v", cfg), cdnPurgeTestToken)
				encoded, err := json.Marshal(cfg)
				require.NoError(t, err)
				assert.NotContains(t, string(encoded), cdnPurgeTestToken)
			})
		}
	}
}

func TestClearEnvForTestClearsCDNPurgeSettings(t *testing.T) {
	for _, name := range []string{"CLOUDFLARE_CACHE_PURGE_ZONE_ID", "CLOUDFLARE_CACHE_PURGE_API_TOKEN"} {
		t.Setenv(name, "set")
	}
	ClearEnvForTest(t)
	for _, name := range []string{"CLOUDFLARE_CACHE_PURGE_ZONE_ID", "CLOUDFLARE_CACHE_PURGE_API_TOKEN"} {
		assert.Empty(t, os.Getenv(name), "%s must be in loadedEnvVars", name)
	}
}
