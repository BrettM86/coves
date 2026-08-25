package xrpc

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"Coves/internal/api/middleware"
	"Coves/internal/api/reqbody"
)

// DecodeJSON decodes a size-capped JSON request body into dst and, on
// failure, writes the standard XRPC error response: 413 PayloadTooLarge when
// the body exceeds limit, 400 InvalidRequest otherwise. It returns false when
// a response has been written and the handler must stop.
//
// Handlers that need bespoke rejection logging or a non-XRPC response shape
// call reqbody.DecodeJSON directly instead.
func DecodeJSON(w http.ResponseWriter, r *http.Request, limit reqbody.Limit, dst any, opts ...reqbody.DecodeOption) bool {
	err := reqbody.DecodeJSON(w, r, limit, dst, opts...)
	if err == nil {
		return true
	}

	// Security event: oversize bodies and malformed JSON on public endpoints
	// are bot traffic worth counting. GetClientIP reads the proxy-set
	// X-Real-IP header, so the address survives Caddy in front of us —
	// r.RemoteAddr would only name the proxy.
	clientIP := middleware.GetClientIP(r)

	var tooLarge *reqbody.TooLargeError
	if errors.As(err, &tooLarge) {
		slog.Info("request body over limit",
			slog.String("path", r.URL.Path),
			slog.String("client_ip", clientIP),
			slog.Int64("limit", tooLarge.Limit),
		)
		WriteError(w, http.StatusRequestEntityTooLarge, "PayloadTooLarge",
			fmt.Sprintf("Request body too large (limit %d bytes)", tooLarge.Limit))
		return false
	}

	slog.Info("request body decode failed",
		slog.String("path", r.URL.Path),
		slog.String("client_ip", clientIP),
		slog.String("error", err.Error()),
	)
	WriteError(w, http.StatusBadRequest, "InvalidRequest", "Invalid request body")
	return false
}
