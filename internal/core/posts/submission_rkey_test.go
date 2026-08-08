package posts

import (
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The deterministic record key a post is written at in its AUTHOR's repo
// (docs/PRD_AUTHOR_OWNED_POSTS.md §4.2).
//
// This closes the lost-response asymmetry §4.2 names: when a PDS write's outcome
// is ambiguous the record may or may not exist, and a client that retries with a
// server-chosen TID gets a SECOND post. Derived from the submission instead, the
// retry aims at the record the first attempt may already have written, and a
// create-only write reports the standing one rather than minting a twin.
//
// THE GOLDEN VALUES ARE HARD-CODED, NOT RECOMPUTED — the same discipline
// rkey_test.go applies to SubjectRkey, and for a sharper reason here. A test that
// recomputed the derivation would pass against a key that ignored the community,
// against one that ignored the bucket, and against one whose timestamp landed in
// a different century; every one of those is a silent production defect the day
// it ships. The constants below were produced OUTSIDE Go, by a Python
// transcription of the SPEC:
//
//	import hashlib
//	ALPHABET = "234567abcdefghijklmnopqrstuvwxyz"
//	material = community + "\n" + fingerprint + "\n" + str(bucket)
//	d        = hashlib.sha256(material.encode()).digest()
//	micros   = bucket * window_micros + int.from_bytes(d[0:8], "big") % window_micros
//	clock    = int.from_bytes(d[8:10], "big") & 0x3FF
//	v        = (((micros & 0x1FFFFFFFFFFFFF) << 10) | clock) & 0x7FFFFFFFFFFFFFFF
//	rkey     = "".join(ALPHABET[(v >> (5*i)) & 0x1F] for i in range(12, -1, -1))

const (
	// goldenSubmissionFingerprint is sha256("golden-submission") in hex — the
	// shape submissionFingerprint produces.
	goldenSubmissionFingerprint = "6ccdb9079108e824fcc444f7e8c1aabad14690d9ec5cb28f783bd6f33230bcce"

	// goldenOtherFingerprint is sha256("a different submission"): different
	// content, everything else held equal.
	goldenOtherFingerprint = "3628901f33d006a95ced161472d5cbb52575388a68fec22b831b86fa224d4132"

	goldenCommunityA = "did:plc:aaaaaaaaaaaaaaaaaaaaaaaa"
	goldenCommunityB = "did:plc:bbbbbbbbbbbbbbbbbbbbbbbb"

	// goldenBucket is an ordinary dedupeBucket value for a one-hour window: the
	// index of the hour, counted from the epoch.
	goldenBucket = 486000
)

// The four vectors. Each pair differs from the first in exactly ONE input, so a
// derivation that dropped that input produces the first value again and the
// assertion names which input went missing.
const (
	goldenSubmissionRkey    = "3lrc77gmww4nc" // community A, golden fingerprint, golden bucket
	goldenOtherCommunityRky = "3lrc4zsbrqy6t" // community B, same content, same bucket
	goldenNextBucketRkey    = "3lrccdypy25km" // community A, same content, bucket + 1
	goldenOtherContentRkey  = "3lrc72ssh73fl" // community A, different content, same bucket
)

// goldenWindow is the dedupe window the vectors were computed against. It is
// spelled as a duration rather than as microseconds because that is what
// SubmissionLimits.DedupeWindow holds and what the call site passes.
const goldenWindow = time.Hour

func TestSubmissionRkey_GoldenVectors(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		community string

		fingerprint string
		want        string
		bucket      int64
	}{
		{
			name:        "the canonical submission",
			community:   goldenCommunityA,
			fingerprint: goldenSubmissionFingerprint,
			bucket:      goldenBucket,
			want:        goldenSubmissionRkey,
		},
		{
			// THE COMMUNITY IS IN THE MATERIAL, and this is the vector that
			// proves it. submissionFingerprint deliberately EXCLUDES the
			// community (the client types a handle one time and a DID the next,
			// and the ledger's unique key already scopes it) — so a derivation
			// that hashed only the fingerprint and the bucket would give the
			// same author the same rkey for two genuinely different posts, and
			// the second crosspost would overwrite the first in their own repo.
			name:        "the same content submitted to a different community",
			community:   goldenCommunityB,
			fingerprint: goldenSubmissionFingerprint,
			bucket:      goldenBucket,
			want:        goldenOtherCommunityRky,
		},
		{
			// THE BUCKET IS IN THE MATERIAL, which is what makes the collision
			// EXPIRE. Without it, the rkey for a piece of content would be fixed
			// forever, and an author who deliberately reposted the same thing a
			// year later would be writing at an rkey their old post still holds.
			name:        "the same submission one dedupe window later",
			community:   goldenCommunityA,
			fingerprint: goldenSubmissionFingerprint,
			bucket:      goldenBucket + 1,
			want:        goldenNextBucketRkey,
		},
		{
			name:        "different content, same author and community and window",
			community:   goldenCommunityA,
			fingerprint: goldenOtherFingerprint,
			bucket:      goldenBucket,
			want:        goldenOtherContentRkey,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, SubmissionRkey(tc.community, tc.fingerprint, tc.bucket, goldenWindow),
				"the rkey is fixed by §4.2 and by every post record already written under it; a different "+
					"value here means the derivation changed and every in-flight client retry just stopped "+
					"being idempotent")
		})
	}
}

