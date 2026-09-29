package testkit

import (
	"context"
	"encoding/json"
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// SignIn is client logic — a fixed sequence of HTTP exchanges between the
// AppView's OAuth routes and the PDS' authorization server — so it is tested
// here against two httptest servers that speak that protocol, and at T2
// (tests/e2e/session_contract_test.go) against the real AppView and PDS.
//
// The fakes are STRICT on purpose. Each one answers 4xx with a body naming the
// requirement that failed, because a lenient fake is how a helper passes here
// and then fails against @atproto/oauth-provider with an error nobody can map
// back to a missing header. The provider-side requirements are the ones
// @atproto/oauth-provider 0.22.1 (the pinned PDS 0.4.5027) enforces in
// create-authorization-page-middleware.js, create-api-middleware.js and
// assets/csrf.js; where a fake is stricter than the provider (an exact Referer,
// a required Origin) it is only ever stricter.

const (
	fakeSignInHandle   = "alice.test"
	fakeSignInDID      = "did:plc:fakesigninaccount"
	fakeSignInClientIP = "203.0.113.7"
	fakeSignInClientID = "https://appview.test/oauth/client-metadata.json"

	// The secrets. Each is a distinctive sentinel so that finding one in an
	// error message is unambiguous: none of them can appear by coincidence.
	fakeSignInPassword   = "password-SENTINEL-b7e1"
	fakeSignInRequestURI = "urn:ietf:params:oauth:request_uri:requesturi-SENTINEL-4c2d"
	fakeSignInState      = "state-SENTINEL-91fa"
	fakeSignInCode       = "code-SENTINEL-3e8b"
	fakeSignInSealed     = "sealed-SENTINEL-a06f"
	fakeSignInEphemeral  = "ephemeral-SENTINEL-5d17"
	fakeSignInBinding    = "binding-SENTINEL-6e05"
	fakeSignInDeviceID   = "device-SENTINEL-2f9a"
	fakeSignInSession    = "session-SENTINEL-before-8c41"
	fakeSignInRotated    = "session-SENTINEL-after-d372"
	// The provider accepts a CSRF token only if it is exactly 24 characters.
	fakeSignInCSRF = "csrfSENTINEL0123456789ab"
	// Carried in the query of a Location header that is not a valid URL.
	fakeSignInLocationSecret = "location-SENTINEL-19c7"
	// Cookies a provider sets on a response that refuses the request: the flow
	// received them, so they are secrets too, even though it never uses them.
	fakeSignInRefusalCookie          = "rotated-SENTINEL-0b3e"
	fakeSignInAuthorizeRefusalCookie = "authorize-SENTINEL-7f20"
	// A password holding the characters Go's %q escapes, a double quote and a
	// backslash, so an error that quotes it carries neither its raw nor its
	// query-escaped form.
	fakeSignInQuotablePassword = `pass"word\SENTINEL-7a1c`

	fakeSignInAPIPrefix = "/@atproto/oauth-provider/~api"
)

// observedRequest is what a fake records about one request it served: enough to
// assert where the helper went and what it claimed about the caller.
type observedRequest struct {
	Host     string
	Path     string
	ClientIP string
}

// fakeOAuthStack is an AppView's OAuth routes and a PDS' authorization server,
// wired to each other.
type fakeOAuthStack struct {
	AppView *httptest.Server
	PDS     *httptest.Server

	// appViewURL and pdsURL are the servers' canonical base URLs. They, and
	// failure, are fixed before either server starts, so the handlers read them
	// without synchronization.
	appViewURL string
	pdsURL     string
	// failure makes one step of the flow go wrong in the way a real deployment
	// can.
	failure fakeOAuthFailure

	mu              sync.Mutex
	appViewRequests []observedRequest
	pdsRequests     []observedRequest
}

func (s *fakeOAuthStack) record(into *[]observedRequest, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	*into = append(*into, observedRequest{Host: r.Host, Path: r.URL.Path, ClientIP: r.Header.Get("X-Real-IP")})
}

// AppViewRequests returns a copy of every request the fake AppView served.
func (s *fakeOAuthStack) AppViewRequests() []observedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]observedRequest(nil), s.appViewRequests...)
}

