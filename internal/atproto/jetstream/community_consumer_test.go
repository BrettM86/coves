package jetstream

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	t.Run("only one of the two present leaves the other alone", func(t *testing.T) {
		t.Parallel()
		record := base()
		record["avatar"] = blobRef(avatarCID)

		avatar, banner := cidsOf(t, record)
		assert.Equal(t, avatarCID, avatar)
		assert.Empty(t, banner,
			"a record with an avatar and no banner must not produce a banner CID: the consumer "+
				"only assigns when extraction succeeds, which is what stops an update that omits "+
				"the banner from blanking a stored one")
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

	t.Run("an omitted image does not blank a stored one", func(t *testing.T) {
		t.Parallel()
		// THE `if ok` GUARDS, stated as the invariant they exist for.
		//
		// Both the create and update paths assign conditionally:
		//
		//	if avatarCID, ok := extractBlobCID(profile.Avatar); ok { … = avatarCID }
		//
		// so a record that omits a picture leaves the stored CID untouched
		// rather than clearing it. That matters because UpdateCommunity rebuilds
		// the profile record from scratch and only sets `avatar` when a NEW blob
		// was uploaded — so a display-name-only edit ships a record with no
		// avatar key at all, and the guard is the only thing that stops every
		// such edit from wiping the community's picture.
		//
		// Modelled here as the decision the consumer makes (extraction
		// succeeded, or it did not), which is what the guard branches on; the
		// end-to-end version is the community contract's update step, and the
		// erasure risk on the WRITE side is
		// TestService_CreateAndUpdateWriteBlobRefsIntoTheProfileRecord.
		withImage := base()
		withImage["avatar"] = blobRef(avatarCID)
		stored, ok := extractBlobCID(mustParse(t, withImage).Avatar)
		require.True(t, ok)
		require.Equal(t, avatarCID, stored)

		// The follow-up edit carries no avatar at all.
		_, ok = extractBlobCID(mustParse(t, base()).Avatar)
		require.False(t, ok,
			"extraction must FAIL for an omitted image, because failing is what makes the "+
				"consumer's `if ok` skip the assignment and keep the stored CID. If this ever "+
				"returned ok with an empty string, every display-name-only update would blank "+
				"the community's avatar")
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

	// Non-commit kinds too: identity and account events arrive on this feed
	// regardless of wantedCollections.
	require.NoError(t, c.HandleEvent(ctx, &JetstreamEvent{Kind: "identity", Did: "did:plc:somebody"}))
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
