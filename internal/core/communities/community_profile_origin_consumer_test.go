//go:build integration

package communities_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"Coves/internal/atproto/identity"
	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/communities"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"
)

// How a community profile's originUrl and originSoftware become row values.
//
// Both fields are display data written by a trusted bridge. The consumer keeps
// them only when the community's identity was resolved fresh (document and
// Resolve, through the uncached resolver), verified, on a trusted bridge PDS
// whose registrable domain matches the verified handle's, and the record's
// origin was admitted. One shared harness drives every case; cases are rows.

const (
	trustedBridgePDS   = "https://bridge.test"
	untrustedPDS       = "https://pds.example.com"
	originDisplayCID   = "bafyorigindisplay"
	gamingOriginURL    = "https://lemmy.world/c/gaming"
	gamingOriginHost   = "lemmy.world"
	gamingSoftwareRaw  = "Lemmy"
	gamingSoftwareKept = "lemmy"
)

// scriptedCommunityResolver answers identity questions about one DID from a
// script and counts what it was asked. It serves as both the caching
// (positional) resolver and the uncached resolver.
type scriptedCommunityResolver struct {
	wantDID string
	// answer is what Resolve returns; the declared document is derived from it
	// unless declared is set.
	answer      *identity.Identity
	declared    *identity.DeclaredIdentity
	resolveErr  error
	documentErr error

	resolves      int
	documentReads int
	purges        int
}

func (r *scriptedCommunityResolver) Resolve(_ context.Context, identifier string) (*identity.Identity, error) {
	r.resolves++
	if identifier != r.wantDID {
		return nil, errors.New("resolved a different identifier than the event DID")
	}
	if r.resolveErr != nil {
		return nil, r.resolveErr
	}
	return r.answer, nil
}

func (r *scriptedCommunityResolver) ResolveDeclaredIdentity(_ context.Context, did string) (*identity.DeclaredIdentity, error) {
	r.documentReads++
	if did != r.wantDID {
		return nil, errors.New("read the document of a different DID than the event DID")
	}
	if r.documentErr != nil {
		return nil, r.documentErr
	}
	if r.declared != nil {
		return r.declared, nil
	}
	if r.answer == nil {
		return nil, &identity.ErrNotFound{Identifier: did, Reason: "no scripted document"}
	}
	return &identity.DeclaredIdentity{DID: r.answer.DID, DeclaredHandle: r.answer.Handle, PDSURL: r.answer.PDSURL}, nil
}

func (r *scriptedCommunityResolver) ResolveHandle(context.Context, string) (string, string, error) {
	return "", "", errors.New("scriptedCommunityResolver: ResolveHandle is not scripted")
}

func (r *scriptedCommunityResolver) ResolveDID(context.Context, string) (*identity.DIDDocument, error) {
	return nil, errors.New("scriptedCommunityResolver: ResolveDID is not scripted")
}

func (r *scriptedCommunityResolver) Purge(context.Context, string) error {
	r.purges++
	return nil
}

// countingCommunityResolver wraps the resolver the consumer holds as its
// caching resolver and counts Resolve and Purge calls.
type countingCommunityResolver struct {
	identity.Resolver
	resolves int
	purges   int
}

func (r *countingCommunityResolver) Resolve(ctx context.Context, identifier string) (*identity.Identity, error) {
	r.resolves++
	return r.Resolver.Resolve(ctx, identifier)
}

func (r *countingCommunityResolver) Purge(ctx context.Context, identifier string) error {
	r.purges++
	return r.Resolver.Purge(ctx, identifier)
}

// communityOriginHarness is one community DID, a real Postgres repository, a
// consumer trusting https://bridge.test, and the two resolvers.
type communityOriginHarness struct {
	db       *sql.DB
	repo     communities.Repository
	consumer *jetstream.CommunityEventConsumer
	// caching is the scripted caching resolver; nil with
	// withRealCachingResolver, where realCaching counts the real one instead.
	caching     *scriptedCommunityResolver
	realCaching *countingCommunityResolver
	// uncached is nil with withoutUncachedResolver.
	uncached *scriptedCommunityResolver
	did      string
}

type communityOriginHarnessConfig struct {
	withoutUncached bool
	revGate         bool
	realCaching     bool
	refreshLock     bool
	wrapRepository  func(communities.Repository) communities.Repository
	wrapCaching     func(identity.Resolver) identity.Resolver
	// verifyAs, when set, builds the consumer with hostedBy verification on
	// and this instance DID.
	verifyAs string
}

type communityOriginHarnessOption func(*communityOriginHarnessConfig)

// withoutUncachedResolver builds the consumer with no uncached resolver.
func withoutUncachedResolver() communityOriginHarnessOption {
	return func(c *communityOriginHarnessConfig) { c.withoutUncached = true }
}

// withRevGate installs the rev gate on the harness database.
func withRevGate() communityOriginHarnessOption {
	return func(c *communityOriginHarnessConfig) { c.revGate = true }
}

// withRealCachingResolver makes the caching resolver the real
// identity.NewResolver over the harness database (local PLC), wrapped in a
// counter; seed its answer with seedCachedIdentity.
func withRealCachingResolver() communityOriginHarnessOption {
	return func(c *communityOriginHarnessConfig) { c.realCaching = true }
}

