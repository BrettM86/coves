package jetstream

import (
	"Coves/internal/atproto/identity"
	covesoauth "Coves/internal/atproto/oauth"
	"Coves/internal/atproto/utils"
	"Coves/internal/core/communities"
	"Coves/internal/core/richtext"
	"Coves/internal/validation"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	"golang.org/x/net/publicsuffix"
	"golang.org/x/time/rate"
)

// CommunityEventConsumer consumes community-related events from Jetstream
type CommunityEventConsumer struct {
	repo             communities.Repository // Repository for community operations
	identityResolver interface {
		Resolve(context.Context, string) (*identity.Identity, error)
	} // For resolving handles from DIDs
	httpClient       *http.Client                     // Shared HTTP client with connection pooling
	didCache         *lru.Cache[string, cachedDIDDoc] // Bounded LRU cache for .well-known verification results
	wellKnownLimiter *rate.Limiter                    // Rate limiter for .well-known fetches
	instanceDID      string                           // DID of this Coves instance
	skipVerification bool                             // Skip did:web verification (for dev mode)
	revGate          *RevGate                         // Optional: cross-feed ordering guard for commit events (nil = ungated)

	// bridgeTrust decides which repos may assert an `origin` that differs from
	// their verified handle's domain (see admitCommunityOrigin). nil is
	// default-deny: only a handle-matching origin is kept.
	bridgeTrust *BridgeTrust

	// allowPrivateHosts disables the SSRF address guard on the .well-known
	// fetch. NEVER set in production: the domain this consumer dials comes off a
	// community record published by anyone on the federated network.
	allowPrivateHosts bool

	// transportOptions is the TEST SEAM, carried on the consumer so that the
	// client the guard tests exercise is the one this constructor builds.
	//
	// The alternative — a fixture that constructs the consumer and then assigns
	// newWellKnownClient(...) over its client — is what this field exists to
	// delete, and it is the SECOND time that mistake was made here. The comment
	// block above guardedConsumer in community_consumer_ssrf_test.go records the
	// first: a client injected through withWellKnownHTTPClient could not see a
	// mutation of newWellKnownClient, so the seam was moved onto that function —
	// and its one caller, applyWellKnownClient below, was left just as
	// uncovered, because the fixture went on to overwrite what the caller built.
	// c.allowPrivateHosts is read in exactly one place; threading the seam
	// through the constructor is what puts that place under test.
	//
	// It is UNEXPORTED, and so is the option that sets it: oauth.WithHostResolver
	// must not be reachable from any non-test package, which scripts/ssrf-audit.sh
	// enforces as a hard gate. See pds.bearerClientConfig.transportOptions, the
	// same field for the same reason.
	transportOptions []covesoauth.Option
}

// CommunityConsumerOption configures optional CommunityEventConsumer behaviour.
type CommunityConsumerOption func(*CommunityEventConsumer)

// WithCommunityRevGate installs the per-record rev gate (see rev_gate.go) so
// profile, subscription, and block commits are applied in repo commit order even
// when the same repo is carried by multiple Jetstream feeds. Subscriptions and
// blocks are HARD-deleted, so the gate row is the only tombstone that can reject
// a stale cross-feed copy of the create arriving after the delete (which would
// otherwise silently re-subscribe/re-block the user).
func WithCommunityRevGate(gate *RevGate) CommunityConsumerOption {
	return func(c *CommunityEventConsumer) {
		c.revGate = gate
	}
}

// WithCommunityBridgeTrust installs the provenance gate that decides which
// community repos may self-assert an `origin` foreign to their handle's domain
// (a Tidepool-bridged community claiming lemmy.world). Without it, a foreign
// origin is dropped and the community is indexed without one — mirrors
// WithPostBridgeTrust.
func WithCommunityBridgeTrust(bt *BridgeTrust) CommunityConsumerOption {
	return func(c *CommunityEventConsumer) { c.bridgeTrust = bt }
}

// WithPrivateHostsAllowed disables the SSRF address guard on the .well-known
// DID-document fetch.
//
// THE NAME IS THE CONTRACT: production must not call this. cmd/server derives
// the value from config once (the IS_DEV_ENV gate); tests that serve a DID
// document from httptest pass it because loopback is exactly what the guard
// refuses.
func WithPrivateHostsAllowed() CommunityConsumerOption { // coves:allow-ssrf-hatch: this IS the hatch itself; the name is the contract
	return func(c *CommunityEventConsumer) { c.allowPrivateHosts = true }
}

// withTransportOptions passes oauth transport options through to the client
// applyWellKnownClient builds, and is UNEXPORTED so production cannot reach it.
//
// It is not a second hatch: whatever a resolver seam answers is classified by
// the same pass a real DNS answer goes through, and the dial still goes only to
// addresses that survived it. See CommunityEventConsumer.transportOptions, and
// pds/factory.go's withTransportOptions, which is the shape this copies.
//
// Unlike withWellKnownHTTPClient, this does NOT replace the consumer's client —
// it configures the one the constructor builds, which is the whole difference
// between a test that exercises this consumer's wiring and one that exercises a
// client the test assembled itself.
func withTransportOptions(opts ...covesoauth.Option) CommunityConsumerOption {
	return func(c *CommunityEventConsumer) { c.transportOptions = append(c.transportOptions, opts...) }
}

// withWellKnownHTTPClient replaces the client used for the .well-known fetch,
// and is UNEXPORTED because replacing is all it can do.
//
// IT EXISTS FOR TESTS, and it is the narrowest seam that makes the two
// mechanisms separable. The guard classifies ADDRESSES, so proving it requires a
// hostname that passes validation and answers with a private address — and
// there is no hermetic way to make a name resolve to a chosen address without
// oauth.WithHostResolver, which must not appear in production code. Injecting a
// client the test built with that resolver keeps the seam on the test side.
//
// A caller MUST still inject a GUARDED client, or it proves nothing: replacing
// the guarded client with a permissive one is how a fixture repair silently
// deletes the property it was meant to preserve. That sentence used to be the
// whole defence, addressed to a reader the compiler cannot reach, on an EXPORTED
// option guarding a fetch whose domain comes off a community record published by
// anyone federated with this instance. pds/factory.go's withTransportOptions is
// unexported for exactly this hazard, so leaving this one exported had the
// codebase answering one question two opposite ways. The lowercase name is what
// makes "a caller outside this package cannot drop the guard" a fact rather than
// a request; every caller is a fixture in this package, so it costs them
// nothing.
//
// TestNoExportedSeamCanReplaceTheGuardedClient pins the property in general —
// nothing exported from this package takes an *http.Client — so the next seam
// somebody adds is covered too.
func withWellKnownHTTPClient(client *http.Client) CommunityConsumerOption {
	return func(c *CommunityEventConsumer) {
		if client == nil {
			return
		}
		c.httpClient = client
	}
}

// PrivateHostOptions returns the options a caller holding an allow-private
// boolean should pass to NewCommunityEventConsumer: the hatch when it is set,
// and NOTHING when it is not.
//
// It mirrors oauth.PrivateAddressOptions, imageproxy.PrivateHostOptions and
// unfurl.PrivateHostOptions, and it is a function rather than an `if` in
// cmd/server/wiring.go for the reason documented there: `.env.ci:140` sets
// IS_DEV_ENV=true, so `make ci` takes the PERMISSIVE branch at every call site
// holding such a boolean. A unit test against this function is the only place in
// the repository where the branch production actually runs is ever evaluated. Do
// not inline it back.
//
// FALSE RETURNS ZERO OPTIONS, AND THAT IS THE CONTRACT — not "options that are
// safe", but none, so that what production gets is exactly the constructor's own
// defaults.
func PrivateHostOptions(allowPrivate bool) []CommunityConsumerOption {
	if !allowPrivate {
		return nil
	}
	return []CommunityConsumerOption{WithPrivateHostsAllowed()} // coves:allow-ssrf-hatch: the gate helper allow-branch; its false branch returns nothing
}

