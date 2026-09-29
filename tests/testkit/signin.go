package testkit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Headless OAuth web sign-in.
//
// WHY THIS EXISTS
//
// RequireAuth accepts one credential: a sealed token naming a row in the
// AppView's OAuth session store, and the only thing that issues one is
// /oauth/callback at the end of a real authorization-code flow. T2 may not
// write to the AppView's database, so it has no other way to get a session
// than to do what a browser does: start at /oauth/login, sign in and consent at
// the PDS' authorization server, and hand the resulting code to the callback.
// SignIn is that browser, minus the browser.
//
// WHAT IT DEPENDS ON
//
// The PDS side is not an XRPC API. It is the JSON API behind the sign-in page of
// @atproto/oauth-provider (the ~api/sign-in and ~api/consent endpoints), which
// is an implementation detail of the provider's UI, pinned here by the PDS image
// in docker-compose.ci.yml (last verified against pds:0.4.5027). Its checks are
// what shape the requests below: the authorize page must be loaded as a
// top-level navigation to bind a device, and the API calls must look
// same-origin (Origin, Referer and Sec-Fetch-* headers) and carry the
// double-submit CSRF token the page set. A PDS upgrade that changes that UI
// breaks this helper, and the fix belongs here.
//
// WHAT IT NEVER DOES
//
// It follows no redirect on its own: each hop is a step whose answer is checked,
// so a failure names the step instead of surfacing as a confusing final page.
// Its errors start with that step and carry the HTTP status, the provider's
// error code and description, a redirect's path but never its query, and
// transport errors. Secrecy is enforced by value at one point, where SignInE
// returns: every secret the flow handled — the password, the ephemeral token,
// the request_uri, the authorization code and state, and every cookie value
// either side set, the sealed session included — is scrubbed from the message.

// oauthProviderAPIPrefix is where @atproto/oauth-provider mounts its UI's API.
const oauthProviderAPIPrefix = "/@atproto/oauth-provider/~api"

// signInTimeout bounds SignIn as a whole. The flow is five requests against
// local services, or six when a dev-mode AppView first canonicalizes the login
// host; a minute is ample on a loaded CI machine and still fails a hung PDS well
// inside the go test timeout.
const signInTimeout = 60 * time.Second

// maxProviderDescription caps how much of a provider's error_description an
// error repeats.
const maxProviderDescription = 200

// redactedSecret stands in for a secret wherever an error would have quoted it.
const redactedSecret = "[redacted]"

// oauthErrorCode matches what an OAuth error code looks like, a first filter on
// what reaches a test log. It is a shape check only: secrecy comes from
// scrubbing.
var oauthErrorCode = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// signInFlow is the state of one sign-in: the two sides' cookies, the
// addresses each later step must reuse, and the secrets handled so far.
type signInFlow struct {
	appView *AppView
	// appViewOrigin is where the flow's AppView requests go. It starts as the
	// client's base URL and moves if /oauth/login canonicalizes the host,
	// because the binding cookie it sets is host-only.
	appViewOrigin string
	// appViewHTTP keeps a fresh cookie jar for the AppView side, so the
	// host-only, /oauth-scoped binding cookie reaches the callback exactly as
	// a browser would send it.
	appViewHTTP *http.Client

	pdsHTTP *http.Client
	// pdsCookies is a name→value map rather than a cookiejar: the provider
	// marks csrf-token Secure, and net/http/cookiejar will not send a Secure
	// cookie over the plain-http address the stack's PDS listens on.
	pdsCookies   map[string]string
	authorizeURL string
	issuer       string

	// secrets holds every secret value the flow has handled, raw and
	// query-escaped, for scrubbing out of errors.
	secrets []string
}

// SignIn runs the real OAuth web login for account against this AppView and
// returns the sealed session token, usable with As. Fatal on failure.
func (a *AppView) SignIn(t TestingT, account *Account) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), signInTimeout)
	defer cancel()
	token, err := a.SignInE(ctx, account.Handle, account.Password)
	if err != nil {
		t.Fatalf("testkit: signing %s in through the AppView's OAuth web flow: %v", account.Handle, err)
	}
	return token
}

// SignInE is the error-returning form of SignIn. Its error is scrubbed of every
// secret the flow handled; errors.Is still recognizes the context's deadline or
// cancellation through it.
func (a *AppView) SignInE(ctx context.Context, handle, password string) (string, error) {
	flow, err := a.newSignInFlow()
	if err != nil {
		return "", err
	}
	flow.remember(password)
	token, err := flow.run(ctx, handle, password)
	if err != nil {
		return "", flow.scrub(err)
	}
	return token, nil
}

