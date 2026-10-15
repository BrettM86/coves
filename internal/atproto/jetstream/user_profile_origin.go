package jetstream

import (
	"net/url"
	"strings"

	"Coves/internal/validation"
)

// userProfileOrigin holds the four origin fields of a social.coves.actor.profile
// record, raw or sanitized.
type userProfileOrigin struct {
	Origin         string
	OriginUsername string
	OriginURL      string
	OriginSoftware string
}

// sanitizeOriginSoftware returns the originSoftware value to store: trimmed,
// lowercased, 1-64 UTF-8 bytes, or "" when it does not fit. The length is
// checked after lowercasing, as the lexicon validator counts maxLength.
func sanitizeOriginSoftware(raw string) string {
	software := strings.ToLower(strings.TrimSpace(raw))
	if software == "" || len(software) > 64 {
		return ""
	}
	return software
}

// sanitizeOriginURL returns rawURL when it is an absolute https URL on the
// admitted, already normalized origin host, otherwise "".
//
// ValidURI enforces the lexicon's format=uri (printable ASCII, lowercase
// scheme, at most 8192 bytes), which also rules out a Unicode lookalike host
// that strings.EqualFold would match. Userinfo is rejected because it makes a
// misleading display link.
func sanitizeOriginURL(rawURL, origin string) string {
	if origin == "" {
		return ""
	}
	parsed, err := url.Parse(rawURL)
	if err == nil && validation.ValidURI(rawURL) && parsed.IsAbs() && parsed.User == nil &&
		strings.EqualFold(parsed.Scheme, "https") && strings.EqualFold(parsed.Host, origin) {
		return rawURL
	}
	return ""
}

// sanitizeUserProfileOrigin turns raw record values into the values to store.
func sanitizeUserProfileOrigin(raw userProfileOrigin) userProfileOrigin {
	sanitized := userProfileOrigin{OriginSoftware: sanitizeOriginSoftware(raw.OriginSoftware)}

	origin, err := validation.NormalizeDomain(raw.Origin)
	if err != nil || raw.OriginUsername == "" || len(raw.OriginUsername) > 256 {
		return sanitized
	}
	sanitized.Origin = origin
	sanitized.OriginUsername = raw.OriginUsername
	sanitized.OriginURL = sanitizeOriginURL(raw.OriginURL, origin)
	return sanitized
}