// withIdentityRefreshLock gives the consumer the Postgres per-DID identity
// refresh lock over the harness database.
func withIdentityRefreshLock() communityOriginHarnessOption {
	return func(c *communityOriginHarnessConfig) { c.refreshLock = true }
}

// withConsumerRepository hands the consumer wrap(repo) instead of the real
// repository; seeding and reads still go through the real one.
func withConsumerRepository(wrap func(communities.Repository) communities.Repository) communityOriginHarnessOption {
	return func(c *communityOriginHarnessConfig) { c.wrapRepository = wrap }
}

// withCachingResolver hands the consumer wrap(real caching resolver); it
// implies withRealCachingResolver.
func withCachingResolver(wrap func(identity.Resolver) identity.Resolver) communityOriginHarnessOption {
	return func(c *communityOriginHarnessConfig) { c.realCaching, c.wrapCaching = true, wrap }
}

// withHostedByVerification builds the consumer with hostedBy verification
// enabled (skipVerification=false) and consumerInstanceDID as its instance DID.
func withHostedByVerification(consumerInstanceDID string) communityOriginHarnessOption {
	return func(c *communityOriginHarnessConfig) { c.verifyAs = consumerInstanceDID }
}

// identityOn is a resolution answering did with handle on pdsURL.
func identityOn(did, handle, pdsURL string) *identity.Identity {
	return &identity.Identity{
		DID: did, Handle: handle, PDSURL: pdsURL,
		Method: identity.MethodDNS, ResolvedAt: time.Now().UTC(),
	}
}

// newCommunityOriginHarness scripts both resolvers to answer did with the
// given handle on the given PDS; rows override the scripts before delivery.
func newCommunityOriginHarness(t *testing.T, did, handle, pdsURL string, opts ...communityOriginHarnessOption) *communityOriginHarness {
	t.Helper()
	var config communityOriginHarnessConfig
	for _, opt := range opts {
		opt(&config)
	}
	db := testkit.DB(t)
	h := &communityOriginHarness{db: db, did: did}
	h.repo = postgres.NewCommunityRepository(db, credentialciphertest.Fixed())

	consumerOpts := []jetstream.CommunityConsumerOption{
		jetstream.WithCommunityBridgeTrust(jetstream.NewBridgeTrust([]string{trustedBridgePDS})),
	}
	if !config.withoutUncached {
		h.uncached = &scriptedCommunityResolver{wantDID: did, answer: identityOn(did, handle, pdsURL)}
		consumerOpts = append(consumerOpts, jetstream.WithCommunityUncachedIdentityResolver(h.uncached))
	}
	if config.revGate {
		consumerOpts = append(consumerOpts, jetstream.WithCommunityRevGate(jetstream.NewRevGate(db)))
	}
	if config.refreshLock {
		consumerOpts = append(consumerOpts, jetstream.WithCommunityIdentityRefreshLock(
			jetstream.NewPostgresIdentityRefreshLock(db, jetstream.IdentityRefreshLockWaitTimeout)))
	}
	consumerRepo := h.repo
	if config.wrapRepository != nil {
		consumerRepo = config.wrapRepository(h.repo)
	}
	consumerInstanceDID, skipVerification := instanceDID, true
	if config.verifyAs != "" {
		consumerInstanceDID, skipVerification = config.verifyAs, false
	}
	if config.realCaching {
		h.realCaching = &countingCommunityResolver{Resolver: identity.NewResolver(db, identity.Config{
			PLCURL:   testkit.Endpoints().PLC.BaseURL,
			CacheTTL: 24 * time.Hour,
		})}
		var caching identity.Resolver = h.realCaching
		if config.wrapCaching != nil {
			caching = config.wrapCaching(caching)
		}
		h.consumer = jetstream.NewCommunityEventConsumer(consumerRepo, consumerInstanceDID, skipVerification, caching, consumerOpts...)
		return h
	}
	h.caching = &scriptedCommunityResolver{wantDID: did, answer: identityOn(did, handle, pdsURL)}
	h.consumer = jetstream.NewCommunityEventConsumer(consumerRepo, consumerInstanceDID, skipVerification, h.caching, consumerOpts...)
	return h
}

// seedCachedIdentity writes an identity_cache row for the harness DID and
// checks the real caching resolver serves it, then zeroes its call count.
func (h *communityOriginHarness) seedCachedIdentity(t *testing.T, handle, pdsURL string) {
	t.Helper()
	if err := identity.NewPostgresCache(h.db, 24*time.Hour).Set(context.Background(), identityOn(h.did, handle, pdsURL)); err != nil {
		t.Fatalf("seed cached identity: %v", err)
	}
	got, err := h.realCaching.Resolve(context.Background(), h.did)
	if err != nil || got == nil || got.DID != h.did || got.Handle != handle || got.PDSURL != pdsURL {
		t.Fatalf("caching resolver did not serve the cached identity: identity=%+v, err=%v", got, err)
	}
	h.realCaching.resolves = 0
	h.realCaching.purges = 0
}

