package jetstream

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"Coves/internal/atproto/identity"
	"Coves/internal/atproto/utils"
	"Coves/internal/core/userblocks"
	"Coves/internal/core/users"
)

// CovesProfileCollection is the atProto collection for Coves user profiles.
// NOTE: Alias of users.ProfileCollection, the canonical definition — kept as an
// exported constant of this package because existing callers reference it here.
const CovesProfileCollection = users.ProfileCollection

// CovesActorBlockCollection is the atProto collection for user-to-user blocks.
// Records live in the blocker's repository at at://blocker_did/social.coves.actor.block/{tid}
const CovesActorBlockCollection = "social.coves.actor.block"

// SessionHandleUpdater is an interface for updating OAuth session handles
// when identity changes occur. This keeps active sessions in sync with
// the user's current handle.
type SessionHandleUpdater interface {
	UpdateHandleByDID(ctx context.Context, did, newHandle string) (int64, error)
}

// JetstreamEvent represents an event from the Jetstream firehose
// Jetstream documentation: https://docs.bsky.app/docs/advanced-guides/jetstream
type JetstreamEvent struct {
	Account  *AccountEvent  `json:"account,omitempty"`
	Identity *IdentityEvent `json:"identity,omitempty"`
	Commit   *CommitEvent   `json:"commit,omitempty"`
	Did      string         `json:"did"`
	Kind     string         `json:"kind"`
	TimeUS   int64          `json:"time_us"`
}

type AccountEvent struct {
	Did    string `json:"did"`
	Time   string `json:"time"`
	Seq    int64  `json:"seq"`
	Active bool   `json:"active"`
}

type IdentityEvent struct {
	Did    string `json:"did"`
	Handle string `json:"handle"`
	Time   string `json:"time"`
	Seq    int64  `json:"seq"`
}

// CommitEvent represents a record commit from Jetstream
type CommitEvent struct {
	Rev        string                 `json:"rev"`
	Operation  string                 `json:"operation"` // "create", "update", "delete"
	Collection string                 `json:"collection"`
	RKey       string                 `json:"rkey"`
	Record     map[string]interface{} `json:"record,omitempty"`
	CID        string                 `json:"cid,omitempty"`
}

// UserEventConsumer consumes user-related events from Jetstream.
// Connection management (WebSocket, cursor, retries, dead letters) lives in
// Connector; this type only implements EventHandler.
type UserEventConsumer struct {
	userService              users.UserService
	identityResolver         identity.Resolver
	uncachedIdentityResolver identity.UncachedResolver
	identityRefreshFailures  *identityRefreshFailureDebounce
	identityRefreshLock      IdentityRefreshLock   // Required for identity refreshes; serializes refreshes of one DID
	sessionHandleUpdater     SessionHandleUpdater  // Optional: updates OAuth sessions on handle change
	userBlockRepo            userblocks.Repository // Optional: indexes user-to-user blocks
	bridgeTrust              *BridgeTrust          // Optional: admits new identities hosted by trusted bridge PDSes
	revGate                  *RevGate              // Optional: cross-feed ordering guard for commit events (nil = ungated)
}

// ConsumerOption is a functional option for configuring UserEventConsumer
type ConsumerOption func(*UserEventConsumer)

// WithSessionHandleUpdater sets the session handle updater for syncing OAuth sessions
// when identity changes occur. If not set, OAuth sessions won't be updated on handle changes.
func WithSessionHandleUpdater(updater SessionHandleUpdater) ConsumerOption {
	return func(c *UserEventConsumer) {
		c.sessionHandleUpdater = updater
	}
}

// WithUserBlockRepo sets the user block repository for indexing user-to-user blocks
// from the Jetstream firehose. If not set, block events will be ignored.
func WithUserBlockRepo(repo userblocks.Repository) ConsumerOption {
	return func(c *UserEventConsumer) {
		c.userBlockRepo = repo
	}
}

// WithUserBridgeTrust enables discovery of bridged profiles. Unknown native
// users remain ignored, preserving the consumer's avoid-indexing-the-world
// policy.
func WithUserBridgeTrust(bt *BridgeTrust) ConsumerOption {
	return func(c *UserEventConsumer) {
		c.bridgeTrust = bt
	}
}

