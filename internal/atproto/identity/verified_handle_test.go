package identity

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The one question every caller of a resolver actually wants answered: is this
// handle safe to store against this DID?
//
// # WHY THIS LIVES HERE AT ALL
//
// Before this predicate existed the check was written FOUR times in the tree,
// in disagreeing forms:
//
//   - internal/atproto/jetstream/authorpost.go — checked the placeholder AND
//     the subject DID (now calls this).
//   - internal/atproto/jetstream/community_consumer.go, two sites — checked the
//     placeholder but NOT the subject DID (both now call this).
//   - internal/atproto/oauth/handlers.go:623 — checks both; still inline, filed
//     for adoption behind an import-cycle fix.
//
// Two of those call sites were missing half the predicate, so "extract the
// common form" would have ratified the weaker one. The subject-DID comparison
// is in because a CACHING resolver sits in front of this in production
// (NewResolver with the Postgres cache): a stale or mis-keyed cache row is the
// realistic way a DID and a handle that do not belong together arrive at a
// caller looking perfectly well-formed. Nothing downstream can detect that, and
// communities.handle is UNIQUE, so the wrong pair is written once and is then
// permanent.
//
// # WHY TWO SENTINELS AND NOT A BOOL
//
// The two failures are classified DIFFERENTLY by every caller, and the caller
// cannot reconstruct which it hit from a bare false:
//
//   - An unverified handle is a fact about the RESOLUTION. DNS may be slow, the
//     directory may be unreachable this second and answer correctly the next.
//     The event must be retried, so the caller marks it transient.
//   - A subject mismatch is a CONTRADICTION. Redriving it a thousand times
//     produces the same answer. The caller marks it permanent and dead-letters
//     it, because a retry queue that can never drain is worse than a rejection.
//
// A bool collapses "try again later" into "never" or vice versa, and both
// directions are outages. CLAUDE.md: prefer error codes over boolean
// data-integrity markers.
//
// # WHY THE SIGNATURE TAKES AN *Identity AND NOT A Resolver
//
// This is a predicate over a VALUE that has already been resolved, not a
// resolution step. That is what lets the community consumer keep its narrow
// inline `interface { Resolve(ctx, string) (*identity.Identity, error) }` — the
// seam that makes it testable without a directory — and still use this, with no
// widening of the interface it depends on and no new import direction.

func TestVerifiedHandle(t *testing.T) {
	t.Parallel()

	const subject = "did:plc:communitysubject"

	tests := []struct {
		name       string
		resolved   *Identity
		subjectDID string
		wantHandle string
		wantErr    error
		why        string
	}{
		{
			name:       "a verified handle for the subject asked about",
			resolved:   &Identity{DID: subject, Handle: "c-nba.coves.social"},
			subjectDID: subject,
			wantHandle: "c-nba.coves.social",
			why:        "the only shape that may produce a handle: the directory verified it and it is about this DID",
		},
		{
			name:       "the reserved placeholder for an unverifiable handle",
			resolved:   &Identity{DID: subject, Handle: InvalidHandle},
			subjectDID: subject,
			wantErr:    ErrHandleUnverified,
			why: "indigo reports an unverifiable handle by returning " + InvalidHandle + " rather than an error, " +
				"so a caller that only checks err writes the placeholder into a UNIQUE column and every later " +
				"unverifiable identity collides with it",
		},
		{
			name:       "an empty handle",
			resolved:   &Identity{DID: subject, Handle: ""},
			subjectDID: subject,
			wantErr:    ErrHandleUnverified,
			why:        "an empty string is not a handle, and it is what a zero-valued or partially populated Identity carries",
		},
		{
			name:       "a nil identity with a nil error",
			resolved:   nil,
			subjectDID: subject,
			wantErr:    ErrHandleUnverified,
			why: "resolvers really do return (nil, nil): mockIdentityResolverForUser in the jetstream package does it " +
				"on a map miss, and the community consumer would nil-deref today rather than reject",
		},
		{
			name:       "a resolution about a different DID",
			resolved:   &Identity{DID: "did:plc:someoneelse", Handle: "c-nba.coves.social"},
			subjectDID: subject,
			wantErr:    ErrIdentitySubjectMismatch,
			why: "a cache keyed or filled wrongly hands back a well-formed identity for the wrong subject; " +
				"nothing downstream can tell, and the pair is then stored permanently",
		},
		{
			name:       "a resolution carrying no DID at all",
			resolved:   &Identity{DID: "", Handle: "c-nba.coves.social"},
			subjectDID: subject,
			wantErr:    ErrIdentitySubjectMismatch,
			why:        "an unpopulated DID field is a mismatch, not a pass: it establishes nothing about the subject",
		},
		{
			name:       "a DID differing only in case",
			resolved:   &Identity{DID: "did:plc:COMMUNITYSUBJECT", Handle: "c-nba.coves.social"},
			subjectDID: subject,
			wantErr:    ErrIdentitySubjectMismatch,
			why: "DIDs are case-SENSITIVE, unlike handles. Folding case here would accept a genuinely different " +
				"identifier, and the fold is a tempting symmetry with the handle comparison that must not be applied",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			handle, err := VerifiedHandle(tt.resolved, tt.subjectDID)

			if tt.wantErr != nil {
				// errors.Is, never a string match: the sentinel identity is the
				// contract callers branch on to classify transient vs permanent,
				// and a message that merely reads right is not that contract.
				require.Error(t, err, tt.why)
				assert.True(t, errors.Is(err, tt.wantErr),
					"expected %v, got %v — %s", tt.wantErr, err, tt.why)
				assert.Empty(t, handle,
					"a rejected resolution must return no handle at all: a caller that ignores err "+
						"must not be handed something storable")
				return
			}

			require.NoError(t, err, tt.why)
			assert.Equal(t, tt.wantHandle, handle, tt.why)
		})
	}
}

// TestVerifiedHandle_DistinguishesItsTwoFailures pins the thing the table
// cannot: that the sentinels are not aliases of one another.
//
// A single sentinel returned for both failures would satisfy every case above,
// because each case only asserts that its own error matches. But the whole
// reason there are two is that callers classify them oppositely — transient
// versus permanent — so a fix that made errors.Is answer true for both would
// send contradictions into an eternal retry queue while still passing.
func TestVerifiedHandle_DistinguishesItsTwoFailures(t *testing.T) {
	t.Parallel()

	const subject = "did:plc:communitysubject"

	_, unverified := VerifiedHandle(&Identity{DID: subject, Handle: InvalidHandle}, subject)
	_, mismatch := VerifiedHandle(&Identity{DID: "did:plc:someoneelse", Handle: "c-nba.coves.social"}, subject)

	require.Error(t, unverified)
	require.Error(t, mismatch)

	assert.False(t, errors.Is(unverified, ErrIdentitySubjectMismatch),
		"an unverifiable handle must not also read as a subject mismatch: the caller would dead-letter an "+
			"event the directory could have answered on redrive")
	assert.False(t, errors.Is(mismatch, ErrHandleUnverified),
		"a subject mismatch must not also read as an unverified handle: the caller would retry a "+
			"contradiction forever")
}
