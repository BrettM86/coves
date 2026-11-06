//go:build integration

package votes_test

import (
	"context"
	"testing"
	"time"

	"Coves/internal/core/comments"
	"Coves/internal/core/communities"
	"Coves/internal/core/votes"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/fixtures/accountbanfixture"
	"Coves/tests/fixtures/communityremovalfixture"
	"Coves/tests/testkit"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/stretchr/testify/require"
)

// TestCreateVote_CommunityBan is the vote write path's ban gate wired to the
// real ban index: a voter banned in the community a post lives in — or in the
// community of the post at the root of a comment's thread — is refused with
// votes.ErrBanned before anything reaches their PDS. A ban elsewhere, or one
// that has lapsed, writes the vote.
func TestCreateVote_CommunityBan(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()

	communityDID, err := fixtures.Community(ctx, db, "banvotec", "banvoteowner")
	require.NoError(t, err)
	otherCommunityDID, err := fixtures.Community(ctx, db, "banvotec2", "banvoteowner")
	require.NoError(t, err)

	postAuthorDID := fixtures.DID("banvoteauthor")
	postURI := fixtures.Post(t, db, communityDID, postAuthorDID, "a post to vote on", 0, time.Now())

	commentRKey := testkit.TID()
	commentURI := "at://" + postAuthorDID + "/social.coves.community.comment/" + commentRKey
	require.NoError(t, postgres.NewCommentRepository(db).Create(ctx, &comments.Comment{
		URI:          commentURI,
		CID:          "bafybanvotecomment",
		RKey:         commentRKey,
		CommenterDID: postAuthorDID,
		RootURI:      postURI,
		RootCID:      "bafytest",
		ParentURI:    postURI,
		ParentCID:    "bafytest",
		Content:      "a comment to vote on",
		CreatedAt:    time.Now(),
	}))

	// A reply to commentURI: resolving its community walks two comments up.
	depthTwoRKey := testkit.TID()
	depthTwoURI := "at://" + postAuthorDID + "/social.coves.community.comment/" + depthTwoRKey
	require.NoError(t, postgres.NewCommentRepository(db).Create(ctx, &comments.Comment{
		URI:          depthTwoURI,
		CID:          "bafybanvotedepthtwo",
		RKey:         depthTwoRKey,
		CommenterDID: postAuthorDID,
		RootURI:      postURI,
		RootCID:      "bafytest",
		ParentURI:    commentURI,
		ParentCID:    "bafybanvotecomment",
		Content:      "a reply to vote on",
		CreatedAt:    time.Now(),
	}))

	bans := postgres.NewCommunityBanRepository(db)
	service := func(fake *fakePDS) votes.Service {
		return votes.NewServiceWithPDSFactory(postgres.NewVoteRepository(db), nil,
			func(context.Context, *oauth.ClientSessionData) (votes.PDSClient, error) { return fake, nil },
			votes.WithCommunityBans(bans))
	}

	activeUntil := time.Now().Add(24 * time.Hour).UTC()
	expiredAt := time.Now().Add(-time.Hour).UTC()

	rows := []struct {
		name         string
		voterDID     string
		subjectURI   string
		banCommunity string
		banExpiresAt *time.Time
		wantBanned   bool
	}{
		{"active ban in the post's community refuses a vote on the post", fixtures.DID("banvoteactive"), postURI, communityDID, &activeUntil, true},
		{"active ban in the thread root's community refuses a vote on a comment", fixtures.DID("banvotecomment"), commentURI, communityDID, &activeUntil, true},
		{"active ban in the thread root's community refuses a vote on a reply to a comment", fixtures.DID("banvotedeep"), depthTwoURI, communityDID, &activeUntil, true},
		{"a ban only in another community writes the vote", fixtures.DID("banvoteother"), postURI, otherCommunityDID, &activeUntil, false},
		{"a ban that expired an hour ago writes the vote", fixtures.DID("banvoteexpired"), postURI, communityDID, &expiredAt, false},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			require.NoError(t, bans.UpsertCommunityBan(ctx, communities.CommunityBan{
				CommunityDID: row.banCommunity,
				SubjectDID:   row.voterDID,
				RecordKey:    communities.BanRecordKey(row.voterDID),
				RecordCID:    "bafybanvoterecord",
				Reason:       "spam",
				ExpiresAt:    row.banExpiresAt,
			}))

			fake := newFakePDS(t, row.voterDID)
			_, err := service(fake).CreateVote(ctx, voter(t, row.voterDID), votes.CreateVoteRequest{
				Subject:   votes.StrongRef{URI: row.subjectURI, CID: "bafytest"},
				Direction: "up",
			})

			if row.wantBanned {
				require.ErrorIs(t, err, votes.ErrBanned)
				require.Empty(t, fake.ops, "a refused vote must not write to the voter's PDS")
				return
			}
			require.NoError(t, err)
			require.Len(t, fake.creates(), 1, "an admitted vote must be written to the voter's PDS")
			require.Equal(t, "up", fake.records[fake.creates()[0]].Direction)
			require.Equal(t, row.subjectURI, fake.records[fake.creates()[0]].Subject.URI)
		})
	}
}