// PDSRequests returns a copy of every request the fake PDS served.
func (s *fakeOAuthStack) PDSRequests() []observedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]observedRequest(nil), s.pdsRequests...)
}

// authorizeURL is the PDS authorize URL the AppView's login redirects to. The
// PDS checks the API calls' Referer against exactly this string.
func (s *fakeOAuthStack) authorizeURL() string {
	return s.pdsURL + "/oauth/authorize?" + url.Values{
		"client_id":   {fakeSignInClientID},
		"request_uri": {fakeSignInRequestURI},
	}.Encode()
}

// fakeOAuthFailure names a way the flow can go wrong.
type fakeOAuthFailure int

const (
	fakeOAuthSucceeds fakeOAuthFailure = iota
	// The PDS rejects the password with a 401 envelope that echoes the
	// submitted password in its error_description and message.
	fakeOAuthSignInRejected
	// The AppView's login bounces to its own login page with an error code.
	fakeOAuthLoginRedirectsToLoginPage
	// The AppView's callback redirects without setting coves_session.
	fakeOAuthCallbackSetsNoSession
	// Consent answers with a redirect url that carries no code.
	fakeOAuthConsentOmitsCode
	// The AppView drops the callback connection without answering, so the
	// helper's callback request fails at the transport layer — where Go's
	// *url.Error quotes the full request URL, code and state included.
	fakeOAuthCallbackConnectionDropped
	// The AppView's login redirects to a Location that is not a valid URL.
	// net/http parses Location before CheckRedirect runs and quotes the raw
	// header, query and all, in the error it returns.
	fakeOAuthLoginMalformedLocation
	// Sign-in rejects with an "error" field that is the submitted password: a
	// provider's error code is input, not a safe string.
	fakeOAuthSignInErrorIsPassword
	// Sign-in rejects with the provider's real 400 shape: a code plus a
	// human-readable error_description.
	fakeOAuthSignInErrorDescribed
	// Sign-in rejects with an error_description that echoes the password.
	fakeOAuthSignInDescriptionEchoesPassword
	// The authorize page sets the device cookies but no csrf-token.
	fakeOAuthAuthorizeSetsNoCSRFCookie
	// Sign-in answers 200 with an HTML page instead of JSON.
	fakeOAuthSignInAnswersHTML
	// The AppView's login answers 429 from its rate limiter.
	fakeOAuthLoginRateLimited
	// The AppView's login redirects to /oauth/login on every request.
	fakeOAuthLoginRedirectLoop
	// The AppView's callback fails the way the real one does: a redirect to
	// its login page with error=server_error.
	fakeOAuthCallbackRedirectsWithServerError
	// Consent answers with a redirect url that refuses the authorization.
	fakeOAuthConsentAccessDenied
	// The authorize page answers 400 with an HTML error page.
	fakeOAuthAuthorizeRejected
	// Sign-in refuses with a 400 that sets a new cookie and echoes its value
	// in the error_description.
	fakeOAuthSignInRefusalSetsEchoedCookie
	// The authorize page refuses with a 400 that sets a cookie and echoes its
	// value in an OAuth error body.
	fakeOAuthAuthorizeRefusalSetsEchoedCookie
	// Sign-in answers 200 with a non-JSON Content-Type whose parameter is the
	// password it received.
	fakeOAuthSignInContentTypeEchoesPassword
)

// refuse answers a request that broke the protocol, naming what it broke.
func refuse(w http.ResponseWriter, status int, format string, args ...any) {
	http.Error(w, fmt.Sprintf(format, args...), status)
}

