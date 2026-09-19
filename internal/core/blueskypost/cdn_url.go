package blueskypost

import (
	"net/url"
	"strings"
)

const blueskyCDNHost = "cdn.bsky.app"

// isValidBlueskyCDNURL accepts only the resolved image URLs returned by the
// Bluesky AppView. Accepted strings are retained byte-for-byte by callers.
func isValidBlueskyCDNURL(rawURL string) bool {
	if rawURL == "" || containsUnsafeURLCharacter(rawURL) {
		return false
	}

	parsedURL, err := url.Parse(rawURL)
	if err != nil || !parsedURL.IsAbs() || parsedURL.Opaque != "" {
		return false
	}
	if parsedURL.Scheme != "https" || parsedURL.Host != blueskyCDNHost || parsedURL.User != nil {
		return false
	}
	if parsedURL.RawQuery != "" || parsedURL.ForceQuery || parsedURL.Fragment != "" {
		return false
	}

	path := parsedURL.EscapedPath()
	for range 8 {
		lowerPath := strings.ToLower(path)
		if strings.Contains(lowerPath, "%2f") || strings.Contains(lowerPath, "%5c") {
			return false
		}
		decodedPath, err := url.PathUnescape(path)
		if err != nil || containsUnsafeURLCharacter(decodedPath) || !isValidCDNImagePath(decodedPath) {
			return false
		}
		if decodedPath == path {
			return true
		}
		path = decodedPath
	}

	return false
}

func isValidCDNImagePath(path string) bool {
	segments := strings.Split(path, "/")
	if len(segments) < 4 || segments[0] != "" || segments[1] != "img" || segments[2] == "" {
		return false
	}
	for _, segment := range segments[2:] {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func containsUnsafeURLCharacter(value string) bool {
	if strings.Contains(value, `\`) {
		return true
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return true
		}
	}
	return false
}