const (
	// wellKnownTimeout is the ceiling this consumer has always run the
	// DID-document fetch under. It is re-applied over the shared SSRF client's
	// own 15s; see newWellKnownClient.
	wellKnownTimeout = 10 * time.Second

	// wellKnownMaxIdleConnsPerHost is the per-host pool depth this consumer has
	// always run with, kept because net/http's default is 2 and a federated
	// instance is verified repeatedly.
	wellKnownMaxIdleConnsPerHost = 10
)

// newWellKnownClient builds the client the DID-document fetch goes through.
//
// The SSRF-safe transport of internal/atproto/oauth resolves the host, refuses
// private, loopback and link-local addresses, and then dials only the address it
// vetted — closing the check-then-dial window a naive guard leaves open. The
// domain it is pointed at comes off a community record published by anyone
// federated with this instance, and the fetch runs from inside the AppView's own
// network, next to its Postgres, its PDS and, in production, a metadata endpoint
// that hands credentials to anything that can reach it.
//
// IT IS HALF OF A TWO-MECHANISM DEFENCE and does not subsume the other half. A
// safe dialler has an opinion about addresses and none about paths, so
// `internal-admin/v1/secrets?x=y#` walks straight past it: that names no private
// address, it names whatever `internal-admin` resolves to, and the trailing `#`
// turns the `.well-known/did.json` suffix into a fragment that is never sent.
// validation.NormalizeDomain, called in verifyDIDDocument BEFORE the URL is
// built, is what closes that. Neither mechanism is redundant with the other.
//
// EVERY SETTING THIS CONSUMER ALREADY HAD IS RE-APPLIED. The shared transport
// carries MaxIdleConns 100 and IdleConnTimeout 90s of its own; the per-host pool
// depth and the 10s ceiling are the two it does not, and both are restored here
// — a firehose path silently re-timed from 10s to the shared client's 15s would
// be a second change wearing an SSRF fix's clothes, the way
// blobs.NewBlobService, imageproxy.NewPDSFetcher and unfurl.NewService all say.
//
// # THE opts PARAMETER IS THE TEST SEAM, AND IT IS WHY THE GUARD IS PROVABLE
//
// It mirrors internal/api/handlers/aggregator's registerHTTPClient exactly, and
// it exists because of a hole a mutation found: with no seam here, the only way
// to test the guard was for a test to build its OWN oauth client with
// WithHostResolver and inject it. That proves internal/atproto/oauth works,
// which the transport tests already prove, and says nothing about this consumer —
// flipping the boolean below to a constant `true` failed no test at all,
// because every input the other tests use is eaten by validation.NormalizeDomain
// one branch earlier and never reaches a transport.
//
// Passing the resolver through HERE means the client under test is the one this
// function builds, from the same allowPrivateHosts boolean production passes. So
// disabling the guard on this line now fails
// TestVerifyDIDDocument_RefusesAValidationPassingHostThatResolvesPrivate.
//
// IT CANNOT OPEN THE GUARD. WithPrivateAddressesAllowed is the only thing that
// does, it is one-way, and it is named at a call site or nowhere; an option
// passed here is classified by the same pass a real DNS answer goes through.
// The seam chooses what gets classified, never whether classification happens.
//
// Production passes nothing.
func newWellKnownClient(allowPrivateHosts bool, opts ...covesoauth.Option) *http.Client {
	client := covesoauth.NewSSRFSafeHTTPClient(append(
		covesoauth.PrivateAddressOptions(allowPrivateHosts),
		append([]covesoauth.Option{
			covesoauth.WithMaxIdleConnsPerHost(wellKnownMaxIdleConnsPerHost),
		}, opts...)...,
	)...)
	client.Timeout = wellKnownTimeout
	return client
}

// cachedDIDDoc represents a cached verification result with expiration
type cachedDIDDoc struct {
	expiresAt time.Time // When this cache entry expires
	valid     bool      // Whether verification passed
}

// NewCommunityEventConsumer creates a new Jetstream consumer for community events
// instanceDID: The DID of this Coves instance (for hostedBy verification)
// skipVerification: Skip did:web verification (for dev mode)
// identityResolver: Optional resolver for resolving handles from DIDs (can be nil for tests)
func NewCommunityEventConsumer(repo communities.Repository, instanceDID string, skipVerification bool, identityResolver interface {
	Resolve(context.Context, string) (*identity.Identity, error)
}, opts ...CommunityConsumerOption,
) *CommunityEventConsumer {
	// Create bounded LRU cache for DID document verification results
	// Max 1000 entries to prevent unbounded memory growth (PR review feedback)
	// Each entry ~100 bytes → max ~100KB memory overhead
	cache, err := lru.New[string, cachedDIDDoc](1000)
	if err != nil {
		// This should never happen with a valid size, but handle gracefully
		log.Printf("WARNING: Failed to create DID cache (size=1000), verification will be slower: %v", err)
		// Create minimal cache to avoid nil pointer
		cache, fallbackErr := lru.New[string, cachedDIDDoc](1)
		if fallbackErr != nil {
			// Both attempts failed - this indicates a serious issue with the LRU library
			log.Printf("CRITICAL: Failed to create fallback DID cache (size=1): %v", fallbackErr)
			panic(fmt.Sprintf("cannot create LRU cache: primary error=%v, fallback error=%v", err, fallbackErr))
		}
		fallback := &CommunityEventConsumer{
			repo:             repo,
			identityResolver: identityResolver,
			instanceDID:      instanceDID,
			skipVerification: skipVerification,
			didCache:         cache,
			wellKnownLimiter: rate.NewLimiter(10, 20),
		}
		for _, opt := range opts {
			opt(fallback)
		}
		applyWellKnownClient(fallback)
		return fallback
	}

	consumer := &CommunityEventConsumer{
		repo:             repo,
		identityResolver: identityResolver, // Optional - can be nil for tests
		instanceDID:      instanceDID,
		skipVerification: skipVerification,
		// Bounded LRU cache for .well-known verification results (max 1000 entries)
		// Automatically evicts least-recently-used entries when full
		didCache: cache,
		// Rate limiter: 10 requests per second, burst of 20
		// Prevents DoS via excessive .well-known fetches
		wellKnownLimiter: rate.NewLimiter(10, 20),
	}
	for _, opt := range opts {
		opt(consumer)
	}
	applyWellKnownClient(consumer)
	return consumer
}

// applyWellKnownClient gives a consumer its guarded .well-known client, AFTER
// the options have run.
//
// The ordering is the whole of it, in both directions. The hatch is an option,
// so building the client before the loop would read allowPrivateHosts before
// anything could set it and hand every developer a client that refuses their own
// machine. And withWellKnownHTTPClient is also an option, so overwriting
// unconditionally after the loop would throw away the client a test injected —
// which would make every ordering assertion in community_consumer_ssrf_test.go
// pass against a consumer quietly using a client the test cannot see.
//
// Hence the nil check: an injected client wins, and a consumer that was given
// none gets the guarded one.
//
// The transport options are threaded for a third reason, and it is what makes
// this function's own argument observable: the guard tests build a consumer
// through the constructor and pass the resolver seam as an option, so THIS line
// is the one that carries their seam. Replacing c.allowPrivateHosts with a
// constant here now fails them.
func applyWellKnownClient(c *CommunityEventConsumer) {
	if c.httpClient != nil {
		return
	}
	c.httpClient = newWellKnownClient(c.allowPrivateHosts, c.transportOptions...)
}

// RevGated reports whether this consumer applies the per-record rev gate (true when a
// gate was injected via WithCommunityRevGate). main.go checks this at boot to refuse
// multi-feed operation with an ungated consumer.
func (c *CommunityEventConsumer) RevGated() bool { return c.revGate != nil }

