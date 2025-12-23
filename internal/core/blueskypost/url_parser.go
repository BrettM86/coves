package blueskypost

import (
	"Coves/internal/atproto/identity"
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// blueskyPostURLPattern matches https://bsky.app/profile/{handle}/post/{rkey}
var blueskyPostURLPattern = regexp.MustCompile(`^https://bsky\.app/profile/([^/]+)/post/([^/]+)$`)

// IsBlueskyURL checks if a URL is a valid bsky.app post URL.
// Returns true for URLs matching https://bsky.app/profile/{handle}/post/{rkey}
func IsBlueskyURL(urlStr string) bool {
	return blueskyPostURLPattern.MatchString(urlStr)
}

// ParseBlueskyURL converts a bsky.app URL to an AT-URI.
// Example: https://bsky.app/profile/user.bsky.social/post/abc123
//
//	-> at://did:plc:xxx/app.bsky.feed.post/abc123
//
// Returns error if the URL is invalid or handle resolution fails.
func ParseBlueskyURL(ctx context.Context, urlStr string, resolver identity.Resolver) (string, error) {
	// Parse and validate the URL
	parsedURL, err := url.Parse(urlStr)
	if err != nil {
		return "", fmt.Errorf("invalid URL: %w", err)
	}

	// Validate URL scheme and host
	if parsedURL.Scheme != "https" {
		return "", fmt.Errorf("URL must use HTTPS scheme")
	}
	if parsedURL.Host != "bsky.app" {
		return "", fmt.Errorf("URL must be from bsky.app")
	}

	// Extract handle and rkey using regex
	matches := blueskyPostURLPattern.FindStringSubmatch(urlStr)
	if matches == nil || len(matches) != 3 {
		return "", fmt.Errorf("invalid bsky.app URL format, expected: https://bsky.app/profile/{handle}/post/{rkey}")
	}

	handle := matches[1]
	rkey := matches[2]

	// Validate handle and rkey are not empty
	if handle == "" || rkey == "" {
		return "", fmt.Errorf("handle and rkey cannot be empty")
	}

	// Validate rkey format
	// TID format: base32-sortable timestamp IDs are typically 13 characters
	// Allow alphanumeric characters, reasonable length (3-20 chars to be permissive)
	if err := validateRkey(rkey); err != nil {
		return "", fmt.Errorf("invalid rkey: %w", err)
	}

	// Resolve handle to DID
	// If the handle is already a DID (starts with "did:"), use it directly
	var did string
	if strings.HasPrefix(handle, "did:") {
		did = handle
	} else {
		// Resolve handle to DID using identity resolver
		resolvedDID, _, err := resolver.ResolveHandle(ctx, handle)
		if err != nil {
			return "", fmt.Errorf("failed to resolve handle %s: %w", handle, err)
		}
		did = resolvedDID
	}

	// Construct AT-URI
	// Format: at://{did}/app.bsky.feed.post/{rkey}
	atURI := fmt.Sprintf("at://%s/app.bsky.feed.post/%s", did, rkey)

	return atURI, nil
}

// rkeyPattern matches valid rkey formats (alphanumeric, typically base32 TID format)
var rkeyPattern = regexp.MustCompile(`^[a-zA-Z0-9]+$`)

// validateRkey validates the rkey (record key) format.
// TIDs (Timestamp Identifiers) are typically 13 characters in base32-sortable format.
// We allow 3-20 characters to be permissive while preventing abuse.
func validateRkey(rkey string) error {
	if len(rkey) < 3 {
		return fmt.Errorf("rkey too short (minimum 3 characters)")
	}
	if len(rkey) > 20 {
		return fmt.Errorf("rkey too long (maximum 20 characters)")
	}
	if !rkeyPattern.MatchString(rkey) {
		return fmt.Errorf("rkey contains invalid characters (must be alphanumeric)")
	}
	return nil
}
