package oauth

import (
	"encoding/base64"
	"fmt"
	"os"
	"strings"
)

// GetEnvBase64OrPlain retrieves an environment variable that may be base64 encoded.
// If the value starts with "base64:", it will be decoded.
// Otherwise, it returns the plain value.
//
// This allows storing sensitive values like JWKs in base64 format to avoid
// shell escaping issues and newline handling problems.
//
// Example usage in .env:
//
//	OAUTH_PRIVATE_JWK={"alg":"ES256",...}  (plain JSON)
//	OAUTH_PRIVATE_JWK=base64:eyJhbGc... (base64 encoded)
func GetEnvBase64OrPlain(key string) (string, error) {
	value := os.Getenv(key)
	if value == "" {
		return "", nil
	}

	// Check if value is base64 encoded
	if strings.HasPrefix(value, "base64:") {
		encoded := strings.TrimPrefix(value, "base64:")
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return "", fmt.Errorf("invalid base64 encoding for %s: %w", key, err)
		}
		return string(decoded), nil
	}

	// Return plain value
	return value, nil
}