// HandleEvent processes a Jetstream event for community records
// This is called by the main Jetstream consumer when it receives commit events
func (c *CommunityEventConsumer) HandleEvent(ctx context.Context, event *JetstreamEvent) error {
	// We only care about commit events for community records
	if event.Kind != "commit" || event.Commit == nil {
		return nil
	}

	commit := event.Commit

	// Route to appropriate handler based on collection
	// IMPORTANT: Collection names refer to RECORD TYPES in repositories, not XRPC procedures
	// - social.coves.community.profile: Community profile records (in community's own repo)
	// - social.coves.community.subscription: Subscription records (in user's repo)
	// - social.coves.community.block: Block records (in user's repo)
	//
	// XRPC procedures (social.coves.community.subscribe/unsubscribe) are just HTTP endpoints
	// that CREATE or DELETE records in these collections
	// All three collections run under the rev gate (check→write→advance, see
	// rev_gate.go): events apply in repo commit order even when the same repo
	// is carried by multiple Jetstream feeds. Subscriptions and blocks are
	// HARD-deleted, so the gate row is the tombstone that rejects a stale
	// cross-feed copy of the create arriving after the delete — without it,
	// an unsubscribed user would be silently re-subscribed hours later.
	switch commit.Collection {
	case "social.coves.community.profile":
		return applyGated(ctx, c.revGate, ConsumerCommunities, event.Did, commit, func() error {
			return c.handleCommunityProfile(ctx, event.Did, commit)
		})
	case "social.coves.community.subscription":
		// Handle both create (subscribe) and delete (unsubscribe) operations
		return applyGated(ctx, c.revGate, ConsumerCommunities, event.Did, commit, func() error {
			return c.handleSubscription(ctx, event.Did, commit)
		})
	case "social.coves.community.block":
		// Handle both create (block) and delete (unblock) operations
		return applyGated(ctx, c.revGate, ConsumerCommunities, event.Did, commit, func() error {
			return c.handleBlock(ctx, event.Did, commit)
		})
	default:
		// Not a community-related collection
		return nil
	}
}

// handleCommunityProfile processes community profile create/update/delete events
func (c *CommunityEventConsumer) handleCommunityProfile(ctx context.Context, did string, commit *CommitEvent) error {
	switch commit.Operation {
	case "create":
		return c.createCommunity(ctx, did, commit)
	case "update":
		return c.updateCommunity(ctx, did, commit)
	case "delete":
		return c.deleteCommunity(ctx, did)
	default:
		log.Printf("Unknown operation for community profile: %s", commit.Operation)
		return nil
	}
}

// createCommunity indexes a new community from the firehose
func (c *CommunityEventConsumer) createCommunity(ctx context.Context, did string, commit *CommitEvent) error {
	if commit.Record == nil {
		return fmt.Errorf("%w: community profile create event missing record data", ErrPermanentEvent)
	}

	// Parse the community profile record
	profile, err := parseCommunityProfile(commit.Record)
	if err != nil {
		return fmt.Errorf("failed to parse community profile: %w", err)
	}

	// atProto Best Practice: Handles are NOT stored in records (they're mutable, resolved from DIDs)
	// If handle is missing from record (new atProto-compliant records), resolve it from PLC/DID
	var resolvedPDSURL string
	var resolved *identity.Identity
	if profile.Handle == "" {
		if c.identityResolver != nil {
			// Production: Resolve handle from PLC (source of truth)
			// NO FALLBACK - if PLC is down, we fail and backfill later
			// This prevents creating communities with incorrect handles in federated scenarios
			identity, err := c.identityResolver.Resolve(ctx, did)
			if err != nil {
				return fmt.Errorf("failed to resolve handle from PLC for %s: %w (no fallback - will retry during backfill)", did, err)
			}
			resolved = identity
			// "handle.invalid" IS NOT A HANDLE. atProto identity resolution
			// reports a DID whose handle it could not verify bidirectionally
			// by returning that reserved placeholder rather than an error, so
			// what arrives here is a perfectly well-formed identity naming a
			// non-handle — and communities.handle is UNIQUE. Store it once and
			// every subsequent unverifiable community collides with it, which
			// the insert reports as a conflict and the swallow below used to
			// discard silently.
			//
			// TRANSIENT, deliberately: this is a fact about the RESOLUTION, not
			// about the record. The PLC directory may be unreachable this
			// second and answer fine the next, so the redrive has to be allowed
			// to succeed. The user path applies the same guard for the same
			// reason (authorpost.go, hydrateAuthorOpportunistically).
			if identity.Handle == "" || identity.Handle == invalidHandle {
				return fmt.Errorf("resolving the handle of community %s: identity resolution returned %q, "+
					"which is the reserved placeholder for an unverifiable handle and must never be stored in a unique column "+
					"(retryable — the directory may verify it later)", did, identity.Handle)
			}
			profile.Handle = identity.Handle
			// Persist the resolved PDS host: BridgeTrust gates bridgedStats
			// on the post's community row carrying its repo's PDS URL, and a
			// firehose-indexed community (a Tidepool bridge community above
			// all) is created HERE — leaving pds_url empty makes the trust
			// gate default-deny every bridged vote count forever.
			resolvedPDSURL = identity.PDSURL
			log.Printf("✓ Resolved handle from PLC: %s (did=%s, method=%s)",
				profile.Handle, did, identity.Method)
		} else {
			// Test mode only: construct deterministically when no resolver available
			profile.Handle = constructHandleFromProfile(profile)
			log.Printf("✓ Constructed handle (test mode): %s (name=%s, hostedBy=%s)",
				profile.Handle, profile.Name, profile.HostedBy)
		}
	}

	// SECURITY: Verify hostedBy claim matches handle domain
	// This prevents malicious instances from claiming to host communities for domains they don't own
	if err := c.verifyHostedByClaim(ctx, profile.Handle, profile.HostedBy); err != nil {
		log.Printf("🚨 SECURITY: Rejecting community %s - hostedBy verification failed: %v", did, err)
		log.Printf("    Handle: %s, HostedBy: %s", profile.Handle, profile.HostedBy)
		return fmt.Errorf("hostedBy verification failed: %w", err)
	}

	// Build AT-URI for this record
	// V2 Architecture (ONLY):
	//   - 'did' parameter IS the community DID (community owns its own repo)
	//   - rkey MUST be "self" for community profiles
	//   - URI: at://community_did/social.coves.community.profile/self

	// REJECT non-V2 communities (pre-production: no V1 compatibility).
	// PERMANENT: the rkey is immutable — replays fail identically.
	if commit.RKey != "self" {
		return fmt.Errorf("%w: invalid community profile rkey: expected 'self', got '%s' (V1 communities not supported)", ErrPermanentEvent, commit.RKey)
	}

	uri := fmt.Sprintf("at://%s/social.coves.community.profile/self", did)

	// V2: Community ALWAYS owns itself
	ownerDID := did

	// The origin gate measures against the repo's RESOLVED identity, never the
	// record's own handle field: a record can carry any handle it likes. A
	// fresh resolution also backfills the PDS host the gate (and bridgedStats)
	// reads.
	origin, provenancePDSURL := c.admitRecordOrigin(ctx, did, profile, resolved, profile.Handle, resolvedPDSURL)
	if resolvedPDSURL == "" {
		resolvedPDSURL = provenancePDSURL
	}

	// Create community entity
	community := &communities.Community{
		DID:                    did, // V2: Repository DID IS the community DID
		Handle:                 profile.Handle,
		Name:                   profile.Name,
		DisplayName:            profile.DisplayName,
		Description:            profile.Description,
		OwnerDID:               ownerDID, // V2: same as DID (self-owned)
		CreatedByDID:           profile.CreatedBy,
		HostedByDID:            profile.HostedBy,
		PDSURL:                 resolvedPDSURL,
		Visibility:             profile.Visibility,
		AllowExternalDiscovery: profile.Federation.AllowExternalDiscovery,
		ModerationType:         profile.ModerationType,
		ContentWarnings:        profile.ContentWarnings,
		MemberCount:            profile.MemberCount,
		SubscriberCount:        profile.SubscriberCount,
		FederatedFrom:          profile.FederatedFrom,
		FederatedID:            profile.FederatedID,
		CreatedAt:              profile.CreatedAt,
		UpdatedAt:              time.Now(),
		RecordURI:              uri,
		RecordCID:              commit.CID,
		Origin:                 origin,
	}

	// Handle blobs (avatar/banner) if present
	if avatarCID, ok := extractBlobCID(profile.Avatar); ok {
		community.AvatarCID = avatarCID
	}
	if bannerCID, ok := extractBlobCID(profile.Banner); ok {
		community.BannerCID = bannerCID
	}

	// Handle description facets (rich text). Sanitized like post/comment facets:
	// federated profiles cannot be rejected back to their author, and clients
	// must never receive ranges that slice outside the description.
	if kept := sanitizedDescriptionFacets(profile, did); kept != nil {
		facetsJSON, marshalErr := json.Marshal(kept)
		if marshalErr != nil {
			log.Printf("WARNING: Failed to marshal description facets for community %s: %v (facets will be omitted)", did, marshalErr)
		} else {
			community.DescriptionFacets = facetsJSON
		}
	}

	// Index in AppView database.
	//
	// THE TWO CONFLICTS MEAN OPPOSITE THINGS, and treating them alike is what
	// turned one handle collision into a flood of unrelated dead letters.
	// communities.IsConflict matches both, so it is too wide to switch on here.
	_, err = c.repo.Create(ctx, community)
	if err != nil {
		switch {
		case errors.Is(err, communities.ErrCommunityAlreadyExists):
			// The DID is already in the table: a genuine idempotent replay.
			// The connector rewinds its cursor after every reconnect and the
			// AppView consumes overlapping feeds, so this path is walked
			// constantly for every community and must stay a silent no-op.
			log.Printf("Community already indexed: %s (%s)", community.Handle, community.DID)
			return nil

		case errors.Is(err, communities.ErrHandleTaken):
			// A DIFFERENT DID already holds this handle. Nothing about that is
			// idempotent: the community in this event was NOT indexed, is in
			// the table under no DID at all, and never will be while the
			// incumbent stands.
			//
			// PERMANENT. A handle held by someone else does not resolve itself
			// by waiting, so a transient classification spends the connector's
			// full inline retry budget — about 4.2 seconds of blocking, on a
			// lane that also carries posts — and then ten redrives, per
			// delivery, forever. Both DIDs and the handle go in the message
			// because a log line is the only place this is diagnosable from.
			return fmt.Errorf("%w: cannot index community %s: handle %q is already held by community %s",
				ErrPermanentEvent, community.DID, community.Handle, c.incumbentOfHandle(ctx, community.Handle))

		case communities.IsConflict(err):
			// A conflict this build does not recognise. Reported rather than
			// swallowed: the swallow is what hid the handle collision, and a
			// new unique constraint would otherwise inherit the same silence.
			return fmt.Errorf("failed to index community %s: unclassified conflict: %w", community.DID, err)
		}
		return fmt.Errorf("failed to index community: %w", err)
	}

	log.Printf("Indexed new community: %s (%s)", community.Handle, community.DID)
	return nil
}

