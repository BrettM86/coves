//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"testing"
	"time"

	"Coves/internal/core/discover"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The hosted-community bonus as the snapshot build actually stores it.
//
// discover.DiscoverHotRank already takes the bonus and internal/core/discover
// proves what it does with it, but a rank nothing persists ranks nothing: the
// value a page is ordered by is discover_hot_candidates.base_rank, written once
// per snapshot build. So the question here is the repository's, not the
// formula's — does the build ask "do we hold this community's PDS refresh
// token?" for every candidate, and does it feed the bonus, keyed under its own
// cursor secret, into the rank it writes down.
//
// The four communities are the four answers that predicate can give, and three
// of them are traps:
//
//   - stored ciphertext: hosted, bonus.
//   - non-NULL but EMPTY ciphertext: still hosted. Presence is the fact
//     (internal/db/postgres/community_hosted.go); an implementation that
//     decrypted, or that tested for a non-empty value, would silently drop the
//     bonus for a community whose credentials are merely malformed.
//   - NULL ciphertext with hosted_by_did naming this instance: NOT hosted.
//     hosted_by_did is copied out of the community's own profile record, so it
//     is a claim by whoever controls that repo — an implementation keyed off it
//     lets anyone buy a Discover boost by writing one field.
//   - NULL ciphertext, hosted elsewhere: not hosted, the ordinary federated case.
//
// Two builds, each checked against ITS OWN persisted ranking time, because a
// rank is only meaningful next to the clock that decayed it: comparing the
// second snapshot's ranks against the first snapshot's ranking time would fail
// for every post whether or not the bonus were applied, and passing the build's
// own time back in is the only way the bonus is the sole difference being
// measured.
const (
	// hostedBonusCursorSecret is the secret the repositories below are built
	// with, and therefore the key the bonus must be derived from: the bonus is
	// HMACed under the repository's own cursor secret, so an expectation that
	// hashed the URI alone would pass against an implementation an author can
	// grind rkeys against offline.
	hostedBonusCursorSecret = "hosted-bonus-secret"

	// neutralCommunityAdjustment is what CommunityAdjustment returns when no
	// community has enough history to form a reference. This suite seeds far
	// fewer than minimumReferencePosts per community inside the history window,
	// so the build computes exactly this — and the non-hosted score-25 post
	// below is what proves the assumption rather than assuming it, since the
	// adjustment only enters the numerator for a positive score.
	neutralCommunityAdjustment = 1.0

	// minimumSecretSeparation is how far apart two secrets' bonuses for one URI
	// have to be before a stored rank can be attributed to one of them. Two
	// secrets can always hash a given URI to nearby values by chance, and a
	// post whose two bonuses nearly agree proves nothing about which key was
	// used.
	minimumSecretSeparation = 0.25

	// hostedBonusRankTolerance is relative: the build and the assertion run the
	// same float64 expression, so they agree to the last few bits.
	hostedBonusRankTolerance = 1e-12

	// minimumDistinguishingBonus keeps this test honest. HostedCommunityBonus
	// spreads over [0, 3.0), so a URI can hash to a bonus small enough that a
	// build ignoring it lands within any sane tolerance anyway. Every post here
	// is seeded under an rkey whose bonus clears this, so a bonus-free
	// implementation cannot pass by luck — and the posts that must NOT be given
	// the bonus carry a large one too, so misapplying it is equally visible.
	minimumDistinguishingBonus = 0.01
)