func newFakeOAuthStack(t *testing.T, failure fakeOAuthFailure) *fakeOAuthStack {
	t.Helper()
	stack := &fakeOAuthStack{failure: failure}
	stack.PDS = httptest.NewUnstartedServer(http.HandlerFunc(stack.servePDS))
	stack.AppView = httptest.NewUnstartedServer(http.HandlerFunc(stack.serveAppView))
	stack.pdsURL = "http://" + stack.PDS.Listener.Addr().String()
	stack.appViewURL = "http://" + stack.AppView.Listener.Addr().String()
	stack.PDS.Start()
	t.Cleanup(stack.PDS.Close)
	stack.AppView.Start()
	t.Cleanup(stack.AppView.Close)
	return stack
}

func (s *fakeOAuthStack) serveAppView(w http.ResponseWriter, r *http.Request) {
	s.record(&s.appViewRequests, r)
	canonicalHost := strings.TrimPrefix(s.appViewURL, "http://")
	switch r.URL.Path {
	case "/oauth/login":
		// A dev-mode AppView reached under a host other than its public URL
		// sends the browser to the same login on its canonical host first,
		// because the binding cookie it is about to set is host-only.
		if r.Host != canonicalHost {
			http.Redirect(w, r, s.appViewURL+"/oauth/login?"+r.URL.RawQuery, http.StatusFound)
			return
		}
		switch s.failure {
		case fakeOAuthLoginRedirectsToLoginPage:
			http.Redirect(w, r, "/login?error=some_code", http.StatusFound)
			return
		case fakeOAuthLoginMalformedLocation:
			// Written by hand: http.Redirect would clean the URL up.
			w.Header().Set("Location", "/%zz?request_uri="+fakeSignInLocationSecret)
			w.WriteHeader(http.StatusFound)
			return
		case fakeOAuthLoginRateLimited:
			// The real limiter's answer (internal/api/middleware/ratelimit.go).
			http.Error(w, "Rate limit exceeded. Please try again later.", http.StatusTooManyRequests)
			return
		case fakeOAuthLoginRedirectLoop:
			http.Redirect(w, r, s.appViewURL+"/oauth/login?"+r.URL.RawQuery, http.StatusFound)
			return
		}
		if got := r.URL.Query().Get("handle"); got != fakeSignInHandle {
			refuse(w, http.StatusBadRequest, "appview /oauth/login: handle = %q, want %q", got, fakeSignInHandle)
			return
		}
		// Host-only (no Domain) and scoped to /oauth, as the real AppView sets it.
		http.SetCookie(w, &http.Cookie{
			Name: "oauth_web_binding", Value: fakeSignInBinding, Path: "/oauth", HttpOnly: true,
		})
		http.Redirect(w, r, s.authorizeURL(), http.StatusFound)
	case "/oauth/callback":
		if r.Host != canonicalHost {
			refuse(w, http.StatusBadRequest, "appview /oauth/callback: served only on the canonical host %q, got %q", canonicalHost, r.Host)
			return
		}
		binding, err := r.Cookie("oauth_web_binding")
		if err != nil || binding.Value != fakeSignInBinding {
			refuse(w, http.StatusBadRequest, "appview /oauth/callback: missing or wrong oauth_web_binding cookie")
			return
		}
		query := r.URL.Query()
		for name, want := range map[string]string{
			"code": fakeSignInCode, "state": fakeSignInState, "iss": s.pdsURL,
		} {
			if got := query.Get(name); got != want {
				refuse(w, http.StatusBadRequest, "appview /oauth/callback: %s = %q, want %q", name, got, want)
				return
			}
		}
		switch s.failure {
		case fakeOAuthCallbackSetsNoSession:
			http.Redirect(w, r, "/", http.StatusFound)
			return
		case fakeOAuthCallbackRedirectsWithServerError:
			http.Redirect(w, r, "/login?error=server_error", http.StatusFound)
			return
		case fakeOAuthCallbackConnectionDropped:
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				refuse(w, http.StatusInternalServerError, "appview /oauth/callback: cannot hijack the connection to drop it")
				return
			}
			connection, _, err := hijacker.Hijack()
			if err != nil {
				refuse(w, http.StatusInternalServerError, "appview /oauth/callback: hijacking the connection: %v", err)
				return
			}
			_ = connection.Close()
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name: "coves_session", Value: fakeSignInSealed, Path: "/", HttpOnly: true,
		})
		http.Redirect(w, r, "/", http.StatusFound)
	default:
		refuse(w, http.StatusNotFound, "appview: no route for %s %s", r.Method, r.URL.Path)
	}
}

