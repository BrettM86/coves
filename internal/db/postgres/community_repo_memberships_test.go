//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/core/communities"
	"Coves/tests/testkit"
)

// community_repo_memberships.go against real SQL: memberships, moderation
// actions, and the five counter mutators.
//
// All eleven functions in that file were at zero coverage before this one, and
// they are not equally harmless. Three groups, in increasing order of how badly
// a mistake would show:
//
//   - Memberships carry the reputation score, the ban flag and the moderator
//     flag. ListMembers orders by reputation, which is what a community's
//     member list and any future "trusted contributor" rule read.
//   - Moderation actions are what one instance broadcasts to another about a
//     community it has delisted or quarantined. The action column is a CHECK
//     constraint, and a value that slips past it is a moderation signal no peer
//     knows how to interpret.
//   - The counters are the numbers a client renders. They are unconditional
//     UPDATEs, so they are also where the sharpest surprise in this file lives —
//     see TestCommunityRepo_CountersOnAnAbsentCommunityAreSilent.
//
// Every membership row has a foreign key to communities(did) ON DELETE CASCADE,
// so each test seeds a real community first. That is not ceremony: the FK is
// how the repository distinguishes "this community does not exist" from any
// other insert failure, and a test that inserted against a bare table would
// never exercise that branch.

// memberOf seeds a community and returns the repository and the community, so a
// test names only the thing it is about.
func memberOf(t *testing.T) (communities.Repository, *communities.Community) {
	t.Helper()
	repo, _, community := memberOfWithDB(t)
	return repo, community
}

// memberOfWithDB is memberOf, additionally handing back the pool. The stored
// `communities.post_count` column is no longer SERVED — GetByDID computes
// postCount live from the visibility predicate — so a test about the stored
// counter has to read the column rather than the API value.
func memberOfWithDB(t *testing.T) (communities.Repository, *sql.DB, *communities.Community) {
	t.Helper()
	db := testkit.DB(t)
	repo := NewCommunityRepository(db)
	id := testkit.UniqueID(t)
	community, err := repo.Create(context.Background(), &communities.Community{
		DID:          "did:plc:mem" + id,
		Handle:       "c-members-" + id + ".coves.social",
		Name:         "members-" + id,
		OwnerDID:     "did:web:coves.social",
		CreatedByDID: "did:plc:memcreator",
		HostedByDID:  "did:web:coves.social",
		Visibility:   "public",
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	})
	require.NoError(t, err, "seeding the community the memberships hang off")
	return repo, db, community
}