func TestDiscoverRepo_HotPersistedBaseRankCarriesHostedCommunityBonus(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	const authorDID = "did:plc:hotbonusauthor"
	createTestUser(t, db, "hotbonusauthor.test", authorDID)

	credentialed := hostedCommunity(t, db, "hotbonuscreds")
	emptyCiphertext := emptyCiphertextCommunity(t, db, "hotbonusempty")
	claimingHost := uncredentialedCommunity(t, db, "hotbonusclaim")
	federated := federatedCommunity(t, db, "hotbonusfederated")

	now := time.Now()
	seeded := []struct {
		community string
		score     int
		age       time.Duration
		hosted    bool
		reason    string
	}{
		{credentialed, 0, 3 * time.Hour, true, "a stored refresh token is the hosted fact"},
		{credentialed, 25, 9 * time.Hour, true, "a positive score takes the bonus on top of its adjusted numerator"},
		{credentialed, -4, 20 * time.Hour, false, "a negatively scored post is not boosted for being hosted"},
		{emptyCiphertext, 0, 31 * time.Hour, true, "an empty ciphertext is still a stored credential"},
		{claimingHost, 0, 14 * time.Hour, false, "hosted_by_did naming this instance is a claim, not a credential"},
		{federated, 0, 44 * time.Hour, false, "a community hosted elsewhere earns nothing"},
		{federated, 25, 5 * time.Hour, false, "the neutral-adjustment witness: a positive score with no bonus"},
	}
	type expectation struct {
		uri       string
		score     int
		createdAt time.Time
		bonus     float64
		reason    string
	}
	expected := make([]expectation, 0, len(seeded))
	for _, post := range seeded {
		createdAt := now.Add(-post.age).Truncate(time.Second)
		uri := seedDistinguishableBonusPost(t, db, post.community, authorDID, post.score, createdAt,
			func(uri string) bool {
				return discover.HostedCommunityBonus(hostedBonusCursorSecret, uri) > minimumDistinguishingBonus
			})
		bonus := discover.HostedCommunityBonus(hostedBonusCursorSecret, uri)
		require.Greaterf(t, bonus, minimumDistinguishingBonus,
			"%s hashes to a bonus too small to tell an implementation apart", uri)
		if !post.hosted {
			bonus = 0
		}
		expected = append(expected, expectation{
			uri:       uri,
			score:     post.score,
			createdAt: createdAt,
			bonus:     bonus,
			reason:    post.reason,
		})
	}

	// Two distinct builds without waiting out discoverHotSnapshotReuse: the
	// thirty-second reuse window is per viewer scope, so an authenticated read
	// cannot reuse the anonymous snapshot and has to build its own.
	const viewerDID = "did:plc:hotbonusviewer"
	createTestUser(t, db, "hotbonusviewer.test", viewerDID)
	readDiscoverHotRoot(t, NewDiscoverRepository(db, hostedBonusCursorSecret), "", 10)
	anonymous := readPersistedDiscoverHotSnapshot(t, db, "")
	readDiscoverHotRoot(t, NewDiscoverRepository(db, hostedBonusCursorSecret), viewerDID, 10)
	authenticated := readPersistedDiscoverHotSnapshot(t, db, viewerDID)
	require.NotEqual(t, anonymous.id, authenticated.id, "the two reads must have built separate snapshots")

	for _, snapshot := range []persistedDiscoverHotSnapshot{anonymous, authenticated} {
		for _, post := range expected {
			stored, ok := snapshot.baseRanks[post.uri]
			require.Truef(t, ok, "snapshot %d is missing candidate %s", snapshot.id, post.uri)
			want := discover.DiscoverHotRank(post.score, post.createdAt, snapshot.rankingTime,
				neutralCommunityAdjustment, post.bonus)
			assert.InEpsilonf(t, want, stored, hostedBonusRankTolerance,
				"snapshot %d stored the wrong base rank for %s: %s", snapshot.id, post.uri, post.reason)
		}
	}
}