// seededCommunity is the stored row a case starts from. An empty hostedByDID
// stores instanceDID.
type seededCommunity struct {
	name, handle, pdsURL, description string
	origin, originURL, originSoftware string
	hostedByDID                       string
}

// seed writes the community row directly through the repository and checks the
// display fields landed, so a case never starts from a row it did not intend.
func (h *communityOriginHarness) seed(t *testing.T, seeded seededCommunity) {
	t.Helper()
	h.seedDID(t, h.did, seeded)
}

// seedDID is seed for a community other than the harness DID.
func (h *communityOriginHarness) seedDID(t *testing.T, did string, seeded seededCommunity) {
	t.Helper()
	hostedByDID := seeded.hostedByDID
	if hostedByDID == "" {
		hostedByDID = instanceDID
	}
	now := time.Now().UTC()
	if _, err := h.repo.Create(context.Background(), &communities.Community{
		DID: did, Handle: seeded.handle, Name: seeded.name,
		DisplayName: "Seeded " + seeded.name, Description: seeded.description,
		OwnerDID: did, CreatedByDID: "did:plc:communityconsumer", HostedByDID: hostedByDID,
		Visibility: "public", PDSURL: seeded.pdsURL,
		Origin: seeded.origin, OriginURL: seeded.originURL, OriginSoftware: seeded.originSoftware,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed community %s: %v", did, err)
	}
	stored, err := h.repo.GetByDID(context.Background(), did)
	if err != nil {
		t.Fatalf("read community %s: %v", did, err)
	}
	if stored.HostedByDID != hostedByDID {
		t.Fatalf("seeded hosted_by_did = %q, want %q stored before delivery", stored.HostedByDID, hostedByDID)
	}
	if got, want := (originDisplay{stored.Origin, stored.OriginURL, stored.OriginSoftware}),
		(originDisplay{seeded.origin, seeded.originURL, seeded.originSoftware}); got != want {
		t.Fatalf("seeded origin display = %+v, want %+v stored before delivery", got, want)
	}
	byHandle, err := h.repo.GetByHandle(context.Background(), seeded.handle)
	if err != nil {
		t.Fatalf("read community by handle %s: %v", seeded.handle, err)
	}
	if byHandle.OriginURL != seeded.originURL || byHandle.OriginSoftware != seeded.originSoftware {
		t.Fatalf("GetByHandle(%s) originUrl, originSoftware = %q, %q, want %q, %q",
			seeded.handle, byHandle.OriginURL, byHandle.OriginSoftware, seeded.originURL, seeded.originSoftware)
	}
}

// deliver hands the consumer one profile commit for the harness DID.
func (h *communityOriginHarness) deliver(operation string, record map[string]interface{}) error {
	return h.deliverEvent(h.profileCommit(operation, record))
}

// profileCommit is one profile commit for the harness DID with its own rev;
// delivering the same value again replays that rev.
func (h *communityOriginHarness) profileCommit(operation string, record map[string]interface{}) *jetstream.JetstreamEvent {
	return profileEvent(h.did, operation, "self", originDisplayCID, record)
}

func (h *communityOriginHarness) deliverEvent(event *jetstream.JetstreamEvent) error {
	return h.consumer.HandleEvent(context.Background(), event)
}

func (h *communityOriginHarness) read(t *testing.T) *communities.Community {
	t.Helper()
	stored, err := h.repo.GetByDID(context.Background(), h.did)
	if err != nil {
		t.Fatalf("read community %s: %v", h.did, err)
	}
	return stored
}

type originDisplay struct {
	origin, url, software string
}

func (h *communityOriginHarness) assertOriginDisplay(t *testing.T, want originDisplay) {
	t.Helper()
	stored := h.read(t)
	if got := (originDisplay{stored.Origin, stored.OriginURL, stored.OriginSoftware}); got != want {
		t.Errorf("stored origin display = %+v, want %+v", got, want)
	}
}

// assertDisplayFields checks originUrl and originSoftware, not origin.
func (h *communityOriginHarness) assertDisplayFields(t *testing.T, wantURL, wantSoftware string) {
	t.Helper()
	stored := h.read(t)
	if stored.OriginURL != wantURL || stored.OriginSoftware != wantSoftware {
		t.Errorf("stored originUrl, originSoftware = %q, %q, want %q, %q",
			stored.OriginURL, stored.OriginSoftware, wantURL, wantSoftware)
	}
}

func (h *communityOriginHarness) assertProfileText(t *testing.T, wantDisplayName, wantDescription string) {
	t.Helper()
	stored := h.read(t)
	if stored.DisplayName != wantDisplayName || stored.Description != wantDescription {
		t.Errorf("stored displayName, description = %q, %q, want %q, %q",
			stored.DisplayName, stored.Description, wantDisplayName, wantDescription)
	}
}

func (h *communityOriginHarness) assertNoCommunity(t *testing.T) {
	t.Helper()
	stored, err := h.repo.GetByDID(context.Background(), h.did)
	if !communities.IsNotFound(err) {
		found := ""
		if stored != nil {
			found = stored.Handle
		}
		t.Errorf("GetByDID(%s) = community %q, err %v; want a not-found error (no community row)", h.did, found, err)
	}
}

// originRecord is profileRecord(name) carrying the given origin fields; an
// empty argument leaves its key out.
func originRecord(name, origin, originURL, originSoftware string) map[string]interface{} {
	record := profileRecord(name)
	for key, value := range map[string]string{"origin": origin, "originUrl": originURL, "originSoftware": originSoftware} {
		if value != "" {
			record[key] = value
		}
	}
	return record
}

// TestCommunityProfileOrigin_TrustedBridgeCreateStoresDisplayFields is the
// acceptance case: a bridged community verified fresh on the trusted bridge
// keeps both display fields, software normalised.
func TestCommunityProfileOrigin_TrustedBridgeCreateStoresDisplayFields(t *testing.T) {
	t.Parallel()
	h := newCommunityOriginHarness(t, "did:plc:origindisplayb3accept",
		"gaming.lemmy.world.ap.bridge.test", trustedBridgePDS)

	if err := h.deliver("create", originRecord("gamingb3accept",
		gamingOriginHost, gamingOriginURL, gamingSoftwareRaw)); err != nil {
		t.Fatalf("create community profile: %v", err)
	}

	h.assertOriginDisplay(t, originDisplay{"lemmy.world", "https://lemmy.world/c/gaming", "lemmy"})
}

// TestCommunityProfileOrigin_TrustedCreateKeepsOnlySafeOriginURL covers the
// originUrl rule and the origin prerequisite on a trusted, verified community.
func TestCommunityProfileOrigin_TrustedCreateKeepsOnlySafeOriginURL(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, did, community, origin, originURL string
		want                                    originDisplay
	}{
		{
			name: "http URL is dropped, software kept", did: "did:plc:origindisplayb4http", community: "gamingb4http",
			origin: gamingOriginHost, originURL: "http://lemmy.world/c/gaming",
			want: originDisplay{"lemmy.world", "", "lemmy"},
		},
		{
			name: "URL on a foreign host is dropped, software kept", did: "did:plc:origindisplayb4foreign", community: "gamingb4foreign",
			origin: gamingOriginHost, originURL: "https://evil.example/c/gaming",
			want: originDisplay{"lemmy.world", "", "lemmy"},
		},
		{
			name: "URL with userinfo is dropped, software kept", did: "did:plc:origindisplayb4userinfo", community: "gamingb4userinfo",
			origin: gamingOriginHost, originURL: "https://evil.example%2F@lemmy.world/c/gaming",
			want: originDisplay{"lemmy.world", "", "lemmy"},
		},
		{
			name: "record without origin stores neither field", did: "did:plc:origindisplayb4noorigin", community: "gamingb4noorigin",
			origin: "", originURL: gamingOriginURL,
			want: originDisplay{"", "", ""},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newCommunityOriginHarness(t, tc.did, tc.community+".lemmy.world.ap.bridge.test", trustedBridgePDS)

			if err := h.deliver("create", originRecord(tc.community, tc.origin, tc.originURL, gamingSoftwareRaw)); err != nil {
				t.Fatalf("create community profile: %v", err)
			}

			h.assertOriginDisplay(t, tc.want)
		})
	}
}

