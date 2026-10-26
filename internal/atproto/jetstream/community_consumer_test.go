package jetstream

import (
	"context"
	"net"
	"sync"
	"testing"

	"Coves/internal/atproto/identity"
	"Coves/internal/core/communities"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	covesoauth "Coves/internal/atproto/oauth"
)

// Unit coverage for the community consumer's record parsing.
//
// community_consumer.go is the largest consumer in the package and had no test
// file of its own until this one; what coverage it had arrived incidentally,
// through rev-gate and bridged-stats tests that drove it to reach some other
// conclusion. These are the two pure decoders it makes on every event, and both
// were previously exercised only through Postgres:
// tests/integration/subscription_indexing_test.go spent seven database
// round-trips per run establishing what the clamping table below states
// directly, and tests/integration/community_avatar_e2e_test.go spent 993 lines
// and three websocket dials on the blob-ref extraction.
//
// Untagged, because neither function touches anything out of process. The
// pipeline proofs they underpin are tests/e2e/community_contract_test.go (the
// avatar reaching social.coves.community.get as a hydrated URL) and
// tests/e2e/subscription_contract_test.go.

func TestExtractContentVisibility(t *testing.T) {
	t.Parallel()

	// contentVisibility is the subscriber's content-maturity preference for one
	// community, valid at 1-5. It arrives from a REMOTE repo — anyone's PDS can
	// write any number into a subscription record — so the consumer clamps
	// rather than validates: a subscription is not worth rejecting over a
	// preference field, but an out-of-range value stored as-is would leak into
	// filtering comparisons downstream.
	//
	// JSON numbers decode as float64, which is why the interesting cases are
	// float64 and the int cases exist only because the consumer accepts them
	// defensively.
	for _, tc := range []struct {
		name   string
		record map[string]interface{}
		want   int
	}{
		{"missing field defaults to 3", map[string]interface{}{}, 3},
		{"nil value defaults to 3", map[string]interface{}{"contentVisibility": nil}, 3},
		{"a string defaults to 3", map[string]interface{}{"contentVisibility": "4"}, 3},
		{"in range is kept", map[string]interface{}{"contentVisibility": float64(4)}, 4},
		{"the bottom of the range is kept", map[string]interface{}{"contentVisibility": float64(1)}, 1},
		{"the top of the range is kept", map[string]interface{}{"contentVisibility": float64(5)}, 5},
		{"zero clamps up to 1", map[string]interface{}{"contentVisibility": float64(0)}, 1},
		{"negative clamps up to 1", map[string]interface{}{"contentVisibility": float64(-5)}, 1},
		{"above range clamps down to 5", map[string]interface{}{"contentVisibility": float64(10)}, 5},
		{"far above range clamps down to 5", map[string]interface{}{"contentVisibility": float64(100)}, 5},
		{"an int is accepted and clamped", map[string]interface{}{"contentVisibility": 99}, 5},
		{"an in-range int is kept", map[string]interface{}{"contentVisibility": 2}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, extractContentVisibility(tc.record))
		})
	}
}

