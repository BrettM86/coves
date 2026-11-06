//go:build integration

package postgres_test

import (
	"testing"
	"time"

	"Coves/internal/core/moderation"
	"Coves/internal/core/users"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/fixtures/accountbanfixture"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAccountBanProfileStatsOnSelfView bans A, who has two accepted posts, one
// pending post and one comment. During the ban A's own profile still counts
// the two accepted posts and the comment (the pending post stays out, as for
// every profile), while every other viewer reads the suspended profile with
// zero activity.
func TestAccountBanProfileStatsOnSelfView(t *testing.T) {
	db := testkit.DB(t)
	communityDID, authorDID := moderationPostActors(t, db)
	viewerName := testkit.UniqueIDWithPrefix(t, "statsviewer")
	viewerDID := fixtures.DID(viewerName)
	fixtures.User(t, db, viewerName+".test", viewerDID)

	accepted := indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, "accepted post", "")
	indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, "second accepted post", "")
	pending := indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, "pending post", "")
	_, err := db.ExecContext(t.Context(), `
		UPDATE community_post_admissions SET status = 'pending', accepted_cid = NULL
		WHERE community_did = $1 AND post_uri = $2
	`, communityDID, pending.URI)
	require.NoError(t, err)
	commentRecordKey := testkit.TID()
	_, err = db.ExecContext(t.Context(), `
		INSERT INTO comments (uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid, content, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $5, $6, 'a comment', $7)
	`, "at://"+authorDID+"/"+moderation.CommentCollection+"/"+commentRecordKey, moderationPostCID, commentRecordKey,
		authorDID, accepted.URI, accepted.CID, time.Now().Add(-time.Minute))
	require.NoError(t, err)

	accountModerator := accountbanfixture.New(db)
	userRepo := postgres.NewUserRepository(db)
	service := users.NewUserService(userRepo, nil, "", nil, "", users.WithAccountBans(accountModerator.Reader()))

	before, err := userRepo.GetProfileStats(t.Context(), authorDID)
	require.NoError(t, err)
	require.Equal(t, 2, before.PostCount, "fixture: two accepted posts; the pending post is not counted")
	require.Equal(t, 1, before.CommentCount, "fixture: one comment")

	accountModerator.Ban(t, authorDID, nil)

	t.Run("own profile keeps real counts", func(t *testing.T) {
		profile, err := service.GetProfileForViewer(t.Context(), authorDID, authorDID)
		require.NoError(t, err)
		require.NotNil(t, profile.BanNotice, "the banned account reads its own profile with the ban notice")
		require.NotNil(t, profile.Stats)
		assert.Equal(t, 2, profile.Stats.PostCount)
		assert.Equal(t, 1, profile.Stats.CommentCount)
	})
	for _, viewer := range []struct{ name, did string }{{"other viewer", viewerDID}, {"signed out", ""}} {
		t.Run(viewer.name+" reads zero activity", func(t *testing.T) {
			profile, err := service.GetProfileForViewer(t.Context(), authorDID, viewer.did)
			require.NoError(t, err)
			assert.Equal(t, users.AccountStatusSuspended, profile.AccountStatus)
			require.NotNil(t, profile.Stats)
			assert.Zero(t, profile.Stats.PostCount)
			assert.Zero(t, profile.Stats.CommentCount)
		})
	}
}
