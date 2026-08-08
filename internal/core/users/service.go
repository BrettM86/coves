package users

import (
	"Coves/internal/atproto/identity"
	"Coves/internal/core/blobs"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// atProto handle validation regex (per official atProto spec: https://atproto.com/specs/handle)
// - Must have at least one dot (domain-like structure)
// - Each segment max 63 chars, total max 253 chars
// - Segments: alphanumeric start/end, hyphens allowed in middle
// - TLD (final segment) must start with letter (not digit)
// - Case-insensitive, normalized to lowercase
var handleRegex = regexp.MustCompile(`^([a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)

// Disallowed TLDs per atProto spec
var disallowedTLDs = map[string]bool{
	".alt":       true,
	".arpa":      true,
	".example":   true,
	".internal":  true,
	".invalid":   true,
	".local":     true,
	".localhost": true,
	".onion":     true,
	// .test is allowed for development
}

const (
	minPasswordLength = 8 // Reasonable minimum, though PDS may enforce stricter rules
	maxHandleLength   = 253
)

// pdsAdminCallTimeout bounds outbound calls to the PDS admin API. Generous
// enough for cold disk reads on the PDS, tight enough that a hung admin call
// can't pin a captcha-verified caller for the request's full lifetime.
const pdsAdminCallTimeout = 10 * time.Second

// profileBackfillTimeout bounds the detached profile-backfill fetch+store. The
// backfill goroutine is decoupled from the caller's request context, so this
// is its only deadline.
const profileBackfillTimeout = 10 * time.Second

type userService struct {
	userRepo         UserRepository
	identityResolver identity.Resolver
	defaultPDS       string // Default PDS URL for this Coves instance (used when creating new local users via registration API)

	// instanceDomain is this instance's handle domain (e.g. coves.social), used
	// to reserve the "c-" community namespace against local registrations.
	// Empty disables the check. See validateLocalHandleNamespace.
	instanceDomain string

	// turnstile verifies Cloudflare Turnstile tokens during the signup-token
	// handshake. nil → RequestSignupToken returns ErrSignupTokenDisabled (503).
	turnstile TurnstileVerifier

	// pdsAdminPassword authenticates against the PDS admin API for minting
	// single-use invite codes. Paired with the literal admin username "admin"
	// (PDS convention). Empty → RequestSignupToken returns ErrSignupTokenDisabled.
	pdsAdminPassword string

	// pdsAdminClient is reused across calls so HTTP/1.1 keep-alive and TLS
	// session resumption actually kick in.
	pdsAdminClient *http.Client

	// profileBackfillClient, when non-nil, enables profile backfill on IndexUser:
	// a user indexed with no profile data gets their social.coves.actor.profile
	// record fetched directly from their PDS. This reconciles profile firehose
	// events that were missed (a profile is written once at rkey "self", so a
	// missed create event is never re-emitted). nil → backfill disabled.
	profileBackfillClient *http.Client
}

// UserServiceOption configures optional behavior on the user service.
type UserServiceOption func(*userService)

// WithProfileBackfill enables best-effort profile backfill during IndexUser (see
// profileBackfillClient). Pass nil to use a default client with a 10s timeout.
// The fetch+store runs in a detached goroutine so it never blocks IndexUser
// callers (OAuth login, Jetstream consumers); failures are logged only — run
// cmd/backfill-profiles to reconcile users whose backfill fetch failed.
func WithProfileBackfill(client *http.Client) UserServiceOption {
	return func(s *userService) {
		if client == nil {
			client = &http.Client{Timeout: 10 * time.Second}
		}
		s.profileBackfillClient = client
	}
}

// WithInstanceDomain supplies this instance's handle domain (e.g. coves.social)
// so local registrations can be held out of the reserved community namespace.
// Empty (the default) disables the reservation check.
func WithInstanceDomain(domain string) UserServiceOption {
	return func(s *userService) {
		s.instanceDomain = strings.ToLower(strings.TrimSpace(domain))
	}
}

// communityHandlePrefix mirrors the communities package's reserved prefix.
// Duplicated rather than imported to keep users from depending on communities;
// the two must stay in sync.
const communityHandlePrefix = "c-"

// validateLocalHandleNamespace rejects handles that squat the community
// namespace on THIS instance. Community actors are provisioned as
// c-{name}.{instanceDomain}, so a user holding c-gardening.coves.social could
// either block provisioning of the "gardening" community or hold the handle the
// AppView treats as that community's identity.
//
// Scoped deliberately to our own domain: a remote user legitimately named
// c-foo.example.com is in someone else's namespace and must still index fine.
func (s *userService) validateLocalHandleNamespace(handle string) error {
	if s.instanceDomain == "" {
		return nil
	}
	handle = strings.ToLower(strings.TrimSpace(handle))
	if !strings.HasSuffix(handle, "."+s.instanceDomain) {
		return nil
	}
	firstLabel, _, _ := strings.Cut(handle, ".")
	if strings.HasPrefix(firstLabel, communityHandlePrefix) {
		return &InvalidHandleError{
			Handle: handle,
			Reason: `handles starting with "c-" are reserved for communities on this instance`,
		}
	}
	return nil
}

// NewUserService creates a new user service.
// turnstile and pdsAdminPassword may be nil/empty when the signup-token endpoint
// is not enabled (e.g., integration tests that don't exercise bot protection); in
// that case RequestSignupToken returns ErrSignupTokenDisabled — surfaced as 503,
// distinct from a captcha rejection so misconfiguration is observable.
func NewUserService(
	userRepo UserRepository,
	identityResolver identity.Resolver,
	defaultPDS string,
	turnstile TurnstileVerifier,
	pdsAdminPassword string,
	opts ...UserServiceOption,
) UserService {
	s := &userService{
		userRepo:         userRepo,
		identityResolver: identityResolver,
		defaultPDS:       defaultPDS,
		turnstile:        turnstile,
		pdsAdminPassword: pdsAdminPassword,
		pdsAdminClient:   &http.Client{Timeout: pdsAdminCallTimeout},
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// CreateUser creates a new user in the AppView database
// This method is idempotent: if a user with the same DID already exists, it returns the existing user
func (s *userService) CreateUser(ctx context.Context, req CreateUserRequest) (*User, error) {
	if err := s.validateCreateRequest(req); err != nil {
		return nil, err
	}

	// Normalize handle
	req.Handle = strings.TrimSpace(strings.ToLower(req.Handle))
	req.DID = strings.TrimSpace(req.DID)
	req.PDSURL = strings.TrimSpace(req.PDSURL)

	user := &User{
		DID:    req.DID,
		Handle: req.Handle,
		PDSURL: req.PDSURL,
	}

	// Try to create the user
	createdUser, err := s.userRepo.Create(ctx, user)
	if err != nil {
		// If user with this DID already exists, fetch and return it (idempotent behavior)
		if errors.Is(err, ErrUserAlreadyExists) {
			existingUser, getErr := s.userRepo.GetByDID(ctx, req.DID)
			if getErr != nil {
				return nil, fmt.Errorf("user exists but failed to fetch: %w", getErr)
			}
			return existingUser, nil
		}
		// For other errors (validation, handle conflict, etc.), return the error
		return nil, err
	}

	return createdUser, nil
}

// GetUserByDID retrieves a user by their DID
func (s *userService) GetUserByDID(ctx context.Context, did string) (*User, error) {
	if strings.TrimSpace(did) == "" {
		return nil, fmt.Errorf("DID is required")
	}

	return s.userRepo.GetByDID(ctx, did)
}

// GetUserByHandle retrieves a user by their handle
func (s *userService) GetUserByHandle(ctx context.Context, handle string) (*User, error) {
	handle = strings.TrimSpace(strings.ToLower(handle))
	if handle == "" {
		return nil, fmt.Errorf("handle is required")
	}

	return s.userRepo.GetByHandle(ctx, handle)
}

// UpdateHandle updates the handle for a user with the given DID
func (s *userService) UpdateHandle(ctx context.Context, did, newHandle string) (*User, error) {
	did = strings.TrimSpace(did)
	newHandle = strings.TrimSpace(strings.ToLower(newHandle))

	if did == "" {
		return nil, fmt.Errorf("DID is required")
	}
	if newHandle == "" {
		return nil, fmt.Errorf("handle is required")
	}

	// Validate new handle format
	if err := validateHandle(newHandle); err != nil {
		return nil, err
	}
	if err := s.validateLocalHandleNamespace(newHandle); err != nil {
		return nil, err
	}

	return s.userRepo.UpdateHandle(ctx, did, newHandle)
}

// ResolveHandleToDID resolves a handle to a DID
// This is critical for login: users enter their handle, we resolve to DID
// First checks local database for indexed users (fast path), then falls back
// to external DNS TXT record lookup and HTTPS .well-known/atproto-did resolution
func (s *userService) ResolveHandleToDID(ctx context.Context, handle string) (string, error) {
	handle = strings.TrimSpace(strings.ToLower(handle))
	if handle == "" {
		return "", fmt.Errorf("handle is required")
	}

	// Fast path: check local database first for users we've already indexed
	// This avoids external network calls for known users
	user, err := s.userRepo.GetByHandle(ctx, handle)
	if err == nil && user != nil {
		return user.DID, nil
	}
	// Log database errors (but not "not found" which is expected for unindexed users)
	if err != nil && !errors.Is(err, ErrUserNotFound) {
		log.Printf("Warning: database error during handle lookup for %s (falling back to external resolution): %v", handle, err)
	}
	// If not found locally or error, fall through to external resolution

	// Slow path: use identity resolver for external DNS/HTTPS resolution
	did, _, err := s.identityResolver.ResolveHandle(ctx, handle)
	if err != nil {
		// Translate the resolver's typed errors into this package's vocabulary so
		// callers can tell "no such handle" from "resolution broke". Without this
		// the two are indistinguishable at the API boundary and a nonexistent
		// handle — the single most common failure of a public profile lookup —
		// reads as a server fault.
		var notFound *identity.ErrNotFound
		var invalidIdentifier *identity.ErrInvalidIdentifier
		switch {
		case errors.As(err, &notFound):
			return "", fmt.Errorf("%w: %w", ErrUserNotFound, err)
		case errors.As(err, &invalidIdentifier):
			return "", &InvalidHandleError{Handle: handle, Reason: invalidIdentifier.Reason}
		}
		return "", fmt.Errorf("failed to resolve handle %s: %w", handle, err)
	}

	return did, nil
}

// RegisterAccount creates a new account on the PDS via XRPC
// This is what a UI signup button would call - it handles the PDS account creation
func (s *userService) RegisterAccount(ctx context.Context, req RegisterAccountRequest) (*RegisterAccountResponse, error) {
	if err := s.validateRegisterRequest(req); err != nil {
		return nil, err
	}

	// Call PDS com.atproto.server.createAccount XRPC endpoint
	pdsURL := strings.TrimSuffix(s.defaultPDS, "/")
	endpoint := fmt.Sprintf("%s/xrpc/com.atproto.server.createAccount", pdsURL)

	payload := map[string]string{
		"handle":   req.Handle,
		"email":    req.Email,
		"password": req.Password,
	}
	if req.InviteCode != "" {
		payload["inviteCode"] = req.InviteCode
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	// Set timeout to prevent hanging on slow/unavailable PDS
	client := &http.Client{
		Timeout: 10 * time.Second,
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to call PDS: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			log.Printf("Failed to close response body: %v", closeErr)
		}
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("PDS returned status %d: %s", resp.StatusCode, string(body))
	}

	var pdsResp RegisterAccountResponse
	if err := json.Unmarshal(body, &pdsResp); err != nil {
		return nil, fmt.Errorf("failed to parse PDS response: %w", err)
	}

	// Set the PDS URL in the response (PDS doesn't return this)
	pdsResp.PDSURL = s.defaultPDS

	// Index the new user in local database so they're immediately available for profile lookups
	// This is idempotent - safe to call even if user somehow already exists
	if indexErr := s.IndexUser(ctx, pdsResp.DID, pdsResp.Handle, s.defaultPDS); indexErr != nil {
		// Log but don't fail - the account was created successfully on PDS
		// They'll be indexed on first OAuth login if this fails
		log.Printf("Warning: failed to index new user after signup (DID: %s): %v", pdsResp.DID, indexErr)
	}

	return &pdsResp, nil
}

// RequestSignupToken verifies a Cloudflare Turnstile token and, on success, mints
// a single-use PDS invite code. This endpoint does NOT validate handle/email —
// the actual signup endpoint (social.coves.actor.signup) is the authoritative
// place for those checks (uniqueness, lexicon constraints, PDS rules). This
// keeps the captcha gate honest: one job, one round-trip.
//
// Returns ErrSignupTokenDisabled when the endpoint is unconfigured (missing
// Turnstile secret or PDS admin password) — distinct from a captcha rejection so
// operators see misconfiguration as a 503, not a sea of spurious 403s.
func (s *userService) RequestSignupToken(ctx context.Context, req RequestSignupTokenRequest) (*RequestSignupTokenResponse, error) {
	if s.turnstile == nil || s.pdsAdminPassword == "" {
		return nil, ErrSignupTokenDisabled
	}

	if err := s.turnstile.Verify(ctx, req.TurnstileToken, req.RemoteIP); err != nil {
		return nil, err
	}

	code, err := s.mintInviteCode(ctx)
	if err != nil {
		return nil, err
	}

	return &RequestSignupTokenResponse{InviteCode: code}, nil
}

// mintInviteCode calls the PDS admin createInviteCode XRPC. The literal
// admin username "admin" matches the PDS convention; the password comes from
// PDS_ADMIN_PASSWORD. useCount: 1 enforces single-use at the PDS — the invite
// is consumed by the first signup that presents it, and any replay is rejected
// by the PDS as already-used.
func (s *userService) mintInviteCode(ctx context.Context) (string, error) {
	pdsURL := strings.TrimSuffix(s.defaultPDS, "/")
	endpoint := fmt.Sprintf("%s/xrpc/com.atproto.server.createInviteCode", pdsURL)

	body, err := json.Marshal(map[string]int{"useCount": 1})
	if err != nil {
		// In practice unreachable — fixed-shape struct — but wrap with the same
		// sentinel so any "couldn't talk to PDS" path is uniformly observable.
		// Double-%w preserves both the sentinel and the underlying cause for
		// errors.Is/As — collapsing the cause with %v would hide transport
		// errors from callers that switch on them.
		return "", fmt.Errorf("%w: failed to marshal invite request: %w", ErrPDSAdminUnavailable, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewBuffer(body))
	if err != nil {
		return "", fmt.Errorf("%w: failed to create invite request: %w", ErrPDSAdminUnavailable, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// Stdlib idiom: matches the PDS basic-auth convention (username literal "admin").
	httpReq.SetBasicAuth("admin", s.pdsAdminPassword)

	resp, err := s.pdsAdminClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("%w: failed to call PDS admin API: %w", ErrPDSAdminUnavailable, err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			slog.Warn("mintInviteCode: failed to close response body", slog.String("error", closeErr.Error()))
		}
	}()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("%w: failed to read PDS admin response: %w", ErrPDSAdminUnavailable, err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", NewInviteMintError(resp.StatusCode, string(respBody))
	}

	var parsed struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("%w: failed to parse invite response: %w", ErrPDSAdminUnavailable, err)
	}
	if parsed.Code == "" {
		return "", NewInviteMintError(resp.StatusCode, "empty code in response")
	}
	return parsed.Code, nil
}

// IndexUser creates or updates a user in the local database.
// This is idempotent and safe to call multiple times for the same user.
// If the user exists, their handle is updated if it changed.
// When profile backfill is enabled (WithProfileBackfill) and the indexed user has no
// profile data, their profile record is fetched from their PDS asynchronously and
// best-effort — this heals users whose profile firehose event was never delivered
// without ever blocking or failing the IndexUser call itself.
func (s *userService) IndexUser(ctx context.Context, did, handle, pdsURL string) error {
	// THE ERASURE GATE, and IndexUser is where it belongs because this is the
	// FIREHOSE's door into the users table. A DID appearing in a profile or
	// identity event means only that some repo somewhere emitted a record — a
	// bridge, a replay, an overlapping feed — and any of those can arrive months
	// after the account was erased. Letting it through would make the erasure
	// undone by exactly the replays the marker exists to defend against, and
	// undone silently: the users row reappears, repo.Create clears the marker on
	// its way past, and the next replayed post indexes normally.
	//
	// The marker's only exit is a genuine re-registration, which reaches the
	// repository's insert directly rather than through here.
	//
	// A LOOKUP FAILURE REFUSES. "I could not read the marker table" and "there
	// is no marker" must never be the same answer, because the second one
	// indexes.
	if lookup, ok := s.userRepo.(ErasureLookup); ok {
		erased, err := lookup.IsAccountDeleted(ctx, did)
		if err != nil {
			return fmt.Errorf("checking the erasure marker for %s before indexing: %w", did, err)
		}
		if erased {
			// Nil, not an error. Every caller is a firehose consumer, and the
			// connector dead-letters what a handler returns — so refusing with
			// an error would turn each erased account into a permanent stream of
			// redriving profile events. This is not a failure; it is an event
			// with nothing to do.
			log.Printf("INFO: not indexing %s from the firehose: the account was erased (migration 036 marker)", did)
			return nil
		}
	}

	// Try to create the user (idempotent - CreateUser returns existing user if DID exists)
	user, err := s.CreateUser(ctx, CreateUserRequest{
		DID:    did,
		Handle: handle,
		PDSURL: pdsURL,
	})

	if err != nil {
		// Check if it's a handle conflict (user exists with different handle)
		// In this case, update the handle instead
		if !errors.Is(err, ErrHandleAlreadyTaken) {
			return err
		}
		// User exists but handle changed - update it
		user, err = s.UpdateHandle(ctx, did, handle)
		if err != nil {
			return fmt.Errorf("failed to update handle for existing user: %w", err)
		}
	}

	// Best-effort and asynchronous: never fails or blocks indexing (a dead or
	// slow PDS must not stall a login callback or firehose event, and must not
	// dead-letter the post/comment event that triggered this index).
	s.maybeBackfillProfile(ctx, user)

	return nil
}

// maybeBackfillProfile reconciles a missing profile for a freshly indexed user. A
// profile record fires exactly one firehose event (rkey "self" is written once); if
// that event was missed — e.g. relay throttling — nothing ever replays it, so the
// AppView must reconcile by reading the record directly from the user's own PDS.
// Only users with a completely empty profile are touched: for anyone else the
// firehose is the source of truth and a fetch could race a newer update.
//
// The emptiness check runs synchronously (the common no-op paths spawn nothing);
// when a fetch is actually needed, the fetch+store runs in a detached goroutine —
// decoupled from the caller's context via context.WithoutCancel with its own
// profileBackfillTimeout deadline — so a slow or dead PDS never blocks the
// IndexUser caller (OAuth login callback, Jetstream consumers) and the fetch is
// not killed when the triggering request context ends.
func (s *userService) maybeBackfillProfile(ctx context.Context, user *User) {
	if s.profileBackfillClient == nil || user == nil {
		return
	}
	if user.DisplayName != "" || user.Bio != "" || user.AvatarCID != "" || user.BannerCID != "" {
		return // profile already indexed — firehose keeps it current
	}

	go s.backfillProfile(context.WithoutCancel(ctx), user.DID, user.PDSURL)
}

// backfillProfile is the detached best-effort fetch+store behind
// maybeBackfillProfile. Failures are logged only — for firehose-discovered users
// there is no automatic retry; run cmd/backfill-profiles to reconcile.
func (s *userService) backfillProfile(ctx context.Context, did, pdsURL string) {
	ctx, cancel := context.WithTimeout(ctx, profileBackfillTimeout)
	defer cancel()

	input, err := FetchProfileRecord(ctx, s.profileBackfillClient, pdsURL, did)
	if err != nil {
		slog.Warn("profile backfill: failed to fetch profile record (run cmd/backfill-profiles to reconcile)",
			slog.String("did", did),
			slog.String("pds_url", pdsURL),
			slog.String("error", err.Error()))
		return
	}
	if input == nil {
		return // user has no profile record — nothing to apply
	}

	// Re-check emptiness before writing: a concurrent firehose profile event may
	// have landed while we were fetching, and the firehose is the source of truth.
	current, err := s.userRepo.GetByDID(ctx, did)
	if err != nil {
		slog.Warn("profile backfill: failed to re-check user before storing fetched profile",
			slog.String("did", did),
			slog.String("error", err.Error()))
		return
	}
	if current.DisplayName != "" || current.Bio != "" || current.AvatarCID != "" || current.BannerCID != "" {
		return // firehose delivered a profile while we were fetching — keep it
	}

	if _, err := s.userRepo.UpdateProfile(ctx, did, *input); err != nil {
		slog.Warn("profile backfill: failed to store fetched profile",
			slog.String("did", did),
			slog.String("error", err.Error()))
		return
	}

	slog.Info("profile backfill: hydrated profile from PDS",
		slog.String("did", did),
		slog.String("pds_url", pdsURL))
}

// GetProfile retrieves a user's full profile with aggregated statistics.
// Returns a ProfileViewDetailed matching the social.coves.actor.defs#profileViewDetailed lexicon.
// Avatar and Banner CIDs are transformed to URLs using the user's PDS URL.
func (s *userService) GetProfile(ctx context.Context, did string) (*ProfileViewDetailed, error) {
	did = strings.TrimSpace(did)
	if did == "" {
		return nil, fmt.Errorf("DID is required")
	}

	// Get the user first
	user, err := s.userRepo.GetByDID(ctx, did)
	if err != nil {
		return nil, fmt.Errorf("failed to get user: %w", err)
	}

	// Get aggregated stats
	stats, err := s.userRepo.GetProfileStats(ctx, did)
	if err != nil {
		return nil, fmt.Errorf("failed to get profile stats: %w", err)
	}

	profile := &ProfileViewDetailed{
		DID:         user.DID,
		Handle:      user.Handle,
		CreatedAt:   user.CreatedAt,
		Stats:       stats,
		DisplayName: user.DisplayName,
		Bio:         user.Bio,
	}

	// Transform avatar/banner CIDs to URLs using image proxy config.
	// The 'avatar' preset is the full-size rendering used by the profile detail
	// view; feeds and comment threads use 'avatar_small'. Dimensions live in
	// internal/core/imageproxy's preset registry, not here — they have already
	// changed once since this comment was written.
	config := blobs.GetImageURLConfig()
	profile.Avatar = blobs.HydrateImageURL(config, user.PDSURL, user.DID, user.AvatarCID, "avatar")
	profile.Banner = blobs.HydrateImageURL(config, user.PDSURL, user.DID, user.BannerCID, "banner")

	return profile, nil
}

// UpdateProfile updates a user's profile fields (display name, bio, avatar, banner).
// Nil values in the input mean "don't change this field" - only non-nil values are updated.
// Empty string values will clear the field in the database.
// Returns the updated user with all fields populated.
// Returns ErrUserNotFound if the user does not exist.
func (s *userService) UpdateProfile(ctx context.Context, did string, input UpdateProfileInput) (*User, error) {
	did = strings.TrimSpace(did)
	if did == "" {
		return nil, fmt.Errorf("DID is required")
	}

	return s.userRepo.UpdateProfile(ctx, did, input)
}

func (s *userService) validateCreateRequest(req CreateUserRequest) error {
	if strings.TrimSpace(req.DID) == "" {
		return fmt.Errorf("DID is required")
	}

	if strings.TrimSpace(req.Handle) == "" {
		return fmt.Errorf("handle is required")
	}

	if strings.TrimSpace(req.PDSURL) == "" {
		return fmt.Errorf("PDS URL is required")
	}

	// DID format validation
	if !strings.HasPrefix(req.DID, "did:") {
		return fmt.Errorf("invalid DID format: must start with 'did:'")
	}

	// Validate handle format
	if err := validateHandle(req.Handle); err != nil {
		return err
	}

	return nil
}

func (s *userService) validateRegisterRequest(req RegisterAccountRequest) error {
	if strings.TrimSpace(req.Handle) == "" {
		return fmt.Errorf("handle is required")
	}

	if strings.TrimSpace(req.Email) == "" {
		return &InvalidEmailError{Email: req.Email}
	}

	// Basic email validation
	if !strings.Contains(req.Email, "@") || !strings.Contains(req.Email, ".") {
		return &InvalidEmailError{Email: req.Email}
	}

	// Password validation
	if strings.TrimSpace(req.Password) == "" {
		return &WeakPasswordError{Reason: "password is required"}
	}

	if len(req.Password) < minPasswordLength {
		return &WeakPasswordError{Reason: fmt.Sprintf("password must be at least %d characters", minPasswordLength)}
	}

	// Validate handle format
	if err := validateHandle(req.Handle); err != nil {
		return err
	}
	// Registration creates an actor on THIS instance, so it must not squat the
	// community namespace. Deliberately not applied to user indexing, which
	// ingests remote actors whose namespaces are not ours to police.
	if err := s.validateLocalHandleNamespace(req.Handle); err != nil {
		return err
	}

	return nil
}

// validateHandle validates handle per atProto spec: https://atproto.com/specs/handle
func validateHandle(handle string) error {
	// Normalize to lowercase (handles are case-insensitive)
	handle = strings.TrimSpace(strings.ToLower(handle))

	if handle == "" {
		return &InvalidHandleError{Handle: handle, Reason: "handle cannot be empty"}
	}

	// Check length
	if len(handle) > maxHandleLength {
		return &InvalidHandleError{Handle: handle, Reason: fmt.Sprintf("handle exceeds maximum length of %d characters", maxHandleLength)}
	}

	// Check regex pattern
	if !handleRegex.MatchString(handle) {
		return &InvalidHandleError{Handle: handle, Reason: "handle must be domain-like (e.g., user.bsky.social), with segments of alphanumeric/hyphens separated by dots"}
	}

	// Check for disallowed TLDs
	for tld := range disallowedTLDs {
		if strings.HasSuffix(handle, tld) {
			return &InvalidHandleError{Handle: handle, Reason: fmt.Sprintf("TLD %s is not allowed", tld)}
		}
	}

	return nil
}

// DeleteAccount removes a user and all associated data from the Coves AppView.
// This ONLY deletes AppView indexed data, NOT the user's atProto identity on their PDS.
// The user's identity remains intact for use with other atProto apps.
func (s *userService) DeleteAccount(ctx context.Context, did string) error {
	did = strings.TrimSpace(did)
	if did == "" {
		return &InvalidDIDError{DID: did, Reason: "DID is required"}
	}

	// Validate DID format
	if !strings.HasPrefix(did, "did:") {
		return &InvalidDIDError{DID: did, Reason: "must start with 'did:'"}
	}

	// Get user handle for audit log (before deletion)
	// We fetch the user first to include handle in the audit log
	user, err := s.userRepo.GetByDID(ctx, did)
	if err != nil {
		// If user not found, return that error
		if errors.Is(err, ErrUserNotFound) {
			return ErrUserNotFound
		}
		return fmt.Errorf("failed to get user for deletion: %w", err)
	}

	// Perform the deletion
	if err := s.userRepo.Delete(ctx, did); err != nil {
		// Log failed deletion attempt
		slog.Error("account deletion failed",
			slog.String("did", did),
			slog.String("handle", user.Handle),
			slog.String("error", err.Error()),
		)
		return fmt.Errorf("failed to delete account: %w", err)
	}

	// Log successful deletion for audit trail
	// SECURITY: Only log DID and handle (non-sensitive identifiers), never tokens
	slog.Info("account deleted successfully",
		slog.String("did", did),
		slog.String("handle", user.Handle),
		slog.Time("deleted_at", time.Now().UTC()),
	)

	return nil
}