// TestCreateVote_AccountBan is the vote write path's instance account ban gate
// wired to the Postgres account-ban read, with the ban written by the real
// moderation service: a banned account is refused with votes.ErrAccountBanned before
// anything reaches its PDS — including on an unindexed subject, which the
// community-ban gate admits. Lifting the ban, or a temporary ban running out,
// writes the vote.
func TestCreateVote_AccountBan(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()

	communityDID, err := fixtures.Community(ctx, db, "acctbanvotec", "acctbanvoteowner")
	require.NoError(t, err)
	postURI := fixtures.Post(t, db, communityDID, fixtures.DID("acctbanvoteauthor"), "a post to vote on", 0, time.Now())
	unindexedURI := "at://" + fixtures.DID("acctbanvoteauthor") + "/social.coves.community.post/" + testkit.TID()

	moderator := accountbanfixture.New(db)
	service := func(fake *fakePDS) votes.Service {
		return votes.NewServiceWithPDSFactory(postgres.NewVoteRepository(db), nil,
			func(context.Context, *oauth.ClientSessionData) (votes.PDSClient, error) { return fake, nil },
			votes.WithCommunityBans(postgres.NewCommunityBanRepository(db)),
			votes.WithAccountBans(moderator.Reader()))
	}
	banUntilTomorrow := func(t *testing.T, accountDID string) {
		expiresAt := time.Now().Add(24 * time.Hour).UTC()
		moderator.Ban(t, accountDID, &expiresAt)
	}

	rows := []struct {
		name       string
		voterDID   string
		subjectURI string
		setup      func(t *testing.T, accountDID string)
		wantBanned bool
	}{
		{"a permanent account ban refuses the vote", fixtures.DID("acctbanvoteperm"), postURI, func(t *testing.T, accountDID string) {
			moderator.Ban(t, accountDID, nil)
		}, true},
		{"a temporary account ban in force refuses the vote", fixtures.DID("acctbanvotetemp"), postURI, banUntilTomorrow, true},
		{"an account ban refuses a vote on an unindexed subject", fixtures.DID("acctbanvoteunx"), unindexedURI, func(t *testing.T, accountDID string) {
			moderator.Ban(t, accountDID, nil)
		}, true},
		{"an unbanned account writes the vote", fixtures.DID("acctbanvoteunban"), postURI, func(t *testing.T, accountDID string) {
			moderator.Ban(t, accountDID, nil)
			moderator.Unban(t, accountDID)
		}, false},
		{"a lapsed temporary ban writes the vote with no unban", fixtures.DID("acctbanvotelapse"), postURI, func(t *testing.T, accountDID string) {
			banUntilTomorrow(t, accountDID)
			moderator.Lapse(t, accountDID)
		}, false},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			row.setup(t, row.voterDID)

			fake := newFakePDS(t, row.voterDID)
			_, err := service(fake).CreateVote(ctx, voter(t, row.voterDID), votes.CreateVoteRequest{
				Subject:   votes.StrongRef{URI: row.subjectURI, CID: "bafytest"},
				Direction: "up",
			})

			if row.wantBanned {
				require.ErrorIs(t, err, votes.ErrAccountBanned)
				require.Empty(t, fake.ops, "a refused vote must not write to the voter's PDS")
				return
			}
			require.NoError(t, err)
			require.Len(t, fake.creates(), 1, "an admitted vote must be written to the voter's PDS")
			require.Equal(t, row.subjectURI, fake.records[fake.creates()[0]].Subject.URI)
		})
	}
}

