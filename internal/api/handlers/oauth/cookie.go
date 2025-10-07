package oauth

import (
	"fmt"
	"sync"

	"github.com/gorilla/sessions"
)

var (
	// Global singleton cookie store
	cookieStoreInstance *sessions.CookieStore
	cookieStoreOnce     sync.Once
	cookieStoreErr      error
)

// InitCookieStore initializes the global cookie store singleton
// Must be called once at application startup before any handlers are created
func InitCookieStore(secret string) error {
	cookieStoreOnce.Do(func() {
		if len(secret) < MinCookieSecretLength {
			cookieStoreErr = fmt.Errorf("OAUTH_COOKIE_SECRET must be at least %d bytes for security", MinCookieSecretLength)
			return
		}
		cookieStoreInstance = sessions.NewCookieStore([]byte(secret))
	})
	return cookieStoreErr
}

// GetCookieStore returns the global cookie store singleton
// Panics if InitCookieStore has not been called successfully
func GetCookieStore() *sessions.CookieStore {
	if cookieStoreInstance == nil {
		panic("cookie store not initialized - call InitCookieStore first")
	}
	return cookieStoreInstance
}
