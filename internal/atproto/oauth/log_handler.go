package oauth

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"runtime"
	"strings"

	"github.com/lib/pq"
)

// NewOAuthLogHandler protects Coves and Indigo OAuth logs at process startup.
// OAuth messages, errors, and inherited attributes may contain credentials or
// identity data. Only explicitly classified diagnostics survive. Other sources
// retain the downstream handler's attributes, groups, levels, and behavior.
// Use a concrete downstream handler, not slog's default log-bridge handler,
// when installing this with slog.SetDefault (the bridge can recurse).
func NewOAuthLogHandler(next slog.Handler) slog.Handler {
	return &oauthLogHandler{next: next, safe: next}
}

type oauthLogHandler struct{ next, safe slog.Handler }

func (h *oauthLogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *oauthLogHandler) Handle(ctx context.Context, record slog.Record) error {
	frame, _ := runtime.CallersFrames([]uintptr{record.PC}).Next()
	fromCoves := strings.HasPrefix(frame.Function, "Coves/internal/atproto/oauth.")
	if !fromCoves && !strings.HasPrefix(frame.Function, "github.com/bluesky-social/indigo/atproto/auth/oauth.") {
		return h.next.Handle(ctx, record)
	}
	// Coves messages are compile-time literals (pinned by
	// TestOAuthPackage_LogMessagesAreCompileTimeLiterals), so they are kept.
	// Indigo messages may embed provider-controlled values, so they are replaced.
	// Function names are compiler metadata and are always safe to forward.
	message := "OAuth diagnostic"
	if fromCoves {
		message = record.Message
	}
	clean := slog.NewRecord(record.Time, record.Level, message, record.PC)
	clean.AddAttrs(slog.String("source", frame.Function))
	record.Attrs(func(attr slog.Attr) bool {
		clean.AddAttrs(safeOAuthLogAttrs(attr)...)
		return true
	})
	return h.safe.Handle(ctx, clean)
}

func (h *oauthLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	var safe []slog.Attr
	for _, attr := range attrs {
		safe = append(safe, safeOAuthLogAttrs(attr)...)
	}
	return &oauthLogHandler{next: h.next.WithAttrs(attrs), safe: h.safe.WithAttrs(safe)}
}

func (h *oauthLogHandler) WithGroup(name string) slog.Handler {
	// Group names are caller-controlled too. Flatten only OAuth diagnostics;
	// preserve the original grouping for every unrelated source.
	return &oauthLogHandler{next: h.next.WithGroup(name), safe: h.safe}
}

func safeOAuthLogAttrs(attr slog.Attr) []slog.Attr {
	if attr.Value.Kind() == slog.KindGroup {
		var attrs []slog.Attr
		for _, child := range attr.Value.Group() {
			attrs = append(attrs, safeOAuthLogAttrs(child)...)
		}
		return attrs
	}
	if attr.Key == "error" || attr.Key == "err" {
		if err, ok := attr.Value.Any().(error); ok {
			return oauthErrorAttributes(err)
		}
	}
	switch attr.Key {
	case "operation":
		if attr.Value.Kind() == slog.KindString {
			switch attr.Value.String() {
			case "login_start", "callback", "mobile_lookup", "binding_claim", "return_url":
				return []slog.Attr{attr}
			}
		}
	case "category":
		if attr.Value.Kind() == slog.KindString {
			switch attr.Value.String() {
			case "provider", "database", "timeout", "cancelled", "internal", "cookie", "state", "binding", "store", "too_long", "host", "identity":
				return []slog.Attr{attr}
			}
		}
	case "code":
		// Only the clamped provider error set: anything else is provider-chosen text.
		if attr.Value.Kind() == slog.KindString {
			switch attr.Value.String() {
			case "access_denied", "invalid_request", "server_error", "temporarily_unavailable":
				return []slog.Attr{attr}
			}
		}
	case "request":
		if attr.Value.Kind() == slog.KindString {
			switch attr.Value.String() {
			case "PAR", "token", "initial-token", "token-refresh":
				return []slog.Attr{attr}
			}
		}
	case "statusCode":
		if attr.Value.Kind() == slog.KindInt64 && attr.Value.Int64() >= 100 && attr.Value.Int64() <= 599 {
			return []slog.Attr{attr}
		}
	case "sqlstate":
		if attr.Value.Kind() == slog.KindString && validSQLState(attr.Value.String()) {
			return []slog.Attr{attr}
		}
	case "length":
		if attr.Value.Kind() == slog.KindInt64 && attr.Value.Int64() >= 0 {
			return []slog.Attr{attr}
		}
	}
	return nil
}

func validSQLState(code string) bool {
	if len(code) != 5 {
		return false
	}
	for _, char := range code {
		if !(char >= '0' && char <= '9') && !(char >= 'A' && char <= 'Z') {
			return false
		}
	}
	return true
}

// Never format Error(): wrapped transport and database errors can carry OAuth
// URLs, provider response bodies, or SQL values. Keep only typed classifications.
func oauthErrorAttributes(err error) []slog.Attr {
	category := "internal"
	var databaseError *pq.Error
	var networkError net.Error
	switch {
	case errors.As(err, &databaseError):
		attrs := []slog.Attr{slog.String("category", "database")}
		if validSQLState(string(databaseError.Code)) {
			attrs = append(attrs, slog.String("sqlstate", string(databaseError.Code)))
		}
		return attrs
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &networkError) && networkError.Timeout():
		category = "timeout"
	case errors.Is(err, context.Canceled):
		category = "cancelled"
	case errors.Is(err, ErrWebBindingNotSaved):
		category = "binding"
	case isIdentityResolutionFailure(err):
		category = "identity"
	}
	return []slog.Attr{slog.String("category", category)}
}

func logOAuthFailure(ctx context.Context, operation string, err error) {
	attrs := []slog.Attr{slog.String("operation", operation)}
	attrs = append(attrs, oauthErrorAttributes(err)...)
	slog.LogAttrs(ctx, slog.LevelError, "OAuth operation failed", attrs...)
}