// incumbentOfHandle names the community that already holds a contested handle,
// for the refusal message.
//
// BEST EFFORT, and never allowed to change the outcome. The refusal is already
// decided by the time this runs; this only fills in the half of the message an
// operator cannot otherwise get — "some other community has it" sends them to
// the database, "did:plc:… has it" sends them to the community. A lookup that
// fails yields a placeholder rather than an error, because replacing a precise
// permanent refusal with a vague transient one would trade the diagnosis for
// the flood this whole change exists to stop.
func (c *CommunityEventConsumer) incumbentOfHandle(ctx context.Context, handle string) string {
	const unknown = "an unidentified DID"
	if c.repo == nil {
		return unknown
	}
	incumbent, err := c.repo.GetByHandle(ctx, handle)
	if err != nil || incumbent == nil {
		return unknown
	}
	return incumbent.DID
}

// updateCommunity updates an existing community from the firehose
func (c *CommunityEventConsumer) updateCommunity(ctx context.Context, did string, commit *CommitEvent) error {
	if commit.Record == nil {
		return fmt.Errorf("%w: community profile update event missing record data", ErrPermanentEvent)
	}

	// REJECT non-V2 communities (pre-production: no V1 compatibility).
	// PERMANENT: the rkey is immutable — replays fail identically.
	if commit.RKey != "self" {
		return fmt.Errorf("%w: invalid community profile rkey: expected 'self', got '%s' (V1 communities not supported)", ErrPermanentEvent, commit.RKey)
	}

	// Parse profile
	profile, err := parseCommunityProfile(commit.Record)
	if err != nil {
		return fmt.Errorf("failed to parse community profile: %w", err)
	}

	// atProto Best Practice: Handles are NOT stored in records (they're mutable, resolved from DIDs)
	// If handle is missing from record (new atProto-compliant records), resolve it from PLC/DID
	var resolvedPDSURL string
	var resolved *identity.Identity
	if profile.Handle == "" {
		if c.identityResolver != nil {
			// Production: Resolve handle from PLC (source of truth)
			// NO FALLBACK - if PLC is down, we fail and backfill later
			// This prevents creating communities with incorrect handles in federated scenarios
			identity, err := c.identityResolver.Resolve(ctx, did)
			if err != nil {
				return fmt.Errorf("failed to resolve handle from PLC for %s: %w (no fallback - will retry during backfill)", did, err)
			}
			resolved = identity
			profile.Handle = identity.Handle
			// Backfill the stored PDS host too (see createCommunity): rows
			// indexed before this fix carry an empty pds_url, which makes
			// BridgeTrust default-deny their posts' bridgedStats. Update()
			// only overwrites when non-empty.
			resolvedPDSURL = identity.PDSURL
			log.Printf("✓ Resolved handle from PLC: %s (did=%s, method=%s)",
				profile.Handle, did, identity.Method)
		} else {
			// Test mode only: construct deterministically when no resolver available
			profile.Handle = constructHandleFromProfile(profile)
			log.Printf("✓ Constructed handle (test mode): %s (name=%s, hostedBy=%s)",
				profile.Handle, profile.Name, profile.HostedBy)
		}
	}

	// V2: Repository DID IS the community DID
	// Get existing community using the repo DID
	existing, err := c.repo.GetByDID(ctx, did)
	if err != nil {
		if communities.IsNotFound(err) {
			// Community doesn't exist yet - treat as create
			log.Printf("Community not found for update, creating: %s", did)
			return c.createCommunity(ctx, did, commit)
		}
		return fmt.Errorf("failed to get existing community: %w", err)
	}

	// The origin gate runs BEFORE the record's fields are copied over, so it
	// measures against the handle this row was created with (verified then,
	// never rewritten by Update) and, when a resolver is configured, against a
	// fresh resolution of the repo — not against whatever handle or stale
	// pds_url the record's author would like it to see. The fresh PDS host
	// replaces the stored one for the same reason.
	admittedOrigin, provenancePDSURL := c.admitRecordOrigin(ctx, did, profile, resolved, existing.Handle, existing.PDSURL)
	if resolvedPDSURL == "" && provenancePDSURL != existing.PDSURL {
		resolvedPDSURL = provenancePDSURL
	}
	if existing.Origin != "" && admittedOrigin == "" {
		log.Printf("WARNING: community %s loses its stored origin %q: this profile update carries origin %q, which was absent or not admitted", did, existing.Origin, profile.Origin)
	}

	// Update fields
	if resolvedPDSURL != "" {
		existing.PDSURL = resolvedPDSURL
	}
	existing.Handle = profile.Handle
	existing.Name = profile.Name
	existing.DisplayName = profile.DisplayName
	existing.Description = profile.Description
	existing.Visibility = profile.Visibility
	existing.AllowExternalDiscovery = profile.Federation.AllowExternalDiscovery
	existing.ModerationType = profile.ModerationType
	existing.ContentWarnings = profile.ContentWarnings
	existing.RecordCID = commit.CID
	existing.Origin = admittedOrigin

	// Update blobs
	if avatarCID, ok := extractBlobCID(profile.Avatar); ok {
		existing.AvatarCID = avatarCID
	}
	if bannerCID, ok := extractBlobCID(profile.Banner); ok {
		existing.BannerCID = bannerCID
	}

	// Update description facets (sanitized; see the create path). The incoming
	// record replaces the stored one wholesale, so a record without (surviving)
	// facets clears the stored facets — keeping them would leave stale offsets
	// annotating the freshly updated description text.
	if kept := sanitizedDescriptionFacets(profile, existing.DID); kept != nil {
		facetsJSON, marshalErr := json.Marshal(kept)
		if marshalErr != nil {
			log.Printf("WARNING: Failed to marshal description facets for community %s: %v (clearing stored facets)", existing.DID, marshalErr)
			existing.DescriptionFacets = nil
		} else {
			existing.DescriptionFacets = facetsJSON
		}
	} else {
		existing.DescriptionFacets = nil
	}

	// Save updates
	_, err = c.repo.Update(ctx, existing)
	if err != nil {
		return fmt.Errorf("failed to update community: %w", err)
	}

	log.Printf("Updated community: %s (%s)", existing.Handle, existing.DID)
	return nil
}

