package jetstream

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/atproto/identity"
	"Coves/internal/core/communities"
)

// The trust rule for a community profile's self-asserted `origin`.
//
// `origin` exists so a Tidepool-bridged community whose DNS handle is the lossy
// comicstrips.lemmy-world.tdpl.io can render as !comicstrips@lemmy.world. It is
// written by whoever writes the record, so without a gate any repo could claim
// to be !nba@coves.social. The gate is admitCommunityOrigin, and the property
// under test is that it can only ever DROP the field — never the event. A
// community with a wrong display name is still a community; a refused one
// dead-letters every post naming it.
//
// Untagged: the rule is pure, and the create/update paths are exercised through
// an in-memory repository with verification disabled (the hostedBy check is a
// separate security property with its own tests).

const (
	trustedBridgePDS   = "https://pds.tdpl.io"
	untrustedPDS       = "https://pds.example.net"
	bridgedHandle      = "comicstrips.lemmy-world.tdpl.io"
	bridgedOrigin      = "lemmy.world"
	nativeHandle       = "c-nba.coves.social"
	nativeOrigin       = "coves.social"
	subdomainHandle    = "c-nba.dev.coves.social"
	subdomainOrigin    = "dev.coves.social"
	originTestInstance = "did:web:test.local"
)

func trustingBridge() *BridgeTrust { return NewBridgeTrust([]string{trustedBridgePDS}) }

func TestAdmitCommunityOrigin(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		bt     *BridgeTrust
		pdsURL string
		handle string
		origin string
		want   string
	}{
		// The bridged case this field exists for.
		{"trusted bridge asserts a foreign origin", trustingBridge(), trustedBridgePDS, bridgedHandle, bridgedOrigin, bridgedOrigin},
		{"trusted bridge origin is canonicalised", trustingBridge(), trustedBridgePDS, bridgedHandle, "Lemmy.World", bridgedOrigin},

		// Everyone else may claim only what their verified handle already proves.
		{"untrusted repo asserting a foreign origin is dropped", trustingBridge(), untrustedPDS, nativeHandle, bridgedOrigin, ""},
		{"untrusted repo asserting its own domain is kept", trustingBridge(), untrustedPDS, nativeHandle, nativeOrigin, nativeOrigin},
		{"untrusted repo asserting its parent subdomain is kept", trustingBridge(), untrustedPDS, subdomainHandle, subdomainOrigin, subdomainOrigin},
		{"untrusted repo asserting the registrable domain of a subdomain handle is kept", trustingBridge(), untrustedPDS, subdomainHandle, nativeOrigin, nativeOrigin},
		{"untrusted repo asserting a sibling subdomain is dropped", trustingBridge(), untrustedPDS, subdomainHandle, "prod.coves.social", ""},
		{"a look-alike suffix without a dot boundary is dropped", trustingBridge(), untrustedPDS, "c-nba.evilcoves.social", nativeOrigin, ""},
		{"matching origin compares case-insensitively", trustingBridge(), untrustedPDS, "C-NBA.Coves.Social", "COVES.SOCIAL", nativeOrigin},

		// Default-deny: no gate, no allowlist, no known PDS all mean "own domain only".
		{"nil gate accepts a matching origin", nil, trustedBridgePDS, nativeHandle, nativeOrigin, nativeOrigin},
		{"nil gate drops a foreign origin even from the bridge host", nil, trustedBridgePDS, bridgedHandle, bridgedOrigin, ""},
		{"empty allowlist drops a foreign origin", NewBridgeTrust(nil), trustedBridgePDS, bridgedHandle, bridgedOrigin, ""},
		{"unknown PDS drops a foreign origin", trustingBridge(), "", bridgedHandle, bridgedOrigin, ""},

		// Hygiene: the value goes into a display string and a JSON field.
		{"absent origin stays absent", trustingBridge(), trustedBridgePDS, bridgedHandle, "", ""},
		{"a URL is not a hostname, even from a trusted bridge", trustingBridge(), trustedBridgePDS, bridgedHandle, "https://lemmy.world", ""},
		{"a single-label name is refused", trustingBridge(), trustedBridgePDS, bridgedHandle, "localhost", ""},
		{"whitespace is refused", trustingBridge(), trustedBridgePDS, bridgedHandle, "lemmy .world", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, admitCommunityOrigin(tc.bt, "did:plc:origintest", tc.pdsURL, tc.handle, tc.origin))
		})
	}
}

