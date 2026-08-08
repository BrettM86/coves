package posts

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The deterministic record key of docs/PRD_AUTHOR_OWNED_POSTS.md §3.2.
//
// Every idempotency claim in the acceptance design rests on this function and
// nothing else. Three independent writers — the synchronous fast path, the
// firehose engine, and the notify endpoint — are safe to race only because they
// all compute the SAME rkey for the same post, so a race is a putRecord of one
// record rather than two records nobody can reconcile. Re-acceptance after an
// edit is an update rather than a second acceptance for the same reason.
//
// THE GOLDEN VALUES ARE HARD-CODED, NOT RECOMPUTED. A test that hashed the URI
// itself and compared would pass against base32-Hex, against uppercase, against
// padded output, and against a completely different digest — every variant that
// would silently repartition every community repo in the network the day it
// shipped. The constants below were produced OUTSIDE Go, with:
//
//	python3 -c 'import hashlib,base64;print(base64.b32encode(
//	    hashlib.sha256(URI.encode()).digest()).decode().lower().rstrip("="))'
//
// so they encode the spec rather than the implementation.

const (
	// goldenSubjectURI is the canonical vector: an ordinary post in an ordinary
	// author repo.
	goldenSubjectURI  = "at://did:plc:abc123/social.coves.community.postv2/3kjzl5kcb2s2v"
	goldenSubjectRkey = "xxdmibjaexx43drostplutjbp7g4oaw3uriugf5twafpldfkupca"

	// goldenSiblingURI differs from the canonical vector in its final character
	// only. It is here so that "distinct URIs get distinct keys" is proven at
	// the smallest possible difference rather than at a comfortable one.
	goldenSiblingURI  = "at://did:plc:abc123/social.coves.community.postv2/3kjzl5kcb2s2w"
	goldenSiblingRkey = "iyhgczhg7xsbrayzrrs2qa4fks6amctx7ghyjakqyrlhxztbbl5a"
)

// subjectRkeyLength is what §3.2 promises: SHA-256 is 256 bits, base32 packs 5
// bits per character, and 256/5 rounds up to 52 characters (with the padding
// stripped, which is why 52 and not 56).
const subjectRkeyLength = 52

// rkeyCharset is the set base32 draws from once lowercased. It is also a subset
// of the atProto record-key charset, which is the property that makes a digest
// safe to use as a key at all.
const rkeyCharset = "abcdefghijklmnopqrstuvwxyz234567"

func TestSubjectRkey_GoldenVector(t *testing.T) {
	t.Parallel()

	assert.Equal(t, goldenSubjectRkey, SubjectRkey(goldenSubjectURI),
		"the rkey for %s is fixed by §3.2 and by every acceptance record already written under it; "+
			"a different value here means the encoding changed (uppercase, padded, base32-Hex, "+
			"or a different digest) and every community repo in the network just repartitioned",
		goldenSubjectURI)

	assert.Equal(t, goldenSiblingRkey, SubjectRkey(goldenSiblingURI))
}

func TestSubjectRkey_ShapeIsFixedRegardlessOfSubjectLength(t *testing.T) {
	t.Parallel()

	// The point of a digest is that it is TOTAL over the legal subject space.
	// A readable transform of the URI is not: DIDs may run to 2048 bytes, so
	// the transform can exceed the 512-byte rkey limit, and they may carry
	// percent-escapes, so it can leave the rkey charset. Both of those are
	// attacker-chosen inputs.
	for _, tc := range []struct {
		name string
		uri  string
	}{
		{name: "ordinary did:plc subject", uri: goldenSubjectURI},
		{
			// 580 bytes: a did:web whose authority alone is over the 512-byte
			// rkey limit, so a readable transform would produce a key the PDS
			// refuses outright.
			name: "did:web subject longer than the 512-byte rkey limit",
			uri:  "at://" + longDIDWeb() + "/social.coves.community.postv2/3kjzl5kcb2s2v",
		},
		{
			// The maximum legal DID length. Nothing about the answer's shape
			// may vary with it.
			name: "2048-byte DID, the legal maximum",
			uri:  "at://" + didOfLength(2048) + "/social.coves.community.postv2/3kjzl5kcb2s2v",
		},
		{
			name: "percent-escaped authority",
			uri:  "at://did:web:example.com%3A8443/social.coves.community.postv2/3kjzl5kcb2s2v",
		},
		{
			name: "empty subject",
			uri:  "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rkey := SubjectRkey(tc.uri)

			assert.Lenf(t, rkey, subjectRkeyLength,
				"§3.2 promises a fixed %d characters for a subject of any length; this one was %d bytes",
				subjectRkeyLength, len(tc.uri))

			for i, r := range rkey {
				assert.Containsf(t, rkeyCharset, string(r),
					"rkey %q holds %q at index %d, which is outside the unpadded lowercase base32 "+
						"alphabet — uppercase and '=' are the two slips that produce a key the PDS rejects",
					rkey, string(r), i)
			}
		})
	}
}

