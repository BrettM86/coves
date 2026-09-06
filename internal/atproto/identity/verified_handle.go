package identity

import "errors"

// InvalidHandle is the reserved placeholder atProto identity resolution returns
// for a DID whose declared handle it could NOT verify bidirectionally.
//
// It is not an error condition in indigo's contract — the lookup succeeds and
// reports this string — which is precisely why it needs a name: it is a handle
// shaped hole that every caller must recognise before storing.
const InvalidHandle = "handle.invalid"

var (
	// ErrHandleUnverified says the resolution established no handle for the
	// subject. Never permanent: the directory may verify the handle later.
	// Firehose consumers classify it jetstream.ErrUnresolvedReference (bounded
	// redrives, no in-line retries); interactive callers surface it as-is.
	ErrHandleUnverified = errors.New("identity resolution did not establish a verified handle")

	// ErrIdentitySubjectMismatch says the resolution answered about a different
	// DID than the one asked about. PERMANENT at every call site.
	ErrIdentitySubjectMismatch = errors.New("identity resolution answered about a different DID than the subject")
)

// VerifiedHandle returns the bidirectionally verified handle a resolution
// established for subjectDID, or one of the two sentinels above.
//
// It exists because atProto resolution reports "I could not verify this handle"
// as a SUCCESS. indigo hands back InvalidHandle in the Handle field with a nil
// error, so a caller that branches on err alone treats the placeholder as a real
// handle and stores it. communities.handle is UNIQUE, so the first unverifiable
// identity takes the placeholder and every later one collides with it — a
// failure that surfaces as a duplicate-community rejection a long way from its
// cause.
//
// The subjectDID comparison is not redundant with the lookup that produced
// resolved. Production wires a CACHING resolver (NewResolver over the Postgres
// cache) in front of the directory, and a stale or mis-keyed cache row is the
// realistic way a DID and a handle that do not belong together reach a caller
// looking perfectly well-formed. Nothing downstream can tell they do not match,
// and the pair is then stored permanently.
//
// That comparison is EXACT. DIDs are case-SENSITIVE, so the case-folding that is
// correct for comparing handles would accept a genuinely different identifier
// here. The symmetry is tempting and must not be applied.
//
// The two failures are separate sentinels because callers classify them
// oppositely and cannot reconstruct which they hit from a bare false: an
// unverified handle is a fact about the RESOLUTION, which DNS or the directory
// may answer correctly on the next attempt, so it is transient and the event
// should be retried; a subject mismatch is a CONTRADICTION that answers the same
// way however often it is redriven, so it is permanent and belongs in the dead
// letter queue rather than in a retry queue that can never drain.
func VerifiedHandle(resolved *Identity, subjectDID string) (string, error) {
	// A nil identity alongside a nil error is a real shape in this tree —
	// resolver fakes return it on a lookup miss — and it is the ABSENCE of a
	// resolution rather than a resolution about somebody else, so it is the
	// transient failure and not a mismatch.
	if resolved == nil {
		return "", ErrHandleUnverified
	}

	// Ordered before the handle check because a resolution about the wrong DID
	// establishes nothing about this subject, whatever handle it carries.
	if resolved.DID != subjectDID {
		return "", ErrIdentitySubjectMismatch
	}

	if resolved.Handle == "" || resolved.Handle == InvalidHandle {
		return "", ErrHandleUnverified
	}

	return resolved.Handle, nil
}