// originRepo is the narrowest communities.Repository the create and update
// paths need. The embedded interface is nil, so any method the consumer starts
// calling that is not modelled here panics rather than silently passing.
type originRepo struct {
	communities.Repository
	byDID map[string]*communities.Community
}

func newOriginRepo() *originRepo {
	return &originRepo{byDID: map[string]*communities.Community{}}
}

func (r *originRepo) Create(_ context.Context, c *communities.Community) (*communities.Community, error) {
	if _, ok := r.byDID[c.DID]; ok {
		return nil, communities.ErrCommunityAlreadyExists
	}
	cp := *c
	r.byDID[c.DID] = &cp
	return c, nil
}

func (r *originRepo) GetByDID(_ context.Context, did string) (*communities.Community, error) {
	c, ok := r.byDID[did]
	if !ok {
		return nil, communities.ErrCommunityNotFound
	}
	cp := *c
	return &cp, nil
}

func (r *originRepo) Update(_ context.Context, c *communities.Community) (*communities.Community, error) {
	if _, ok := r.byDID[c.DID]; !ok {
		return nil, communities.ErrCommunityNotFound
	}
	cp := *c
	r.byDID[c.DID] = &cp
	return c, nil
}

// fixedResolver answers every DID with one identity: the shape of a bridged
// community whose record carries no handle, so the consumer resolves one and,
// with it, the PDS host BridgeTrust reads.
type fixedResolver struct{ id identity.Identity }

func (f fixedResolver) Resolve(context.Context, string) (*identity.Identity, error) {
	id := f.id
	return &id, nil
}

func profileCommit(op, name, origin string, extra map[string]interface{}) *CommitEvent {
	record := map[string]interface{}{
		"$type":     "social.coves.community.profile",
		"name":      name,
		"hostedBy":  originTestInstance,
		"createdBy": "did:plc:origincreator",
		"createdAt": time.Now().UTC().Format(time.RFC3339),
	}
	if origin != "" {
		record["origin"] = origin
	}
	for k, v := range extra {
		record[k] = v
	}
	return &CommitEvent{
		Rev:        "3lorigin" + op,
		Operation:  op,
		Collection: "social.coves.community.profile",
		RKey:       "self",
		CID:        "bafyorigin" + op,
		Record:     record,
	}
}

func TestCreateCommunity_TrustedBridge_KeepsForeignOrigin(t *testing.T) {
	t.Parallel()
	repo := newOriginRepo()
	resolver := fixedResolver{identity.Identity{Handle: bridgedHandle, PDSURL: trustedBridgePDS}}
	consumer := NewCommunityEventConsumer(repo, originTestInstance, true, resolver,
		WithCommunityBridgeTrust(trustingBridge()))

	const did = "did:plc:bridgedcomicstrips"
	require.NoError(t, consumer.createCommunity(context.Background(), did, profileCommit("create", "comicstrips", bridgedOrigin, nil)))

	stored := repo.byDID[did]
	require.NotNil(t, stored, "the community must be indexed")
	assert.Equal(t, bridgedHandle, stored.Handle)
	assert.Equal(t, trustedBridgePDS, stored.PDSURL, "the resolved PDS host is what BridgeTrust gated on; it must be persisted")
	assert.Equal(t, bridgedOrigin, stored.Origin)
	assert.Equal(t, "!comicstrips@lemmy.world", stored.GetDisplayHandle(),
		"this string is the whole point of the field: the DNS handle cannot express lemmy.world")
}