// deleteCommunity removes a community from the index
func (c *CommunityEventConsumer) deleteCommunity(ctx context.Context, did string) error {
	err := c.repo.Delete(ctx, did)
	if err != nil {
		if communities.IsNotFound(err) {
			log.Printf("Community already deleted: %s", did)
			return nil
		}
		return fmt.Errorf("failed to delete community: %w", err)
	}

	log.Printf("Deleted community: %s", did)
	return nil
}

// verifyHostedByClaim verifies that the community's hostedBy claim matches the handle domain
// This prevents malicious instances from claiming to host communities for domains they don't own
func (c *CommunityEventConsumer) verifyHostedByClaim(ctx context.Context, handle, hostedByDID string) error {
	// Skip verification in dev mode
	if c.skipVerification {
		return nil
	}

	// Add 15 second overall timeout to prevent slow verification from blocking consumer (PR review feedback)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	// Verify hostedByDID is did:web format.
	// PERMANENT: derived purely from the immutable record — replays fail identically.
	if !strings.HasPrefix(hostedByDID, "did:web:") {
		return fmt.Errorf("%w: hostedByDID must use did:web method, got: %s", ErrPermanentEvent, hostedByDID)
	}

	// Extract domain from did:web DID
	hostedByDomain := strings.TrimPrefix(hostedByDID, "did:web:")

	// Extract domain from community handle
	// Handle format examples:
	//   - "!gaming@coves.social" → domain: "coves.social"
	//   - "gaming.community.coves.social" → domain: "coves.social"
	handleDomain := extractDomainFromHandle(handle)
	if handleDomain == "" {
		return fmt.Errorf("%w: failed to extract domain from handle: %s", ErrPermanentEvent, handle)
	}

	// Verify handle domain matches hostedBy domain.
	// PERMANENT security rejection: the mismatch is inherent to the record.
	if handleDomain != hostedByDomain {
		return fmt.Errorf("%w: handle domain (%s) doesn't match hostedBy domain (%s)", ErrPermanentEvent, handleDomain, hostedByDomain)
	}

	// SECURITY: Verify DID document exists and is valid (Bluesky-compatible security model)
	// MANDATORY bidirectional verification: DID document must claim this handle in alsoKnownAs
	// This matches Bluesky's security requirements and prevents domain impersonation
	if err := c.verifyDIDDocument(ctx, hostedByDID, hostedByDomain, handle); err != nil {
		log.Printf("🚨 SECURITY: Rejecting community - bidirectional DID verification failed: %v", err)
		return fmt.Errorf("bidirectional DID verification required: %w", err)
	}

	return nil
}

