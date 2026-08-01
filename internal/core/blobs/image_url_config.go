package blobs

import (
	"log/slog"
	"sync"
)

// Process-wide image URL configuration.
//
// Every view builder in the AppView — post feeds, comment threads, profiles,
// community headers — needs the same answer to "what URL should a client fetch
// this blob from?", and none of them are constructed with that answer in hand.
// Threading one immutable value through a dozen service constructors buys
// nothing, so it is published once at startup and read wherever a view is
// rendered.
//
// It lives in blobs rather than in any one consumer: posts, comments, users,
// communities and the Postgres repos all read it, and all of them already
// depend on blobs for HydrateImageURL.
var (
	imageURLConfigMu  sync.RWMutex
	imageURLConfig    ImageURLConfig
	imageURLConfigSet bool
)

// SetImageURLConfig publishes the image URL configuration for the process.
// The server wiring calls this once during startup, on every path — including
// the one where the proxy is disabled — because readers must know whether to
// render proxy URLs or direct blob URLs.
//
// The first call wins. A later call carrying a different configuration is
// ignored and logged: the value is read concurrently by request handlers, and
// swapping it mid-flight would hand different clients different URLs for the
// same blob. A repeat call is a wiring bug, not a reconfiguration hook.
func SetImageURLConfig(config ImageURLConfig) {
	imageURLConfigMu.Lock()
	defer imageURLConfigMu.Unlock()

	if imageURLConfigSet {
		if config != imageURLConfig {
			slog.Warn("[IMAGE-PROXY] SetImageURLConfig called again with a different configuration; ignoring",
				"existing_proxy_enabled", imageURLConfig.ProxyEnabled,
				"existing_proxy_base_url", imageURLConfig.ProxyBaseURL,
				"existing_cdn_url", imageURLConfig.CDNURL,
				"ignored_proxy_enabled", config.ProxyEnabled,
				"ignored_proxy_base_url", config.ProxyBaseURL,
				"ignored_cdn_url", config.CDNURL,
			)
		}
		return
	}

	imageURLConfig = config
	imageURLConfigSet = true
}

// GetImageURLConfig returns the published image URL configuration. Safe for
// concurrent use. Before SetImageURLConfig runs it reports the proxy as
// disabled, which makes URL generation fall back to direct PDS blob URLs.
func GetImageURLConfig() ImageURLConfig {
	imageURLConfigMu.RLock()
	defer imageURLConfigMu.RUnlock()
	return imageURLConfig
}

// ResetImageURLConfigForTesting clears the published configuration so a test
// can publish its own. Tests only — production code must treat the
// configuration as write-once.
func ResetImageURLConfigForTesting() {
	imageURLConfigMu.Lock()
	defer imageURLConfigMu.Unlock()
	imageURLConfig = ImageURLConfig{}
	imageURLConfigSet = false
}
