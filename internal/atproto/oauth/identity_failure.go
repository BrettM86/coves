package oauth

import (
	"errors"

	"github.com/bluesky-social/indigo/atproto/identity"
)

// ErrLoginIdentifierInvalid reports a login identifier that is neither a
// handle, a DID, nor an https:// authorization server URL.
var ErrLoginIdentifierInvalid = errors.New("login identifier is not a handle or DID")

// Indigo's ClientApp.StartAuthFlow returns this exact, unwrapped message when
// the resolved identity declares no PDS. It has no sentinel, so the pinned
// text is matched whole; TestWebOAuth_IdentityResolutionFailuresEndWithInvalidRequest
// exercises the real Indigo path and fails if the message changes.
const indigoNoPDSMessage = "identity does not link to an atproto host (PDS)"

// isIdentityResolutionFailure reports whether a login start failed because the
// identifier does not resolve to a usable account, as opposed to an outage.
// Indigo's ErrDIDResolutionFailed and ErrHandleResolutionFailed stay internal:
// both wrap DNS, HTTP transport, and status failures (including 503), which are
// outages rather than the user's identity being wrong.
func isIdentityResolutionFailure(err error) bool {
	for _, sentinel := range []error{
		ErrLoginIdentifierInvalid, identity.ErrInvalidHandle, identity.ErrHandleNotFound,
		identity.ErrHandleMismatch, identity.ErrHandleNotDeclared,
		identity.ErrHandleReservedTLD, identity.ErrDIDNotFound,
	} {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	return err != nil && err.Error() == indigoNoPDSMessage
}
