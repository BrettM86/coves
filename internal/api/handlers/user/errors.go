package user

import (
	"net/http"

	"Coves/internal/api/xrpc"
	"Coves/internal/atproto/pds"
	"Coves/internal/core/users"
)

// writeJSONError writes a JSON error response.
func writeJSONError(w http.ResponseWriter, statusCode int, errorType, message string) {
	xrpc.WriteError(w, statusCode, errorType, message)
}

// writeUpdateProfileError writes a JSON error response for update profile failures.
func writeUpdateProfileError(w http.ResponseWriter, statusCode int, errorType, message string) {
	xrpc.WriteError(w, statusCode, errorType, message)
}

// accountErrorMapper maps users service errors for the account endpoints.
//
// The users service talks to the PDS admin API over plain HTTP rather than
// through internal/atproto/pds, so it raises no PDS sentinels; the inherited
// rules cost nothing and cover it if that changes.
var accountErrorMapper = xrpc.NewMapper("user",
	xrpc.Sentinel(users.ErrUserNotFound, http.StatusNotFound,
		"AccountNotFound", "Account not found"),
	xrpc.As[*users.InvalidDIDError](http.StatusBadRequest, "InvalidDID",
		func(e *users.InvalidDIDError) string { return e.Error() }),
)

// updateProfileMapper is the base for the updateProfile endpoint, the only
// handler that drives a pds.Client directly.
//
// It answers 401 with AuthExpired rather than the default AuthRequired because
// clients already key on that code here. 403 is left to the inherited rule: it
// is a permissions problem — an OAuth grant predating the blob:*/* scope, say —
// not an expired session, so it must not trigger a sign-out, even though
// signing in again is what re-grants the scope.
var updateProfileMapper = xrpc.NewMapper("user.updateProfile").
	WithReauth("AuthExpired", "Your session may have expired. Please re-authenticate.")

// sessionRestoreMapper covers building the PDS client. If that fails at all
// there is no usable session for this request, whatever the cause, so every
// outcome is 401 — the fallback included. The cause is still logged.
var sessionRestoreMapper = xrpc.NewMapper("user.updateProfile.session").
	WithReauth("SessionError", "Failed to restore session. Please sign in again.").
	WithFallback(http.StatusUnauthorized, "SessionError", "Failed to restore session. Please sign in again.")

// newBlobUploadMapper is shared by the avatar and banner uploads, which differ
// only in the code they use for an oversized image.
func newBlobUploadMapper(tooLargeCode, tooLargeMessage, failureMessage string) *xrpc.Mapper {
	return updateProfileMapper.
		WithFallback(http.StatusInternalServerError, "BlobUploadFailed", failureMessage).
		With(
			xrpc.Sentinel(pds.ErrForbidden, http.StatusForbidden, "PermissionDenied",
				"Your session does not have permission to upload images. Sign out and back in to grant it."),
			xrpc.Sentinel(pds.ErrRateLimited, http.StatusTooManyRequests, "RateLimited",
				"Too many requests. Please try again later."),
			xrpc.Sentinel(pds.ErrPayloadTooLarge, http.StatusRequestEntityTooLarge,
				tooLargeCode, tooLargeMessage),
		)
}

var (
	avatarUploadMapper = newBlobUploadMapper(
		"AvatarTooLarge", "Avatar exceeds PDS size limit.", "Failed to upload avatar")
	bannerUploadMapper = newBlobUploadMapper(
		"BannerTooLarge", "Banner exceeds PDS size limit.", "Failed to upload banner")

	// putProfileMapper covers writing the profile record itself.
	putProfileMapper = updateProfileMapper.
				WithFallback(http.StatusInternalServerError, "PDSError", "Failed to update profile").
				With(
			xrpc.Sentinel(pds.ErrForbidden, http.StatusForbidden, "PermissionDenied",
				"Your session does not have permission to update your profile. Sign out and back in to grant it."),
			xrpc.Sentinel(pds.ErrRateLimited, http.StatusTooManyRequests, "RateLimited",
				"Too many requests. Please try again later."),
			// Same code as the inherited rule, kept for its profile-specific wording.
			xrpc.Sentinel(pds.ErrPayloadTooLarge, http.StatusRequestEntityTooLarge,
				"PayloadTooLarge", "Profile data exceeds PDS size limit."),
		)
)
