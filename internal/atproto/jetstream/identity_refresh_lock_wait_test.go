package jetstream

import "testing"

// A community refresh holds the DID lock across one identity resolution and
// the hostedBy did:web verification. A competing refresh must be able to wait
// out that holder rather than defer the event as busy.
func TestIdentityRefreshLockWaitCoversLongestHolder(t *testing.T) {
	longestHolder := identityResolveTimeout + wellKnownVerificationTimeout
	if IdentityRefreshLockWaitTimeout <= longestHolder {
		t.Errorf("IdentityRefreshLockWaitTimeout = %s, want more than identityResolveTimeout + wellKnownVerificationTimeout = %s",
			IdentityRefreshLockWaitTimeout, longestHolder)
	}
}