// TestCommunityProfileOrigin_UntrustedCreateStoresNeitherField: a community on
// an untrusted PDS keeps its own-domain origin but neither display field.
func TestCommunityProfileOrigin_UntrustedCreateStoresNeitherField(t *testing.T) {
	t.Parallel()
	h := newCommunityOriginHarness(t, "did:plc:origindisplayb5untrusted", "gaming.example.com", untrustedPDS)

	if err := h.deliver("create", originRecord("gamingb5untrusted",
		"example.com", "https://example.com/c/gaming", gamingSoftwareKept)); err != nil {
		t.Fatalf("create community profile: %v", err)
	}

	h.assertOriginDisplay(t, originDisplay{"example.com", "", ""})
}

const (
	seededOriginURL      = "https://lemmy.world/c/seeded"
	seededOriginSoftware = "seededsoft"
	anotherRepoDID       = "did:plc:origindisplayanotherrepo"
)

var errDirectoryUnavailable = errors.New("directory unavailable")

// bridgeHandle is the bridge-domain handle a trusted community verifies as.
func bridgeHandle(community string) string {
	return community + ".lemmy.world.ap.bridge.test"
}

// seedTrusted stores community on the trusted bridge with origin lemmy.world,
// both display fields, and description.
func (h *communityOriginHarness) seedTrusted(t *testing.T, community, description string) {
	t.Helper()
	h.seed(t, seededCommunity{
		name: community, handle: bridgeHandle(community), pdsURL: trustedBridgePDS, description: description,
		origin: gamingOriginHost, originURL: seededOriginURL, originSoftware: seededOriginSoftware,
	})
}

// A resolutionScript sets every answer of the uncached resolver for a
// community verified as handle on the trusted bridge.
type resolutionScript func(r *scriptedCommunityResolver, did, handle string)

func directoryUnavailable(r *scriptedCommunityResolver, _, _ string) {
	r.declared, r.answer = nil, nil
	r.documentErr, r.resolveErr = errDirectoryUnavailable, errDirectoryUnavailable
}

