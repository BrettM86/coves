package imageproxy

import (
	"regexp"
	"strings"

	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/ipfs/go-cid"
)

// ValidateDID validates that a DID string matches expected atproto DID formats.
// It uses the Indigo library's syntax.ParseDID for consistent validation across the codebase.
// Returns ErrInvalidDID if the DID is invalid.
func ValidateDID(did string) error {
	// Check for path traversal attempts before parsing
	if strings.Contains(did, "..") || strings.Contains(did, "/") || strings.Contains(did, "\\") || strings.Contains(did, "\x00") {
		return ErrInvalidDID
	}

	// Use Indigo's DID parser for consistent validation with the rest of the codebase
	_, err := syntax.ParseDID(did)
	if err != nil {
		return ErrInvalidDID
	}

	return nil
}

// ownerDIDPattern is the canonical owner spelling: a lowercase did:plc, or a
// lowercase did:web host with an optional %3A port that has no leading zero.
var ownerDIDPattern = regexp.MustCompile(`^(?:did:plc:[a-z0-9._-]+|did:web:[a-z0-9-]+(?:\.[a-z0-9-]+)*(?:%3A[1-9][0-9]{0,4})?)$`)

// ValidateOwnerDID accepts only the canonical spelling of a did:plc or did:web
// blob owner, so that each owner has exactly one string. Two lookups key on it:
//   - the media block check compares owner_did = $1 exactly, so an uppercase or
//     percent-escaped spelling of a blocked owner would not match its block;
//   - the disk cache directory is the DID with ":" rewritten to "_" and ".."
//     stripped, so did:web:x:a:b and did:web:x:a_b would share cached bytes.
//
// The pattern therefore refuses uppercase, every percent escape except one
// %3A port, and any colon after the method.
//
// A real PLC identifier is 24 base32 characters. The pattern also allows ".",
// "_" and "-" so that test fixtures such as did:plc:apikey_aggregator pass;
// the cache path rewrites none of those characters, so they cannot alias.
// ValidateDID must run first: it refuses "..", which the pattern accepts and
// the cache strips (did:plc:ab..cd would share did:plc:abcd's directory).
func ValidateOwnerDID(did string) error {
	if err := ValidateDID(did); err != nil {
		return err
	}
	if !ownerDIDPattern.MatchString(did) {
		return ErrInvalidDID
	}
	return nil
}

// ValidateCID validates that a CID string is a valid content identifier.
// It uses the Indigo library's syntax.ParseCID for consistent validation across the codebase.
// Returns ErrInvalidCID if the CID is invalid.
func ValidateCID(cid string) error {
	// Check for path traversal attempts before parsing
	if strings.Contains(cid, "..") || strings.Contains(cid, "/") || strings.Contains(cid, "\\") || strings.Contains(cid, "\x00") {
		return ErrInvalidCID
	}

	// Use Indigo's CID parser for consistent validation with the rest of the codebase
	_, err := syntax.ParseCID(cid)
	if err != nil {
		return ErrInvalidCID
	}

	return nil
}

// CanonicalCID decodes a CID in any multibase encoding and returns its
// canonical string form. A PDS resolves every encoding of a CID to the same
// blob, so moderation blocks, cache keys and fetches must all use this one
// form; ValidateCID alone is a syntax check and accepts strings that are not
// CIDs. Returns ErrInvalidCID if the value does not decode.
func CanonicalCID(value string) (string, error) {
	if err := ValidateCID(value); err != nil {
		return "", err
	}
	parsed, err := cid.Decode(value)
	if err != nil {
		return "", ErrInvalidCID
	}
	return parsed.String(), nil
}

// SanitizePathComponent ensures a string is safe to use as a filesystem path component.
// It removes or replaces characters that could be used for path traversal attacks.
// This is used as an additional safety layer beyond DID/CID validation.
func SanitizePathComponent(s string) string {
	// Replace any path separators
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "\\", "_")

	// Remove any path traversal sequences
	s = strings.ReplaceAll(s, "..", "")

	// Replace colons for filesystem compatibility (Windows and general safety)
	s = strings.ReplaceAll(s, ":", "_")

	// Remove null bytes
	s = strings.ReplaceAll(s, "\x00", "")

	return s
}

// ValidatePreset validates that a preset name is safe and exists.
// This combines format validation with registry lookup.
func ValidatePreset(preset string) error {
	// Check for empty preset
	if preset == "" {
		return ErrInvalidPreset
	}

	// Check for path separators (dangerous characters)
	// Note: We use ContainsAny for individual chars and Contains for substrings
	if strings.ContainsAny(preset, "/\\") {
		return ErrInvalidPreset
	}

	// Check for path traversal sequences (must check ".." as a substring, not individual dots)
	if strings.Contains(preset, "..") {
		return ErrInvalidPreset
	}

	// Verify preset exists in registry
	_, err := GetPreset(preset)
	return err
}
