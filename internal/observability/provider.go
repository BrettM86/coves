package observability

import (
	"context"
	"fmt"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Provider manages the OpenTelemetry TracerProvider lifecycle.
type Provider struct {
	enabled        bool
	tracerProvider *trace.TracerProvider
}

// NewProvider creates a new OpenTelemetry provider based on the given configuration.
// If tracing is not enabled, it returns a Provider with enabled=false and no active tracer.
// When enabled, it creates an OTLP HTTP exporter and configures the global tracer provider.
func NewProvider(ctx context.Context, cfg Config) (*Provider, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid observability config: %w", err)
	}

	if !cfg.Enabled {
		return &Provider{enabled: false}, nil
	}

	// Build exporter options for HTTP
	opts := []otlptracehttp.Option{
		otlptracehttp.WithEndpoint(stripScheme(cfg.Endpoint)),
	}

	if cfg.Insecure {
		opts = append(opts, otlptracehttp.WithInsecure())
	}

	// Parse and add headers if provided
	if cfg.Headers != "" {
		headers := parseHeaders(cfg.Headers)
		if len(headers) > 0 {
			opts = append(opts, otlptracehttp.WithHeaders(headers))
		}
	}

	// Create the OTLP HTTP exporter
	exporter, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create OTLP trace exporter: %w", err)
	}

	// Create the resource with service information
	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(cfg.ServiceName),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %w", err)
	}

	// Create the sampler based on the sample ratio
	var sampler trace.Sampler
	if cfg.SampleRatio >= 1.0 {
		sampler = trace.AlwaysSample()
	} else if cfg.SampleRatio <= 0.0 {
		sampler = trace.NeverSample()
	} else {
		sampler = trace.TraceIDRatioBased(cfg.SampleRatio)
	}

	// Create the TracerProvider with the exporter and sampler
	tp := trace.NewTracerProvider(
		trace.WithBatcher(exporter),
		trace.WithResource(res),
		trace.WithSampler(sampler),
	)

	// Register the global tracer provider
	otel.SetTracerProvider(tp)

	return &Provider{
		enabled:        true,
		tracerProvider: tp,
	}, nil
}

// Shutdown flushes any remaining spans and shuts down the tracer provider.
// It should be called when the application is shutting down to ensure
// all traces are exported before the process exits.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil || !p.enabled || p.tracerProvider == nil {
		return nil
	}

	if err := p.tracerProvider.Shutdown(ctx); err != nil {
		return fmt.Errorf("failed to shutdown tracer provider: %w", err)
	}

	return nil
}

// Enabled returns whether tracing is enabled for this provider.
func (p *Provider) Enabled() bool {
	if p == nil {
		return false
	}
	return p.enabled
}

// stripScheme removes the http:// or https:// prefix from an endpoint URL.
// The HTTP exporter expects just host:port, not a full URL with scheme.
func stripScheme(endpoint string) string {
	endpoint = strings.TrimPrefix(endpoint, "http://")
	endpoint = strings.TrimPrefix(endpoint, "https://")
	return endpoint
}

// parseHeaders parses a comma-separated list of key=value pairs into a map.
// Example: "key1=value1,key2=value2" -> map[string]string{"key1": "value1", "key2": "value2"}
func parseHeaders(headers string) map[string]string {
	result := make(map[string]string)
	pairs := strings.Split(headers, ",")
	for _, pair := range pairs {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			value := strings.TrimSpace(parts[1])
			if key != "" {
				result[key] = value
			}
		}
	}
	return result
}