// TestCreateVote_CommunityRemoval is the vote write path's community removal
// gate wired to the Postgres removal reader, with the removal written by the
// real moderation service: a vote on a post, or on a comment, in a removed
// community is refused with votes.ErrCommunityRemoved before anything reaches
// the voter's PDS. After restore the vote is written.
func TestCreateVote_CommunityRemoval(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	ctx := context.Background()

	communityDID, err := fixtures.Community(ctx, db, "removalvotec", "removalvoteowner")
	require.NoError(t, err)
	postAuthorDID := fixtures.DID("removalvoteauthor")
	postURI := fixtures.Post(t, db, communityDID, postAuthorDID, "a post to vote on", 0, time.Now())

	commentRKey := testkit.TID()
	commentURI := "at://" + postAuthorDID + "/social.coves.community.comment/" + commentRKey
	require.NoError(t, postgres.NewCommentRepository(db).Create(ctx, &comments.Comment{
		URI:          commentURI,
		CID:          "bafyremovalvotecomment",
		RKey:         commentRKey,
		CommenterDID: postAuthorDID,
		RootURI:      postURI,
		RootCID:      "bafytest",
		ParentURI:    postURI,
		ParentCID:    "bafytest",
		Content:      "a comment to vote on",
		CreatedAt:    time.Now(),
	}))

	vote := func(t *testing.T, voterDID, subjectURI string) (*fakePDS, error) {
		t.Helper()
		fake := newFakePDS(t, voterDID)
		service := votes.NewServiceWithPDSFactory(postgres.NewVoteRepository(db), nil,
			func(context.Context, *oauth.ClientSessionData) (votes.PDSClient, error) { return fake, nil },
			votes.WithCommunityBans(postgres.NewCommunityBanRepository(db)),
			votes.WithCommunityRemovals(postgres.NewCommunityRemovalReader(db, fixtures.InstanceDID())))
		_, err := service.CreateVote(ctx, voter(t, voterDID), votes.CreateVoteRequest{
			Subject:   votes.StrongRef{URI: subjectURI, CID: "bafytest"},
			Direction: "up",
		})
		return fake, err
	}

	moderator := communityremovalfixture.New(db)
	moderator.Remove(t, communityDID)
	for _, row := range []struct {
		name       string
		voterDID   string
		subjectURI string
	}{
		{"a vote on a post in the removed community is refused", fixtures.DID("removalvotepost"), postURI},
		{"a vote on a comment in the removed community is refused", fixtures.DID("removalvotecomment"), commentURI},
	} {
		t.Run(row.name, func(t *testing.T) {
			fake, err := vote(t, row.voterDID, row.subjectURI)
			require.ErrorIs(t, err, votes.ErrCommunityRemoved)
			require.Empty(t, fake.ops, "a refused vote must not write to the voter's PDS")
		})
	}

	moderator.Restore(t, communityDID)
	t.Run("after restore a vote on the post is written", func(t *testing.T) {
		fake, err := vote(t, fixtures.DID("removalvoterestored"), postURI)
		require.NoError(t, err)
		require.Len(t, fake.creates(), 1, "an admitted vote must be written to the voter's PDS")
		require.Equal(t, postURI, fake.records[fake.creates()[0]].Subject.URI)
	})
}