// storedPostCount reads the raw communities.post_count column — the vestigial
// stored counter IncrementPostCount advances, which nothing serves any more.
func storedPostCount(t *testing.T, db *sql.DB, communityDID string) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT post_count FROM communities WHERE did = $1`, communityDID).Scan(&count))
	return count
}

func aMembership(userDID, communityDID string) *communities.Membership {
	return &communities.Membership{
		UserDID:           userDID,
		CommunityDID:      communityDID,
		ReputationScore:   0,
		ContributionCount: 0,
		JoinedAt:          time.Now().UTC(),
		LastActiveAt:      time.Now().UTC(),
	}
}

func TestCommunityRepo_CreateMembership(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("round-trips every field", func(t *testing.T) {
		t.Parallel()
		repo, community := memberOf(t)
		joined := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)

		created, err := repo.CreateMembership(ctx, &communities.Membership{
			UserDID:           "did:plc:member0000000000000",
			CommunityDID:      community.DID,
			ReputationScore:   17,
			ContributionCount: 4,
			JoinedAt:          joined,
			LastActiveAt:      joined.Add(time.Hour),
			IsBanned:          false,
			IsModerator:       true,
		})
		require.NoError(t, err)
		assert.NotZero(t, created.ID, "the serial primary key must come back: callers have no other handle on the row")

		got, err := repo.GetMembership(ctx, "did:plc:member0000000000000", community.DID)
		require.NoError(t, err)
		assert.Equal(t, 17, got.ReputationScore)
		assert.Equal(t, 4, got.ContributionCount)
		assert.True(t, got.IsModerator, "the moderator flag decides who can act on a community; a "+
			"column that silently defaults to false is a moderator who cannot moderate")
		assert.False(t, got.IsBanned)
		assert.WithinDuration(t, joined, got.JoinedAt, time.Second)
	})

	t.Run("refuses a second membership for the same pair", func(t *testing.T) {
		t.Parallel()
		repo, community := memberOf(t)
		userDID := "did:plc:dupemember000000000"

		_, err := repo.CreateMembership(ctx, aMembership(userDID, community.DID))
		require.NoError(t, err)

		_, err = repo.CreateMembership(ctx, aMembership(userDID, community.DID))
		require.Error(t, err, "UNIQUE(user_did, community_did) is what keeps a member from being "+
			"counted twice; a duplicate that inserted would double their reputation and their vote weight")
		assert.ErrorContains(t, err, "membership already exists",
			"the raw constraint name is not something a handler can map; the repository owns the translation")
	})

	t.Run("reports a community that does not exist as such", func(t *testing.T) {
		t.Parallel()
		repo, _ := memberOf(t)

		_, err := repo.CreateMembership(ctx, aMembership("did:plc:member0000000000000", "did:plc:nosuchcommunity0000"))
		require.ErrorIs(t, err, communities.ErrCommunityNotFound,
			"the foreign key is the only thing that catches a membership for a community the AppView "+
				"has not indexed; surfacing it as a raw SQL error would make the handler answer 500 "+
				"to what is really a 404")
	})

	t.Run("the same user may join two communities", func(t *testing.T) {
		t.Parallel()
		repo, first := memberOf(t)
		id := testkit.UniqueID(t)
		second, err := repo.Create(ctx, &communities.Community{
			DID: "did:plc:second" + id, Handle: "c-second-" + id + ".coves.social", Name: "second-" + id,
			OwnerDID: "did:web:coves.social", CreatedByDID: "did:plc:memcreator",
			HostedByDID: "did:web:coves.social", Visibility: "public",
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
		})
		require.NoError(t, err)

		userDID := "did:plc:joiner0000000000000"
		_, err = repo.CreateMembership(ctx, aMembership(userDID, first.DID))
		require.NoError(t, err)
		_, err = repo.CreateMembership(ctx, aMembership(userDID, second.DID))
		require.NoError(t, err, "the uniqueness is per pair, not per user")
	})
}

func TestCommunityRepo_GetMembershipReportsAbsenceAsADomainError(t *testing.T) {
	t.Parallel()
	repo, community := memberOf(t)

	_, err := repo.GetMembership(context.Background(), "did:plc:stranger00000000000", community.DID)
	require.ErrorIs(t, err, communities.ErrMembershipNotFound,
		"sql.ErrNoRows leaking out of the repository would make every caller import database/sql to "+
			"tell 'not a member' from 'the query broke'")
}

func TestCommunityRepo_UpdateMembership(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("changes reputation, contributions and the two flags", func(t *testing.T) {
		t.Parallel()
		repo, community := memberOf(t)
		userDID := "did:plc:promoted00000000000"
		_, err := repo.CreateMembership(ctx, aMembership(userDID, community.DID))
		require.NoError(t, err)

		active := time.Date(2026, 6, 7, 8, 9, 10, 0, time.UTC)
		_, err = repo.UpdateMembership(ctx, &communities.Membership{
			UserDID:           userDID,
			CommunityDID:      community.DID,
			ReputationScore:   99,
			ContributionCount: 12,
			LastActiveAt:      active,
			IsBanned:          true,
			IsModerator:       true,
		})
		require.NoError(t, err)

		got, err := repo.GetMembership(ctx, userDID, community.DID)
		require.NoError(t, err)
		assert.Equal(t, 99, got.ReputationScore)
		assert.Equal(t, 12, got.ContributionCount)
		assert.True(t, got.IsBanned, "a ban that does not persist is a banned user who is not banned")
		assert.True(t, got.IsModerator)
		assert.WithinDuration(t, active, got.LastActiveAt, time.Second)
	})

	// joined_at is deliberately absent from the UPDATE. It is when the person
	// joined, and no later activity may rewrite it — a member's tenure is used
	// for ordering ties in ListMembers and would otherwise creep forward every
	// time their reputation changed.
	t.Run("leaves the join date alone", func(t *testing.T) {
		t.Parallel()
		repo, community := memberOf(t)
		userDID := "did:plc:tenured000000000000"
		joined := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

		membership := aMembership(userDID, community.DID)
		membership.JoinedAt = joined
		_, err := repo.CreateMembership(ctx, membership)
		require.NoError(t, err)

		_, err = repo.UpdateMembership(ctx, &communities.Membership{
			UserDID: userDID, CommunityDID: community.DID,
			ReputationScore: 5, LastActiveAt: time.Now().UTC(),
		})
		require.NoError(t, err)

		got, err := repo.GetMembership(ctx, userDID, community.DID)
		require.NoError(t, err)
		assert.WithinDuration(t, joined, got.JoinedAt, time.Second,
			"an update rewrote when the member joined")
	})

	t.Run("reports a membership that is not there", func(t *testing.T) {
		t.Parallel()
		repo, community := memberOf(t)

		_, err := repo.UpdateMembership(ctx, &communities.Membership{
			UserDID: "did:plc:stranger00000000000", CommunityDID: community.DID,
			LastActiveAt: time.Now().UTC(),
		})
		require.ErrorIs(t, err, communities.ErrMembershipNotFound,
			"RETURNING with no matching row is the only signal here; without the mapping an update "+
				"that hit nothing would look identical to one that worked")
	})
}

func TestCommunityRepo_ListMembers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// A fixed set of members with distinct reputations and join dates, so both
	// halves of the ORDER BY are observable.
	seed := func(t *testing.T) (communities.Repository, *communities.Community) {
		t.Helper()
		repo, community := memberOf(t)
		base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		for _, member := range []struct {
			did        string
			reputation int
			joinedDays int
		}{
			{"did:plc:member-low000000000", 1, 0},
			{"did:plc:member-high00000000", 100, 5},
			{"did:plc:member-mid000000000", 50, 2},
			// Same reputation as mid, joined earlier: the tiebreak is joined_at
			// ASC, so this one must come first of the two.
			{"did:plc:member-mid-elder000", 50, 1},
		} {
			membership := aMembership(member.did, community.DID)
			membership.ReputationScore = member.reputation
			membership.JoinedAt = base.AddDate(0, 0, member.joinedDays)
			_, err := repo.CreateMembership(ctx, membership)
			require.NoError(t, err)
		}
		return repo, community
	}

	t.Run("orders by reputation, then by seniority", func(t *testing.T) {
		t.Parallel()
		repo, community := seed(t)

		members, err := repo.ListMembers(ctx, community.DID, 10, 0)
		require.NoError(t, err)
		require.Len(t, members, 4)

		assert.Equal(t, []string{
			"did:plc:member-high00000000",
			"did:plc:member-mid-elder000",
			"did:plc:member-mid000000000",
			"did:plc:member-low000000000",
		}, didsOfMembers(members),
			"the member list is ranked by contribution; the join-date tiebreak is what keeps two "+
				"members with equal reputation from swapping places between page loads, which is a "+
				"pagination bug as well as a cosmetic one")
	})

	t.Run("paginates without skipping or repeating", func(t *testing.T) {
		t.Parallel()
		repo, community := seed(t)

		first, err := repo.ListMembers(ctx, community.DID, 2, 0)
		require.NoError(t, err)
		second, err := repo.ListMembers(ctx, community.DID, 2, 2)
		require.NoError(t, err)

		require.Len(t, first, 2)
		require.Len(t, second, 2)
		assert.Equal(t, []string{"did:plc:member-high00000000", "did:plc:member-mid-elder000"}, didsOfMembers(first))
		assert.Equal(t, []string{"did:plc:member-mid000000000", "did:plc:member-low000000000"}, didsOfMembers(second))

		third, err := repo.ListMembers(ctx, community.DID, 2, 4)
		require.NoError(t, err)
		assert.Empty(t, third, "reading past the end must be an empty page, not an error")
	})

	t.Run("scopes to the community asked for", func(t *testing.T) {
		t.Parallel()
		repo, community := seed(t)

		members, err := repo.ListMembers(ctx, "did:plc:someothercommunity0", 10, 0)
		require.NoError(t, err)
		assert.Empty(t, members,
			"an unknown community must answer with no members rather than with everybody's")

		mine, err := repo.ListMembers(ctx, community.DID, 10, 0)
		require.NoError(t, err)
		assert.Len(t, mine, 4)
	})

	t.Run("returns an empty slice rather than nil for a community with no members", func(t *testing.T) {
		t.Parallel()
		repo, community := memberOf(t)

		members, err := repo.ListMembers(ctx, community.DID, 10, 0)
		require.NoError(t, err)
		require.NotNil(t, members, "callers marshal this straight to JSON, where nil is null and an "+
			"empty slice is []; a client distinguishing them would see 'no such community'")
		assert.Empty(t, members)
	})
}

func didsOfMembers(members []*communities.Membership) []string {
	dids := make([]string, 0, len(members))
	for _, member := range members {
		dids = append(dids, member.UserDID)
	}
	return dids
}

func TestCommunityRepo_ModerationActions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("round-trips an action with a reason and an expiry", func(t *testing.T) {
		t.Parallel()
		repo, community := memberOf(t)
		expires := time.Date(2026, 12, 25, 0, 0, 0, 0, time.UTC)

		created, err := repo.CreateModerationAction(ctx, &communities.ModerationAction{
			CommunityDID: community.DID,
			Action:       "quarantine",
			Reason:       "repeated spam",
			InstanceDID:  "did:web:coves.social",
			Broadcast:    true,
			CreatedAt:    time.Now().UTC(),
			ExpiresAt:    &expires,
		})
		require.NoError(t, err)
		assert.NotZero(t, created.ID)

		actions, err := repo.ListModerationActions(ctx, community.DID, 10, 0)
		require.NoError(t, err)
		require.Len(t, actions, 1)
		assert.Equal(t, "quarantine", actions[0].Action)
		assert.Equal(t, "repeated spam", actions[0].Reason)
		assert.True(t, actions[0].Broadcast,
			"broadcast is what decides whether this moderation signal is shared with other instances; "+
				"losing it turns a federated action into a local one")
		require.NotNil(t, actions[0].ExpiresAt, "a temporary action with no expiry is a permanent one")
		assert.WithinDuration(t, expires, *actions[0].ExpiresAt, time.Second)
	})

	// reason is nullable and is written through nullString, so an action taken
	// with no explanation stores NULL rather than "". The read side scans into
	// a sql.NullString and flattens it back to "", which is what makes the
	// round trip lossless for the caller.
	t.Run("survives an action with no reason and no expiry", func(t *testing.T) {
		t.Parallel()
		repo, community := memberOf(t)

		_, err := repo.CreateModerationAction(ctx, &communities.ModerationAction{
			CommunityDID: community.DID,
			Action:       "delist",
			InstanceDID:  "did:web:coves.social",
			CreatedAt:    time.Now().UTC(),
		})
		require.NoError(t, err)

		actions, err := repo.ListModerationActions(ctx, community.DID, 10, 0)
		require.NoError(t, err)
		require.Len(t, actions, 1)
		assert.Empty(t, actions[0].Reason, "a NULL reason must read back as the empty string, not as a scan error")
		assert.Nil(t, actions[0].ExpiresAt)
		assert.False(t, actions[0].Broadcast, "broadcast defaults to false: a moderation action is "+
			"local unless someone says otherwise")
	})

	t.Run("refuses an action the lexicon does not define", func(t *testing.T) {
		t.Parallel()
		repo, community := memberOf(t)

		_, err := repo.CreateModerationAction(ctx, &communities.ModerationAction{
			CommunityDID: community.DID,
			Action:       "shadowban",
			InstanceDID:  "did:web:coves.social",
			CreatedAt:    time.Now().UTC(),
		})
		require.Error(t, err, "the CHECK constraint allows delist, quarantine and remove only. A "+
			"moderation action no peer instance knows how to interpret is worse than none, because it "+
			"federates")
	})

	t.Run("reports a community that does not exist", func(t *testing.T) {
		t.Parallel()
		repo, _ := memberOf(t)

		_, err := repo.CreateModerationAction(ctx, &communities.ModerationAction{
			CommunityDID: "did:plc:nosuchcommunity0000",
			Action:       "remove",
			InstanceDID:  "did:web:coves.social",
			CreatedAt:    time.Now().UTC(),
		})
		require.ErrorIs(t, err, communities.ErrCommunityNotFound)
	})

	t.Run("lists newest first and paginates", func(t *testing.T) {
		t.Parallel()
		repo, community := memberOf(t)
		base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
		for i, action := range []string{"delist", "quarantine", "remove"} {
			_, err := repo.CreateModerationAction(ctx, &communities.ModerationAction{
				CommunityDID: community.DID,
				Action:       action,
				InstanceDID:  "did:web:coves.social",
				CreatedAt:    base.AddDate(0, 0, i),
			})
			require.NoError(t, err)
		}

		actions, err := repo.ListModerationActions(ctx, community.DID, 10, 0)
		require.NoError(t, err)
		require.Len(t, actions, 3)
		assert.Equal(t, []string{"remove", "quarantine", "delist"}, actionNames(actions),
			"the most recent decision is the one in force; listing oldest-first would put a lifted "+
				"quarantine above the removal that replaced it")

		page, err := repo.ListModerationActions(ctx, community.DID, 1, 1)
		require.NoError(t, err)
		require.Len(t, page, 1)
		assert.Equal(t, "quarantine", page[0].Action)
	})

	t.Run("returns an empty slice for a community nobody has moderated", func(t *testing.T) {
		t.Parallel()
		repo, community := memberOf(t)

		actions, err := repo.ListModerationActions(ctx, community.DID, 10, 0)
		require.NoError(t, err)
		require.NotNil(t, actions)
		assert.Empty(t, actions)
	})
}

func actionNames(actions []*communities.ModerationAction) []string {
	names := make([]string, 0, len(actions))
	for _, action := range actions {
		names = append(names, action.Action)
	}
	return names
}

func TestCommunityRepo_Counters(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	countsOf := func(t *testing.T, repo communities.Repository, did string) (members, subscribers, posts int) {
		t.Helper()
		community, err := repo.GetByDID(ctx, did)
		require.NoError(t, err)
		return community.MemberCount, community.SubscriberCount, community.PostCount
	}

	t.Run("each counter moves only its own column", func(t *testing.T) {
		t.Parallel()
		repo, db, community := memberOfWithDB(t)

		require.NoError(t, repo.IncrementMemberCount(ctx, community.DID))
		members, subscribers, posts := countsOf(t, repo, community.DID)
		assert.Equal(t, 1, members)
		assert.Zero(t, subscribers, "incrementing members moved the subscriber count")
		assert.Zero(t, posts)
		assert.Zero(t, storedPostCount(t, db, community.DID), "incrementing members moved the post count column")

		require.NoError(t, repo.IncrementSubscriberCount(ctx, community.DID))
		require.NoError(t, repo.IncrementSubscriberCount(ctx, community.DID))
		require.NoError(t, repo.IncrementPostCount(ctx, community.DID))
		members, subscribers, posts = countsOf(t, repo, community.DID)
		assert.Equal(t, 1, members)
		assert.Equal(t, 2, subscribers)
		assert.Equal(t, 1, storedPostCount(t, db, community.DID),
			"IncrementPostCount must still move its own column and only its own column")

		// The SERVED postCount is not that column. It is a live count over the
		// read-path visibility predicate, and this community has no posts — so
		// advancing the stored counter changes nothing a client can see. That
		// asymmetry is the point: the stored column is vestigial (PRD §12), and
		// a reader who assumes wiring the incrementer would fix postCount needs
		// to meet this assertion rather than discover it in production.
		assert.Zerof(t, posts,
			"the SERVED postCount followed the stored column. It must be the live visibility-gated count — this "+
				"community has zero posts, so the only honest answer is 0 no matter what IncrementPostCount did")
	})

	t.Run("decrements come back down", func(t *testing.T) {
		t.Parallel()
		repo, community := memberOf(t)

		require.NoError(t, repo.IncrementMemberCount(ctx, community.DID))
		require.NoError(t, repo.IncrementMemberCount(ctx, community.DID))
		require.NoError(t, repo.DecrementMemberCount(ctx, community.DID))
		require.NoError(t, repo.IncrementSubscriberCount(ctx, community.DID))
		require.NoError(t, repo.DecrementSubscriberCount(ctx, community.DID))

		members, subscribers, _ := countsOf(t, repo, community.DID)
		assert.Equal(t, 1, members)
		assert.Zero(t, subscribers)
	})

	// GREATEST(0, …) is the reason a duplicate unsubscribe cannot drive a
	// community to "-1 subscribers". The firehose delivers at-least-once and
	// the consumer's decrement is not idempotent, so this floor is load-bearing
	// rather than defensive.
	t.Run("decrements floor at zero instead of going negative", func(t *testing.T) {
		t.Parallel()
		repo, community := memberOf(t)

		require.NoError(t, repo.DecrementMemberCount(ctx, community.DID))
		require.NoError(t, repo.DecrementMemberCount(ctx, community.DID))
		require.NoError(t, repo.DecrementSubscriberCount(ctx, community.DID))

		members, subscribers, _ := countsOf(t, repo, community.DID)
		assert.Zero(t, members, "a member count below zero renders as '-1 members' and breaks every "+
			"percentage computed from it")
		assert.Zero(t, subscribers)
	})

	// There is no DecrementPostCount. Posts are counted up only, and a deleted
	// post leaves the number where it is. Pinned so the asymmetry is a decision
	// on the record rather than something a reader has to notice.
	t.Run("the post count only goes up", func(t *testing.T) {
		t.Parallel()
		repo, db, community := memberOfWithDB(t)
		require.NoError(t, repo.IncrementPostCount(ctx, community.DID))

		assert.Equal(t, 1, storedPostCount(t, db, community.DID))
		assert.NotImplements(t, (*interface {
			DecrementPostCount(context.Context, string) error
		})(nil), repo,
			"IF THIS FAILED, a post-count decrement was added. Deleting a post should then reduce the "+
				"count, and this test should assert that instead")
	})

	t.Run("counters scope to the community named", func(t *testing.T) {
		t.Parallel()
		repo, first := memberOf(t)
		id := testkit.UniqueID(t)
		second, err := repo.Create(ctx, &communities.Community{
			DID: "did:plc:bystander" + id, Handle: "c-bystander-" + id + ".coves.social",
			Name: "bystander-" + id, OwnerDID: "did:web:coves.social",
			CreatedByDID: "did:plc:memcreator", HostedByDID: "did:web:coves.social",
			Visibility: "public", CreatedAt: time.Now(), UpdatedAt: time.Now(),
		})
		require.NoError(t, err)

		require.NoError(t, repo.IncrementSubscriberCount(ctx, first.DID))

		_, subscribers, _ := countsOf(t, repo, second.DID)
		assert.Zero(t, subscribers, "a subscription to one community moved another's count")
	})
}

// TestCommunityRepo_CountersOnAnAbsentCommunityAreSilent pins the sharpest
// behaviour in this file.
//
// All five counters are bare UPDATE … WHERE did = $1 with no RowsAffected
// check, so incrementing a community the AppView has never indexed matches zero
// rows and returns nil. The caller is told the count went up.
//
// This is not (today) a defect on its own: every caller is a firehose consumer
// that has already resolved the community, and the sibling consumers gate on
// the community existing. It IS the mechanism behind the vote-count drift filed
// as issue 2026-07-29-vote-before-subject-lost-then-subtracts — the same shape
// one table over, an UPDATE that matches nothing, logged and forgotten. That
// issue is FIXED for votes: the vote consumer now refuses an event whose subject
// has no row instead of indexing it against a zero-row count update. The
// mechanism described here survives untouched, because these counters were never
// what was changed. Recorded so the next person to add a counter caller knows
// that a nil error from these methods is not evidence anything happened.
func TestCommunityRepo_CountersOnAnAbsentCommunityAreSilent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := NewCommunityRepository(testkit.DB(t))
	absent := "did:plc:neverindexed0000000"

	for name, increment := range map[string]func(context.Context, string) error{
		"IncrementMemberCount":     repo.IncrementMemberCount,
		"DecrementMemberCount":     repo.DecrementMemberCount,
		"IncrementSubscriberCount": repo.IncrementSubscriberCount,
		"DecrementSubscriberCount": repo.DecrementSubscriberCount,
		"IncrementPostCount":       repo.IncrementPostCount,
	} {
		assert.NoErrorf(t, increment(ctx, absent),
			"IF THIS FAILED, %s learned to report that it matched no rows. That is an improvement — "+
				"assert the new error here rather than reverting", name)
	}

	_, err := repo.GetByDID(ctx, absent)
	assert.ErrorIs(t, err, communities.ErrCommunityNotFound,
		"and no row was conjured by the updates")
}

// TestCommunityRepo_DeletingACommunityTakesItsMembershipsWithIt covers the ON
// DELETE CASCADE on both child tables, which is the only thing standing between
// a deleted community and rows that reference a DID nothing hosts.
func TestCommunityRepo_DeletingACommunityTakesItsMembershipsWithIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, community := memberOf(t)
	userDID := "did:plc:orphan0000000000000"

	_, err := repo.CreateMembership(ctx, aMembership(userDID, community.DID))
	require.NoError(t, err)
	_, err = repo.CreateModerationAction(ctx, &communities.ModerationAction{
		CommunityDID: community.DID, Action: "delist",
		InstanceDID: "did:web:coves.social", CreatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	require.NoError(t, repo.Delete(ctx, community.DID))

	_, err = repo.GetMembership(ctx, userDID, community.DID)
	assert.ErrorIs(t, err, communities.ErrMembershipNotFound,
		"a membership outliving its community is a row no query can join and no user can leave")

	actions, err := repo.ListModerationActions(ctx, community.DID, 10, 0)
	require.NoError(t, err)
	assert.Empty(t, actions)
}
