package communitysignals

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config controls alert delivery and the weekly digest. Events are recorded
// whatever it says; it only decides what is sent.
type Config struct {
	AlertsEnabled       bool          // COMMUNITY_SIGNAL_ALERTS_ENABLED, default false
	SweepInterval       time.Duration // COMMUNITY_SIGNAL_SWEEP_INTERVAL, default 5m
	AttemptLimit        int           // COMMUNITY_SIGNAL_ATTEMPT_LIMIT, default 8
	RetryBaseDelay      time.Duration // COMMUNITY_SIGNAL_RETRY_BASE_DELAY, default 1m
	DigestMinPosts      int           // COMMUNITY_SIGNAL_DIGEST_MIN_POSTS, default 10
	DigestRiseThreshold float64       // COMMUNITY_SIGNAL_DIGEST_RISE_POINTS, default 20
}

// ConfigFromEnv reads Config from the COMMUNITY_SIGNAL_* environment
// variables. An unset or empty variable takes its default; a malformed or
// out-of-range one is an error.
func ConfigFromEnv() (Config, error) {
	cfg := Config{
		SweepInterval:       5 * time.Minute,
		AttemptLimit:        8,
		RetryBaseDelay:      time.Minute,
		DigestMinPosts:      10,
		DigestRiseThreshold: 20,
	}
	var err error
	if raw := lookup("COMMUNITY_SIGNAL_ALERTS_ENABLED"); raw != "" {
		if cfg.AlertsEnabled, err = strconv.ParseBool(raw); err != nil {
			return Config{}, fmt.Errorf("COMMUNITY_SIGNAL_ALERTS_ENABLED: %w", err)
		}
	}
	if raw := lookup("COMMUNITY_SIGNAL_SWEEP_INTERVAL"); raw != "" {
		if cfg.SweepInterval, err = time.ParseDuration(raw); err != nil {
			return Config{}, fmt.Errorf("COMMUNITY_SIGNAL_SWEEP_INTERVAL: %w", err)
		}
	}
	if raw := lookup("COMMUNITY_SIGNAL_ATTEMPT_LIMIT"); raw != "" {
		if cfg.AttemptLimit, err = strconv.Atoi(raw); err != nil {
			return Config{}, fmt.Errorf("COMMUNITY_SIGNAL_ATTEMPT_LIMIT: %w", err)
		}
	}
	if raw := lookup("COMMUNITY_SIGNAL_RETRY_BASE_DELAY"); raw != "" {
		if cfg.RetryBaseDelay, err = time.ParseDuration(raw); err != nil {
			return Config{}, fmt.Errorf("COMMUNITY_SIGNAL_RETRY_BASE_DELAY: %w", err)
		}
	}
	if raw := lookup("COMMUNITY_SIGNAL_DIGEST_MIN_POSTS"); raw != "" {
		if cfg.DigestMinPosts, err = strconv.Atoi(raw); err != nil {
			return Config{}, fmt.Errorf("COMMUNITY_SIGNAL_DIGEST_MIN_POSTS: %w", err)
		}
	}
	if raw := lookup("COMMUNITY_SIGNAL_DIGEST_RISE_POINTS"); raw != "" {
		if cfg.DigestRiseThreshold, err = strconv.ParseFloat(raw, 64); err != nil {
			return Config{}, fmt.Errorf("COMMUNITY_SIGNAL_DIGEST_RISE_POINTS: %w", err)
		}
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) validate() error {
	switch {
	case c.SweepInterval <= 0:
		return fmt.Errorf("COMMUNITY_SIGNAL_SWEEP_INTERVAL must be positive, got %s", c.SweepInterval)
	case c.RetryBaseDelay <= 0:
		return fmt.Errorf("COMMUNITY_SIGNAL_RETRY_BASE_DELAY must be positive, got %s", c.RetryBaseDelay)
	case c.AttemptLimit < 1:
		return fmt.Errorf("COMMUNITY_SIGNAL_ATTEMPT_LIMIT must be at least 1, got %d", c.AttemptLimit)
	case c.DigestMinPosts < 1:
		return fmt.Errorf("COMMUNITY_SIGNAL_DIGEST_MIN_POSTS must be at least 1, got %d", c.DigestMinPosts)
	case c.DigestRiseThreshold < 0 || math.IsNaN(c.DigestRiseThreshold) || math.IsInf(c.DigestRiseThreshold, 0):
		return fmt.Errorf("COMMUNITY_SIGNAL_DIGEST_RISE_POINTS must be a finite number >= 0, got %v", c.DigestRiseThreshold)
	}
	return nil
}

func lookup(key string) string {
	return strings.TrimSpace(os.Getenv(key))
}