// resolveUnavailable: the document declares handle on the bridge, so it admits
// the lookup; Resolve errors.
func resolveUnavailable(r *scriptedCommunityResolver, did, handle string) {
	r.documentErr, r.resolveErr = nil, errDirectoryUnavailable
	r.declared = &identity.DeclaredIdentity{DID: did, DeclaredHandle: handle, PDSURL: trustedBridgePDS}
	r.answer = nil
}

// resolvesHandleInvalid: the document declares handle on the bridge; Resolve
// answers handle.invalid.
func resolvesHandleInvalid(r *scriptedCommunityResolver, did, handle string) {
	r.documentErr, r.resolveErr = nil, nil
	r.declared = &identity.DeclaredIdentity{DID: did, DeclaredHandle: handle, PDSURL: trustedBridgePDS}
	r.answer = identityOn(did, identity.InvalidHandle, trustedBridgePDS)
}

func verifiedOnBridge(r *scriptedCommunityResolver, did, handle string) {
	r.documentErr, r.resolveErr = nil, nil
	r.declared = nil
	r.answer = identityOn(did, handle, trustedBridgePDS)
}

func documentForAnotherDID(r *scriptedCommunityResolver, did, handle string) {
	r.documentErr, r.resolveErr = nil, nil
	r.declared = &identity.DeclaredIdentity{DID: anotherRepoDID, DeclaredHandle: handle, PDSURL: trustedBridgePDS}
	r.answer = identityOn(did, handle, trustedBridgePDS)
}

func resolveForAnotherDID(r *scriptedCommunityResolver, did, handle string) {
	r.documentErr, r.resolveErr = nil, nil
	r.declared = &identity.DeclaredIdentity{DID: did, DeclaredHandle: handle, PDSURL: trustedBridgePDS}
	r.answer = identityOn(anotherRepoDID, handle, trustedBridgePDS)
}

// TestCommunityProfileOrigin_TrustedUpdateReplacesBothDisplayFields: every
// update replaces both fields; a field the record omits is cleared.
func TestCommunityProfileOrigin_TrustedUpdateReplacesBothDisplayFields(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, did, community, originURL, originSoftware string
		want                                            originDisplay
	}{
		{
			name: "update omitting both fields clears both", did: "did:plc:origindisplayb6omit", community: "gamingb6omit",
			want: originDisplay{"lemmy.world", "", ""},
		},
		{
			name: "update carrying only originSoftware clears the URL", did: "did:plc:origindisplayb6software", community: "gamingb6software",
			originSoftware: "piefed",
			want:           originDisplay{"lemmy.world", "", "piefed"},
		},
		{
			name: "update carrying only originUrl clears the software", did: "did:plc:origindisplayb6url", community: "gamingb6url",
			originURL: "https://lemmy.world/c/gaming2",
			want:      originDisplay{"lemmy.world", "https://lemmy.world/c/gaming2", ""},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newCommunityOriginHarness(t, tc.did, bridgeHandle(tc.community), trustedBridgePDS)
			h.seedTrusted(t, tc.community, "seeded b6 description")

			if err := h.deliver("update", originRecord(tc.community, gamingOriginHost, tc.originURL, tc.originSoftware)); err != nil {
				t.Fatalf("update community profile: %v", err)
			}

			h.assertOriginDisplay(t, tc.want)
		})
	}
}

// TestCommunityProfileOrigin_FreshUntrustedResolutionOverridesTrustedCache: a
// cached identity on the bridge does not admit the fields when the fresh
// resolution verifies the repo on an untrusted PDS, and the caching resolver
// is not asked.
func TestCommunityProfileOrigin_FreshUntrustedResolutionOverridesTrustedCache(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, did, community string
		declaresBridge       bool
	}{
		{name: "document and Resolve both name the untrusted PDS", did: "did:plc:origindisplayb7fresh", community: "gamingb7fresh"},
		{name: "document names the bridge, Resolve names the untrusted PDS", did: "did:plc:origindisplayb7declared", community: "gamingb7declared", declaresBridge: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			handle := bridgeHandle(tc.community)
			h := newCommunityOriginHarness(t, tc.did, handle, untrustedPDS, withRealCachingResolver())
			if tc.declaresBridge {
				h.uncached.declared = &identity.DeclaredIdentity{DID: tc.did, DeclaredHandle: handle, PDSURL: trustedBridgePDS}
			}
			h.seedCachedIdentity(t, handle, trustedBridgePDS)
			h.seedTrusted(t, tc.community, "seeded b7 description")

			if err := h.deliver("update", originRecord(tc.community,
				gamingOriginHost, "https://lemmy.world/c/gamingb7", "mbin")); err != nil {
				t.Fatalf("update community profile: %v", err)
			}

			h.assertDisplayFields(t, "", "")
			if h.realCaching.resolves != 0 {
				t.Errorf("caching resolver Resolve calls = %d, want 0", h.realCaching.resolves)
			}
		})
	}
}