func TestCommunityProfileBlobRefs(t *testing.T) {
	t.Parallel()

	// The avatar and banner on a community profile are blob REFERENCES: the
	// bytes live in the community's PDS and the record carries only a CID. What
	// the consumer stores is that CID, and what the serving endpoint returns is
	// a URL built from it (blobs.HydrateImageURL) — so an extraction that picks
	// up the wrong field, or silently picks up nothing, produces a community
	// with no picture and no error anywhere.
	//
	// What is tested here is the COMPOSITION createCommunity performs —
	// parseCommunityProfile, then extractBlobCID over the field it produced —
	// rather than either half alone. extractBlobCID's own edge cases already
	// have a table in user_consumer_test.go (TestExtractBlobCID), and repeating
	// them would say nothing new. The join is what has no coverage, and it is
	// where the realistic mistake lives: a json tag that does not match the
	// lexicon's field name leaves Avatar nil, extractBlobCID declines a nil map
	// exactly as it should, and every community silently loses its picture with
	// no error on any path.
	const avatarCID = "bafkreiavatarcidaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const bannerCID = "bafkreibannercidaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	blobRef := func(cid string) map[string]interface{} {
		return map[string]interface{}{
			"$type":    "blob",
			"ref":      map[string]interface{}{"$link": cid},
			"mimeType": "image/png",
			"size":     float64(1234),
		}
	}

	base := func() map[string]interface{} {
		return map[string]interface{}{
			"$type":       "social.coves.community.profile",
			"name":        "blobtest",
			"handle":      "c-blobtest.coves.local",
			"displayName": "Blob Test",
			"createdBy":   "did:plc:aaaaaaaaaaaaaaaaaaaaaaaa",
			"hostedBy":    "did:web:coves.local",
			"createdAt":   "2026-03-01T00:00:00Z",
		}
	}

	// cidsOf is the composition createCommunity and updateCommunity both apply.
	cidsOf := func(t *testing.T, record map[string]interface{}) (string, string) {
		t.Helper()
		profile := mustParse(t, record)
		avatar, _ := extractBlobCID(profile.Avatar)
		banner, _ := extractBlobCID(profile.Banner)
		return avatar, banner
	}

	t.Run("both images are extracted, and not from each other", func(t *testing.T) {
		t.Parallel()
		record := base()
		record["avatar"] = blobRef(avatarCID)
		record["banner"] = blobRef(bannerCID)

		avatar, banner := cidsOf(t, record)
		assert.Equal(t, avatarCID, avatar)
		assert.Equal(t, bannerCID, banner,
			"the banner must not be read out of the avatar's ref, or replacing one changes both")
	})

	t.Run("an absent image is empty rather than an error", func(t *testing.T) {
		t.Parallel()
		profile, err := parseCommunityProfile(base())
		require.NoError(t, err,
			"a community without a picture is ordinary, not a malformed record")
		avatar, banner := cidsOf(t, base())
		assert.Empty(t, avatar)
		assert.Empty(t, banner)
		assert.Nil(t, profile.Avatar)
	})

	t.Run("an avatar without a banner extracts only the avatar", func(t *testing.T) {
		t.Parallel()
		record := base()
		record["avatar"] = blobRef(avatarCID)

		avatar, banner := cidsOf(t, record)
		assert.Equal(t, avatarCID, avatar)
		assert.Empty(t, banner, "an absent banner must not produce a CID")
	})

	t.Run("a malformed ref yields nothing rather than a partial value", func(t *testing.T) {
		t.Parallel()
		// The realistic remote-record shape: a peer that wrote the CID inline
		// instead of as a blob ref. It unmarshals into the map field fine (it is
		// an object) and must produce no CID at all — a stored value of
		// "map[...]" would render a URL that 502s forever.
		record := base()
		record["avatar"] = map[string]interface{}{"cid": avatarCID}

		avatar, _ := cidsOf(t, record)
		assert.Empty(t, avatar)
	})

	t.Run("an omitted image has no extractable CID", func(t *testing.T) {
		t.Parallel()
		cid, ok := extractBlobCID(mustParse(t, base()).Avatar)
		require.Empty(t, cid)
		require.False(t, ok, "an absent avatar is not a blob reference")
	})

	t.Run("a picture that is not an object fails the whole record", func(t *testing.T) {
		t.Parallel()
		// Worth pinning because it is the one malformed-image case that is NOT
		// tolerated: a bare string where the lexicon says object cannot unmarshal
		// into map[string]interface{}, so parseCommunityProfile rejects the
		// profile outright — permanently, taking the community's name and
		// description down with the picture. Tolerating it would be defensible;
		// what is not defensible is not knowing which way it goes.
		record := base()
		record["avatar"] = avatarCID

		_, err := parseCommunityProfile(record)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrPermanentEvent)
	})
}

