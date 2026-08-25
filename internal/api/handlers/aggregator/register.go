package aggregator

import (
	"Coves/internal/api/reqbody"
	"Coves/internal/api/xrpc"
	"Coves/internal/atproto/identity"
	covesoauth "Coves/internal/atproto/oauth"
	"Coves/internal/core/users"
	"Coves/internal/validation"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

const (
	// maxWellKnownSize limits the response body size when fetching .well-known/atproto-did.
	// DIDs are typically ~60 characters. A 4KB limit leaves ample room for whitespace or
	// future metadata while still preventing attackers from streaming unbounded data.
	maxWellKnownSize = 4 * 1024 // bytes
)

// ErrDomainInvalid is what a registration whose domain is not a hostname is
// refused with.
//
// IT IS THE SHARED SENTINEL, not a second one. The validator this handler used
// to own now lives in internal/validation, because the community consumer builds
// a URL out of a federated domain exactly the way this handler builds one out of
// a registrant's and could not reach an unexported function in this package —
// which is the only reason that call site stayed open. Re-exporting the value
// rather than declaring a new error keeps `errors.Is` answering the same for
// both call sites: there is one predicate and one identity for its refusal, so a
// test here and a test there cannot come to disagree about what "invalid domain"
// means.
var ErrDomainInvalid = validation.ErrDomainInvalid

// RegisterHandler handles aggregator registration
type RegisterHandler struct {
	userService      users.UserService
	identityResolver identity.Resolver
	httpClient       *http.Client // Allows test injection

	// allowPrivateHosts disables the SSRF guard on the .well-known fetch.
	//
	// The destination is a domain a stranger POSTs to an unauthenticated route
	// (rate-limited 10/10min and nothing else), so in production this must stay
	// shut. It is construction state rather than an environment read inside the
	// fetch for the reason blobs.blobService.allowPrivateHosts documents: the fixtures that
	// exercise this path serve .well-known from httptest, which listens on
	// loopback, and Go forbids t.Setenv alongside t.Parallel.
	allowPrivateHosts bool

	// transportOptions is the TEST SEAM, carried on the handler so that the
	// client the tests exercise is the one NewRegisterHandler builds.
	//
	// The alternative — a fixture that constructs the handler and then assigns
	// registerHTTPClient(...) over its client — is what this field exists to
	// delete. That fixture proves registerHTTPClient guards, which is one call
	// frame short of the claim: h.allowPrivateHosts is read in exactly one place
	// (below), and a build where that line reads a constant `true` instead
	// leaves every such test green. Passing the seam THROUGH the constructor is
	// what makes the constructor's own wiring the thing under test.
	//
	// It is UNEXPORTED, and so is the option that sets it: oauth.WithHostResolver
	// must not be reachable from any non-test package, which scripts/ssrf-audit.sh
	// enforces as a hard gate. See pds.bearerClientConfig.transportOptions, the
	// same field for the same reason.
	transportOptions []covesoauth.Option
}

// RegisterHandlerOption configures optional RegisterHandler behaviour.
type RegisterHandlerOption func(*RegisterHandler)

// WithPrivateHostsAllowed disables the SSRF address guard on the .well-known
// domain-verification fetch.
//
// THE NAME IS THE CONTRACT: production must not call this. It is greppable,
// which is how "which handlers have the guard off" stays an answerable
// question.
func WithPrivateHostsAllowed() RegisterHandlerOption { // coves:allow-ssrf-hatch: this IS the hatch itself; the name is the contract
	return func(h *RegisterHandler) { h.allowPrivateHosts = true }
}

// withTransportOptions passes oauth transport options through to the client
// NewRegisterHandler builds, and is UNEXPORTED so production cannot reach it.
//
// It is not a second hatch: whatever a resolver seam answers is classified by
// the same pass a real DNS answer goes through, and the dial still goes only to
// addresses that survived it. See RegisterHandler.transportOptions, and
// pds/factory.go's withTransportOptions, which is the shape this copies.
func withTransportOptions(opts ...covesoauth.Option) RegisterHandlerOption {
	return func(h *RegisterHandler) { h.transportOptions = append(h.transportOptions, opts...) }
}

// PrivateHostOptions returns the options a caller holding an allow-private
// boolean should pass to NewRegisterHandler: the hatch when it is set, and
// NOTHING when it is not.
//
// It mirrors oauth.PrivateAddressOptions. `.env.ci:140` sets IS_DEV_ENV=true,
// so `make ci` takes the PERMISSIVE branch at every call site holding such a
// boolean; a unit test against this function is the only place in the
// repository where the branch production actually runs is evaluated. That
// matters especially here because
// RegisterAggregatorRoutes has no config of its own — the alternative to a pure
// function is an inline conditional three call layers away from anything that
// could reach it. Do not inline it back.
//
// FALSE RETURNS ZERO OPTIONS, AND THAT IS THE CONTRACT — not "options that are
// safe", but none.
func PrivateHostOptions(allowPrivate bool) []RegisterHandlerOption {
	if !allowPrivate {
		return nil
	}
	return []RegisterHandlerOption{WithPrivateHostsAllowed()} // coves:allow-ssrf-hatch: the gate helper allow-branch; its false branch returns nothing
}

// registerHTTPClient builds the client the .well-known domain-verification
// fetch goes through: the SSRF-safe transport of internal/atproto/oauth, which
// resolves the host, refuses private, loopback and link-local addresses, and
// then dials only the address it vetted.
//
// # THE GUARD IS NOT REDUNDANT WITH validation.NormalizeDomain
//
// The validator in front of this refuses every SPELLING of an address —
// 127.0.0.1, localhost, 2130706433, internal-admin — because none is a
// two-label name with an alphabetic TLD. What it cannot see is the DNS answer.
// A well-formed public hostname whose owner points it at 127.0.0.1 passes
// validation completely, and is the cheapest SSRF available here since the
// attacker owns the zone. That input is exactly what this client refuses and
// the validator cannot.
//
// The variadic oauth options exist for the caller's own tests: a hostname
// cannot be made to resolve to a chosen address hermetically, so
// oauth.WithHostResolver is how this package proves ITS wiring rather than
// re-proving oauth's. The seam cannot open the guard — whatever it answers is
// classified by the same pass a real DNS answer goes through.
//
// The 10s ceiling is this handler's own and is re-applied over the shared
// client's 15s.
func registerHTTPClient(allowPrivateHosts bool, opts ...covesoauth.Option) *http.Client {
	client := covesoauth.NewSSRFSafeHTTPClient(
		append(covesoauth.PrivateAddressOptions(allowPrivateHosts), opts...)...)
	client.Timeout = 10 * time.Second
	return client
}

// NewRegisterHandler creates a new registration handler.
//
// THE CLIENT IT BUILDS IS GUARDED UNLESS THE CALLER SAYS OTHERWISE, so that
// forgetting is safe: NewRegisterHandler with no options is what the next caller
// will write. RegisterAggregatorRoutes threads its own variadic registerOpts
// through, which in production is what PrivateHostOptions(false) returns —
// nothing — so the guarded construction is what an empty slice lands on too.
func NewRegisterHandler(userService users.UserService, identityResolver identity.Resolver,
	opts ...RegisterHandlerOption) *RegisterHandler {
	h := &RegisterHandler{
		userService:      userService,
		identityResolver: identityResolver,
	}
	for _, opt := range opts {
		opt(h)
	}
	// After the options, because the hatch and the transport options they may
	// have set are what this client is built from. Both are threaded, and the
	// second one is why this line is covered at all: the handler's guard tests
	// build through this constructor, so changing the argument below to a
	// constant fails them.
	h.httpClient = registerHTTPClient(h.allowPrivateHosts, h.transportOptions...)
	return h
}

// setHTTPClient overrides the HTTP client, and is UNEXPORTED because there is no
// safe way for a caller outside this package to use it.
//
// IT REPLACES RATHER THAN WRAPS, DELIBERATELY. Wrapping the guard around
// whatever it is handed reads like the safer design and is not: two fixtures in
// this package (register_test.go's and register_users_row_test.go's, both via
// stubClient) install a transport with a pinned dialer so that a request for a
// made-up domain reaches an httptest listener, and a wrapped guard would either
// refuse that listener's loopback address or fail to resolve the name at all
// under an egress-blocked CI. A third, newRecordingHandler, installs a
// RoundTripper that answers nothing and records what it was asked for. The seam
// has to keep replacing for any of those to work.
//
// AND REPLACING IS EXACTLY WHY IT CANNOT BE EXPORTED. A production caller
// reaching for it silently discards the guarded client NewRegisterHandler built,
// which is the hazard pds/factory.go's withTransportOptions is unexported to
// avoid — so leaving this one exported had the codebase answering the same
// question two opposite ways. An earlier version of this comment conceded the
// footgun and deferred the fix to "a greppable regression fence or a rename";
// the lowercase name is better than either, because it is the compiler that
// enforces it and there is nothing left to grep for. Every caller is a fixture
// in this package, so it costs them nothing.
//
// TestNoExportedSeamCanReplaceTheGuardedClient pins the property in general —
// nothing exported from this package takes an *http.Client — so the NEXT seam
// somebody adds is covered too.
func (h *RegisterHandler) setHTTPClient(client *http.Client) {
	h.httpClient = client
}

// RegisterRequest represents the registration request
type RegisterRequest struct {
	DID    string `json:"did"`
	Domain string `json:"domain"`
}

// RegisterResponse represents the registration response
type RegisterResponse struct {
	DID     string `json:"did"`
	Handle  string `json:"handle"`
	Message string `json:"message"`
}

// HandleRegister handles aggregator registration
// POST /xrpc/social.coves.aggregator.register
//
// Architecture Note: This handler contains business logic for domain verification.
// This is intentional for the following reasons:
// 1. Registration is a one-time setup operation, not core aggregator business logic
// 2. It primarily delegates to UserService (proper service layer)
// 3. Domain verification is an infrastructure concern (like TLS verification)
// 4. Moving to AggregatorService would create circular dependency (aggregators table has FK to users)
// 5. Similar pattern used in Bluesky's PDS for account creation
func (h *RegisterHandler) HandleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse request body. Registration is unauthenticated, so the body cap
	// is load-bearing, not hygiene — the tightest of the bounds (router
	// backstop, rate limit) standing in front of this handler.
	var req RegisterRequest
	if !xrpc.DecodeJSON(w, r, reqbody.LimitSmall, &req) {
		return
	}

	// Validate input
	if err := validateRegistrationRequest(req); err != nil {
		writeError(w, http.StatusBadRequest, "InvalidDID", err.Error())
		return
	}

	// Normalize inputs
	req.DID = strings.TrimSpace(req.DID)
	req.Domain = strings.TrimSpace(req.Domain)

	// Reject HTTP explicitly (HTTPS required for domain verification)
	if strings.HasPrefix(req.Domain, "http://") {
		writeError(w, http.StatusBadRequest, "InvalidDID", "Domain must use HTTPS, not HTTP")
		return
	}

	req.Domain = strings.TrimPrefix(req.Domain, "https://")
	req.Domain = strings.TrimSuffix(req.Domain, "/")

	// Re-validate after normalization to catch edge cases like "   " or "https://"
	if req.Domain == "" {
		writeError(w, http.StatusBadRequest, "InvalidDID", "Domain cannot be empty")
		return
	}

	// The domain must be a hostname before it can be concatenated into a URL,
	// and this check has to happen HERE — before any HTTP client is touched.
	//
	// verifyDomainOwnership builds its URL with
	// fmt.Sprintf("https://%s/.well-known/atproto-did", domain), and a string
	// parser has no way to know the concatenation was meant to stop at the host:
	// `internal-admin/v1/secrets?x=y#` fetches /v1/secrets?x=y from
	// internal-admin, because the trailing `#` turns the intended suffix into a
	// fragment that is never sent. `evil.com@internal-host` makes evil.com
	// userinfo and the internal name the host. So an unauthenticated caller
	// chooses the host, the path AND the query of a request this server makes
	// from inside its own network. Validating any later — after the request is
	// built, or inside verifyDomainOwnership — is too late: by then the caller's
	// host has already been resolved and dialled, which is the whole of the SSRF.
	//
	// The normalized form is what gets used from here on, so that one domain has
	// one spelling in the URL, in the logs and in anything that compares them.
	normalizedDomain, err := validation.NormalizeDomain(req.Domain)
	if err != nil {
		log.Printf("Registration refused a domain that is not a hostname for DID %s: %v", req.DID, err)
		writeError(w, http.StatusBadRequest, "InvalidDID",
			"Domain must be a hostname such as example.com, with no scheme, port, path, query or credentials")
		return
	}
	req.Domain = normalizedDomain

	// Verify domain ownership via .well-known
	if err := h.verifyDomainOwnership(r.Context(), req.DID, req.Domain); err != nil {
		log.Printf("Domain verification failed for DID %s, domain %s: %v", req.DID, req.Domain, err)
		writeError(w, http.StatusUnauthorized, "DomainVerificationFailed",
			"Could not verify domain ownership. Ensure .well-known/atproto-did serves your DID over HTTPS")
		return
	}

	// Check if user already exists (before CreateUser since it's idempotent)
	existingUser, err := h.userService.GetUserByDID(r.Context(), req.DID)
	if err == nil && existingUser != nil {
		writeError(w, http.StatusConflict, "AlreadyRegistered",
			"This aggregator is already registered with this instance")
		return
	}

	// Resolve DID to get handle and PDS URL
	identityInfo, err := h.identityResolver.Resolve(r.Context(), req.DID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "DIDResolutionFailed",
			"Could not resolve DID. Please verify it exists in the PLC directory")
		return
	}

	// Register the aggregator in the users table
	createReq := users.CreateUserRequest{
		DID:    req.DID,
		Handle: identityInfo.Handle,
		PDSURL: identityInfo.PDSURL,
	}

	user, err := h.userService.CreateUser(r.Context(), createReq)
	if err != nil {
		log.Printf("Failed to create user for aggregator DID %s: %v", req.DID, err)
		writeError(w, http.StatusInternalServerError, "RegistrationFailed",
			"Failed to register aggregator")
		return
	}

	// Return success response
	response := RegisterResponse{
		DID:     user.DID,
		Handle:  user.Handle,
		Message: fmt.Sprintf("Aggregator registered successfully. Next step: create a service declaration record at at://%s/social.coves.aggregator.service/self", user.DID),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
	}
}