// TestCommunityProfileOrigin_UntrustedDocumentUpdateAppliesWithoutResolving: a
// known community whose fresh DID document names an untrusted PDS can never
// have the display fields admitted, so its update is applied against the stored
// handle and clears them, and neither resolver verifies the handle. A Resolve
// that would fail or answer handle.invalid is therefore never reached.
func TestCommunityProfileOrigin_UntrustedDocumentUpdateAppliesWithoutResolving(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, did, community string
		script               resolutionScript
	}{
		{
			name: "Resolve would error", did: "did:plc:origindisplayf1resolveerror", community: "gamingf1resolveerror",
			script: func(r *scriptedCommunityResolver, _, _ string) { r.resolveErr = errDirectoryUnavailable },
		},
		{
			name: "Resolve would answer handle.invalid", did: "did:plc:origindisplayf1handleinvalid", community: "gamingf1handleinvalid",
			script: func(r *scriptedCommunityResolver, did, handle string) {
				r.declared = &identity.DeclaredIdentity{DID: did, DeclaredHandle: handle, PDSURL: untrustedPDS}
				r.answer = identityOn(did, identity.InvalidHandle, untrustedPDS)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			handle := tc.community + ".example.com"
			h := newCommunityOriginHarness(t, tc.did, handle, untrustedPDS)
			tc.script(h.uncached, tc.did, handle)
			h.seed(t, seededCommunity{
				name: tc.community, handle: handle, pdsURL: untrustedPDS, description: "seeded f1 description",
				origin: "example.com", originURL: "https://example.com/c/seeded", originSoftware: seededOriginSoftware,
			})
			record := originRecord(tc.community, "example.com", "https://example.com/c/gamingf1", "mbin")
			record["description"] = "applied f1 description"

			if err := h.deliver("update", record); err != nil {
				t.Fatalf("update community profile: %v", err)
			}

			h.assertOriginDisplay(t, originDisplay{"example.com", "", ""})
			if got := h.read(t).Description; got != "applied f1 description" {
				t.Errorf("stored description = %q, want %q", got, "applied f1 description")
			}
			if h.uncached.resolves != 0 || h.caching.resolves != 0 {
				t.Errorf("Resolve calls: uncached %d, caching %d; want 0 and 0", h.uncached.resolves, h.caching.resolves)
			}
		})
	}
}

// declareBridgeWithHandleOutsideIt scripts an untrusted DID document: it names
// the trusted bridge PDS but declares a handle outside the bridge host's
// registrable domain, so no resolver verifies anything.
func declareBridgeWithHandleOutsideIt(r *scriptedCommunityResolver, did, outsideHandle string) {
	r.documentErr, r.resolveErr = nil, nil
	r.declared = &identity.DeclaredIdentity{DID: did, DeclaredHandle: outsideHandle, PDSURL: trustedBridgePDS}
	r.answer = identityOn(did, outsideHandle, trustedBridgePDS)
}

// TestCommunityProfileOrigin_UntrustedDocumentOriginMeasuredAgainstStoredPDS:
// an untrusted DID document that names the trusted bridge PDS is not
// verification, so an off-bridge community cannot use it to assert a foreign
// origin. The origin is measured against the stored pds_url and handle; the
// rest of the edit applies and neither resolver runs.
func TestCommunityProfileOrigin_UntrustedDocumentOriginMeasuredAgainstStoredPDS(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, did, community, storedPDSURL string
	}{
		{name: "stored pds_url untrusted", did: "did:plc:origindisplayr2untrusted", community: "nbar2untrusted", storedPDSURL: untrustedPDS},
		{name: "stored pds_url empty", did: "did:plc:origindisplayr2empty", community: "nbar2empty", storedPDSURL: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			handle := "c-" + tc.community + ".evil.test"
			h := newCommunityOriginHarness(t, tc.did, handle, tc.storedPDSURL)
			declareBridgeWithHandleOutsideIt(h.uncached, tc.did, handle)
			h.seed(t, seededCommunity{
				name: tc.community, handle: handle, pdsURL: tc.storedPDSURL, description: "seeded r2 description",
			})
			record := originRecord(tc.community, "coves.social", "https://coves.social/c/nba", "lemmy")
			record["displayName"] = "Applied R2 Name"
			record["description"] = "applied r2 description"

			if err := h.deliver("update", record); err != nil {
				t.Fatalf("update community profile: %v", err)
			}

			h.assertOriginDisplay(t, originDisplay{"", "", ""})
			h.assertProfileText(t, "Applied R2 Name", "applied r2 description")
			if got := h.read(t).PDSURL; got != tc.storedPDSURL {
				t.Errorf("stored pds_url = %q, want %q (unchanged)", got, tc.storedPDSURL)
			}
			if h.uncached.resolves != 0 || h.caching.resolves != 0 {
				t.Errorf("Resolve calls: uncached %d, caching %d; want 0 and 0", h.uncached.resolves, h.caching.resolves)
			}
		})
	}
}