func (a *AppView) newSignInFlow() (*signInFlow, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("sign-in: creating cookie jar: %w", err)
	}
	transport := a.HTTP.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	guarded := locationGuard{next: transport}
	noRedirects := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &signInFlow{
		appView:       a,
		appViewOrigin: a.BaseURL,
		appViewHTTP:   &http.Client{Transport: guarded, Timeout: a.HTTP.Timeout, Jar: jar, CheckRedirect: noRedirects},
		pdsHTTP:       &http.Client{Transport: guarded, Timeout: a.HTTP.Timeout, CheckRedirect: noRedirects},
		pdsCookies:    map[string]string{},
	}, nil
}

// run is the flow itself. Its errors are not yet scrubbed; SignInE does that.
func (f *signInFlow) run(ctx context.Context, handle, password string) (string, error) {
	authorizeURL, err := f.startLogin(ctx, handle)
	if err != nil {
		return "", err
	}
	if err := f.loadAuthorizePage(ctx, authorizeURL); err != nil {
		return "", err
	}

	var signIn struct {
		Account struct {
			DID string `json:"did"`
		} `json:"account"`
		EphemeralToken string `json:"ephemeralToken"`
	}
	err = f.callProviderAPI(ctx, "sign-in", map[string]any{
		"locale":   "en",
		"username": handle,
		"password": password,
		"remember": false,
	}, "", &signIn)
	if err != nil {
		return "", err
	}
	f.remember(signIn.EphemeralToken)
	if signIn.Account.DID == "" || signIn.EphemeralToken == "" {
		return "", errors.New("pds sign-in: response carried no account.did or no ephemeralToken")
	}

	// Consent answers with the URL the provider would send the browser to. It
	// carries exactly the parameters the AppView's callback receives, so it is
	// read, never fetched.
	var consent struct {
		URL string `json:"url"`
	}
	if err := f.callProviderAPI(ctx, "consent", map[string]any{"did": signIn.Account.DID}, signIn.EphemeralToken, &consent); err != nil {
		return "", err
	}
	authorization, err := url.Parse(consent.URL)
	if err != nil || consent.URL == "" {
		return "", errors.New("pds consent: response carried no parseable redirect url")
	}
	query := authorization.Query()
	f.remember(query.Get("code"), query.Get("state"))
	if code := query.Get("error"); code != "" {
		return "", fmt.Errorf("pds consent: authorization refused with %s", sanitizeOAuthError(code))
	}
	callback := url.Values{}
	for _, name := range []string{"code", "state", "iss"} {
		if query.Get(name) == "" {
			return "", fmt.Errorf("pds consent: redirect url carried no %s", name)
		}
		callback.Set(name, query.Get(name))
	}

	return f.completeCallback(ctx, callback)
}

// startLogin requests /oauth/login and returns the PDS authorize URL it
// redirects to. A dev-mode AppView reached under a host other than the one in
// its configured public URL first redirects to /oauth/login on the public URL's
// host; that one hop is taken.
func (f *signInFlow) startLogin(ctx context.Context, handle string) (string, error) {
	target := f.appViewOrigin + "/oauth/login?" + url.Values{"handle": {handle}}.Encode()
	for hop := 0; hop < 2; hop++ {
		resp, err := f.appViewRequest(ctx, "appview login", target)
		if err != nil {
			return "", err
		}
		location, err := redirectLocation("appview login", resp)
		if err != nil {
			return "", err
		}
		switch location.Path {
		case "/oauth/authorize":
			return location.String(), nil
		case "/oauth/login":
			f.appViewOrigin = location.Scheme + "://" + location.Host
			target = location.String()
		case "/login":
			return "", fmt.Errorf("appview login: redirected to the login page with %s", sanitizeOAuthError(location.Query().Get("error")))
		default:
			return "", fmt.Errorf("appview login: redirected to unexpected path %q", location.Path)
		}
	}
	return "", errors.New("appview login: redirected to /oauth/login more than once")
}