// refuseUnlessFromAuthorizePage applies the checks the provider runs on every
// call to its UI's API before looking at the body: fetch metadata that says
// same-origin, the issuer's Origin, the authorize page as Referer, a JSON body,
// a double-submitted CSRF token, and the device cookie. It reports whether the
// request passed; when it did not, the refusal has been written.
func (s *fakeOAuthStack) refuseUnlessFromAuthorizePage(w http.ResponseWriter, r *http.Request, step string) bool {
	if r.Header.Get("Sec-Fetch-Mode") != "same-origin" || r.Header.Get("Sec-Fetch-Site") != "same-origin" {
		refuse(w, http.StatusBadRequest, "%s: Sec-Fetch-Mode and Sec-Fetch-Site must be same-origin, got mode %q site %q",
			step, r.Header.Get("Sec-Fetch-Mode"), r.Header.Get("Sec-Fetch-Site"))
		return false
	}
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "application/json" {
		refuse(w, http.StatusUnsupportedMediaType, "%s: Content-Type = %q, want application/json", step, r.Header.Get("Content-Type"))
		return false
	}
	csrf, err := r.Cookie("csrf-token")
	if err != nil || len(csrf.Value) != len(fakeSignInCSRF) || csrf.Value != r.Header.Get("X-CSRF-Token") {
		refuse(w, http.StatusBadRequest, "%s: a 24-character csrf-token cookie must be present and equal the X-CSRF-Token header", step)
		return false
	}
	if got := r.Header.Get("Origin"); got != s.pdsURL {
		refuse(w, http.StatusBadRequest, "%s: Origin = %q, want the issuer origin %q", step, got, s.pdsURL)
		return false
	}
	if got := r.Header.Get("Referer"); got != s.authorizeURL() {
		refuse(w, http.StatusBadRequest, "%s: Referer = %q, want the full authorize URL %q", step, got, s.authorizeURL())
		return false
	}
	if device, err := r.Cookie("dev-id"); err != nil || device.Value != fakeSignInDeviceID {
		refuse(w, http.StatusBadRequest, "%s: missing or wrong dev-id cookie", step)
		return false
	}
	return true
}

// writeProviderError answers as the provider's API does when it refuses.
func writeProviderError(w http.ResponseWriter, status int, envelope map[string]string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(envelope)
}