func TestSubjectRkey_LongSubjectGoldenVectors(t *testing.T) {
	t.Parallel()

	// The long vectors get golden values too, not merely a shape check. A
	// truncating implementation — hashing only the first N bytes, or a
	// readable transform that clipped to 512 — passes the shape assertions
	// above and collides two different long subjects onto one key.
	assert.Equal(t,
		"fktiwazbhqfsqjg5e7yypm3ukbalk72bfplcr2d2ukm7iuwqeyqa",
		SubjectRkey("at://"+longDIDWeb()+"/social.coves.community.postv2/3kjzl5kcb2s2v"))

	assert.Equal(t,
		"4pri7ptezxzyzpzzysj5for6shd54yt5vvch6v3nj4e7kmfg62qq",
		SubjectRkey("at://"+didOfLength(2048)+"/social.coves.community.postv2/3kjzl5kcb2s2v"))
}

func TestSubjectRkey_IsStableAcrossCalls(t *testing.T) {
	t.Parallel()

	// Stability is the whole contract. If this ever stops holding — a map
	// iteration folded into the derivation, a timestamp, a random salt — the
	// three independent writers stop converging and every re-fire mints a
	// second acceptance record for the same post.
	first := SubjectRkey(goldenSubjectURI)
	for i := 0; i < 32; i++ {
		require.Equalf(t, first, SubjectRkey(goldenSubjectURI),
			"call %d returned a different key for the same subject", i)
	}
}

func TestSubjectRkey_DistinguishesSubjectsThatDifferInBytesOnly(t *testing.T) {
	t.Parallel()

	// The row's bytes are the identity. No normalization, no percent-decoding,
	// no case folding — because the AppView indexes and looks records up under
	// exactly the bytes it stored, so a writer that normalized would key its
	// records to a URI the reader never asks for.
	escaped := "at://did:web:example.com%3A8443/social.coves.community.postv2/3kjzl5kcb2s2v"
	decoded := "at://did:web:example.com:8443/social.coves.community.postv2/3kjzl5kcb2s2v"

	assert.NotEqual(t, SubjectRkey(escaped), SubjectRkey(decoded),
		"the escaped and decoded spellings are different byte strings and must key differently; "+
			"a function that percent-decoded first would let two rows that the AppView keeps apart "+
			"collide onto one acceptance record")

	assert.Equal(t, "ip6rb5ppturmcux6r54fglc57je3cgfofvi3eajodrxqkgcwjrna", SubjectRkey(escaped))
	assert.Equal(t, "jusonlequb2pkc34y5uwa65ng22cwz5qef3gorg5huqebglag37a", SubjectRkey(decoded))

	assert.NotEqual(t, SubjectRkey(goldenSubjectURI), SubjectRkey(goldenSiblingURI),
		"two posts differing in one character of their rkey must not share an acceptance record")

	// Case is a byte difference like any other.
	assert.NotEqual(t, SubjectRkey(goldenSubjectURI), SubjectRkey(strings.ToUpper(goldenSubjectURI)))
}

func TestSubjectRkey_IsSharedByTheAcceptanceAndTheRemoval(t *testing.T) {
	t.Parallel()

	// PINNED SO NOBODY SALTS IT PER COLLECTION. There is exactly one derivation
	// for a subject, and the acceptance and the removal both use it. That is
	// what makes the removal commit of §3.3 shapeable: WriteRemoval pre-reads
	// BOTH records to decide whether to emit a delete and whether the removal
	// is a create or an update, and it can only do that with two lookups of one
	// computed value.
	//
	// Record keys are scoped to their collection, so there is no collision to
	// avoid and a per-collection salt would buy nothing at all.
	rkey := SubjectRkey(goldenSubjectURI)
	require.NotEmpty(t, rkey)

	acceptanceURI := "at://did:plc:community/" + AcceptanceCollection + "/" + rkey
	removalURI := "at://did:plc:community/" + RemovalCollection + "/" + rkey

	assert.True(t, strings.HasSuffix(acceptanceURI, "/"+rkey))
	assert.True(t, strings.HasSuffix(removalURI, "/"+rkey),
		"the removal record for a subject must live at the SAME rkey as its acceptance")
}

// longDIDWeb is a did:web whose authority alone exceeds the 512-byte rkey
// limit: eight maximum-length DNS labels and a TLD.
func longDIDWeb() string {
	label := strings.Repeat("a", 63)
	labels := make([]string, 8)
	for i := range labels {
		labels[i] = label
	}
	return "did:web:" + strings.Join(labels, ".") + ".example.com"
}

// didOfLength returns a syntactically DID-shaped identifier of exactly n bytes.
func didOfLength(n int) string {
	const prefix = "did:web:"
	return prefix + strings.Repeat("b", n-len(prefix))
}