// loadAuthorizePage loads the PDS authorize page as a top-level navigation,
// which binds the pending authorization request to a new device session.
func (f *signInFlow) loadAuthorizePage(ctx context.Context, authorizeURL string) error {
	parsed, err := url.Parse(authorizeURL)
	if err != nil {
		return errors.New("pds authorize: redirect location is not a URL")
	}
	f.remember(parsed.Query().Get("request_uri"))
	f.authorizeURL = authorizeURL
	f.issuer = parsed.Scheme + "://" + parsed.Host

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, authorizeURL, nil)
	if err != nil {
		return errors.New("pds authorize: building request failed")
	}
	req.Header.Set("Accept", "text/html")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := send(f.pdsHTTP, req, "pds authorize")
	if err != nil {
		return err
	}
	defer drainAndClose(resp)
	// Absorb before the status check: a refusal's cookies must be remembered
	// before its body can echo them into the error.
	f.absorbPDSCookies(resp)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("pds authorize: HTTP %d%s", resp.StatusCode, f.providerErrorSuffix(resp))
	}
	// The provider's CSRF check is double-submit: the header must equal the
	// cookie the page set, so a page that set none is a failure here.
	var missing []string
	for _, name := range []string{"dev-id", "ses-id", "csrf-token"} {
		if f.pdsCookies[name] == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("pds authorize: page set no %s cookie", strings.Join(missing, ", "))
	}
	return nil
}

// callProviderAPI POSTs in to the provider UI's endpoint, as the sign-in page's
// own script would, and decodes a 200 answer into out.
func (f *signInFlow) callProviderAPI(ctx context.Context, endpoint string, in any, bearer string, out any) error {
	step := "pds " + endpoint
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("%s: encoding request failed", step)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.issuer+oauthProviderAPIPrefix+"/"+endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%s: building request failed", step)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", f.issuer)
	req.Header.Set("Referer", f.authorizeURL)
	req.Header.Set("Sec-Fetch-Mode", "same-origin")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("X-CSRF-Token", f.pdsCookies["csrf-token"])
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for name, value := range f.pdsCookies {
		req.AddCookie(&http.Cookie{Name: name, Value: value})
	}
	resp, err := send(f.pdsHTTP, req, step)
	if err != nil {
		return err
	}
	defer drainAndClose(resp)
	// Sign-in rotates ses-id; consent must present the rotated one. Absorb
	// before the status check so a refusal's cookies are remembered before its
	// body can echo them into the error.
	f.absorbPDSCookies(resp)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d%s", step, resp.StatusCode, f.providerErrorSuffix(resp))
	}
	// maxErrorBody bounds this success body too; the provider's answers are small.
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxErrorBody)).Decode(out); err != nil {
		return fmt.Errorf("%s: response (Content-Type %q) is not the expected JSON: %w", step, resp.Header.Get("Content-Type"), err)
	}
	return nil
}

// completeCallback hands the authorization response to the AppView's callback
// and returns the sealed session it set. A callback that answers without
// setting coves_session failed, whatever its status says.
func (f *signInFlow) completeCallback(ctx context.Context, params url.Values) (string, error) {
	resp, err := f.appViewRequest(ctx, "appview callback", f.appViewOrigin+"/oauth/callback?"+params.Encode())
	if err != nil {
		return "", err
	}
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "coves_session" && cookie.Value != "" && cookie.MaxAge >= 0 {
			return cookie.Value, nil
		}
	}
	if location, err := resp.Location(); err == nil && location.Path == "/login" {
		return "", fmt.Errorf("appview callback: HTTP %d, redirected to the login page with %s",
			resp.StatusCode, sanitizeOAuthError(location.Query().Get("error")))
	}
	return "", fmt.Errorf("appview callback: HTTP %d without setting coves_session", resp.StatusCode)
}

// appViewRequest GETs target with the flow's AppView cookie jar and the
// client's headers, which is how the client IP reaches the AppView: login and
// callback share a rate limiter keyed on it. The body is drained and closed;
// callers read only the status, Location and cookies.
func (f *signInFlow) appViewRequest(ctx context.Context, step, target string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: building request failed", step)
	}
	for name, values := range f.appView.Headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	resp, err := send(f.appViewHTTP, req, step)
	if err != nil {
		return nil, err
	}
	drainAndClose(resp)
	for _, cookie := range resp.Cookies() {
		f.remember(cookie.Value)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%s: HTTP 429, rate limited by the AppView's OAuth login limiter "+
			"(10 per minute per client IP; each sign-in spends two or three)", step)
	}
	return resp, nil
}

func (f *signInFlow) absorbPDSCookies(resp *http.Response) {
	for _, cookie := range resp.Cookies() {
		f.remember(cookie.Value)
		if cookie.MaxAge < 0 || cookie.Value == "" {
			delete(f.pdsCookies, cookie.Name)
			continue
		}
		f.pdsCookies[cookie.Name] = cookie.Value
	}
}