// TestCommunityProfileOrigin_UntrustedDocumentKeepsBridgedOrigin: a community
// stored on the trusted bridge keeps its origin when a profile edit arrives
// with an untrusted DID document; only the display fields are cleared.
func TestCommunityProfileOrigin_UntrustedDocumentKeepsBridgedOrigin(t *testing.T) {
	t.Parallel()
	const did, community = "did:plc:origindisplayr2bridged", "gamingr2bridged"
	h := newCommunityOriginHarness(t, did, bridgeHandle(community), trustedBridgePDS)
	declareBridgeWithHandleOutsideIt(h.uncached, did, community+".other.test")
	h.seedTrusted(t, community, "seeded r2 description")
	record := originRecord(community, gamingOriginHost, "https://lemmy.world/c/gamingr2", "mbin")
	record["description"] = "applied r2 description"

	if err := h.deliver("update", record); err != nil {
		t.Fatalf("update community profile: %v", err)
	}

	h.assertOriginDisplay(t, originDisplay{gamingOriginHost, "", ""})
	if got := h.read(t).PDSURL; got != trustedBridgePDS {
		t.Errorf("stored pds_url = %q, want %q (unchanged)", got, trustedBridgePDS)
	}
	if h.uncached.resolves != 0 || h.caching.resolves != 0 {
		t.Errorf("Resolve calls: uncached %d, caching %d; want 0 and 0", h.uncached.resolves, h.caching.resolves)
	}
}

// TestCommunityProfileOrigin_UnresolvedFreshIdentityLeavesUpdateForRedrive:
// the same update (one rev) fails while the fresh resolution cannot verify the
// repo, leaves the row as it was, and applies once it can.
func TestCommunityProfileOrigin_UnresolvedFreshIdentityLeavesUpdateForRedrive(t *testing.T) {
	t.Parallel()
	const did, community = "did:plc:origindisplayb8update", "gamingb8update"
	handle := bridgeHandle(community)
	h := newCommunityOriginHarness(t, did, handle, trustedBridgePDS, withRevGate())
	h.seedTrusted(t, community, "seeded b8 description")
	record := originRecord(community, gamingOriginHost, "https://lemmy.world/c/gamingb8", "mbin")
	record["description"] = "redriven b8 description"
	event := h.profileCommit("update", record)

	for _, attempt := range []struct {
		name            string
		script          resolutionScript
		wantErr         error
		want            originDisplay
		wantDescription string
	}{
		{
			name: "uncached resolver errors", script: directoryUnavailable, wantErr: jetstream.ErrUnresolvedReference,
			want: originDisplay{"lemmy.world", seededOriginURL, seededOriginSoftware}, wantDescription: "seeded b8 description",
		},
		{
			name: "document admits, Resolve errors", script: resolveUnavailable, wantErr: jetstream.ErrUnresolvedReference,
			want: originDisplay{"lemmy.world", seededOriginURL, seededOriginSoftware}, wantDescription: "seeded b8 description",
		},
		{
			name: "Resolve answers handle.invalid", script: resolvesHandleInvalid, wantErr: jetstream.ErrUnresolvedReference,
			want: originDisplay{"lemmy.world", seededOriginURL, seededOriginSoftware}, wantDescription: "seeded b8 description",
		},
		{
			name: "verified trusted identity applies the same event", script: verifiedOnBridge,
			want: originDisplay{"lemmy.world", "https://lemmy.world/c/gamingb8", "mbin"}, wantDescription: "redriven b8 description",
		},
	} {
		t.Run(attempt.name, func(t *testing.T) {
			attempt.script(h.uncached, did, handle)

			err := h.deliverEvent(event)
			if attempt.wantErr == nil && err != nil {
				t.Fatalf("update community profile: %v", err)
			}
			if attempt.wantErr != nil && !errors.Is(err, attempt.wantErr) {
				t.Errorf("update community profile error = %v, want errors.Is %v", err, attempt.wantErr)
			}

			h.assertOriginDisplay(t, attempt.want)
			if got := h.read(t).Description; got != attempt.wantDescription {
				t.Errorf("stored description = %q, want %q", got, attempt.wantDescription)
			}
		})
	}
}

// TestCommunityProfileOrigin_UnresolvedFreshIdentityCreatesNoRow: a create
// carrying the fields is retried, not indexed, while the fresh resolution
// cannot verify the repo.
func TestCommunityProfileOrigin_UnresolvedFreshIdentityCreatesNoRow(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, did, community string
		script               resolutionScript
	}{
		{name: "uncached resolver errors", did: "did:plc:origindisplayb8createerror", community: "gamingb8createerror", script: directoryUnavailable},
		{name: "Resolve answers handle.invalid", did: "did:plc:origindisplayb8createinvalid", community: "gamingb8createinvalid", script: resolvesHandleInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newCommunityOriginHarness(t, tc.did, bridgeHandle(tc.community), trustedBridgePDS, withRevGate())
			tc.script(h.uncached, tc.did, bridgeHandle(tc.community))

			err := h.deliver("create", originRecord(tc.community, gamingOriginHost, "https://lemmy.world/c/gamingb8create", "mbin"))
			if !errors.Is(err, jetstream.ErrUnresolvedReference) {
				t.Errorf("create community profile error = %v, want errors.Is ErrUnresolvedReference", err)
			}

			h.assertNoCommunity(t)
		})
	}
}

