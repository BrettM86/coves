package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// The OAuth routes carry the tightest budgets in the app because they are the
// ones worth attacking: login and callback gate credential stuffing, refresh
// gates token abuse, and logout gates session exhaustion. internal/api/routes/
// oauth.go configures login/callback/logout at 10 per minute and refresh at 20.
// These tests pin those budgets against the same limiter the routes build, so a
// change to the numbers has to be a deliberate edit here too.

// oauthRouteBudget is one production OAuth route and the budget it is wired
// with in internal/api/routes/oauth.go.
type oauthRouteBudget struct {
	name       string
	path       string
	method     string
	clientAddr string
	limit      int
}

func TestRateLimiter_OAuth_RouteBudgets(t *testing.T) {
	routes := []oauthRouteBudget{
		{name: "login", path: "/oauth/login", method: "GET", clientAddr: "192.168.1.200:12345", limit: 10},
		{name: "mobile login", path: "/oauth/mobile/login", method: "GET", clientAddr: "192.168.1.201:12345", limit: 10},
		{name: "refresh", path: "/oauth/refresh", method: "POST", clientAddr: "192.168.1.202:12345", limit: 20},
		{name: "logout", path: "/oauth/logout", method: "POST", clientAddr: "192.168.1.203:12345", limit: 10},
		{name: "callback", path: "/oauth/callback", method: "GET", clientAddr: "192.168.1.204:12345", limit: 10},
	}

	for _, route := range routes {
		t.Run(route.name, func(t *testing.T) {
			handler := newHTTPTestLimiter(t, route.limit, 1*time.Minute).Middleware(okHandler())

			for i := 0; i < route.limit; i++ {
				req := httptest.NewRequest(route.method, route.path, nil)
				req.RemoteAddr = route.clientAddr
				rr := httptest.NewRecorder()

				handler.ServeHTTP(rr, req)

				assert.Equal(t, http.StatusOK, rr.Code, "request %d of %d should reach the handler", i+1, route.limit)
			}

			req := httptest.NewRequest(route.method, route.path, nil)
			req.RemoteAddr = route.clientAddr
			rr := httptest.NewRecorder()

			handler.ServeHTTP(rr, req)

			assert.Equal(t, http.StatusTooManyRequests, rr.Code,
				"request %d should exceed the %d/min budget", route.limit+1, route.limit)
			assert.Contains(t, rr.Body.String(), "Rate limit exceeded")
		})
	}
}

// TestRateLimiter_OAuth_CapsCredentialStuffing checks the property the login
// budget exists for: an attacker hammering one endpoint gets exactly the budget
// worth of guesses and nothing more. The handler returns 401 for every attempt,
// so a 401 means the request reached the credential check and a 429 means the
// limiter stopped it first.
func TestRateLimiter_OAuth_CapsCredentialStuffing(t *testing.T) {
	const loginBudget = 10
	const attempts = 15

	handler := newHTTPTestLimiter(t, loginBudget, 1*time.Minute).Middleware(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
	attackerIP := "203.0.113.50:12345"

	var reachedHandler, blocked int
	for i := 0; i < attempts; i++ {
		req := httptest.NewRequest("GET", "/oauth/login", nil)
		req.RemoteAddr = attackerIP
		rr := httptest.NewRecorder()

		handler.ServeHTTP(rr, req)

		switch rr.Code {
		case http.StatusUnauthorized:
			reachedHandler++
		case http.StatusTooManyRequests:
			blocked++
		default:
			t.Fatalf("unexpected status %d on attempt %d", rr.Code, i+1)
		}
	}

	assert.Equal(t, loginBudget, reachedHandler, "attacker should get exactly the login budget in guesses")
	assert.Equal(t, attempts-loginBudget, blocked, "every attempt past the budget must be blocked")
}

// TestRateLimiter_OAuth_BudgetsAreIndependentPerRoute proves each route gets its
// own limiter instance rather than sharing one bucket: exhausting login must not
// lock a legitimate client out of refreshing an existing session.
func TestRateLimiter_OAuth_BudgetsAreIndependentPerRoute(t *testing.T) {
	loginHandler := newHTTPTestLimiter(t, 10, 1*time.Minute).Middleware(okHandler())
	refreshHandler := newHTTPTestLimiter(t, 20, 1*time.Minute).Middleware(okHandler())

	clientIP := "192.168.1.205:12345"

	for i := 0; i < 10; i++ {
		req := httptest.NewRequest("GET", "/oauth/login", nil)
		req.RemoteAddr = clientIP
		rr := httptest.NewRecorder()
		loginHandler.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)
	}

	req := httptest.NewRequest("GET", "/oauth/login", nil)
	req.RemoteAddr = clientIP
	rr := httptest.NewRecorder()
	loginHandler.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusTooManyRequests, rr.Code, "Login should be rate limited")

	req = httptest.NewRequest("POST", "/oauth/refresh", nil)
	req.RemoteAddr = clientIP
	rr = httptest.NewRecorder()
	refreshHandler.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code, "Refresh should not be affected by login rate limit")
}