func TestSubmissionRkey_IsAValidTID(t *testing.T) {
	t.Parallel()

	// The postv2 lexicon declares `"key": "tid"`. A key that merely looked like
	// one — 13 characters of the right alphabet — would be accepted by a PDS
	// that does not validate record keys and refused by one that does, which is
	// the worst of the two outcomes: it works in development and fails on the
	// first federated peer running a stricter build.
	for _, tc := range []struct {
		name        string
		community   string
		fingerprint string
		bucket      int64
		window      time.Duration
	}{
		{"canonical", goldenCommunityA, goldenSubmissionFingerprint, goldenBucket, goldenWindow},
		{"bucket zero", goldenCommunityA, goldenSubmissionFingerprint, 0, goldenWindow},
		{"a one-minute dedupe window", goldenCommunityA, goldenSubmissionFingerprint, 29160000, time.Minute},
		{"a one-day dedupe window", goldenCommunityA, goldenSubmissionFingerprint, 20250, 24 * time.Hour},
		{"an empty fingerprint", goldenCommunityA, "", goldenBucket, goldenWindow},
		{"a 2048-byte community DID", didOfLength(2048), goldenSubmissionFingerprint, goldenBucket, goldenWindow},
		{
			// A misconfiguration rather than a legal input: config.Validate
			// refuses a non-positive dedupe window at startup. It is here
			// because the derivation divides by the window, and a total
			// function must not turn an operator's mistake into a panic on the
			// write path — dedupeBucket takes the same care for the same
			// reason.
			name: "a non-positive window, which config refuses but arithmetic must survive",

			community: goldenCommunityA, fingerprint: goldenSubmissionFingerprint,
			bucket: 0, window: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rkey := SubmissionRkey(tc.community, tc.fingerprint, tc.bucket, tc.window)

			parsed, err := syntax.ParseTID(rkey)
			require.NoErrorf(t, err, "SubmissionRkey produced %q, which is not a TID the lexicon's "+
				"`key: tid` will accept", rkey)
			assert.Equal(t, rkey, parsed.String())
		})
	}
}