// WithUserRevGate installs the per-record rev gate (see rev_gate.go) so profile
// and block commits are applied in repo commit order even when the same repo is
// carried by multiple Jetstream feeds. Without it, commit events are ungated.
func WithUserRevGate(gate *RevGate) ConsumerOption {
	return func(c *UserEventConsumer) {
		c.revGate = gate
	}
}

// WithUserUncachedIdentityResolver sets the resolver for fresh identity-event
// lookups and for the document-first profile origin admission.
func WithUserUncachedIdentityResolver(resolver identity.UncachedResolver) ConsumerOption {
	return func(c *UserEventConsumer) {
		c.uncachedIdentityResolver = resolver
	}
}

// WithUserIdentityRefreshFailureTTL sets how long an identity-event refresh
// that hit identityResolveTimeout suppresses further resolutions of the same DID. Pass
// IDENTITY_NEGATIVE_CACHE_TTL: config keeps it below REDRIVE_INTERVAL, so each
// redrive reaches the network. Defaults to identity.DefaultNegativeCacheTTL.
func WithUserIdentityRefreshFailureTTL(ttl time.Duration) ConsumerOption {
	return func(c *UserEventConsumer) {
		c.identityRefreshFailures.ttl = ttl
	}
}

// NewUserEventConsumer creates a new Jetstream consumer for user events
func NewUserEventConsumer(userService users.UserService, identityResolver identity.Resolver, opts ...ConsumerOption) *UserEventConsumer {
	c := &UserEventConsumer{
		userService:             userService,
		identityResolver:        identityResolver,
		identityRefreshFailures: newIdentityRefreshFailureDebounce(identity.DefaultNegativeCacheTTL),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// RevGated reports whether this consumer applies the per-record rev gate (true when a
// gate was injected via WithUserRevGate). main.go checks this at boot to refuse
// multi-feed operation with an ungated consumer.
func (c *UserEventConsumer) RevGated() bool { return c.revGate != nil }

// HandleEvent implements EventHandler; it is invoked by the Connector for live
// events and by the DeadLetterRedriver for replays, so it must stay idempotent.
// The two callers may invoke the same consumer instance concurrently, so it must
// also hold no unguarded mutable in-memory state (the identity refresh failure
// debounce is mutex-guarded; all other state lives in the DB).
func (c *UserEventConsumer) HandleEvent(ctx context.Context, event *JetstreamEvent) error {
	switch event.Kind {
	case "identity":
		return c.handleIdentityEvent(ctx, event)
	case "account":
		return c.handleAccountEvent(ctx, event)
	case "commit":
		return c.handleCommitEvent(ctx, event)
	default:
		return nil
	}
}

// Deprecated: HandleIdentityEventPublic is superseded by HandleEvent which routes
// all event kinds. Use HandleEvent for new code; this remains for existing tests.
func (c *UserEventConsumer) HandleIdentityEventPublic(ctx context.Context, event *JetstreamEvent) error {
	return c.handleIdentityEvent(ctx, event)
}

// handleIdentityEvent processes identity events (handle changes)
// NOTE: This only UPDATES existing users - it does NOT create new users.
// Users are created during OAuth login or signup, not from Jetstream events.
// This prevents indexing millions of Bluesky users who never interact with Coves.
func (c *UserEventConsumer) handleIdentityEvent(ctx context.Context, event *JetstreamEvent) error {
	if event.Identity == nil {
		return fmt.Errorf("%w: identity event missing identity data", ErrPermanentEvent)
	}

	did := event.Identity.Did
	if did == "" {
		return fmt.Errorf("%w: identity event missing did", ErrPermanentEvent)
	}

	// Only filters unknown DIDs. The handle compared against is read again
	// inside the refresh lock, since another refresh may change it meanwhile.
	if _, err := c.userService.GetUserByDID(ctx, did); err != nil {
		if errors.Is(err, users.ErrUserNotFound) {
			return nil
		}
		return fmt.Errorf("failed to check if user exists: %w", err)
	}

	refresh, err := c.RefreshUserHandle(ctx, did)
	if err != nil {
		return err
	}
	if !refresh.Changed {
		log.Printf("Handle unchanged for %s (%s)", refresh.VerifiedHandle, did)
		return nil
	}
	log.Printf("Handle changed: %s → %s (DID: %s)", refresh.StoredHandle, refresh.VerifiedHandle, did)
	return nil
}

// RefreshUserHandle runs the locked handle refresh an identity event runs for
// an indexed user: RefreshIdentityHandle with the stored handle re-read inside
// the lock and written through the user service, then, on a change, the new
// handle carried into the user's OAuth sessions. A failed session update is
// logged and does not fail the refresh. The error chain keeps its cause:
// ErrIdentityResolveFailed, identity.ErrHandleUnverified,
// identity.ErrIdentitySubjectMismatch, ErrVerifiedHandleInvalid,
// users.ErrHandleAlreadyTaken or ErrIdentityRefreshBusy, each under the
// connector class the identity-event path uses.
func (c *UserEventConsumer) RefreshUserHandle(ctx context.Context, did string) (IdentityHandleRefresh, error) {
	var refresh IdentityHandleRefresh
	verifiedHandle, changed, err := RefreshIdentityHandle(ctx, did, c.identityRefreshLock,
		c.identityResolver, c.uncachedIdentityResolver, c.identityRefreshFailures,
		func(ctx context.Context) (string, error) {
			user, readErr := c.userService.GetUserByDID(ctx, did)
			if readErr != nil {
				return "", fmt.Errorf("failed to read stored user: %w", readErr)
			}
			refresh.StoredHandle = user.Handle
			return user.Handle, nil
		},
		func(ctx context.Context, handle string) error {
			_, updateErr := c.userService.UpdateHandle(ctx, did, handle)
			if updateErr != nil {
				var invalidHandle *users.InvalidHandleError
				switch {
				case errors.As(updateErr, &invalidHandle):
					return markRefreshError(fmt.Errorf("%w: failed to update handle: %w", ErrPermanentEvent, updateErr),
						ErrVerifiedHandleInvalid)
				case errors.Is(updateErr, users.ErrHandleAlreadyTaken):
					return fmt.Errorf("%w: failed to update handle: %w", ErrUnresolvedReference, updateErr)
				}
				return updateErr
			}
			return nil
		})
	refresh.VerifiedHandle = verifiedHandle
	refresh.Changed = changed
	if err != nil || !changed {
		return refresh, err
	}

	if c.sessionHandleUpdater != nil {
		if sessionsUpdated, updateErr := c.sessionHandleUpdater.UpdateHandleByDID(ctx, did, verifiedHandle); updateErr != nil {
			slog.Error("failed to update OAuth session handles (users may see stale handle)",
				slog.String("did", did),
				slog.String("new_handle", verifiedHandle),
				slog.String("error", updateErr.Error()))
		} else if sessionsUpdated > 0 {
			log.Printf("Updated %d OAuth session(s) with new handle: %s", sessionsUpdated, verifiedHandle)
		}
	}
	return refresh, nil
}

// handleAccountEvent processes account events (account creation/updates)
func (c *UserEventConsumer) handleAccountEvent(ctx context.Context, event *JetstreamEvent) error {
	if event.Account == nil {
		// PERMANENT: structurally invalid event — replays fail identically.
		return fmt.Errorf("%w: account event missing account data", ErrPermanentEvent)
	}

	did := event.Account.Did
	if did == "" {
		return fmt.Errorf("%w: account event missing did", ErrPermanentEvent)
	}

	// Account events don't include handle, so we skip them.
	// Users are indexed via OAuth login or signup, not from account events.
	return nil
}

// handleCommitEvent processes commit events for user-related collections.
// Routes to appropriate handler based on collection:
//   - social.coves.actor.profile: Profile updates for known users, plus discovery
//     of unknown identities hosted by an explicitly trusted bridge PDS
//   - social.coves.actor.block: User-to-user block create/delete events
func (c *UserEventConsumer) handleCommitEvent(ctx context.Context, event *JetstreamEvent) error {
	if event.Commit == nil {
		slog.Warn("received nil commit in handleCommitEvent (malformed event)", slog.String("did", event.Did))
		return nil
	}

	switch event.Commit.Collection {
	case CovesProfileCollection:
		return c.handleProfileCommit(ctx, event)
	case CovesActorBlockCollection:
		return c.handleUserBlock(ctx, event.Did, event.Commit)
	default:
		return nil
	}
}

// handleProfileCommit processes profile commit events. Native profiles still update
// only existing users. A previously unknown identity may be indexed when DID
// resolution proves it is hosted by an explicitly trusted bridge PDS; this is how
// virtual bridge users enter the AppView without opening the door to indexing every
// profile on the network.
func (c *UserEventConsumer) handleProfileCommit(ctx context.Context, event *JetstreamEvent) error {
	// Profile handling requires userService
	if c.userService == nil {
		return nil
	}

	// Only process users who exist in our database, except trusted bridge
	// profiles whose first event is the profile itself.
	var discoveredIdentity *identity.Identity
	existingUser, err := c.userService.GetUserByDID(ctx, event.Did)
	if err != nil {
		if errors.Is(err, users.ErrUserNotFound) {
			existingUser = nil // freshly discovered identity: recency guard below must not apply
			if event.Commit.Operation != "create" && event.Commit.Operation != "update" {
				return nil
			}
			_, carriesOrigin := userProfileOriginFromRecord(event.Commit.Record)
			var resolved *identity.Identity
			var resolveErr error
			if carriesOrigin {
				resolved, resolveErr = c.resolveFreshUserProfileIdentity(ctx, event.Did)
				if errors.Is(resolveErr, errProfileOriginNotAdmitted) {
					return nil
				}
			} else {
				if c.identityResolver == nil {
					return nil
				}
				resolveCtx, cancel := context.WithTimeout(ctx, identityResolveTimeout)
				defer cancel()
				resolved, resolveErr = c.identityResolver.Resolve(resolveCtx, event.Did)
			}
			if resolveErr != nil {
				if carriesOrigin {
					return resolveErr
				}
				return fmt.Errorf("%w: failed to resolve unknown profile identity %s: %w", ErrUnresolvedReference, event.Did, resolveErr)
			}
			if resolved == nil || resolved.DID != event.Did || !c.bridgeTrust.TrustsPDS(resolved.PDSURL) {
				return nil
			}
			if indexErr := c.userService.IndexUser(ctx, resolved.DID, resolved.Handle, resolved.PDSURL); indexErr != nil {
				return fmt.Errorf("failed to index trusted bridge profile identity %s: %w", event.Did, indexErr)
			}
			discoveredIdentity = resolved
		} else {
			// Database error - propagate so the connector can report it.
			return fmt.Errorf("failed to check if user exists: %w", err)
		}
	}

	// REV GATE: the exact cross-feed ordering guard. rev is the repo's own
	// monotonic commit TID, so unlike the wall-clock guard below it orders the
	// same repo's events correctly across feeds with hours of skew. Checked
	// before the write and advanced after it (check→write→advance; the write is
	// idempotent, so a crash in between replays safely).
	uri := commitRecordURI(event.Did, event.Commit)
	stale, err := c.revGate.IsStale(ctx, uri, event.Commit.Rev)
	if err != nil {
		return err
	}
	if stale {
		logSkippedStaleRev(ConsumerUsers, event.Commit.Operation, uri, event.Commit.Rev)
		return nil
	}

	// RECENCY GUARD: a redriven (DeadLetterRedriver) or rewound profile event can
	// arrive AFTER a newer profile write was already applied; applying it would
	// silently revert the newer profile. users.updated_at is bumped by every
	// UpdateProfile/UpdateHandle, so an event older than the row's last successful
	// write is skipped. Skipping is SUCCESS — the newer state wins.
	//
	// CAVEAT (unlike posts/comments, which store the Jetstream event time as their
	// watermark): updated_at is AppView wall clock, so this compares across clock
	// domains. That is safe for redrives (replayed minutes after the newer write)
	// and for human-paced profile edits; only two writes for the same user landing
	// within the ingest lag could be spuriously skipped, and the next profile edit
	// self-heals. The rev gate above is exact for events that carry a rev; this
	// guard remains for rev-less events (old dead letters, synthetic tests).
	if evTime, ok := eventTime(event.TimeUS); ok &&
		existingUser != nil && !existingUser.UpdatedAt.IsZero() && !existingUser.UpdatedAt.Before(evTime) {
		log.Printf("INFO: skipping stale profile event for %s (event time %s <= last user write %s; newer state already applied)",
			event.Did, evTime.Format(time.RFC3339Nano), existingUser.UpdatedAt.Format(time.RFC3339Nano))
		return nil
	}

	var opErr error
	switch event.Commit.Operation {
	case "create", "update":
		storedPDSURL := ""
		if existingUser != nil {
			storedPDSURL = existingUser.PDSURL
		} else if discoveredIdentity != nil {
			storedPDSURL = discoveredIdentity.PDSURL
		}
		opErr = c.handleProfileUpdate(ctx, event.Did, event.Commit, storedPDSURL, discoveredIdentity)
	case "delete":
		opErr = c.handleProfileDelete(ctx, event.Did)
	default:
		return nil
	}
	if opErr != nil {
		return opErr
	}
	return c.revGate.Advance(ctx, uri, event.Commit.Rev)
}

// handleProfileUpdate processes profile create/update operations
// Extracts displayName, description, avatar, banner, and origin from the record.
// discoveredIdentity is already freshly verified when discovery indexed this DID.
func (c *UserEventConsumer) handleProfileUpdate(ctx context.Context, did string, commit *CommitEvent, storedPDSURL string, discoveredIdentity *identity.Identity) error {
	if commit.Record == nil {
		slog.Warn("received nil record in profile commit (profile update silently dropped)",
			slog.String("did", did),
			slog.String("operation", commit.Operation))
		return nil
	}

	input := users.UpdateProfileInput{}

	// Extract displayName
	if dn, ok := commit.Record["displayName"].(string); ok {
		input.DisplayName = &dn
	}

	// Extract description
	if desc, ok := commit.Record["description"].(string); ok {
		input.Description = &desc
	}

	// Extract avatar CID from blob ref structure
	if avatarMap, ok := commit.Record["avatar"].(map[string]interface{}); ok {
		if cid, ok := extractBlobCID(avatarMap); ok {
			input.AvatarCID = &cid
		}
	}

	// Extract banner CID from blob ref structure
	if bannerMap, ok := commit.Record["banner"].(map[string]interface{}); ok {
		if cid, ok := extractBlobCID(bannerMap); ok {
			input.BannerCID = &cid
		}
	}
	rawOrigin, carriesOrigin := userProfileOriginFromRecord(commit.Record)
	origin, err := c.admitUserProfileOrigin(ctx, did, storedPDSURL, rawOrigin, carriesOrigin, discoveredIdentity)
	if err != nil {
		return err
	}
	input.Origin = &origin.Origin
	input.OriginUsername = &origin.OriginUsername
	input.OriginURL = &origin.OriginURL
	input.OriginSoftware = &origin.OriginSoftware

	_, err = c.userService.UpdateProfile(ctx, did, input)
	if err != nil {
		return fmt.Errorf("failed to update user profile: %w", err)
	}

	log.Printf("Updated profile for user %s", did)
	return nil
}

// userProfileOriginFromRecord distinguishes absent fields from present empty or
// malformed values: any asserted origin field requires fresh provenance checking.
func userProfileOriginFromRecord(record map[string]interface{}) (userProfileOrigin, bool) {
	var raw userProfileOrigin
	fields := []struct {
		key   string
		value *string
	}{
		{"origin", &raw.Origin},
		{"originUsername", &raw.OriginUsername},
		{"originUrl", &raw.OriginURL},
		{"originSoftware", &raw.OriginSoftware},
	}
	var present bool
	for _, field := range fields {
		value, exists := record[field.key]
		if exists {
			present = true
			*field.value, _ = value.(string)
		}
	}
	return raw, present
}

// errProfileOriginNotAdmitted marks a fresh resolution that settles the origin
// fields as inadmissible: the identity is hosted outside bridge trust, its
// handle is outside the bridge PDS host's registrable domain, or the directory
// described another DID. The caller drops the fields without a redrive.
var errProfileOriginNotAdmitted = errors.New("profile origin not admitted")

// resolveFreshUserProfileIdentity never consults the caching resolver. It reads
// the DID document first and settles admission from it before any handle
// network call: the document must describe did, name a trusted bridge PDS, and
// declare a handle under that PDS host's registrable domain. Indigo's DID
// lookup resolves the declared handle before it returns, so checking only its
// answer would let any identity make the serial user lane wait on a handle
// domain it controls. Only an identity that passes all three checks has its
// handle verified. The verifying lookup reads the document again, so an
// identity whose document changes in between can still make it wait on another
// handle domain once; that timeout is recorded whatever the outcome. The whole
// lookup runs under identityResolveTimeout; timeouts share the identity refresh
// failure debounce and follow RefreshIdentityHandle's rules.
func (c *UserEventConsumer) resolveFreshUserProfileIdentity(ctx context.Context, did string) (*identity.Identity, error) {
	if c.uncachedIdentityResolver == nil {
		return nil, fmt.Errorf("uncached identity resolver is not configured for profile origin of %s", did)
	}
	if c.identityRefreshFailures == nil {
		return nil, fmt.Errorf("identity refresh failure debounce is not configured for profile origin of %s", did)
	}
	if c.identityRefreshFailures.recentlyFailed(did) {
		return nil, deferRedrive(fmt.Errorf("%w: profile identity resolution for %s debounced after a recent resolution timeout",
			ErrUnresolvedReference, did))
	}
	resolveCtx, cancel := context.WithTimeout(ctx, identityResolveTimeout)
	defer cancel()
	// Same condition as RefreshIdentityHandle: only identityResolveTimeout, not
	// the caller's context, records a failure.
	recordIfTimedOut := func() {
		if ctx.Err() == nil && errors.Is(resolveCtx.Err(), context.DeadlineExceeded) {
			c.identityRefreshFailures.recordFailure(did)
		}
	}

	declared, err := c.uncachedIdentityResolver.ResolveDeclaredIdentity(resolveCtx, did)
	if err != nil {
		recordIfTimedOut()
		return nil, classifyFreshProfileResolveError(did, "read profile identity document", err)
	}
	if declared == nil {
		return nil, fmt.Errorf("%w: read profile identity document %s: no document", ErrUnresolvedReference, did)
	}
	if declared.DID != did {
		return nil, fmt.Errorf("%w: profile identity document for %s describes another DID", errProfileOriginNotAdmitted, did)
	}
	if !c.bridgeTrust.TrustsPDS(declared.PDSURL) {
		return nil, fmt.Errorf("%w: profile identity %s is hosted outside bridge trust", errProfileOriginNotAdmitted, did)
	}
	// The PDS in a DID document is self-asserted; only the bridge's own DNS can
	// vouch for a handle under its zone. A handle outside the bridge PDS host's
	// registrable domain is treated like an untrusted host.
	if !handleUnderPDSRegistrableDomain(declared.DeclaredHandle, declared.PDSURL) {
		return nil, fmt.Errorf("%w: profile identity %s declares a handle outside its bridge PDS domain",
			errProfileOriginNotAdmitted, did)
	}

	resolved, err := c.uncachedIdentityResolver.Resolve(resolveCtx, did)
	// Indigo reports a handle domain that hung past the deadline as
	// handle.invalid with a nil error, and the document it verified may differ
	// from the one admitted above. The timeout is recorded before any outcome,
	// not-admitted included, so a swapped document cannot stall every event.
	recordIfTimedOut()
	if err != nil {
		return nil, classifyFreshProfileResolveError(did, "resolve profile identity", err)
	}
	// The document is fetched again for verification and may have changed in
	// between, so the verified answer is held to the same checks.
	if resolved != nil && resolved.DID == did && !c.bridgeTrust.TrustsPDS(resolved.PDSURL) {
		return nil, fmt.Errorf("%w: profile identity %s is hosted outside bridge trust", errProfileOriginNotAdmitted, did)
	}
	verifiedHandle, err := identity.VerifiedHandle(resolved, did)
	if errors.Is(err, identity.ErrIdentitySubjectMismatch) {
		return nil, fmt.Errorf("%w: verify profile identity %s: %w", errProfileOriginNotAdmitted, did, err)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: verify profile identity %s: %w", ErrUnresolvedReference, did, err)
	}
	c.identityRefreshFailures.clear(did)
	if !handleUnderPDSRegistrableDomain(verifiedHandle, resolved.PDSURL) {
		return nil, fmt.Errorf("%w: profile identity %s has a verified handle outside its bridge PDS domain",
			errProfileOriginNotAdmitted, did)
	}
	return resolved, nil
}

// classifyFreshProfileResolveError makes a malformed DID permanent and every
// other resolver failure a redrivable unresolved reference.
func classifyFreshProfileResolveError(did, step string, err error) error {
	var invalidIdentifier *identity.ErrInvalidIdentifier
	if errors.As(err, &invalidIdentifier) {
		return fmt.Errorf("%w: %s %s: %w", ErrPermanentEvent, step, did, err)
	}
	return fmt.Errorf("%w: %s %s: %w", ErrUnresolvedReference, step, did, err)
}

// handleUnderPDSRegistrableDomain reports whether handle and the PDS host share
// a registrable domain (eTLD+1), the same comparison verifyHostedByClaim makes.
func handleUnderPDSRegistrableDomain(handle, pdsURL string) bool {
	parsed, err := url.Parse(pdsURL)
	if err != nil || parsed.Hostname() == "" {
		return false
	}
	pdsDomain := extractDomainFromHandle(strings.ToLower(parsed.Hostname()))
	handleDomain := extractDomainFromHandle(strings.ToLower(handle))
	return pdsDomain != "" && handleDomain == pdsDomain
}

// admitUserProfileOrigin defaults to clearing all four fields unless the stored
// and freshly resolved PDS hosts are trusted. Discovery supplies its one fresh
// resolution, so the update does not resolve the same DID twice.
func (c *UserEventConsumer) admitUserProfileOrigin(ctx context.Context, did, storedPDSURL string, raw userProfileOrigin, present bool, discoveredIdentity *identity.Identity) (userProfileOrigin, error) {
	if !present || !c.bridgeTrust.TrustsPDS(storedPDSURL) {
		return userProfileOrigin{}, nil
	}
	resolved := discoveredIdentity
	if resolved == nil {
		var err error
		resolved, err = c.resolveFreshUserProfileIdentity(ctx, did)
		if errors.Is(err, errProfileOriginNotAdmitted) {
			return userProfileOrigin{}, nil
		}
		if err != nil {
			return userProfileOrigin{}, err
		}
	}
	if !c.bridgeTrust.TrustsPDS(resolved.PDSURL) {
		return userProfileOrigin{}, nil
	}
	return sanitizeUserProfileOrigin(raw), nil
}

// handleProfileDelete processes profile delete operations
// Clears all profile fields by passing empty strings
func (c *UserEventConsumer) handleProfileDelete(ctx context.Context, did string) error {
	empty := ""
	input := users.UpdateProfileInput{
		DisplayName:    &empty,
		Description:    &empty,
		AvatarCID:      &empty,
		BannerCID:      &empty,
		Origin:         &empty,
		OriginUsername: &empty,
		OriginURL:      &empty,
		OriginSoftware: &empty,
	}
	_, err := c.userService.UpdateProfile(ctx, did, input)
	if err != nil {
		return fmt.Errorf("failed to clear user profile: %w", err)
	}
	log.Printf("Cleared profile for user %s", did)
	return nil
}

// handleUserBlock processes user-to-user block create/delete events.
// CREATE operation = user blocked another user
// DELETE operation = user unblocked another user
func (c *UserEventConsumer) handleUserBlock(ctx context.Context, userDID string, commit *CommitEvent) error {
	if c.userBlockRepo == nil {
		slog.Warn("user block event ignored: userBlockRepo not configured (WithUserBlockRepo not called)",
			slog.String("user_did", userDID),
			slog.String("operation", commit.Operation))
		return nil
	}

	// REV GATE (check→write→advance, see rev_gate.go). The gate row survives
	// the hard delete of the block row, so a stale cross-feed copy of the
	// block's CREATE arriving after the unblock cannot re-index a phantom block.
	return applyGated(ctx, c.revGate, ConsumerUsers, userDID, commit, func() error {
		switch commit.Operation {
		case "create":
			return c.createUserBlock(ctx, userDID, commit)
		case "delete":
			return c.deleteUserBlock(ctx, userDID, commit)
		default:
			// Update operations shouldn't happen on blocks, but ignore gracefully
			log.Printf("Ignoring unexpected operation on user block: %s (userDID=%s, rkey=%s)",
				commit.Operation, userDID, commit.RKey)
			return nil
		}
	})
}

// createUserBlock indexes a new user-to-user block from the firehose.
func (c *UserEventConsumer) createUserBlock(ctx context.Context, userDID string, commit *CommitEvent) error {
	if commit.Record == nil {
		return fmt.Errorf("%w: user block create event missing record data", ErrPermanentEvent)
	}

	// The rejections below are PERMANENT (ErrPermanentEvent): they depend only on
	// the immutable event payload, so retries and redrives would fail identically.

	// Validate userDID format and method (untrusted firehose data).
	if err := requireDIDShaped("user block blocker", userDID); err != nil {
		return err
	}

	// Extract blocked user DID from record's subject field
	blockedDID, ok := commit.Record["subject"].(string)
	if !ok {
		return fmt.Errorf("%w: user block record missing subject field", ErrPermanentEvent)
	}

	// Validate blockedDID format and method (untrusted firehose data).
	if err := requireDIDShaped("user block subject", blockedDID); err != nil {
		return err
	}

	// Validate rkey is non-empty before building AT-URI
	if commit.RKey == "" {
		return fmt.Errorf("%w: user block create event missing rkey", ErrPermanentEvent)
	}

	// Build AT-URI for the block record (lives in the blocker's repository)
	uri := fmt.Sprintf("at://%s/social.coves.actor.block/%s", userDID, commit.RKey)

	// Parse createdAt from record to preserve chronological ordering during replays
	block := &userblocks.UserBlock{
		BlockerDID: userDID,
		BlockedDID: blockedDID,
		BlockedAt:  utils.ParseCreatedAt(commit.Record, uri),
		RecordURI:  uri,
		RecordCID:  commit.CID,
	}

	// Index the block (idempotent via ON CONFLICT DO UPDATE)
	_, err := c.userBlockRepo.BlockUser(ctx, block)
	if err != nil {
		if userblocks.IsConflict(err) {
			log.Printf("User block already indexed: %s -> %s", userDID, blockedDID)
			return nil
		}
		return fmt.Errorf("failed to index user block: %w", err)
	}

	log.Printf("Indexed user block: %s -> %s", userDID, blockedDID)
	return nil
}

// deleteUserBlock removes a user-to-user block from the index.
// DELETE operations don't include record data, so we look up the block by its URI.
func (c *UserEventConsumer) deleteUserBlock(ctx context.Context, userDID string, commit *CommitEvent) error {
	// Validate rkey is non-empty before building AT-URI.
	// PERMANENT: the rkey is immutable — replays fail identically.
	if commit.RKey == "" {
		return fmt.Errorf("%w: user block delete event missing rkey", ErrPermanentEvent)
	}

	// Build AT-URI from the rkey
	uri := fmt.Sprintf("at://%s/social.coves.actor.block/%s", userDID, commit.RKey)

	// Look up the block to get the blocked DID
	block, err := c.userBlockRepo.GetBlockByURI(ctx, uri)
	if err != nil {
		if userblocks.IsNotFound(err) {
			// Already deleted - this is fine (idempotency)
			log.Printf("User block already deleted: %s", uri)
			return nil
		}
		return fmt.Errorf("failed to find user block for deletion: %w", err)
	}

	// Remove the block from the index
	err = c.userBlockRepo.UnblockUser(ctx, userDID, block.BlockedDID)
	if err != nil {
		if userblocks.IsNotFound(err) {
			log.Printf("User block already removed: %s -> %s", userDID, block.BlockedDID)
			return nil
		}
		return fmt.Errorf("failed to remove user block: %w", err)
	}

	log.Printf("Removed user block: %s -> %s", userDID, block.BlockedDID)
	return nil
}
