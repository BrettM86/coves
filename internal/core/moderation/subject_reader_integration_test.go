//go:build integration

package moderation_test

import (
	"testing"
	"time"

	"Coves/internal/core/moderation"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRepositorySubjectReaderIndexedRecords(t *testing.T) {
	db := testkit.DB(t)
	ctx := t.Context()
	reader := moderation.NewRepositorySubjectReader(postgres.NewPostRepository(db), postgres.NewCommentRepository(db))

	authorName := testkit.UniqueIDWithPrefix(t, "subauthor")
	authorDID := fixtures.DID(authorName)
	fixtures.User(t, db, authorName+".test", authorDID)
	communityName := testkit.UniqueIDWithPrefix(t, "subcommunity")
	communityDID, err := fixtures.Community(ctx, db, communityName, "owner"+communityName)
	require.NoError(t, err)

	legacyURI := fixtures.Post(t, db, communityDID, authorDID, "legacy", 0, time.Now())
	deletedLegacyURI := fixtures.Post(t, db, communityDID, authorDID, "deleted legacy", 0, time.Now())
	const legacyCID = "bafytest"

	insertPostV2 := func(title, cid string) string {
		t.Helper()
		rkey := testkit.TID()
		uri := "at://" + authorDID + "/" + moderation.PostV2Collection + "/" + rkey
		_, err := db.ExecContext(ctx, `
			INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, NOW())
		`, uri, cid, rkey, authorDID, communityDID, title)
		require.NoError(t, err)
		return uri
	}
	postV2CID := "bafyreipresent" + testkit.UniqueIDWithPrefix(t, "cid")
	postV2URI := insertPostV2("present postv2", postV2CID)
	deletedPostV2URI := insertPostV2("deleted postv2", "bafyreideletedpostv2")
	_, err = db.ExecContext(ctx, `UPDATE posts SET deleted_at = NOW() WHERE uri IN ($1, $2)`, deletedPostV2URI, deletedLegacyURI)
	require.NoError(t, err)

	pendingURI := insertPostV2("unadmitted postv2", "bafyreipendingpostv2")
	_, err = db.ExecContext(ctx, `
		INSERT INTO community_post_admissions (community_did, post_uri, status, evaluated_cid)
		VALUES ($1, $2, 'pending', $3)
	`, communityDID, pendingURI, "bafyreipendingpostv2")
	require.NoError(t, err)

	insertComment := func(cid string) string {
		t.Helper()
		rkey := testkit.TID()
		uri := "at://" + authorDID + "/" + moderation.CommentCollection + "/" + rkey
		_, err := db.ExecContext(ctx, `
			INSERT INTO comments (uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid, content, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $5, $6, $7, NOW())
		`, uri, cid, rkey, authorDID, postV2URI, postV2CID, "a comment")
		require.NoError(t, err)
		return uri
	}
	commentURI := insertComment("bafyreipresentcomment")
	deletedCommentURI := insertComment("bafyreideletedcomment")
	moderatorDeletedCommentCID := "bafyreimoderatordeletedcomment"
	moderatorDeletedCommentURI := insertComment(moderatorDeletedCommentCID)
	_, err = db.ExecContext(ctx, `
		UPDATE comments SET deleted_at = NOW(), deletion_reason = 'author', deleted_by = $1 WHERE uri = $2
	`, authorDID, deletedCommentURI)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		UPDATE comments SET deleted_at = NOW(), deletion_reason = 'moderator', deleted_by = $1, content = '' WHERE uri = $2
	`, communityDID, moderatorDeletedCommentURI)
	require.NoError(t, err)

	for _, test := range []struct {
		name    string
		uri     string
		cid     string
		deleted bool
	}{
		{"indexed postv2", postV2URI, postV2CID, false},
		{"indexed legacy post", legacyURI, legacyCID, false},
		{"indexed comment", commentURI, "bafyreipresentcomment", false},
		{"author-deleted comment", deletedCommentURI, "bafyreideletedcomment", true},
		{"legacy moderator-deleted comment", moderatorDeletedCommentURI, moderatorDeletedCommentCID, false},
		{"soft-deleted postv2", deletedPostV2URI, "bafyreideletedpostv2", true},
		{"soft-deleted legacy post", deletedLegacyURI, legacyCID, true},
		{"pending postv2", pendingURI, "bafyreipendingpostv2", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			record, err := reader.ReadSubject(t.Context(), test.uri)
			require.NoError(t, err)
			assert.Equal(t, &moderation.IndexedRecord{URI: test.uri, CID: test.cid, Deleted: test.deleted}, record)
		})
	}

	for _, test := range []struct {
		name string
		uri  string
	}{
		{"never-indexed postv2", "at://" + authorDID + "/" + moderation.PostV2Collection + "/" + testkit.TID()},
		{"never-indexed legacy post", "at://" + communityDID + "/" + moderation.LegacyPostCollection + "/" + testkit.TID()},
		{"never-indexed comment", "at://" + authorDID + "/" + moderation.CommentCollection + "/" + testkit.TID()},
	} {
		t.Run(test.name, func(t *testing.T) {
			record, err := reader.ReadSubject(t.Context(), test.uri)
			assert.Nil(t, record)
			assert.ErrorIs(t, err, moderation.ErrSubjectNotIndexed)
		})
	}

	service := moderation.NewService(reader, postgres.NewModerationRepository(db), moderation.Config{
		InstanceDID: fixtures.InstanceDID(), IdempotencyRetention: 24 * time.Hour, MaxLiveIdempotencyKeys: 1000,
	})
	t.Run("service author-deleted comment", func(t *testing.T) {
		state, err := service.GetSubjectState(t.Context(), deletedCommentURI)
		require.NoError(t, err)
		require.NotNil(t, state)
		assert.Equal(t, moderation.RecordStateDeleted, state.RecordState)
		assert.Nil(t, state.CurrentSubject)
	})
	t.Run("service legacy moderator-deleted comment", func(t *testing.T) {
		state, err := service.GetSubjectState(t.Context(), moderatorDeletedCommentURI)
		require.NoError(t, err)
		require.NotNil(t, state)
		assert.Equal(t, moderation.RecordStatePresent, state.RecordState)
		assert.Equal(t, &moderation.StrongRef{URI: moderatorDeletedCommentURI, CID: moderatorDeletedCommentCID}, state.CurrentSubject)
	})
	t.Run("legacy moderator-deleted comment still requires matching CID to remove", func(t *testing.T) {
		_, err := service.RemoveContent(t.Context(), fixtures.DID(testkit.UniqueIDWithPrefix(t, "legacyadmin")), moderation.RemoveContentRequest{
			Subject:         moderation.StrongRef{URI: moderatorDeletedCommentURI, CID: "bafyreigj3fwnwjuzr35k2kuzmb5dixxczrzjhqkr5srlqplsh6gq3bj3si"},
			ExpectedVersion: "v0", IdempotencyKey: "legacy-mismatched-cid", Reason: "social.coves.moderation.defs#reasonSpam",
		})
		assert.ErrorIs(t, err, moderation.ErrContentChanged)
	})
	t.Run("service indexed postv2", func(t *testing.T) {
		state, err := service.GetSubjectState(t.Context(), postV2URI)
		require.NoError(t, err)
		require.NotNil(t, state)
		assert.Equal(t, moderation.RecordStatePresent, state.RecordState)
		assert.Equal(t, &moderation.StrongRef{URI: postV2URI, CID: postV2CID}, state.CurrentSubject)
	})
	t.Run("service never-indexed version", func(t *testing.T) {
		uri := "at://" + authorDID + "/" + moderation.PostV2Collection + "/" + testkit.TID()
		first, err := service.GetSubjectState(t.Context(), uri)
		require.NoError(t, err)
		second, err := service.GetSubjectState(t.Context(), uri)
		require.NoError(t, err)
		require.NotNil(t, first)
		require.NotNil(t, second)
		assert.Equal(t, moderation.RecordStateUnavailable, first.RecordState)
		assert.Equal(t, "v0", first.Version)
		assert.Equal(t, first.Version, second.Version)
	})
}

func TestRepositorySubjectReaderDatabaseFailureIsNotMissing(t *testing.T) {
	// This is a dedicated clone: testkit closes its pool during cleanup, and
	// closing the pool here must not interrupt another test's fixture database.
	db := testkit.DB(t)
	reader := moderation.NewRepositorySubjectReader(postgres.NewPostRepository(db), postgres.NewCommentRepository(db))
	require.NoError(t, db.Close())
	uri := "at://" + fixtures.DID(testkit.UniqueIDWithPrefix(t, "dbfailure")) + "/" + moderation.PostV2Collection + "/" + testkit.TID()

	record, err := reader.ReadSubject(t.Context(), uri)
	assert.Nil(t, record)
	require.Error(t, err)
	assert.NotErrorIs(t, err, moderation.ErrSubjectNotIndexed)
}