func TestCreateCommunity_UntrustedRepo_DropsForeignOriginButIndexes(t *testing.T) {
	t.Parallel()
	repo := newOriginRepo()
	resolver := fixedResolver{identity.Identity{Handle: "c-nba.example.net", PDSURL: untrustedPDS}}
	consumer := NewCommunityEventConsumer(repo, originTestInstance, true, resolver,
		WithCommunityBridgeTrust(trustingBridge()))

	const did = "did:plc:impostornba"
	// A repo on some other PDS claiming to be !nba@coves.social.
	err := consumer.createCommunity(context.Background(), did, profileCommit("create", "nba", nativeOrigin, nil))

	require.NoError(t, err, "an untrusted origin must never reject the event")
	stored := repo.byDID[did]
	require.NotNil(t, stored, "the community must still be indexed, just without the origin")
	assert.Empty(t, stored.Origin)
	assert.Equal(t, "!nba@example.net", stored.GetDisplayHandle(),
		"with the origin dropped the display handle falls back to the verified handle's domain")
}

func TestCreateCommunity_NativeRecord_KeepsMatchingOrigin(t *testing.T) {
	t.Parallel()
	repo := newOriginRepo()
	// No resolver: the handle is carried in the record, as this AppView's own
	// CreateCommunity writes it, and no PDS host is known — default-deny, so
	// only the handle's own domain is admissible.
	consumer := NewCommunityEventConsumer(repo, originTestInstance, true, nil)

	const did = "did:plc:nativenba"
	commit := profileCommit("create", "nba", nativeOrigin, map[string]interface{}{"handle": nativeHandle})
	require.NoError(t, consumer.createCommunity(context.Background(), did, commit))

	stored := repo.byDID[did]
	require.NotNil(t, stored)
	assert.Equal(t, nativeOrigin, stored.Origin)
	assert.Equal(t, "!nba@coves.social", stored.GetDisplayHandle())
}

func TestUpdateCommunity_AppliesTheSameRuleFromTheStoredPDS(t *testing.T) {
	t.Parallel()

	t.Run("trusted stored PDS keeps a foreign origin", func(t *testing.T) {
		t.Parallel()
		repo := newOriginRepo()
		const did = "did:plc:bridgedupdate"
		repo.byDID[did] = &communities.Community{DID: did, Handle: bridgedHandle, Name: "comicstrips", PDSURL: trustedBridgePDS}
		consumer := NewCommunityEventConsumer(repo, originTestInstance, true, nil, WithCommunityBridgeTrust(trustingBridge()))

		commit := profileCommit("update", "comicstrips", bridgedOrigin, map[string]interface{}{"handle": bridgedHandle})
		require.NoError(t, consumer.updateCommunity(context.Background(), did, commit))
		assert.Equal(t, bridgedOrigin, repo.byDID[did].Origin)
	})

	t.Run("untrusted stored PDS drops a foreign origin and clears a stale one", func(t *testing.T) {
		t.Parallel()
		repo := newOriginRepo()
		const did = "did:plc:driftupdate"
		// A row that somehow carries an origin its repo could not assert today
		// (an allowlist change, say) is corrected by the next profile write.
		repo.byDID[did] = &communities.Community{DID: did, Handle: "c-nba.example.net", Name: "nba", PDSURL: untrustedPDS, Origin: nativeOrigin}
		consumer := NewCommunityEventConsumer(repo, originTestInstance, true, nil, WithCommunityBridgeTrust(trustingBridge()))

		commit := profileCommit("update", "nba", nativeOrigin, map[string]interface{}{"handle": "c-nba.example.net"})
		require.NoError(t, consumer.updateCommunity(context.Background(), did, commit))
		assert.Empty(t, repo.byDID[did].Origin)
	})

	t.Run("a record without origin clears the stored one", func(t *testing.T) {
		t.Parallel()
		repo := newOriginRepo()
		const did = "did:plc:clearupdate"
		repo.byDID[did] = &communities.Community{DID: did, Handle: nativeHandle, Name: "nba", Origin: nativeOrigin}
		consumer := NewCommunityEventConsumer(repo, originTestInstance, true, nil)

		commit := profileCommit("update", "nba", "", map[string]interface{}{"handle": nativeHandle})
		require.NoError(t, consumer.updateCommunity(context.Background(), did, commit))
		assert.Empty(t, repo.byDID[did].Origin, "the record replaces the stored profile wholesale")
	})
}
