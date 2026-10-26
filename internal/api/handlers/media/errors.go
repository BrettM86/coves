package media

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"Coves/internal/api/xrpc"
	"Coves/internal/atproto/pds"
	"Coves/internal/core/aggregators"
	coremedia "Coves/internal/core/media"
	"Coves/internal/core/posts"
)

// errorMapper maps upload failures to XRPC responses. The pipeline verdicts, an
// aggregator's unresumable stored session, and the PDS failures whose shared
// answer would hide that the upload failed come first; a dead session (401
// AuthRequired, checked ahead of every rule), session contention, rate limits
// and request lifecycle come from xrpc's shared rules.
var errorMapper = xrpc.NewMapper("media",
	pipelineVerdict(coremedia.ErrorCodeUnsupportedImageFormat, http.StatusBadRequest,
		"Images must be JPEG, PNG, WebP or GIF"),
	pipelineVerdict(coremedia.ErrorCodeImageDimensionsTooLarge, http.StatusBadRequest,
		"Image dimensions are too large"),
	pipelineVerdict(coremedia.ErrorCodeInvalidImage, http.StatusBadRequest,
		"Image could not be decoded"),
	pipelineVerdict(coremedia.ErrorCodeImageTooLarge, http.StatusRequestEntityTooLarge,
		"Processed image exceeds the 10,000,000 byte limit"),
	pipelineVerdict(coremedia.ErrorCodeUploadRateLimited, http.StatusTooManyRequests,
		"Too many uploads; try again in a few minutes"),

	// An aggregator's stored session the repo factory cannot resume answers as
	// post.create does, rather than as a PDS failure.
	xrpc.Sentinel(posts.ErrNoAuthorCredentials, http.StatusServiceUnavailable, "NoAuthorCredentials",
		"No credentials are available to write to your repository"),
	xrpc.Sentinel(posts.ErrAuthorCredentialsUnavailable, http.StatusServiceUnavailable, "ServiceUnavailable",
		"Could not load your stored credentials; try again"),

	// The caller's PDS has its own blob limit, which can be well under ours.
	xrpc.Sentinel(pds.ErrPayloadTooLarge, http.StatusRequestEntityTooLarge, "PayloadTooLarge",
		"Your PDS refused the image as larger than it accepts; try a smaller image"),

	// Ahead of the shared rules, which would answer 502 UpstreamFailure,
	// 400 InvalidRequest, 404 NotFound and 409 Conflict: a PDS 5xx, or a PDS
	// refusing the request we built or answering 404 or 409 to it, is a failed
	// upload rather than a fault in the client's request or a record outcome.
	xrpc.Sentinel(pds.ErrServerError, http.StatusBadGateway, string(coremedia.ErrorCodePDSUploadFailed),
		"Your PDS could not complete the upload"),
	xrpc.Sentinel(pds.ErrBadRequest, http.StatusBadGateway, string(coremedia.ErrorCodePDSUploadFailed),
		"Your PDS could not complete the upload"),
	xrpc.Sentinel(pds.ErrNotFound, http.StatusBadGateway, string(coremedia.ErrorCodePDSUploadFailed),
		"Your PDS could not complete the upload"),
	xrpc.Sentinel(pds.ErrConflict, http.StatusBadGateway, string(coremedia.ErrorCodePDSUploadFailed),
		"Your PDS could not complete the upload"),
)

var (
	// openRepoErrorMapper answers a failure to open the caller's repo.
	openRepoErrorMapper = errorMapper.WithFallback(http.StatusBadGateway,
		string(coremedia.ErrorCodePDSUploadFailed), "Could not reach your PDS")
	// uploadErrorMapper answers a failed pipeline run or blob upload.
	uploadErrorMapper = errorMapper.WithFallback(http.StatusBadGateway,
		string(coremedia.ErrorCodePDSUploadFailed), "Your PDS did not store the image")
)

// pipelineVerdict matches a *coremedia.Error carrying code.
func pipelineVerdict(code coremedia.ErrorCode, status int, message string) xrpc.Rule {
	return func(err error) (xrpc.Mapping, bool) {
		var mediaErr *coremedia.Error
		if !errors.As(err, &mediaErr) || mediaErr.Code != code {
			return xrpc.Mapping{}, false
		}
		return xrpc.Mapping{Status: status, Code: string(code), Message: message}, true
	}
}

type failureCause struct {
	target error
	name   string
	level  slog.Level
}

// failureCauses classifies a session or PDS failure for the log. Our own
// outages and wiring bugs log at Error; the rest are the user's session or PDS.
var failureCauses = []failureCause{
	{pds.ErrSessionExpired, "session_expired", slog.LevelWarn},
	{pds.ErrUnauthorized, "unauthorized", slog.LevelWarn},
	{pds.ErrForbidden, "forbidden", slog.LevelWarn},
	{pds.ErrSessionBusy, "session_busy", slog.LevelWarn},
	{pds.ErrRateLimited, "rate_limited", slog.LevelWarn},
	{pds.ErrPayloadTooLarge, "payload_too_large", slog.LevelWarn},
	{pds.ErrServerError, "pds_server_error", slog.LevelWarn},
	{pds.ErrBadRequest, "pds_bad_request", slog.LevelWarn},
	{pds.ErrNotFound, "pds_not_found", slog.LevelWarn},
	{pds.ErrConflict, "pds_conflict", slog.LevelWarn},
	{posts.ErrNoAuthorCredentials, "no_author_credentials", slog.LevelError},
	{posts.ErrAuthorCredentialsUnavailable, "author_credentials_unavailable", slog.LevelError},
	{aggregators.ErrOAuthSessionDead, "stored_session_dead", slog.LevelWarn},
	{context.Canceled, "canceled", slog.LevelWarn},
	{context.DeadlineExceeded, "deadline_exceeded", slog.LevelWarn},
}

func classifyFailure(err error) (string, slog.Level) {
	for _, cause := range failureCauses {
		if errors.Is(err, cause.target) {
			return cause.name, cause.level
		}
	}
	return "unclassified", slog.LevelError
}

// logFailure logs a classified cause, never err's text: session and PDS errors
// carry DIDs, the OAuth session ID and PDS response bodies.
func logFailure(ctx context.Context, message, userDID string, mapping xrpc.Mapping, err error) {
	cause, level := classifyFailure(err)
	slog.Log(ctx, level, message,
		"did", userDID, "error_code", mapping.Code, "status", mapping.Status, "cause", cause)
}