// The key the bonus is derived from, rather than the fact that a bonus exists.
//
// An unkeyed hash of the AT-URI is public arithmetic: the rkey is the author's
// to choose, so an author can grind rkeys offline until one lands near the 3.0
// ceiling and enter Discover at the top of every hosted community they can post
// in. Keying the hash with the repository's cursor secret — a value that never
// leaves the server — is what makes that search impossible to run.
//
// Two repositories over one database, each holding a different secret, are how
// that is measured: they index the identical post row, so the secret is the
// only thing that can separate the ranks they write down. Each snapshot is then
// checked against its own secret AND against what the other secret would have
// produced at that same ranking time, because the two builds were stamped at
// different instants and would store different ranks under one shared secret
// too — holding the clock fixed is what isolates the key.
func TestDiscoverRepo_HotHostedBonusIsKeyedByTheCursorSecret(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	const (
		authorDID    = "did:plc:hotbonuskeyedauthor"
		firstSecret  = "hosted-bonus-first-cursor-secret"
		secondSecret = "hosted-bonus-second-cursor-secret"
	)
	createTestUser(t, db, "hotbonuskeyedauthor.test", authorDID)

	community := hostedCommunity(t, db, "hotbonuskeyed")
	createdAt := time.Now().Add(-7 * time.Hour).Truncate(time.Second)
	uri := seedDistinguishableBonusPost(t, db, community, authorDID, 0, createdAt, func(uri string) bool {
		first := discover.HostedCommunityBonus(firstSecret, uri)
		second := discover.HostedCommunityBonus(secondSecret, uri)
		return first > minimumDistinguishingBonus && second > minimumDistinguishingBonus &&
			math.Abs(first-second) > minimumSecretSeparation
	})

	firstBonus := discover.HostedCommunityBonus(firstSecret, uri)
	secondBonus := discover.HostedCommunityBonus(secondSecret, uri)
	require.Greaterf(t, firstBonus, minimumDistinguishingBonus,
		"%s hashes to a bonus too small to tell an implementation apart", uri)
	require.Greaterf(t, secondBonus, minimumDistinguishingBonus,
		"%s hashes to a bonus too small to tell an implementation apart", uri)
	require.Greater(t, math.Abs(firstBonus-secondBonus), minimumSecretSeparation,
		"no rkey in the search produced a bonus that moved when the secret changed: the bonus is not keyed by the cursor secret")

	// Separate viewer scopes, for the same reason as above: the thirty-second
	// reuse window is per scope, so two reads in one scope would share a single
	// snapshot and the second secret would never build anything.
	const viewerDID = "did:plc:hotbonuskeyedviewer"
	createTestUser(t, db, "hotbonuskeyedviewer.test", viewerDID)

	readDiscoverHotRoot(t, NewDiscoverRepository(db, firstSecret), "", 10)
	firstSnapshot := readPersistedDiscoverHotSnapshot(t, db, "")
	readDiscoverHotRoot(t, NewDiscoverRepository(db, secondSecret), viewerDID, 10)
	secondSnapshot := readPersistedDiscoverHotSnapshot(t, db, viewerDID)
	require.NotEqual(t, firstSnapshot.id, secondSnapshot.id, "the two reads must have built separate snapshots")

	for _, build := range []struct {
		name        string
		secret      string
		otherSecret string
		snapshot    persistedDiscoverHotSnapshot
	}{
		{"anonymous", firstSecret, secondSecret, firstSnapshot},
		{"authenticated", secondSecret, firstSecret, secondSnapshot},
	} {
		stored, ok := build.snapshot.baseRanks[uri]
		require.Truef(t, ok, "snapshot %d is missing candidate %s", build.snapshot.id, uri)

		own := discover.DiscoverHotRank(0, createdAt, build.snapshot.rankingTime,
			neutralCommunityAdjustment, discover.HostedCommunityBonus(build.secret, uri))
		other := discover.DiscoverHotRank(0, createdAt, build.snapshot.rankingTime,
			neutralCommunityAdjustment, discover.HostedCommunityBonus(build.otherSecret, uri))

		assert.InEpsilonf(t, own, stored, hostedBonusRankTolerance,
			"the %s snapshot must rank the post under the cursor secret ITS repository was built with", build.name)
		assert.Greaterf(t, math.Abs(stored-other), math.Abs(own-other)/2,
			"the %s snapshot stored a rank no better explained by its own secret than by the other repository's", build.name)
	}

	assert.NotEqual(t, firstSnapshot.baseRanks[uri], secondSnapshot.baseRanks[uri],
		"two repositories holding different cursor secrets must not agree on one post's base rank")
}

// emptyCiphertextCommunity seeds a community whose credential column holds zero
// bytes — non-NULL, and therefore hosted.
//
// The value is deliberately not a valid ciphertext: nothing on this path
// decrypts it, and a community with a corrupt stored credential is still one
// this AppView provisioned and signs for.
func emptyCiphertextCommunity(t *testing.T, db *sql.DB, name string) string {
	t.Helper()

	did := uncredentialedCommunity(t, db, name)
	_, err := db.ExecContext(context.Background(),
		`UPDATE communities SET pds_refresh_token_encrypted = $2 WHERE did = $1`,
		did, []byte{})
	require.NoErrorf(t, err, "storing an empty credential for %s", did)
	return did
}

// federatedCommunity seeds a community this AppView merely indexes: no stored
// credential, and a hosted_by_did that names someone else.
//
// It is the ordinary federated shape, and the counterweight to
// uncredentialedCommunity — which leaves hosted_by_did naming THIS instance, so
// between the two an implementation cannot satisfy both by reading that column.
func federatedCommunity(t *testing.T, db *sql.DB, name string) string {
	t.Helper()

	did := uncredentialedCommunity(t, db, name)
	_, err := db.ExecContext(context.Background(),
		`UPDATE communities SET hosted_by_did = $2 WHERE did = $1`,
		did, "did:web:elsewhere.invalid")
	require.NoErrorf(t, err, "rehosting %s elsewhere", did)
	return did
}

