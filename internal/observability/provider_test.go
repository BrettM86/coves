package observability

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/sdk/resource"
)

// The OpenTelemetry SDK stamps resource.Default() with the semantic-conventions
// schema URL of whichever semconv package the SDK itself was built against.
// resource.Merge refuses two non-empty, differing schema URLs, so the semconv
// package this module imports MUST track the SDK version in go.mod. A drift
// only surfaces when tracing is enabled (prod), never in an unconfigured test
// stack — which is exactly how it crash-looped the production appview once.
func TestNewResourceMergesWithSDKDefault(t *testing.T) {
	res, err := newResource("coves-appview")
	if err != nil {
		t.Fatalf("newResource: %v", err)
	}
	if got, want := res.SchemaURL(), resource.Default().SchemaURL(); got != want {
		t.Fatalf("schema URL = %q, want the SDK default %q", got, want)
	}
	found := false
	for _, attr := range res.Attributes() {
		if string(attr.Key) == "service.name" && attr.Value.AsString() == "coves-appview" {
			found = true
		}
	}
	if !found {
		t.Fatalf("service.name attribute missing from resource: %v", res.Attributes())
	}
}

// The full constructor with tracing enabled must come up; the exporter does
// not dial until the first export, so no collector is needed.
func TestNewProviderEnabledStartsWithoutCollector(t *testing.T) {
	provider, err := NewProvider(context.Background(), Config{
		Enabled:     true,
		Endpoint:    "http://127.0.0.1:4318", // coves:allow-host-literal: config VALUE handed to the OTLP exporter, never dialled — the exporter connects lazily on first export, and no spans are recorded here, so the cleanup Shutdown flushes nothing
		ServiceName: "coves-appview",
		Insecure:    true,
		SampleRatio: 1.0,
	})
	if err != nil {
		t.Fatalf("NewProvider(enabled): %v", err)
	}
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	if !provider.Enabled() {
		t.Fatal("provider reports disabled despite Enabled=true")
	}
}
