package communities

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// parseJWTExpiration extracts the expiration time from a JWT access token
// This function does NOT verify the signature - it only parses the exp claim
// atproto access tokens use standard JWT format with 'exp' claim (Unix timestamp)
func parseJWTExpiration(token string) (time.Time, error) {
	// Remove "Bearer " prefix if present
	token = strings.TrimPrefix(token, "Bearer ")
	token = strings.TrimSpace(token)

	// JWT format: header.payload.signature
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, fmt.Errorf("invalid JWT format: expected 3 parts, got %d", len(parts))
	}

	// Decode payload (second part) - use RawURLEncoding (no padding)
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to decode JWT payload: %w", err)
	}

	// Extract exp claim (Unix timestamp)
	var claims struct {
		Exp int64 `json:"exp"` // Expiration time (seconds since Unix epoch)
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}, fmt.Errorf("failed to parse JWT claims: %w", err)
	}

	if claims.Exp == 0 {
		return time.Time{}, fmt.Errorf("JWT missing 'exp' claim")
	}

	// Convert Unix timestamp to time.Time
	return time.Unix(claims.Exp, 0), nil
}

// NeedsRefresh checks if an access token should be refreshed
// Returns true if the token expires within the next 5 minutes (or is already expired)
// Uses a 5-minute buffer to ensure we refresh before actual expiration
func NeedsRefresh(accessToken string) (bool, error) {
	if accessToken == "" {
		return false, fmt.Errorf("access token is empty")
	}

	expiration, err := parseJWTExpiration(accessToken)
	if err != nil {
		return false, fmt.Errorf("failed to parse token expiration: %w", err)
	}

	// Refresh if token expires within 5 minutes
	// This prevents service interruptions from expired tokens
	bufferTime := 5 * time.Minute
	expiresWithinBuffer := time.Now().Add(bufferTime).After(expiration)

	return expiresWithinBuffer, nil
}