// seedDistinguishableBonusPost inserts one post under an rkey the caller is
// willing to measure with: usable rejects an rkey whose bonus would be too
// small, or too close to another secret's, to tell two implementations apart.
//
// The search is over rkeys rather than over anything the ranking reads, so it
// biases nothing: every candidate URI is as valid as the first, and the loop
// only refuses the small minority the caller cannot measure.
//
// An exhausted search seeds the last candidate anyway rather than failing here.
// A search that never succeeds is itself a finding about the implementation —
// that every rkey hashes the same way under two different secrets, say — and
// the caller states that as an assertion about the URI it gets back, where the
// message can name the property that was missing.
func seedDistinguishableBonusPost(t *testing.T, db *sql.DB, communityDID, authorDID string, score int, createdAt time.Time, usable func(uri string) bool) string {
	t.Helper()

	base := testkit.UniqueIDWithPrefix(t, "hotbonus")
	rkey := base
	for attempt := 0; attempt < 100 && !usable(discoverHotPostURI(communityDID, rkey)); attempt++ {
		rkey = fmt.Sprintf("%s%d", base, attempt)
	}

	// score = upvotes - downvotes is the invariant the serving queries rely on.
	upvotes, downvotes := score, 0
	if score < 0 {
		upvotes, downvotes = 0, -score
	}
	uri := discoverHotPostURI(communityDID, rkey)
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, created_at, score, upvote_count, downvote_count)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, uri, "bafypost"+rkey, rkey, authorDID, communityDID, "hosted bonus post "+rkey, createdAt, score, upvotes, downvotes)
	require.NoErrorf(t, err, "seeding post %s in %s", rkey, communityDID)
	return uri
}

// discoverHotPostURI builds this fixture's LEGACY post URI: a community-repo
// at://<community did>/social.coves.community.post/<rkey>.
//
// A live record is a postv2 in the AUTHOR's repo, under an rkey the author
// picked — which is the reason the bonus is keyed by a server secret at all.
// The legacy shape is used here because visiblePostsPredicate fails CLOSED for
// a postv2 with no admission row: a postv2 fixture would need an accepted
// admission row carrying the post's own CID before it could even reach the
// candidate query, and seeding the admission engine to measure a ranking number
// would put this suite at the mercy of a gate it is not testing. Nothing on
// this path parses the URI, so the shape changes no answer here; what the bonus
// must be keyed by is asserted directly, in the test below.
func discoverHotPostURI(communityDID, rkey string) string {
	return "at://" + communityDID + "/social.coves.community.post/" + rkey
}

// persistedDiscoverHotSnapshot is what a build wrote down: the ranking time it
// stamped on the snapshot and the base rank it stored per candidate.
type persistedDiscoverHotSnapshot struct {
	id          int64
	rankingTime time.Time
	baseRanks   map[string]float64
}

// readPersistedDiscoverHotSnapshot returns the newest snapshot for a viewer
// scope.
//
// The algorithm version is deliberately not part of the lookup: what is under
// test is the number in base_rank, and pinning the version here would make this
// suite fail for the unrelated reason that the version was bumped.
func readPersistedDiscoverHotSnapshot(t *testing.T, db *sql.DB, viewerDID string) persistedDiscoverHotSnapshot {
	t.Helper()

	ctx := context.Background()
	snapshot := persistedDiscoverHotSnapshot{baseRanks: make(map[string]float64)}
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT id, ranking_time
		FROM discover_hot_snapshots
		WHERE viewer_scope = $1
		ORDER BY created_at DESC
		LIMIT 1
	`, discoverHotViewerScope(viewerDID)).Scan(&snapshot.id, &snapshot.rankingTime))

	rows, err := db.QueryContext(ctx, `
		SELECT uri, base_rank
		FROM discover_hot_candidates
		WHERE snapshot_id = $1
	`, snapshot.id)
	require.NoError(t, err)
	defer rows.Close()

	for rows.Next() {
		var (
			uri      string
			baseRank float64
		)
		require.NoError(t, rows.Scan(&uri, &baseRank))
		snapshot.baseRanks[uri] = baseRank
	}
	require.NoError(t, rows.Err())
	return snapshot
}