// verifyDIDDocument fetches and validates the DID document from .well-known/did.json
// Implements Bluesky's bidirectional verification model:
//  1. Verify DID document exists at https://domain/.well-known/did.json
//  2. Verify DID document ID matches claimed DID
//  3. Verify DID document claims the handle in alsoKnownAs field
//
// Results are cached with TTL and rate-limited to prevent DoS attacks
func (c *CommunityEventConsumer) verifyDIDDocument(ctx context.Context, did, domain, handle string) error {
	// Skip verification in dev mode
	if c.skipVerification {
		return nil
	}

	// SECURITY: the domain came off a community record published by anyone
	// federated with this instance, and the URL below is built by concatenating
	// it into a string. A URL parser has no way to know the concatenation was
	// meant to stop at the host, so every part of a URL that comes after the host
	// can be smuggled in through it:
	//
	//	internal-admin/v1/secrets?x=y#   fetches /v1/secrets?x=y from internal-admin
	//	evil.com@internal-host           fetches from internal-host; evil.com is userinfo
	//	127.0.0.1:5432                   fetches from a port on the loopback interface
	//
	// The trailing `#` in the first is what makes this a full request-forgery
	// primitive rather than an SSRF to one fixed path: it turns the
	// `.well-known/did.json` suffix into a fragment, which is never sent, so the
	// publisher chooses the path AND the query as well as the host.
	//
	// THE GUARDED CLIENT CANNOT CLOSE THIS. It refuses private ADDRESSES and has
	// no opinion about paths, and `internal-admin` is not an address — it is
	// whatever this AppView's resolver says it is, which on a split-horizon DNS
	// is an ordinary-looking answer. The two mechanisms are halves; see
	// newWellKnownClient.
	//
	// IT RUNS BEFORE THE CACHE AND BEFORE THE RATE LIMITER, not just before the
	// URL. The cache is keyed by DID rather than by domain, so a hit would answer
	// for a domain nothing ever looked at; and the limiter blocks, which would
	// let a flood of malformed records spend the budget that legitimate
	// verifications share.
	//
	// The refusal is NOT cached. It costs one pass over a string to recompute,
	// and the cache key is the DID — so caching would let one bad record suppress
	// a later good one for the same DID for the full TTL.
	normalizedDomain, err := validation.NormalizeDomain(domain)
	if err != nil {
		return fmt.Errorf("refusing to fetch a DID document for %s: %q is not a hostname: %w",
			did, domain, err)
	}
	// The canonical form from here on, so one domain has one spelling in the URL
	// and in the logs.
	domain = normalizedDomain

	// Check bounded LRU cache first (thread-safe, no locks needed)
	if cached, ok := c.didCache.Get(did); ok {
		// Check if cache entry is still valid (not expired)
		if time.Now().Before(cached.expiresAt) {
			if !cached.valid {
				return fmt.Errorf("cached verification failure for %s", did)
			}
			log.Printf("✓ DID document verification (cached): %s", domain)
			return nil
		}
		// Cache entry expired - remove it to free up space for fresh entries
		c.didCache.Remove(did)
	}

	// Rate limit .well-known fetches to prevent DoS
	if err := c.wellKnownLimiter.Wait(ctx); err != nil {
		return fmt.Errorf("rate limit exceeded for .well-known fetch: %w", err)
	}

	// Construct .well-known URL
	didDocURL := fmt.Sprintf("https://%s/.well-known/did.json", domain)

	// Create HTTP request with timeout
	req, err := http.NewRequestWithContext(ctx, "GET", didDocURL, nil)
	if err != nil {
		// Cache the failure
		c.cacheVerificationResult(did, false, 5*time.Minute)
		return fmt.Errorf("failed to create request: %w", err)
	}

	// Fetch DID document using shared HTTP client
	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Cache the failure (shorter TTL for network errors)
		c.cacheVerificationResult(did, false, 5*time.Minute)
		return fmt.Errorf("failed to fetch DID document from %s: %w", didDocURL, err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			log.Printf("Failed to close response body: %v", closeErr)
		}
	}()

	// Verify HTTP status
	if resp.StatusCode != http.StatusOK {
		// Cache the failure
		c.cacheVerificationResult(did, false, 5*time.Minute)
		return fmt.Errorf("DID document returned HTTP %d from %s", resp.StatusCode, didDocURL)
	}

	// Parse DID document
	var didDoc struct {
		ID          string   `json:"id"`
		AlsoKnownAs []string `json:"alsoKnownAs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&didDoc); err != nil {
		// Cache the failure
		c.cacheVerificationResult(did, false, 5*time.Minute)
		return fmt.Errorf("failed to parse DID document JSON: %w", err)
	}

	// Verify DID document ID matches claimed DID
	if didDoc.ID != did {
		// Cache the failure
		c.cacheVerificationResult(did, false, 5*time.Minute)
		return fmt.Errorf("DID document ID (%s) doesn't match claimed DID (%s)", didDoc.ID, did)
	}

	// SECURITY: Bidirectional verification - DID document must claim this handle
	// Prevents impersonation where someone points DNS to another user's DID
	// Format: handle "coves.social" or "!community@coves.social" → check for "at://coves.social"
	handleDomain := extractDomainFromHandle(handle)
	expectedAlias := fmt.Sprintf("at://%s", handleDomain)

	found := false
	for _, alias := range didDoc.AlsoKnownAs {
		if alias == expectedAlias {
			found = true
			break
		}
	}

	if !found {
		// Cache the failure
		c.cacheVerificationResult(did, false, 5*time.Minute)
		return fmt.Errorf("DID document does not claim handle domain %s in alsoKnownAs (expected %s, got %v)",
			handleDomain, expectedAlias, didDoc.AlsoKnownAs)
	}

	// Cache the success (24 hour TTL - matches Bluesky recommendations)
	c.cacheVerificationResult(did, true, 24*time.Hour)

	log.Printf("✓ DID document verified: %s", domain)
	return nil
}

// cacheVerificationResult stores a verification result in the bounded LRU cache with the given TTL
// The LRU cache is thread-safe and automatically evicts least-recently-used entries when full
func (c *CommunityEventConsumer) cacheVerificationResult(did string, valid bool, ttl time.Duration) {
	c.didCache.Add(did, cachedDIDDoc{
		valid:     valid,
		expiresAt: time.Now().Add(ttl),
	})
}

// admitRecordOrigin decides the origin to store for a profile record, and
// returns alongside it the PDS host that decision was made against (so the
// caller can persist it).
//
// The gate's inputs are PROVENANCE, and provenance is never read off the
// record: a record can carry any handle its author likes, and the stored
// pds_url can only ever be as fresh as the last time something resolved it.
// So when a resolver is configured the repo is resolved afresh every time an
// origin is asserted, and that identity's handle and PDS host are what the
// rule sees. resolved short-circuits that when the caller already has one.
// Without a resolver (tests, dev) the fallback handle and PDS host are used —
// on update those are the stored row's, which Update never rewrites from the
// record, so they are the create-time-verified values.
//
// Resolution failing is default-deny on the ORIGIN ONLY: the community is
// still indexed, without one. Rejecting the event would dead-letter every post
// naming the community over a display string, and the next profile write
// gets another chance.
func (c *CommunityEventConsumer) admitRecordOrigin(ctx context.Context, did string, profile *CommunityProfile, resolved *identity.Identity, fallbackHandle, fallbackPDSURL string) (origin, pdsURL string) {
	if profile.Origin == "" {
		return "", fallbackPDSURL
	}

	if resolved == nil && c.identityResolver != nil {
		id, err := c.identityResolver.Resolve(ctx, did)
		if err != nil {
			log.Printf("WARNING: dropping origin %q on community %s: could not resolve the repo's identity to check it (%v); indexing without it", profile.Origin, did, err)
			return "", fallbackPDSURL
		}
		resolved = id
	}

	handle, pdsURL := fallbackHandle, fallbackPDSURL
	if resolved != nil {
		if resolved.Handle == "" || resolved.Handle == invalidHandle {
			log.Printf("WARNING: dropping origin %q on community %s: the repo's handle could not be verified (%q); indexing without it", profile.Origin, did, resolved.Handle)
			return "", fallbackPDSURL
		}
		handle = resolved.Handle
		if resolved.PDSURL != "" {
			pdsURL = resolved.PDSURL
		}
	}

	return admitCommunityOrigin(c.bridgeTrust, did, pdsURL, handle, profile.Origin), pdsURL
}

// admitCommunityOrigin applies the trust rule for a profile record's
// self-asserted `origin` and returns the value to store — "" when the field is
// absent or dropped.
//
// `origin` is the instance a community actually lives on, and exists so a
// bridged community whose DNS handle is the lossy comicstrips.lemmy-world.tdpl.io
// can still render as !comicstrips@lemmy.world. Because it is self-asserted by
// whoever writes the record, an unconstrained value would let any repo claim to
// be !nba@coves.social. So:
//
//   - A repo hosted on a trusted bridge PDS (BridgeTrust, the same gate that
//     decides bridgedStats provenance) may assert any well-formed origin.
//   - Any other repo may assert only its OWN domain: the origin must be the
//     registrable domain (eTLD+1) of its verified handle
//     (c-nba.coves.social → coves.social) or a parent domain of the handle that
//     sits UNDER that registrable domain (c-nba.dev.coves.social →
//     dev.coves.social). Both are domains the handle already proves control
//     of. A public suffix above the registrable domain (co.uk, github.io) is
//     not: nobody owns it, so it is refused even though it is a parent of the
//     handle.
//   - Anything else is DROPPED with a warning and the community is indexed
//     without an origin. The event is NEVER rejected over this field: a
//     community whose display name would be wrong is still a community, and a
//     refusal would dead-letter every post naming it.
//
// pdsURL and handle are the repo's provenance as admitRecordOrigin established
// it (a fresh resolution when one is available). Empty pdsURL is default-deny,
// exactly as BridgeTrust.TrustsPDS treats it.
func admitCommunityOrigin(bt *BridgeTrust, did, pdsURL, handle, origin string) string {
	if origin == "" {
		return ""
	}

	normalized, err := validation.NormalizeDomain(origin)
	if err != nil {
		log.Printf("WARNING: dropping origin %q on community %s: not a valid hostname (%v); indexing without it", origin, did, err)
		return ""
	}

	if bt.TrustsPDS(pdsURL) {
		return normalized
	}

	handle = strings.ToLower(strings.TrimSpace(handle))
	registrable := extractDomainFromHandle(handle)
	if registrable != "" && (normalized == registrable ||
		(strings.HasSuffix(handle, "."+normalized) && strings.HasSuffix(normalized, "."+registrable))) {
		return normalized
	}

	log.Printf("WARNING: dropping origin %q on community %s: repo (pds=%q) is not a trusted bridge and the origin does not match its handle %q; indexing without it",
		origin, did, pdsURL, handle)
	return ""
}

// extractDomainFromHandle extracts the registrable domain from a community handle
// Handles both formats:
//   - Bluesky-style: "!gaming@coves.social" → "coves.social"
//   - DNS-style: "c-gaming.coves.social" → "coves.social"
//
// Uses golang.org/x/net/publicsuffix to correctly handle multi-part TLDs:
//   - "c-gaming.coves.co.uk" → "coves.co.uk" (not "co.uk")
//   - "c-gaming.example.com.au" → "example.com.au" (not "com.au")
func extractDomainFromHandle(handle string) string {
	// Remove leading ! if present
	handle = strings.TrimPrefix(handle, "!")

	// Check for @-separated format (e.g., "gaming@coves.social")
	if strings.Contains(handle, "@") {
		parts := strings.Split(handle, "@")
		if len(parts) == 2 {
			domain := parts[1]
			// Validate and extract eTLD+1 from the @-domain part
			registrable, err := publicsuffix.EffectiveTLDPlusOne(domain)
			if err != nil {
				// If publicsuffix fails, fall back to returning the full domain part
				// This handles edge cases like localhost, IP addresses, etc.
				log.Printf("DEBUG: publicsuffix failed for @-format handle domain %q, using raw domain: %v", domain, err)
				return domain
			}
			return registrable
		}
		return ""
	}

	// For DNS-style handles (e.g., "c-gaming.coves.social")
	// Extract the registrable domain (eTLD+1) using publicsuffix
	// This correctly handles multi-part TLDs like .co.uk, .com.au, etc.
	registrable, err := publicsuffix.EffectiveTLDPlusOne(handle)
	if err != nil {
		// If publicsuffix fails (e.g., invalid TLD, localhost, IP address)
		// fall back to naive extraction (last 2 parts)
		// WARNING: This is incorrect for multi-part TLDs (.co.uk -> would return "co.uk")
		// but maintains compatibility for localhost/dev environments
		parts := strings.Split(handle, ".")
		if len(parts) < 2 {
			log.Printf("DEBUG: Invalid handle format (no dots): %q", handle)
			return "" // Invalid handle
		}
		fallbackDomain := strings.Join(parts[len(parts)-2:], ".")
		log.Printf("DEBUG: publicsuffix failed for handle %q, using naive fallback: %q (error: %v)", handle, fallbackDomain, err)
		return fallbackDomain
	}

	return registrable
}

// handleSubscription processes subscription create/delete events
// CREATE operation = user subscribed to community
// DELETE operation = user unsubscribed from community
func (c *CommunityEventConsumer) handleSubscription(ctx context.Context, userDID string, commit *CommitEvent) error {
	switch commit.Operation {
	case "create":
		return c.createSubscription(ctx, userDID, commit)
	case "delete":
		return c.deleteSubscription(ctx, userDID, commit)
	default:
		// Update operations shouldn't happen on subscriptions, but ignore gracefully
		log.Printf("Ignoring unexpected operation on subscription: %s (userDID=%s, rkey=%s)",
			commit.Operation, userDID, commit.RKey)
		return nil
	}
}

// createSubscription indexes a new subscription with retry logic
func (c *CommunityEventConsumer) createSubscription(ctx context.Context, userDID string, commit *CommitEvent) error {
	if commit.Record == nil {
		return fmt.Errorf("%w: subscription create event missing record data", ErrPermanentEvent)
	}

	// Extract community DID from record's subject field (following atProto conventions)
	communityDID, ok := commit.Record["subject"].(string)
	if !ok {
		// PERMANENT: structurally invalid record — replays parse identically.
		return fmt.Errorf("%w: subscription record missing subject field", ErrPermanentEvent)
	}

	// Extract contentVisibility with clamping and default value
	contentVisibility := extractContentVisibility(commit.Record)

	// Build AT-URI for subscription record
	// IMPORTANT: Collection is social.coves.community.subscription (record type), not the XRPC endpoint
	// The record lives in the USER's repository, but uses the communities namespace
	uri := fmt.Sprintf("at://%s/social.coves.community.subscription/%s", userDID, commit.RKey)

	// Create subscription entity
	// Parse createdAt from record to preserve chronological ordering during replays
	subscription := &communities.Subscription{
		UserDID:           userDID,
		CommunityDID:      communityDID,
		ContentVisibility: contentVisibility,
		SubscribedAt:      utils.ParseCreatedAt(commit.Record),
		RecordURI:         uri,
		RecordCID:         commit.CID,
	}

	// Use transactional method to ensure subscription and count are atomically updated
	// This is idempotent - safe for Jetstream replays
	_, err := c.repo.SubscribeWithCount(ctx, subscription)
	if err != nil {
		// If already exists, that's fine (idempotency)
		if communities.IsConflict(err) {
			log.Printf("Subscription already indexed: %s -> %s (visibility: %d)",
				userDID, communityDID, contentVisibility)
			return nil
		}
		// Deliberately NOT ErrPermanentEvent: "community not found" here is an
		// ORDERING failure (the community's create event may simply not have been
		// indexed yet) — the redrive will succeed once the community arrives.
		return fmt.Errorf("failed to index subscription: %w", err)
	}

	log.Printf("✓ Indexed subscription: %s -> %s (visibility: %d)",
		userDID, communityDID, contentVisibility)
	return nil
}

// deleteSubscription removes a subscription from the index
// DELETE operations don't include record data, so we need to look up the subscription
// by its URI to find which community the user unsubscribed from.
//
// Cross-rkey safety: the rev gate is per record URI, so it cannot order a
// redriven unsubscribe of rkey A against a newer subscribe under rkey B.
// SubscribeWithCount therefore pins the row's record_uri to the NEWEST record
// (last-write-wins on conflict); the redriven delete of the old URI then finds
// no row here and is skipped instead of tearing down the valid subscription.
func (c *CommunityEventConsumer) deleteSubscription(ctx context.Context, userDID string, commit *CommitEvent) error {
	// Build AT-URI from the rkey
	uri := fmt.Sprintf("at://%s/social.coves.community.subscription/%s", userDID, commit.RKey)

	// Look up the subscription to get the community DID
	// (DELETE operations don't include record data in Jetstream)
	subscription, err := c.repo.GetSubscriptionByURI(ctx, uri)
	if err != nil {
		if communities.IsNotFound(err) {
			// Already deleted - this is fine (idempotency)
			log.Printf("Subscription already deleted: %s", uri)
			return nil
		}
		return fmt.Errorf("failed to find subscription for deletion: %w", err)
	}

	// Use transactional method to ensure unsubscribe and count are atomically updated
	// This is idempotent - safe for Jetstream replays
	err = c.repo.UnsubscribeWithCount(ctx, userDID, subscription.CommunityDID)
	if err != nil {
		if communities.IsNotFound(err) {
			log.Printf("Subscription already removed: %s -> %s", userDID, subscription.CommunityDID)
			return nil
		}
		return fmt.Errorf("failed to remove subscription: %w", err)
	}

	log.Printf("✓ Removed subscription: %s -> %s", userDID, subscription.CommunityDID)
	return nil
}

// handleBlock processes block create/delete events
// CREATE operation = user blocked a community
// DELETE operation = user unblocked a community
func (c *CommunityEventConsumer) handleBlock(ctx context.Context, userDID string, commit *CommitEvent) error {
	switch commit.Operation {
	case "create":
		return c.createBlock(ctx, userDID, commit)
	case "delete":
		return c.deleteBlock(ctx, userDID, commit)
	default:
		// Update operations shouldn't happen on blocks, but ignore gracefully
		log.Printf("Ignoring unexpected operation on block: %s (userDID=%s, rkey=%s)",
			commit.Operation, userDID, commit.RKey)
		return nil
	}
}

// createBlock indexes a new block
func (c *CommunityEventConsumer) createBlock(ctx context.Context, userDID string, commit *CommitEvent) error {
	if commit.Record == nil {
		return fmt.Errorf("%w: block create event missing record data", ErrPermanentEvent)
	}

	// Extract community DID from record's subject field (following atProto conventions)
	communityDID, ok := commit.Record["subject"].(string)
	if !ok {
		// PERMANENT: structurally invalid record — replays parse identically.
		return fmt.Errorf("%w: block record missing subject field", ErrPermanentEvent)
	}

	// Build AT-URI for block record
	// The record lives in the USER's repository
	uri := fmt.Sprintf("at://%s/social.coves.community.block/%s", userDID, commit.RKey)

	// Create block entity
	// Parse createdAt from record to preserve chronological ordering during replays
	block := &communities.CommunityBlock{
		UserDID:      userDID,
		CommunityDID: communityDID,
		BlockedAt:    utils.ParseCreatedAt(commit.Record),
		RecordURI:    uri,
		RecordCID:    commit.CID,
	}

	// Index the block
	// This is idempotent - safe for Jetstream replays
	_, err := c.repo.BlockCommunity(ctx, block)
	if err != nil {
		// If already exists, that's fine (idempotency)
		if communities.IsConflict(err) {
			log.Printf("Block already indexed: %s -> %s", userDID, communityDID)
			return nil
		}
		return fmt.Errorf("failed to index block: %w", err)
	}

	log.Printf("✓ Indexed block: %s -> %s", userDID, communityDID)
	return nil
}

// deleteBlock removes a block from the index
// DELETE operations don't include record data, so we need to look up the block
// by its URI to find which community the user unblocked
func (c *CommunityEventConsumer) deleteBlock(ctx context.Context, userDID string, commit *CommitEvent) error {
	// Build AT-URI from the rkey
	uri := fmt.Sprintf("at://%s/social.coves.community.block/%s", userDID, commit.RKey)

	// Look up the block to get the community DID
	// (DELETE operations don't include record data in Jetstream)
	block, err := c.repo.GetBlockByURI(ctx, uri)
	if err != nil {
		if communities.IsNotFound(err) {
			// Already deleted - this is fine (idempotency)
			log.Printf("Block already deleted: %s", uri)
			return nil
		}
		return fmt.Errorf("failed to find block for deletion: %w", err)
	}

	// Remove the block from the index
	err = c.repo.UnblockCommunity(ctx, userDID, block.CommunityDID)
	if err != nil {
		if communities.IsNotFound(err) {
			log.Printf("Block already removed: %s -> %s", userDID, block.CommunityDID)
			return nil
		}
		return fmt.Errorf("failed to remove block: %w", err)
	}

	log.Printf("✓ Removed block: %s -> %s", userDID, block.CommunityDID)
	return nil
}

// Helper types and functions

// sanitizedDescriptionFacets drops description facets whose byte ranges fall
// outside the community description (or are otherwise structurally invalid)
// before indexing, mirroring the post/comment consumers: firehose records from
// federated repos cannot be rejected back to their author, and clients must
// never receive ranges that slice outside the description. Returns nil when no
// facets survive.
func sanitizedDescriptionFacets(profile *CommunityProfile, did string) []interface{} {
	if profile.DescriptionFacets == nil {
		return nil
	}
	kept, dropped := richtext.SanitizeFacets(profile.DescriptionFacets, len(profile.Description))
	if dropped > 0 {
		log.Printf("Warning: dropped %d invalid description facet(s) on community %s during indexing", dropped, did)
	}
	return kept
}

type CommunityProfile struct {
	CreatedAt         time.Time              `json:"createdAt"`
	Avatar            map[string]interface{} `json:"avatar"`
	Banner            map[string]interface{} `json:"banner"`
	CreatedBy         string                 `json:"createdBy"`
	Visibility        string                 `json:"visibility"`
	AtprotoHandle     string                 `json:"atprotoHandle"`
	DisplayName       string                 `json:"displayName"`
	Name              string                 `json:"name"`
	Handle            string                 `json:"handle"`
	Origin            string                 `json:"origin"`
	HostedBy          string                 `json:"hostedBy"`
	Description       string                 `json:"description"`
	FederatedID       string                 `json:"federatedId"`
	ModerationType    string                 `json:"moderationType"`
	FederatedFrom     string                 `json:"federatedFrom"`
	ContentWarnings   []string               `json:"contentWarnings"`
	DescriptionFacets []interface{}          `json:"descriptionFacets"`
	MemberCount       int                    `json:"memberCount"`
	SubscriberCount   int                    `json:"subscriberCount"`
	Federation        FederationConfig       `json:"federation"`
}

type FederationConfig struct {
	AllowExternalDiscovery bool `json:"allowExternalDiscovery"`
}

// parseCommunityProfile converts a raw record map to a CommunityProfile
func parseCommunityProfile(record map[string]interface{}) (*CommunityProfile, error) {
	recordJSON, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal record: %w", err)
	}

	var profile CommunityProfile
	if err := json.Unmarshal(recordJSON, &profile); err != nil {
		// PERMANENT: the record's shape doesn't match the lexicon; replaying the
		// identical bytes can never parse differently.
		return nil, fmt.Errorf("%w: failed to unmarshal profile: %v", ErrPermanentEvent, err)
	}

	// The lexicon marks visibility optional with default "public"
	// (lexicons/social/coves/community/profile.json). A record that omits
	// it is valid — writers relying on the declared default (the Tidepool
	// bridge does) must not fail the communities_visibility_check
	// constraint with an empty string.
	if profile.Visibility == "" {
		profile.Visibility = "public"
	}

	return &profile, nil
}

// constructHandleFromProfile constructs a deterministic handle from profile data
// Format: c-{name}.{instanceDomain}
// Example: c-gaming.coves.social
// This is ONLY used in test mode (when identity resolver is nil)
// Production MUST resolve handles from PLC (source of truth)
// Returns empty string if hostedBy is not did:web format (caller will fail validation)
func constructHandleFromProfile(profile *CommunityProfile) string {
	if !strings.HasPrefix(profile.HostedBy, "did:web:") {
		// hostedBy must be did:web format for handle construction
		// Log warning since this indicates invalid community data
		log.Printf("WARNING: constructHandleFromProfile: hostedBy %q is not did:web format, cannot construct handle for community %q",
			profile.HostedBy, profile.Name)
		// Return empty to trigger validation error in repository
		return ""
	}
	instanceDomain := strings.TrimPrefix(profile.HostedBy, "did:web:")
	return fmt.Sprintf("c-%s.%s", profile.Name, instanceDomain)
}

// extractContentVisibility extracts contentVisibility from subscription record with clamping
// Returns default value of 3 if missing or invalid
func extractContentVisibility(record map[string]interface{}) int {
	const defaultVisibility = 3

	cv, ok := record["contentVisibility"]
	if !ok {
		// Field missing - use default
		return defaultVisibility
	}

	// JSON numbers decode as float64
	cvFloat, ok := cv.(float64)
	if !ok {
		// Try int (shouldn't happen but handle gracefully)
		if cvInt, isInt := cv.(int); isInt {
			return clampContentVisibility(cvInt)
		}
		log.Printf("WARNING: contentVisibility has unexpected type %T, using default", cv)
		return defaultVisibility
	}

	// Convert and clamp
	clamped := clampContentVisibility(int(cvFloat))
	if clamped != int(cvFloat) {
		log.Printf("WARNING: Clamped contentVisibility from %d to %d", int(cvFloat), clamped)
	}
	return clamped
}

// clampContentVisibility ensures value is within valid range (1-5)
func clampContentVisibility(value int) int {
	if value < 1 {
		return 1
	}
	if value > 5 {
		return 5
	}
	return value
}

// extractBlobCID extracts the CID from a blob reference
// Blob format: {"$type": "blob", "ref": {"$link": "cid"}, "mimeType": "...", "size": 123}
func extractBlobCID(blob map[string]interface{}) (string, bool) {
	if blob == nil {
		return "", false
	}

	// Check if it's a blob type
	blobType, ok := blob["$type"].(string)
	if !ok || blobType != "blob" {
		return "", false
	}

	// Extract ref
	ref, ok := blob["ref"].(map[string]interface{})
	if !ok {
		return "", false
	}

	// Extract $link (the CID)
	link, ok := ref["$link"].(string)
	if !ok {
		return "", false
	}

	return link, true
}