// mustParse decodes a community profile record or fails the test.
func mustParse(t *testing.T, record map[string]interface{}) *CommunityProfile {
	t.Helper()
	profile, err := parseCommunityProfile(record)
	require.NoError(t, err)
	return profile
}

func TestCommunityConsumer_IgnoresUnrelatedCollections(t *testing.T) {
	t.Parallel()

	// The consumer subscribes to three collections and shares its feed with
	// every other consumer's traffic, so the common case by volume is an event
	// it must do nothing with. A nil repository is the assertion: anything that
	// reached a repo call from here would panic rather than pass.
	c := NewCommunityEventConsumer(nil, "did:web:test.local", true, nil)
	ctx := context.Background()

	for _, collection := range []string{
		"social.coves.community.post",
		"social.coves.actor.profile",
		"social.coves.feed.vote",
		"app.bsky.feed.post",
	} {
		require.NoErrorf(t, c.HandleEvent(ctx, taxonomyEvent(
			"did:plc:somebody", collection, "create", "rk1", map[string]interface{}{"foo": "bar"})),
			"an event for %s must be ignored, not handled", collection)
	}

	// Account events arrive on this feed regardless of wantedCollections.
	// Identity events are handled, not ignored; their malformed shapes are
	// owned by internal/core/communities/consumer_profile_indexing_test.go.
	require.NoError(t, c.HandleEvent(ctx, &JetstreamEvent{Kind: "account", Did: "did:plc:somebody"}))
	require.NoError(t, c.HandleEvent(ctx, &JetstreamEvent{Kind: "commit", Did: "did:plc:somebody"}),
		"a commit event with no commit body must not dereference it")
}

func TestCommunityConsumer_SubscriptionAndBlockIgnoreUpdates(t *testing.T) {
	t.Parallel()

	// Subscriptions and blocks are create-or-delete records: there is nothing in
	// one to edit except contentVisibility, and no client produces an update.
	// The consumer therefore handles create and delete and logs anything else,
	// which is worth pinning because the failure mode is silent — an update
	// operation the consumer decided to ignore looks exactly like an event that
	// never arrived.
	//
	// It is also the shape of a real defect in a neighbouring consumer: the vote
	// consumer drops update commits the same way, and there a client CAN
	// legitimately produce one (see tests/e2e/vote_contract_test.go). The
	// difference is worth being able to point at.
	c := NewCommunityEventConsumer(nil, "did:web:test.local", true, nil)
	ctx := context.Background()

	for _, collection := range []string{
		"social.coves.community.subscription",
		"social.coves.community.block",
	} {
		require.NoErrorf(t, c.HandleEvent(ctx, taxonomyEvent(
			"did:plc:somebody", collection, "update", "rk1",
			map[string]interface{}{"subject": "did:plc:community", "contentVisibility": float64(2)})),
			"an update on %s must be ignored without reaching the repository (a nil repo here "+
				"means anything that did would panic)", collection)
	}
}

// TestCreateCommunityIgnoresRecordAssertedSubscriberCount verifies that an
// undeclared profile field cannot seed the AppView-derived aggregate.
func TestCreateCommunityIgnoresRecordAssertedSubscriberCount(t *testing.T) {
	t.Parallel()

	repo := newOriginRepo()
	consumer := NewCommunityEventConsumer(repo, originTestInstance, true, nil)

	const did = "did:plc:forgedsubscribercount"
	commit := profileCommit("create", "forged", "", map[string]interface{}{
		"subscriberCount": float64(2_000_000_000),
	})

	require.NoError(t, consumer.createCommunity(context.Background(), did, commit))
	require.Contains(t, repo.byDID, did)
	assert.Zero(t, repo.byDID[did].SubscriberCount,
		"subscriberCount is AppView-derived state; a community profile must not seed it")
}

