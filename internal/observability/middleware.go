package observability

import (
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// HTTPMiddleware returns an HTTP middleware that instruments requests with OpenTelemetry tracing.
// If the provider is nil or tracing is not enabled, it returns nil (no middleware applied).
// The middleware adds trace context to incoming requests and creates spans for each request.
func HTTPMiddleware(p *Provider) func(http.Handler) http.Handler {
	if p == nil || !p.Enabled() {
		return nil
	}

	// Use otelhttp for standard net/http instrumentation
	return func(next http.Handler) http.Handler {
		return otelhttp.NewHandler(next, "coves-appview")
	}
}