// TestCommunityProfileOrigin_FreshIdentitySubjectMismatchIsPermanent: a fresh
// resolution describing another DID rejects the event permanently and writes
// nothing.
func TestCommunityProfileOrigin_FreshIdentitySubjectMismatchIsPermanent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, operation, did, community string
		seeded                          bool
		script                          resolutionScript
	}{
		{name: "create, document for another DID", operation: "create", did: "did:plc:origindisplayb8createdoc", community: "gamingb8createdoc", script: documentForAnotherDID},
		{name: "create, Resolve for another DID", operation: "create", did: "did:plc:origindisplayb8createresolve", community: "gamingb8createresolve", script: resolveForAnotherDID},
		{name: "update, document for another DID", operation: "update", did: "did:plc:origindisplayb8updatedoc", community: "gamingb8updatedoc", seeded: true, script: documentForAnotherDID},
		{name: "update, Resolve for another DID", operation: "update", did: "did:plc:origindisplayb8updateresolve", community: "gamingb8updateresolve", seeded: true, script: resolveForAnotherDID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newCommunityOriginHarness(t, tc.did, bridgeHandle(tc.community), trustedBridgePDS, withRevGate())
			if tc.seeded {
				h.seedTrusted(t, tc.community, "seeded mismatch description")
			}
			tc.script(h.uncached, tc.did, bridgeHandle(tc.community))
			record := originRecord(tc.community, gamingOriginHost, "https://lemmy.world/c/gamingmismatch", "mbin")
			record["description"] = "mismatch event description"

			err := h.deliver(tc.operation, record)
			if !errors.Is(err, jetstream.ErrPermanentEvent) {
				t.Errorf("%s community profile error = %v, want errors.Is ErrPermanentEvent", tc.operation, err)
			}

			if !tc.seeded {
				h.assertNoCommunity(t)
				return
			}
			h.assertOriginDisplay(t, originDisplay{"lemmy.world", seededOriginURL, seededOriginSoftware})
			if got := h.read(t).Description; got != "seeded mismatch description" {
				t.Errorf("stored description = %q, want %q", got, "seeded mismatch description")
			}
		})
	}
}

// TestCommunityProfileOrigin_WithoutUncachedResolverStoresNeitherField: with
// no fresh resolution available the fields are cleared and the rest of the
// record applies.
func TestCommunityProfileOrigin_WithoutUncachedResolverStoresNeitherField(t *testing.T) {
	t.Parallel()
	const community = "gamingb9nouncached"
	h := newCommunityOriginHarness(t, "did:plc:origindisplayb9nouncached", bridgeHandle(community), trustedBridgePDS,
		withoutUncachedResolver())
	h.seedTrusted(t, community, "seeded b9 description")
	record := originRecord(community, gamingOriginHost, "https://lemmy.world/c/gamingb9", "mbin")
	record["description"] = "applied b9 description"

	if err := h.deliver("update", record); err != nil {
		t.Fatalf("update community profile: %v", err)
	}

	h.assertDisplayFields(t, "", "")
	if got := h.read(t).Description; got != "applied b9 description" {
		t.Errorf("stored description = %q, want %q", got, "applied b9 description")
	}
}

// TestCommunityProfileOrigin_BridgeHandleOnOtherDomainStoresNeitherField: a
// repo verified on the trusted bridge whose verified handle is not under the
// bridge's registrable domain gets neither field; the rest of the record
// applies and the event is not retried.
func TestCommunityProfileOrigin_BridgeHandleOnOtherDomainStoresNeitherField(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, operation, did, community string
		seeded, declaresBridgeHandle    bool
	}{
		{name: "create", operation: "create", did: "did:plc:origindisplayb11create", community: "gamingb11create"},
		{name: "update", operation: "update", did: "did:plc:origindisplayb11update", community: "gamingb11update", seeded: true},
		{
			name: "update, document declares a bridge-domain handle", operation: "update",
			did: "did:plc:origindisplayb11declared", community: "gamingb11declared", seeded: true, declaresBridgeHandle: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			otherHandle := tc.community + ".other.test"
			h := newCommunityOriginHarness(t, tc.did, otherHandle, trustedBridgePDS)
			if tc.declaresBridgeHandle {
				h.uncached.declared = &identity.DeclaredIdentity{DID: tc.did, DeclaredHandle: bridgeHandle(tc.community), PDSURL: trustedBridgePDS}
			}
			if tc.seeded {
				h.seed(t, seededCommunity{
					name: tc.community, handle: otherHandle, pdsURL: trustedBridgePDS, description: "seeded b11 description",
					origin: gamingOriginHost, originURL: seededOriginURL, originSoftware: seededOriginSoftware,
				})
			}
			record := originRecord(tc.community, gamingOriginHost, "https://lemmy.world/c/gamingb11", "mbin")
			record["displayName"] = "Applied B11 Name"
			record["description"] = "applied b11 description"

			if err := h.deliver(tc.operation, record); err != nil {
				t.Fatalf("%s community profile: %v", tc.operation, err)
			}

			h.assertDisplayFields(t, "", "")
			h.assertProfileText(t, "Applied B11 Name", "applied b11 description")
		})
	}
}
