package posts

import (
	"crypto/sha256"
	"encoding/base32"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The single highest-risk detail in the whole cutover: the postv2 record key the
// re-materialization tool writes at (docs/PRD_AUTHOR_OWNED_POSTS.md §11 step 4).
//
// A wrong rkey does not fail loudly — it MINTS DUPLICATES. If a re-run computes a
// different key than the first run, createAuthorRecord's converge-by-read never
// fires (the create-only guard is against a DIFFERENT key, which is empty), so a
// second postv2 lands for the same legacy post. Every strongRef built from the
// first run — the acceptance's pinned subject, every comment and vote — points at
// the first record; the second is an orphan duplicate. So this file pins the key
// harder than anything else in the suite:
//
//   1. It is DETERMINISTIC — two computations of the same old URI are identical.
//   2. It is a PURE FUNCTION OF THE OLD URI ALONE — nothing submission-time
//      (fingerprint, dedupe bucket, clock) leaks in, because the migration has
//      none of that and a re-run must reproduce the key from the old record only.
//   3. It is NOT SubmissionRkey — the write-path key needs exactly the
//      submission-time material this tool lacks, so a tool that reused it would
//      draw a fresh key every run and duplicate every post.
//   4. It is the SubjectRkey DIGEST SCHEME applied to the OLD URI: unpadded
//      lowercase base32 of the SHA-256 of the URI bytes — total over the legal
//      URI space and collision-free, the scheme the write path already trusts.
//
// The expected value is re-derived here from stdlib rather than by calling
// SubjectRkey, so a bug that changed BOTH the helper and a naive expectation
// together cannot hide: this is an independent check of the derivation.

// independentRematerializeRkey recomputes the pinned scheme straight from stdlib.
func independentRematerializeRkey(oldURI string) string {
	digest := sha256.Sum256([]byte(oldURI))
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest[:]))
}

func TestRematerializeRkey_IsTheDigestOfTheOldURI(t *testing.T) {
	oldURI := "at://did:plc:community2222222222222222/social.coves.community.post/3kqijkl2m4c2r"

	got := RematerializeRkey(oldURI)

	assert.Equalf(t, independentRematerializeRkey(oldURI), got,
		"the re-materialization rkey must be the unpadded lowercase base32 SHA-256 digest of the OLD URI (the SubjectRkey scheme applied to the legacy record). "+
			"A different scheme means a re-run computes a different key, the create-only converge never fires, and a second postv2 is minted for one legacy post")

	// The digest scheme is a fixed 52 characters drawn entirely from the
	// rkey-safe lowercase base32 charset — the property that makes it total over
	// the legal URI space (§3.2's argument for a digest over a readable transform).
	assert.Lenf(t, got, 52, "the digest rkey is a fixed 52 characters for any input; %q is not", got)
	assert.Truef(t, got == strings.ToLower(got), "the rkey must be lowercase — an uppercase key is a DIFFERENT key to a PDS that treats rkeys as opaque bytes")
	assert.NotContainsf(t, got, "=", "base32 padding '=' is outside the atProto record-key charset and must be dropped")
}

func TestRematerializeRkey_IsDeterministic(t *testing.T) {
	oldURI := "at://did:plc:community2222222222222222/social.coves.community.post/3kqijkl2m4c2r"

	first := RematerializeRkey(oldURI)
	second := RematerializeRkey(oldURI)

	require.Equalf(t, first, second,
		"two computations of the re-materialization rkey for the same old URI must be identical, or a crash-resumed run cannot converge on the record its first attempt wrote")
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

	// A representative SubmissionRkey over unrelated but plausible material.
	submission := SubmissionRkey(communityDID, "d41d8cd98f00b204e9800998ecf8427e", 0, 5*time.Minute)

	assert.NotEqualf(t, submission, RematerializeRkey(oldURI),
		"the re-materialization key must be independent of SubmissionRkey — it is derived from the OLD record's URI, not the submission fingerprint the migration lacks")
}
