package oauth

import "time"

const (
	// Session cookie configuration
	SessionMaxAge = 7 * 24 * 60 * 60 // 7 days in seconds

	// Minimum security requirements
	MinCookieSecretLength = 32 // bytes
)

// Time-based constants
var (
	TokenRefreshThreshold = 5 * time.Minute
	SessionDuration       = 7 * 24 * time.Hour
)