// validateRegistrationRequest validates the registration request
func validateRegistrationRequest(req RegisterRequest) error {
	// Validate DID format
	if req.DID == "" {
		return fmt.Errorf("did is required")
	}

	if !strings.HasPrefix(req.DID, "did:") {
		return fmt.Errorf("did must start with 'did:' prefix")
	}

	// We support did:plc for now (most common for aggregators)
	if !strings.HasPrefix(req.DID, "did:plc:") && !strings.HasPrefix(req.DID, "did:web:") {
		return fmt.Errorf("only did:plc and did:web formats are currently supported")
	}

	// Validate domain
	if req.Domain == "" {
		return fmt.Errorf("domain is required")
	}

	return nil
}

// verifyDomainOwnership verifies that the domain serves the correct DID in .well-known/atproto-did
func (h *RegisterHandler) verifyDomainOwnership(ctx context.Context, expectedDID, domain string) error {
	// Construct .well-known URL
	wellKnownURL := fmt.Sprintf("https://%s/.well-known/atproto-did", domain)

	// Create request with context
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, wellKnownURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	// Perform request
	resp, err := h.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to fetch .well-known/atproto-did from %s: %w", domain, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Check status code
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf(".well-known/atproto-did returned status %d (expected 200)", resp.StatusCode)
	}

	// Read body with size limit to prevent DoS attacks from malicious servers
	// streaming arbitrarily large responses. Read one extra byte so we can detect
	// when the response exceeded the allowed size instead of silently truncating.
	limitedReader := io.LimitReader(resp.Body, maxWellKnownSize+1)
	body, err := io.ReadAll(limitedReader)
	if err != nil {
		return fmt.Errorf("failed to read .well-known/atproto-did response: %w", err)
	}

	if len(body) > maxWellKnownSize {
		return fmt.Errorf(".well-known/atproto-did response exceeds %d bytes", maxWellKnownSize)
	}

	// Parse DID from response
	actualDID := strings.TrimSpace(string(body))

	// Verify DID matches
	if actualDID != expectedDID {
		return fmt.Errorf("DID mismatch: .well-known/atproto-did contains '%s', expected '%s'", actualDID, expectedDID)
	}

	return nil
}