// TestUpdateCommunityIgnoresRecordAssertedSubscriberCount is the update-path
// twin: a profile edit carrying the undeclared field must leave the stored,
// relationship-derived value exactly where it was.
func TestUpdateCommunityIgnoresRecordAssertedSubscriberCount(t *testing.T) {
	t.Parallel()

	repo := newOriginRepo()
	consumer := NewCommunityEventConsumer(repo, originTestInstance, true, nil)

	const did = "did:plc:forgedupdatecount"
	seedCommunity(repo, did, nativeHandle, "")
	repo.byDID[did].SubscriberCount = 7

	commit := profileCommit("update", "nba", "", map[string]interface{}{
		"subscriberCount": float64(2_000_000_000),
	})
	require.NoError(t, consumer.updateCommunity(context.Background(), did, commit))
	assert.Equal(t, 7, repo.byDID[did].SubscriberCount,
		"a profile update must not touch the AppView-derived subscriber count")
}

// A rename under the community's stored hosted_by_did domain must reach DID
// document verification of that domain, with verification on and the instance
// DID naming a different domain. A check against any other DID (the instance's,
// the owner's, the community's own, or none) is refused permanently before any
// fetch, which in production would drop every community rename for good.
//
// The success path needs a real HTTPS fetch of did:web's .well-known document,
// which this client cannot be pointed at, so the test asserts the fetch was
// attempted for the stored domain. The resolver seam answers that lookup with
// loopback, which the SSRF guard refuses before any dial: the test makes no DNS
// lookup and opens no connection. A refused fetch is a redrivable failure.
func TestCommunityIdentityEvent_RenameIsVerifiedAgainstTheStoredHostedByDomain(t *testing.T) {
	t.Parallel()

	const (
		did            = "did:plc:communityidentityhostedby"
		hostedByDomain = "community.example" // RFC 2606 reserved; only the seam answers it
		storedHandle   = "gaming.old.example"
		renamedHandle  = "gaming." + hostedByDomain
	)

	repo := newOriginRepo()
	repo.byDID[did] = &communities.Community{
		DID:         did,
		Handle:      storedHandle,
		Name:        "gaming",
		OwnerDID:    "did:plc:communityidentityhostedbyowner",
		HostedByDID: "did:web:" + hostedByDomain,
	}

	var (
		lookupsMu sync.Mutex
		lookups   []string
	)
	recordingLoopbackResolver := func(_ context.Context, host string) ([]net.IP, error) {
		lookupsMu.Lock()
		defer lookupsMu.Unlock()
		lookups = append(lookups, host)
		return []net.IP{net.IPv4(127, 0, 0, 1)}, nil
	}

	uncachedResolver := &scriptedRefreshResolver{answer: &identity.Identity{DID: did, Handle: renamedHandle}}
	consumer := NewCommunityEventConsumer(repo, "did:web:coves.social", false, &purgeRecordingResolver{},
		append(PrivateHostOptions(false),
			WithCommunityUncachedIdentityResolver(uncachedResolver),
			WithCommunityIdentityRefreshLock(&passThroughIdentityRefreshLock{}),
			withTransportOptions(covesoauth.WithHostResolver(recordingLoopbackResolver)))...)

	err := consumer.HandleEvent(context.Background(), &JetstreamEvent{
		Kind:     "identity",
		Did:      did,
		Identity: &IdentityEvent{Did: did, Handle: renamedHandle},
	})

	require.ErrorIs(t, err, ErrUnresolvedReference,
		"a rename whose DID document could not be fetched must be redrivable")
	assert.NotErrorIs(t, err, ErrPermanentEvent,
		"the rename is under the stored hostedBy domain; a permanent refusal means it was checked against another DID")
	lookupsMu.Lock()
	assert.Equal(t, []string{hostedByDomain}, lookups,
		"DID document verification must fetch from the stored hostedBy domain, once")
	lookupsMu.Unlock()
	assert.Equal(t, storedHandle, repo.byDID[did].Handle, "an unverified rename must not be stored")
}
