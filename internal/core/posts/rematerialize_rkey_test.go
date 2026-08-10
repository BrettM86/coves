package posts

import (
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The single highest-risk detail in the whole cutover: the postv2 record key the
// re-materialization tool writes at (docs/PRD_AUTHOR_OWNED_POSTS.md §11 step 4).
//
// It has TWO hard constraints that pull in different directions, and the RED-1
// digest scheme satisfied only one of them (whole-branch review, P9):
//
//  1. STABLE PURE FUNCTION OF THE OLD URI. A wrong or non-deterministic key does
//     not fail loudly — it MINTS DUPLICATES: if a re-run computes a different key,
//     createAuthorRecord's converge-by-read never fires (the create-only guard is
//     against a DIFFERENT, empty key), so a second postv2 lands for one legacy
//     post and every strongRef built from the first dangles. So the key must be a
//     deterministic function of the OLD URI ALONE — nothing submission-time
//     (fingerprint, bucket, clock), which the migration does not have and a re-run
//     could not reproduce. In particular it must NOT be SubmissionRkey.
//
//  2. IT MUST BE A VALID TID. The postv2 lexicon declares "key": "tid", so a
//     validating PDS rejects any rkey that is not a TID, and feed ordering reads
//     the timestamp OUT of the key. The RED-1 scheme, SubjectRkey (a 52-char
//     base32 SHA-256 digest), is NOT a TID — syntax.ParseTID rejects it — so a
//     conformant PDS would refuse every re-materialized record. The key must be a
//     deterministic TID derived purely from the old URI (a hashed timestamp+clock,
//     e.g. tidepool's DeterministicTID shape), NOT SubmissionRkey (which mixes in
//     submission-time material) and NOT SubjectRkey (which is not a TID at all).

func TestRematerializeRkey_IsAValidTID(t *testing.T) {
	oldURI := "at://did:plc:community2222222222222222/social.coves.community.post/3kqijkl2m4c2r"

	rkey := RematerializeRkey(oldURI)

	parsed, err := syntax.ParseTID(rkey)
	require.NoErrorf(t, err,
		"the re-materialization rkey %q is not a valid TID. The postv2 lexicon declares key:tid, so a validating PDS rejects a non-TID rkey and feed ordering cannot read a timestamp out of it. "+
			"The RED-1 SubjectRkey digest scheme is a 52-char base32 hash, which is not a TID — derive a deterministic TID from the old URI instead", rkey)
	assert.Equalf(t, rkey, parsed.String(), "ParseTID must round-trip the rkey unchanged")
}

func TestRematerializeRkey_IsDeterministic(t *testing.T) {
	oldURI := "at://did:plc:community2222222222222222/social.coves.community.post/3kqijkl2m4c2r"

	first := RematerializeRkey(oldURI)
	second := RematerializeRkey(oldURI)

	require.Equalf(t, first, second,
		"two computations of the re-materialization rkey for the same old URI must be identical, or a crash-resumed run cannot converge on the record its first attempt wrote — it mints a duplicate")
}

func TestRematerializeRkey_DependsOnlyOnTheOldURI(t *testing.T) {
	// Two DIFFERENT old URIs must give two different keys (no accidental
	// constant), and the SAME old URI must give the SAME key no matter what else
	// is true of the run — there is no fingerprint, bucket, or clock in the
	// material, by construction.
	a := "at://did:plc:community2222222222222222/social.coves.community.post/3kqijkl2m4c2r"
	b := "at://did:plc:community2222222222222222/social.coves.community.post/3kqijkl2m4c2s"

	assert.NotEqualf(t, RematerializeRkey(a), RematerializeRkey(b),
		"two distinct legacy records must re-materialize to two distinct postv2 rkeys, or one would silently overwrite the other in the author's repo")
}

func TestRematerializeRkey_IsNotSubmissionRkey(t *testing.T) {
	// SubmissionRkey needs (community, fingerprint, bucket, window) — submission-
	// time material the migration does not have. The re-materialization key is a
	// function of the OLD URI ALONE. They must not coincide: if the tool's key
	// happened to equal a SubmissionRkey, a later genuine submission of the same
	// content could collide with a re-materialized post.
	oldURI := "at://did:plc:community2222222222222222/social.coves.community.post/3kqijkl2m4c2r"
	communityDID := "did:plc:community2222222222222222"

	submission := SubmissionRkey(communityDID, "d41d8cd98f00b204e9800998ecf8427e", 0, 5*time.Minute)

	assert.NotEqualf(t, submission, RematerializeRkey(oldURI),
		"the re-materialization key must be independent of SubmissionRkey — it is derived from the OLD record's URI, not the submission fingerprint the migration lacks")
}

// THE GOLDEN VALUES. Determinism WITHIN one process (the test above) is not the
// property production needs: a per-process salt — a package-level random seed, a
// map iteration order that leaked into the digest, a hostname — passes every
// other test in this file and still mints a SECOND postv2 for every already-
// migrated post the first time the tool is restarted.
//
// These literals are the only thing that pins the derivation ACROSS processes,
// releases and machines. They were produced by the shipped implementation and
// must never be "fixed" to match a changed one.
func TestRematerializeRkey_GoldenValues(t *testing.T) {
	golden := []struct {
		name    string
		oldURI  string
		wantKey string
	}{
		{"plc community, tid rkey", "at://did:plc:community2222222222222222/social.coves.community.post/3kqijkl2m4c2r", "beq7r3yeigi53"},
		{"same community, adjacent rkey", "at://did:plc:community2222222222222222/social.coves.community.post/3kqijkl2m4c2s", "2o3lkuliyahmi"},
		{"different community, same rkey", "at://did:plc:community3333333333333333/social.coves.community.post/3kqijkl2m4c2r", "4kwla2cw7a4pv"},
		{"did:web community", "at://did:web:coves.social/social.coves.community.post/3kqijkl2m4c2r", "a3jez3fwk5sz2"},
		{"empty string", "", "6a6cegjsb2orv"},
		{"unicode in the rkey position", "at://did:plc:community2222222222222222/social.coves.community.post/naïve", "7g76vsnjgfeey"},
	}

	for _, tc := range golden {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equalf(t, tc.wantKey, RematerializeRkey(tc.oldURI),
				"THE RE-MATERIALIZATION RKEY DERIVATION CHANGED.\n"+
					"This is not a test to update. Every post already migrated by the shipped derivation is at the OLD key, so a run under the new one "+
					"writes a SECOND postv2 for every one of them: createAuthorRecord's converge-by-read fires against a different, empty key, the "+
					"duplicate lands, and every strongRef built from the first record dangles. Revert the derivation instead.\n"+
					"old URI: %q", tc.oldURI)
		})
	}
}