func TestSubmissionRkey_TimestampLandsInsideItsOwnDedupeBucket(t *testing.T) {
	t.Parallel()

	// A TID's timestamp is not decoration: feeds and repo listings order by it,
	// and a client reading a post's rkey reads a time out of it. Placing the
	// derived time inside the submission's own dedupe bucket keeps that time
	// within one window of when the post was actually submitted — so an hourly
	// window puts every post within the hour it was written, rather than at
	// whatever moment 64 bits of digest happened to name.
	for _, window := range []time.Duration{time.Minute, time.Hour, 24 * time.Hour} {
		t.Run(window.String(), func(t *testing.T) {
			t.Parallel()

			now := time.Date(2026, 8, 8, 13, 47, 11, 0, time.UTC)
			bucket := dedupeBucket(now, window)

			rkey := SubmissionRkey(goldenCommunityA, goldenSubmissionFingerprint, bucket, window)
			parsed, err := syntax.ParseTID(rkey)
			require.NoError(t, err)

			start := time.Unix(0, bucket*int64(window)).UTC()
			end := start.Add(window)

			stamped := parsed.Time()
			assert.Falsef(t, stamped.Before(start),
				"the derived timestamp %s is before its dedupe bucket [%s, %s)", stamped, start, end)
			assert.Truef(t, stamped.Before(end),
				"the derived timestamp %s is at or past the end of its dedupe bucket [%s, %s)", stamped, start, end)
		})
	}
}

func TestSubmissionRkey_IsStableAcrossCalls(t *testing.T) {
	t.Parallel()

	// Stability IS the idempotence claim. If this ever stops holding — a
	// timestamp read at call time, a random salt, a map iteration folded into
	// the material — then a client retrying after a lost response writes a
	// second post instead of colliding with its own first one, which is the
	// exact duplicate §4.2 exists to close.
	first := SubmissionRkey(goldenCommunityA, goldenSubmissionFingerprint, goldenBucket, goldenWindow)
	require.NotEmpty(t, first)

	for i := 0; i < 32; i++ {
		require.Equalf(t, first,
			SubmissionRkey(goldenCommunityA, goldenSubmissionFingerprint, goldenBucket, goldenWindow),
			"call %d returned a different key for the same submission", i)
	}
}

func TestSubmissionRkey_DistinctInputsGiveDistinctKeys(t *testing.T) {
	t.Parallel()

	// The same four vectors as the golden table, asserted as a SET. The table
	// above proves each value is the right one; this proves they are four
	// values and not two — a derivation that silently dropped an input would
	// pass a hand-updated golden table and fail here.
	keys := map[string]string{
		"canonical":            SubmissionRkey(goldenCommunityA, goldenSubmissionFingerprint, goldenBucket, goldenWindow),
		"different community":  SubmissionRkey(goldenCommunityB, goldenSubmissionFingerprint, goldenBucket, goldenWindow),
		"next bucket":          SubmissionRkey(goldenCommunityA, goldenSubmissionFingerprint, goldenBucket+1, goldenWindow),
		"different content":    SubmissionRkey(goldenCommunityA, goldenOtherFingerprint, goldenBucket, goldenWindow),
		"community and bucket": SubmissionRkey(goldenCommunityB, goldenSubmissionFingerprint, goldenBucket+1, goldenWindow),
	}

	seen := make(map[string]string, len(keys))
	for name, key := range keys {
		require.NotEmptyf(t, key, "%s produced no key at all", name)
		if previous, collided := seen[key]; collided {
			t.Errorf("%q and %q derive the same rkey %q — one of the submission's identifying "+
				"inputs is not in the material, so two different posts would be written at one key "+
				"in the author's repo and the second would overwrite the first",
				previous, name, key)
			continue
		}
		seen[key] = name
	}
}

func TestSubmissionRkey_MaterialCannotBeAmbiguouslySplit(t *testing.T) {
	t.Parallel()

	// The material is three fields concatenated, so the delimiter has to be
	// something none of them can contain — otherwise a community DID ending in
	// the delimiter and a fingerprint beginning with it would produce the same
	// bytes as some other pair, and two different submissions would land on one
	// rkey. A DID's legal charset and a hex fingerprint both exclude every
	// control character; a naive concatenation with no delimiter at all does
	// not, and this is the case that catches it.
	assert.NotEqual(t,
		SubmissionRkey("did:web:a.example", "b"+goldenSubmissionFingerprint, goldenBucket, goldenWindow),
		SubmissionRkey("did:web:a.exampleb", goldenSubmissionFingerprint, goldenBucket, goldenWindow),
		"the community and the fingerprint must not be able to run together: a derivation that "+
			"concatenated them without a delimiter lets one submission's key be forged from another's")
}