// providerErrorSuffix reads an OAuth-style {"error", "error_description"} body
// and returns both for an error message. The description is free text that
// can echo the request, so it is scrubbed before it is cut to
// maxProviderDescription bytes, which keeps a cut from splitting a secret.
func (f *signInFlow) providerErrorSuffix(resp *http.Response) string {
	var envelope struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxErrorBody)).Decode(&envelope); err != nil || envelope.Error == "" {
		return ""
	}
	suffix := " " + sanitizeOAuthError(envelope.Error)
	if description := f.scrubText(envelope.Description); description != "" {
		if len(description) > maxProviderDescription {
			description = strings.ToValidUTF8(description[:maxProviderDescription], "") + "…"
		}
		suffix += ": " + description
	}
	return suffix
}

// remember records secret values the flow has handled, so scrub can remove
// them from errors: each raw, query-escaped, and as %q would write it (the
// strconv.Quote form without its outer quotes, kept only when quoting changed
// the value), so a secret formatted with %q is caught too.
func (f *signInFlow) remember(values ...string) {
	for _, value := range values {
		if value != "" {
			f.secrets = append(f.secrets, value, url.QueryEscape(value))
			if quoted := strconv.Quote(value); quoted[1:len(quoted)-1] != value {
				f.secrets = append(f.secrets, quoted[1:len(quoted)-1])
			}
		}
	}
}

// scrubText replaces every remembered form of every secret in text with
// redactedSecret.
func (f *signInFlow) scrubText(text string) string {
	secrets := slices.Clone(f.secrets)
	// Longest first, so a secret that contains another is replaced whole.
	slices.SortFunc(secrets, func(a, b string) int { return len(b) - len(a) })
	for _, secret := range secrets {
		text = strings.ReplaceAll(text, secret, redactedSecret)
	}
	return text
}

// scrub is the flow's one secrecy choke point: it returns err's message with
// every remembered secret removed, in raw, query-escaped and %q-quoted form.
func (f *signInFlow) scrub(err error) error {
	scrubbed := &signInError{message: f.scrubText(err.Error())}
	for _, contextErr := range []error{context.DeadlineExceeded, context.Canceled} {
		if errors.Is(err, contextErr) {
			scrubbed.contextErr = contextErr
			break
		}
	}
	return scrubbed
}

// signInError is a scrubbed sign-in failure. It unwraps only to the context's
// error, so errors.Is still sees a deadline or cancellation while no unscrubbed
// message stays reachable through it.
type signInError struct {
	message    string
	contextErr error
}

func (e *signInError) Error() string { return e.message }

func (e *signInError) Unwrap() error { return e.contextErr }

// locationGuard drops a Location header that does not parse as a URL
// reference. net/http parses Location before CheckRedirect can stop the
// redirect and, when that fails, returns an error quoting the raw header,
// query and all — text the flow has never seen and so cannot scrub. Without
// the header the client returns the response, and redirectLocation reports a
// redirect without a usable Location.
type locationGuard struct {
	next http.RoundTripper
}

func (g locationGuard) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := g.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if location := resp.Header.Get("Location"); location != "" {
		if _, err := req.URL.Parse(location); err != nil {
			resp.Header.Del("Location")
		}
	}
	return resp, nil
}

// send performs req, unwrapping *url.Error: its message quotes the full request
// URL, and the callback's query string holds the authorization code.
func send(client *http.Client, req *http.Request, step string) (*http.Response, error) {
	resp, err := client.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return nil, fmt.Errorf("%s: %w", step, err)
	}
	return resp, nil
}

// redirectLocation requires resp to be a redirect and returns where it points.
// The Location itself stays out of the error: its query can carry anything.
func redirectLocation(step string, resp *http.Response) (*url.URL, error) {
	if resp.StatusCode < 300 || resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%s: HTTP %d, expected a redirect", step, resp.StatusCode)
	}
	location, err := resp.Location()
	if err != nil {
		return nil, fmt.Errorf("%s: HTTP %d redirect without a usable Location", step, resp.StatusCode)
	}
	return location, nil
}

// sanitizeOAuthError renders an OAuth error code for a message, or says there
// was none worth repeating.
func sanitizeOAuthError(code string) string {
	switch {
	case code == "":
		return "no error code"
	case oauthErrorCode.MatchString(code):
		return "error " + code
	default:
		return "an unrecognized error code"
	}
}

// drainAndClose discards what is left of a body, bounded, so the connection can
// be reused, then closes it.
func drainAndClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
	_ = resp.Body.Close()
}
