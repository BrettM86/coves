package posts

// SubjectRkey is the record key a community's records about one post use.
//
// It is the unpadded lowercase base32 encoding of the SHA-256 digest of the
// post's AT-URI: a fixed 52 characters drawn entirely from the rkey-safe
// charset, well inside the 512-byte limit, for a subject of any length
// (docs/PRD_AUTHOR_OWNED_POSTS.md §3.2).
//
// WHY A DIGEST RATHER THAN A READABLE TRANSFORM. The obvious scheme — strip
// `at://`, swap `/` for `:` — is not total over the legal subject space. DIDs
// may run to 2048 bytes and may carry percent-escapes, so the transform can
// produce keys that exceed the rkey limit or leave the rkey charset, and a
// non-total key function fails on exactly the identifiers an attacker gets to
// choose. A fixed-size digest is total and just as deterministic.
//
// WHY ONE FUNCTION FOR BOTH RECORD TYPES. The acceptance and the removal for a
// subject share this key. Record keys are scoped to their collection, so there
// is no collision to avoid, and a per-collection salt would buy nothing while
// costing the property that makes the removal commit shapeable: both records
// for a subject are found by ONE key derivation, so a pre-read of both is two
// lookups of one computed value rather than a search.
//
// THE ARGUMENT IS BYTES, NOT A PARSED URI. Whatever the admission row holds is
// what gets hashed — no normalization, no percent-decoding, no case folding.
// The row's bytes are the identity the AppView indexes under, so a writer that
// normalized would key its records to a URI the reader never looks up.
func SubjectRkey(postURI string) string {
	return ""
}