func (s *fakeOAuthStack) servePDS(w http.ResponseWriter, r *http.Request) {
	s.record(&s.pdsRequests, r)
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/oauth/authorize":
		if r.Header.Get("Sec-Fetch-Mode") != "navigate" || r.Header.Get("Sec-Fetch-Dest") != "document" {
			refuse(w, http.StatusBadRequest,
				"pds /oauth/authorize: must be a top-level navigation (Sec-Fetch-Mode navigate, Sec-Fetch-Dest document), got mode %q dest %q",
				r.Header.Get("Sec-Fetch-Mode"), r.Header.Get("Sec-Fetch-Dest"))
			return
		}
		switch site := r.Header.Get("Sec-Fetch-Site"); site {
		case "same-origin", "same-site", "cross-site", "none":
		default:
			refuse(w, http.StatusBadRequest, "pds /oauth/authorize: Sec-Fetch-Site = %q, want a navigation's value", site)
			return
		}
		if got := r.URL.Query().Get("request_uri"); got != fakeSignInRequestURI {
			refuse(w, http.StatusBadRequest, "pds /oauth/authorize: request_uri = %q, want %q", got, fakeSignInRequestURI)
			return
		}
		if s.failure == fakeOAuthAuthorizeRefusalSetsEchoedCookie {
			http.SetCookie(w, &http.Cookie{Name: "dev-id", Value: fakeSignInAuthorizeRefusalCookie, Path: "/", HttpOnly: true})
			writeProviderError(w, http.StatusBadRequest, map[string]string{
				"error":             "invalid_request",
				"error_description": "device " + fakeSignInAuthorizeRefusalCookie + " rejected",
			})
			return
		}
		if s.failure == fakeOAuthAuthorizeRejected {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("<html><body>Invalid authorization request</body></html>"))
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "dev-id", Value: fakeSignInDeviceID, Path: "/", HttpOnly: true})
		http.SetCookie(w, &http.Cookie{Name: "ses-id", Value: fakeSignInSession, Path: "/", HttpOnly: true})
		if s.failure != fakeOAuthAuthorizeSetsNoCSRFCookie {
			// Secure, as the real provider marks it: net/http/cookiejar will
			// not send it back over the plain-http address the stack uses.
			http.SetCookie(w, &http.Cookie{Name: "csrf-token", Value: fakeSignInCSRF, Path: "/", Secure: true})
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body>sign in</body></html>"))

	case r.Method == http.MethodPost && r.URL.Path == fakeSignInAPIPrefix+"/sign-in":
		if !s.refuseUnlessFromAuthorizePage(w, r, "pds sign-in") {
			return
		}
		if session, err := r.Cookie("ses-id"); err != nil || session.Value != fakeSignInSession {
			refuse(w, http.StatusBadRequest, "pds sign-in: missing or wrong ses-id cookie")
			return
		}
		var body struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			refuse(w, http.StatusBadRequest, "pds sign-in: body is not JSON: %v", err)
			return
		}
		switch s.failure {
		case fakeOAuthSignInRejected:
			writeProviderError(w, http.StatusUnauthorized, map[string]string{
				"error":             "InvalidCredentials",
				"error_description": "Invalid identifier or password " + body.Password,
				"message":           "Invalid identifier or password " + body.Password,
			})
			return
		case fakeOAuthSignInErrorIsPassword:
			writeProviderError(w, http.StatusBadRequest, map[string]string{"error": body.Password})
			return
		case fakeOAuthSignInErrorDescribed:
			writeProviderError(w, http.StatusBadRequest, map[string]string{
				"error":             "invalid_request",
				"error_description": "Invalid identifier or password",
			})
			return
		case fakeOAuthSignInDescriptionEchoesPassword:
			writeProviderError(w, http.StatusBadRequest, map[string]string{
				"error":             "invalid_request",
				"error_description": "bad password " + body.Password,
			})
			return
		case fakeOAuthSignInAnswersHTML:
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html><body>sign in</body></html>"))
			return
		case fakeOAuthSignInRefusalSetsEchoedCookie:
			http.SetCookie(w, &http.Cookie{Name: "ses-id", Value: fakeSignInRefusalCookie, Path: "/", HttpOnly: true})
			writeProviderError(w, http.StatusBadRequest, map[string]string{
				"error":             "invalid_request",
				"error_description": "session " + fakeSignInRefusalCookie + " rejected",
			})
			return
		case fakeOAuthSignInContentTypeEchoesPassword:
			w.Header().Set("Content-Type", `text/html; x="`+body.Password+`"`)
			_, _ = w.Write([]byte("<html><body>sign in</body></html>"))
			return
		}
		if body.Username != fakeSignInHandle || body.Password != fakeSignInPassword {
			refuse(w, http.StatusUnauthorized, "pds sign-in: wrong username or password")
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "ses-id", Value: fakeSignInRotated, Path: "/", HttpOnly: true})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"account": map[string]any{
				"did":           fakeSignInDID,
				"pds":           s.pdsURL,
				"email":         "alice@example.test",
				"emailVerified": false,
				"handle":        fakeSignInHandle,
				"deactivated":   false,
			},
			"ephemeralToken": fakeSignInEphemeral,
		})

	case r.Method == http.MethodPost && r.URL.Path == fakeSignInAPIPrefix+"/consent":
		if !s.refuseUnlessFromAuthorizePage(w, r, "pds consent") {
			return
		}
		if session, err := r.Cookie("ses-id"); err != nil || session.Value != fakeSignInRotated {
			refuse(w, http.StatusBadRequest, "pds consent: ses-id cookie must be the one sign-in rotated to")
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+fakeSignInEphemeral {
			refuse(w, http.StatusUnauthorized, "pds consent: Authorization must be Bearer <ephemeralToken>")
			return
		}
		var body struct {
			DID string `json:"did"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			refuse(w, http.StatusBadRequest, "pds consent: body is not JSON: %v", err)
			return
		}
		if body.DID != fakeSignInDID {
			refuse(w, http.StatusBadRequest, "pds consent: did = %q, want the account.did sign-in returned %q", body.DID, fakeSignInDID)
			return
		}
		authorization := url.Values{
			"redirect_mode": {"query"},
			"redirect_uri":  {s.appViewURL + "/oauth/callback"},
			"code":          {fakeSignInCode},
			"state":         {fakeSignInState},
			"iss":           {s.pdsURL},
		}
		switch s.failure {
		case fakeOAuthConsentOmitsCode:
			authorization.Del("code")
		case fakeOAuthConsentAccessDenied:
			authorization.Del("code")
			authorization.Set("error", "access_denied")
		}
		redirect := s.pdsURL + "/oauth/authorize/redirect?" + authorization.Encode()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"url": redirect})

	default:
		refuse(w, http.StatusNotFound, "pds: no route for %s %s", r.Method, r.URL.Path)
	}
}

func TestSignInE_ReturnsSealedSessionFromCallback(t *testing.T) {
	stack := newFakeOAuthStack(t, fakeOAuthSucceeds)
	appView := NewAppView(t, WithAppViewURL(stack.AppView.URL), WithAppViewClientIP(fakeSignInClientIP))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	token, err := appView.SignInE(ctx, fakeSignInHandle, fakeSignInPassword)
	require.NoError(t, err)
	require.Equal(t, fakeSignInSealed, token, "SignInE must return the coves_session value the callback set")

	appViewRequests := stack.AppViewRequests()
	for _, path := range []string{"/oauth/login", "/oauth/callback"} {
		var seen *observedRequest
		for i := range appViewRequests {
			if appViewRequests[i].Path == path {
				seen = &appViewRequests[i]
				break
			}
		}
		require.NotNil(t, seen, "SignInE never requested %s on the AppView (requests: %v)", path, appViewRequests)
		require.Equal(t, fakeSignInClientIP, seen.ClientIP,
			"%s must carry the client's X-Real-IP, or login and callback land in the shared rate-limit bucket", path)
	}
	for _, request := range appViewRequests {
		require.NotEqual(t, "/", request.Path, "SignInE followed the callback's redirect to / instead of stopping at the cookie")
	}
	for _, request := range stack.PDSRequests() {
		require.NotEqual(t, "/oauth/authorize/redirect", request.Path,
			"SignInE fetched the consent redirect URL; it must only read code, state and iss from it")
	}
}

// A dev-mode AppView reached as localhost first redirects /oauth/login to its
// canonical host, and only that host holds the binding cookie and serves the
// callback. SignIn must take that hop and keep using the canonical host.
func TestSignInE_FollowsTheLoginRedirectToTheCanonicalHost(t *testing.T) {
	stack := newFakeOAuthStack(t, fakeOAuthSucceeds)
	port := stack.AppView.Listener.Addr().(*net.TCPAddr).Port
	aliasHost := "localhost:" + strconv.Itoa(port)
	canonicalHost := strings.TrimPrefix(stack.appViewURL, "http://")
	require.NotEqual(t, aliasHost, canonicalHost, "the test needs the alias and canonical hosts to differ")
	appView := NewAppView(t, WithAppViewURL("http://"+aliasHost), WithAppViewClientIP(fakeSignInClientIP))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	token, err := appView.SignInE(ctx, fakeSignInHandle, fakeSignInPassword)
	require.NoError(t, err)
	require.Equal(t, fakeSignInSealed, token, "SignInE must return the coves_session value the callback set")

	appViewRequests := stack.AppViewRequests()
	require.NotEmpty(t, appViewRequests)
	require.Equal(t, observedRequest{Host: aliasHost, Path: "/oauth/login", ClientIP: fakeSignInClientIP}, appViewRequests[0],
		"the flow must start at /oauth/login on the host the client was given")
	sawCanonicalLogin, sawCanonicalCallback := false, false
	for _, request := range appViewRequests {
		require.Equal(t, fakeSignInClientIP, request.ClientIP,
			"every AppView request must carry the client's X-Real-IP, including the canonical-host hop (%s%s)", request.Host, request.Path)
		if request.Host == canonicalHost && request.Path == "/oauth/login" {
			sawCanonicalLogin = true
		}
		if request.Path == "/oauth/callback" {
			require.Equal(t, canonicalHost, request.Host, "the callback must go to the canonical host that set the binding cookie")
			sawCanonicalCallback = true
		}
	}
	require.True(t, sawCanonicalLogin, "SignInE never followed the login redirect to the canonical host (requests: %v)", appViewRequests)
	require.True(t, sawCanonicalCallback, "SignInE never requested the callback (requests: %v)", appViewRequests)
}

func TestSignInE_FailsNamingTheStepWithoutLeakingSecrets(t *testing.T) {
	// The password is per case; the rest are fixed by the fakes.
	secrets := map[string]string{
		"ephemeral token":          fakeSignInEphemeral,
		"auth code":                fakeSignInCode,
		"state":                    fakeSignInState,
		"sealed session":           fakeSignInSealed,
		"request_uri":              fakeSignInRequestURI,
		"binding cookie":           fakeSignInBinding,
		"dev-id cookie":            fakeSignInDeviceID,
		"ses-id cookie":            fakeSignInSession,
		"rotated ses-id cookie":    fakeSignInRotated,
		"csrf-token cookie":        fakeSignInCSRF,
		"malformed Location URL":   fakeSignInLocationSecret,
		"refusal cookie":           fakeSignInRefusalCookie,
		"authorize refusal cookie": fakeSignInAuthorizeRefusalCookie,
	}

	tests := []struct {
		name    string
		failure fakeOAuthFailure
		// step is the step name the error must start with, before a colon.
		step string
		// mentions are further strings the error must carry to be actionable.
		mentions []string
		// mentionsIgnoringCase are like mentions, compared case-insensitively.
		mentionsIgnoringCase []string
		// password is what SignInE is given; empty means fakeSignInPassword.
		password string
	}{
		{
			name:     "PDS rejects the password",
			failure:  fakeOAuthSignInRejected,
			step:     "pds sign-in",
			mentions: []string{"401", "InvalidCredentials"},
		},
		{
			name:     "AppView login bounces to its login page",
			failure:  fakeOAuthLoginRedirectsToLoginPage,
			step:     "appview login",
			mentions: []string{"some_code"},
		},
		{
			name:    "AppView callback sets no session",
			failure: fakeOAuthCallbackSetsNoSession,
			step:    "appview callback",
		},
		{
			name:    "consent redirect carries no code",
			failure: fakeOAuthConsentOmitsCode,
			step:    "pds consent",
		},
		{
			name:    "callback connection dropped",
			failure: fakeOAuthCallbackConnectionDropped,
			step:    "appview callback",
		},
		{
			name:    "AppView login redirects to a Location that is not a URL",
			failure: fakeOAuthLoginMalformedLocation,
			step:    "appview login",
		},
		{
			name:    "provider error code is the password",
			failure: fakeOAuthSignInErrorIsPassword,
			step:    "pds sign-in",
		},
		{
			name:     "provider error description is reported",
			failure:  fakeOAuthSignInErrorDescribed,
			step:     "pds sign-in",
			mentions: []string{"HTTP 400", "invalid_request", "Invalid identifier or password"},
		},
		{
			name:     "provider error description echoing the password is scrubbed",
			failure:  fakeOAuthSignInDescriptionEchoesPassword,
			step:     "pds sign-in",
			mentions: []string{"bad password"},
		},
		{
			name:     "authorize page sets no csrf-token cookie",
			failure:  fakeOAuthAuthorizeSetsNoCSRFCookie,
			step:     "pds authorize",
			mentions: []string{"csrf-token"},
		},
		{
			name:     "sign-in answers HTML",
			failure:  fakeOAuthSignInAnswersHTML,
			step:     "pds sign-in",
			mentions: []string{"text/html"},
		},
		{
			name:                 "AppView login is rate limited",
			failure:              fakeOAuthLoginRateLimited,
			step:                 "appview login",
			mentions:             []string{"429"},
			mentionsIgnoringCase: []string{"rate limit"},
		},
		{
			name:    "AppView login redirects to itself repeatedly",
			failure: fakeOAuthLoginRedirectLoop,
			step:    "appview login",
		},
		{
			name:     "AppView callback redirects to its login page with server_error",
			failure:  fakeOAuthCallbackRedirectsWithServerError,
			step:     "appview callback",
			mentions: []string{"server_error"},
		},
		{
			name:     "consent refuses the authorization",
			failure:  fakeOAuthConsentAccessDenied,
			step:     "pds consent",
			mentions: []string{"access_denied"},
		},
		{
			name:     "authorize page rejects the request",
			failure:  fakeOAuthAuthorizeRejected,
			step:     "pds authorize",
			mentions: []string{"HTTP 400"},
		},
		{
			name:     "sign-in refusal sets a cookie and echoes it",
			failure:  fakeOAuthSignInRefusalSetsEchoedCookie,
			step:     "pds sign-in",
			mentions: []string{"HTTP 400", "invalid_request"},
		},
		{
			name:     "authorize refusal sets a cookie and echoes it",
			failure:  fakeOAuthAuthorizeRefusalSetsEchoedCookie,
			step:     "pds authorize",
			mentions: []string{"HTTP 400", "invalid_request"},
		},
		{
			name:     "sign-in Content-Type echoes a password that quoting escapes",
			failure:  fakeOAuthSignInContentTypeEchoesPassword,
			step:     "pds sign-in",
			mentions: []string{"text/html"},
			password: fakeSignInQuotablePassword,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stack := newFakeOAuthStack(t, tt.failure)
			appView := NewAppView(t, WithAppViewURL(stack.AppView.URL), WithAppViewClientIP(fakeSignInClientIP))

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			password := tt.password
			if password == "" {
				password = fakeSignInPassword
			}
			token, err := appView.SignInE(ctx, fakeSignInHandle, password)
			require.Error(t, err)
			require.Empty(t, token, "a failed sign-in must not return a token")

			message := err.Error()
			require.True(t, strings.HasPrefix(message, tt.step+":"),
				"the error must start by naming the step that failed (%q), got %q", tt.step, message)
			for _, mention := range tt.mentions {
				require.Contains(t, message, mention)
			}
			for _, mention := range tt.mentionsIgnoringCase {
				require.Contains(t, strings.ToLower(message), strings.ToLower(mention))
			}
			caseSecrets := map[string]string{"password": password}
			for what, secret := range secrets {
				caseSecrets[what] = secret
			}
			// A secret leaks in any form a reader can turn back into it: raw,
			// query-escaped, or escaped by %q (strconv.Quote less its quotes).
			for what, secret := range caseSecrets {
				quoted := strconv.Quote(secret)
				for _, form := range []struct{ name, encoded string }{
					{"raw", secret},
					{"query-escaped", url.QueryEscape(secret)},
					{"%q-escaped", quoted[1 : len(quoted)-1]},
				} {
					require.NotContains(t, message, form.encoded, "the error leaks the %s (%s)", what, form.name)
				}
			}
		})
	}
}
