// Package config loads and validates the Coves AppView's process configuration
// from the environment.
//
// Every environment variable the server reads is declared here, in one place,
// so that a misconfigured deployment fails at startup with a precise message
// instead of surfacing as a confusing runtime failure. Packages that own a
// self-contained subsystem keep their own loaders (observability.ConfigFromEnv,
// imageproxy.ConfigFromEnv); this package covers the server's own wiring.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// lookup reads an environment variable, trimming surrounding whitespace.
// Docker Compose and .env files routinely leave trailing spaces, and a value
// like "true " silently failing a comparison is a miserable thing to debug.
func lookup(key string) string {
	return strings.TrimSpace(os.Getenv(key))
}

// stringVar returns the value of key, or fallback when it is unset or blank.
func stringVar(key, fallback string) string {
	if value := lookup(key); value != "" {
		return value
	}
	return fallback
}

// boolVar reports whether key is set to a recognised truthy value. Anything
// unparseable is reported as an error rather than silently treated as false —
// "IS_DEV_ENV=yes" quietly meaning "production" is exactly the kind of failure
// that only shows up after deploy.
func boolVar(key string, fallback bool) (bool, error) {
	raw := lookup(key)
	if raw == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s: %q is not a boolean (use true/false)", key, raw)
	}
	return parsed, nil
}

// durationVar parses a Go duration string (e.g. "30s", "5m", "1h").
func durationVar(key string, fallback time.Duration) (time.Duration, error) {
	raw := lookup(key)
	if raw == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a duration (e.g. \"30s\", \"5m\"): %w", key, raw, err)
	}
	if parsed < 0 {
		return 0, fmt.Errorf("%s: %q must not be negative", key, raw)
	}
	return parsed, nil
}

// intVar parses a non-negative integer.
func intVar(key string, fallback int) (int, error) {
	raw := lookup(key)
	if raw == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not an integer", key, raw)
	}
	if parsed < 0 {
		return 0, fmt.Errorf("%s: %q must not be negative", key, raw)
	}
	return parsed, nil
}

// csvVar splits a comma-separated list, dropping empty entries and trimming
// whitespace around each. Returns nil (not an empty slice) when unset, so
// callers can distinguish "not configured" from "configured empty".
func csvVar(key string) []string {
	raw := lookup(key)
	if raw == "" {
		return nil
	}
	var values []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			values = append(values, part)
		}
	}
	return values
}
