// Package observability provides OpenTelemetry tracing configuration and middleware for Coves.
package observability

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
)

// Config validation errors
var (
	// ErrInvalidSampleRatio is returned when SampleRatio is outside the valid range [0.0, 1.0]
	ErrInvalidSampleRatio = errors.New("SampleRatio must be between 0.0 and 1.0")
	// ErrMissingEndpoint is returned when Endpoint is empty while tracing is enabled
	ErrMissingEndpoint = errors.New("Endpoint is required when observability is enabled")
)

// Config holds the configuration for OpenTelemetry tracing.
type Config struct {
	// Enabled determines whether OpenTelemetry tracing is active.
	Enabled bool

	// ServiceName is the name of this service in traces.
	ServiceName string

	// Endpoint is the OTLP collector endpoint (e.g., "http://localhost:4317").
	Endpoint string

	// Headers contains optional headers for the OTLP exporter (key=value,key2=value2 format).
	Headers string

	// SampleRatio is the trace sampling ratio between 0.0 and 1.0.
	// 1.0 means all traces are sampled, 0.5 means 50% are sampled.
	SampleRatio float64

	// Insecure determines whether to use an insecure gRPC connection to the collector.
	Insecure bool
}

// DefaultConfig returns a Config with sensible default values.
// By default, tracing is disabled to avoid unexpected overhead.
func DefaultConfig() Config {
	return Config{
		Enabled:     false,
		ServiceName: "coves-appview",
		Endpoint:    "http://localhost:4317",
		Headers:     "",
		SampleRatio: 1.0,
		Insecure:    false,
	}
}

// ConfigFromEnv creates a Config from environment variables.
// Uses defaults for any missing environment variables.
//
// Environment variables:
//   - OTEL_ENABLED: "true"/"1" to enable, "false"/"0" to disable (default: false)
//   - OTEL_SERVICE_NAME: service name for traces (default: "coves-appview")
//   - OTEL_EXPORTER_OTLP_ENDPOINT: OTLP collector endpoint (default: "http://localhost:4317")
//   - OTEL_EXPORTER_OTLP_HEADERS: headers for OTLP exporter in key=value,key2=value2 format (default: "")
//   - OTEL_TRACES_SAMPLER_ARG: sampling ratio 0.0-1.0 (default: 1.0)
//   - OTEL_EXPORTER_OTLP_INSECURE: "true"/"1" for insecure connection (default: false)
func ConfigFromEnv() Config {
	cfg := DefaultConfig()

	if v := os.Getenv("OTEL_ENABLED"); v != "" {
		cfg.Enabled = v == "true" || v == "1"
	}

	if v := os.Getenv("OTEL_SERVICE_NAME"); v != "" {
		cfg.ServiceName = v
	}

	if v := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); v != "" {
		cfg.Endpoint = v
	}

	if v := os.Getenv("OTEL_EXPORTER_OTLP_HEADERS"); v != "" {
		cfg.Headers = v
	}

	if v := os.Getenv("OTEL_TRACES_SAMPLER_ARG"); v != "" {
		if ratio, err := strconv.ParseFloat(v, 64); err == nil && ratio >= 0.0 && ratio <= 1.0 {
			cfg.SampleRatio = ratio
		} else {
			slog.Warn("[OTEL] invalid OTEL_TRACES_SAMPLER_ARG value, using default",
				"value", v,
				"default", cfg.SampleRatio,
				"error", err,
			)
		}
	}

	if v := os.Getenv("OTEL_EXPORTER_OTLP_INSECURE"); v != "" {
		cfg.Insecure = v == "true" || v == "1"
	}

	return cfg
}

// Validate checks the configuration for invalid values.
// Returns nil if the configuration is valid, or an error describing the problem.
// When Enabled is false, only the SampleRatio is validated (for safety).
// When Enabled is true, all required fields must be set.
func (c Config) Validate() error {
	// Always validate SampleRatio regardless of enabled state
	if c.SampleRatio < 0.0 || c.SampleRatio > 1.0 {
		return fmt.Errorf("%w: got %f", ErrInvalidSampleRatio, c.SampleRatio)
	}

	// When enabled, validate required fields
	if c.Enabled {
		if c.Endpoint == "" {
			return ErrMissingEndpoint
		}
	}

	return nil
}
