//go:build integration

package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/tests/testkit"

	"github.com/lib/pq"
	"github.com/lib/pq/pqerror"
	"github.com/stretchr/testify/require"
)

func TestCommunityDeleteContentBlobReferences_PostSubjectsRemainInactive(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	repo := NewCommunityRepository(db, credentialciphertest.Fixed())
	community := visibilityCommunity(t, db, testkit.UniqueID(t))
	author := "did:plc:" + testkit.UniqueID(t)
	createdAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	post := seedVisibilityPost(t, db, community, author, testkit.TID(), "image post", createdAt)
	external := seedVisibilityPostWithEmbed(t, db, community, author, testkit.TID(), "external thumbnail", communityBannerB, createdAt)
	video := seedVisibilityPost(t, db, community, author, testkit.TID(), "video thumbnail", createdAt)
	legacy := seedLegacyPost(t, db, community, author, testkit.TID(), "legacy image", createdAt)
	// Index rows are the delete contract's input; extraction is owned by other tests.
	for _, reference := range []contentBackfillReference{
		{author, communityAvatarA, post, "post_image", true},
		{author, communityBannerB, post, "post_image", true},
		{author, communityBannerB, external, "external_thumb", true},
		{author, communityAvatarC, video, "video_thumb", true},
		{community, communityAvatarA, legacy, "post_image", true},
	} {
		_, err := db.Exec(`INSERT INTO blob_references (owner_did, blob_cid, subject_uri, kind, active)
			VALUES ($1, $2, $3, $4, $5)`, reference.OwnerDID, reference.BlobCID, reference.SubjectURI, reference.Kind, reference.Active)
		require.NoError(t, err)
	}

	require.NoError(t, repo.Delete(context.Background(), community))
	require.ElementsMatch(t, []contentBackfillReference{
		{author, communityAvatarA, post, "post_image", false},
		{author, communityBannerB, post, "post_image", false},
		{author, communityBannerB, external, "external_thumb", false},
		{author, communityAvatarC, video, "video_thumb", false},
		{community, communityAvatarA, legacy, "post_image", false},
	}, contentBackfillRows(t, db, post, external, video, legacy), "deleted post subjects must retain every reference as inactive, regardless of owner or kind")
	var communitiesRemaining, postsRemaining int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM communities WHERE did = $1`, community).Scan(&communitiesRemaining))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM posts WHERE community_did = $1`, community).Scan(&postsRemaining))
	require.Equal(t, 0, communitiesRemaining)
	require.Equal(t, 0, postsRemaining)
}

func TestCommunityDeleteContentBlobReferences_DirectCommentAndReplyRemainInactive(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	repo := NewCommunityRepository(db, credentialciphertest.Fixed())
	community := visibilityCommunity(t, db, testkit.UniqueID(t))
	author := "did:plc:" + testkit.UniqueID(t)
	commenter := "did:plc:" + testkit.UniqueID(t)
	replier := "did:plc:" + testkit.UniqueID(t)
	createdAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	post := seedVisibilityPost(t, db, community, author, testkit.TID(), "comment root", createdAt)
	comment := seedActorComment(t, db, commenter, post, testkit.TID(), createdAt)
	reply := seedActorComment(t, db, replier, post, testkit.TID(), createdAt)
	_, err := db.Exec(`UPDATE comments SET parent_uri = $2, parent_cid = (SELECT cid FROM comments WHERE uri = $2) WHERE uri = $1`, reply, comment)
	require.NoError(t, err)
	for _, reference := range []contentBackfillReference{
		{commenter, communityAvatarA, comment, "comment_image", true},
		{replier, communityBannerB, reply, "comment_image", true},
	} {
		_, err := db.Exec(`INSERT INTO blob_references (owner_did, blob_cid, subject_uri, kind, active)
			VALUES ($1, $2, $3, $4, $5)`, reference.OwnerDID, reference.BlobCID, reference.SubjectURI, reference.Kind, reference.Active)
		require.NoError(t, err)
	}

	require.NoError(t, repo.Delete(context.Background(), community))
	require.ElementsMatch(t, []contentBackfillReference{
		{commenter, communityAvatarA, comment, "comment_image", false},
		{replier, communityBannerB, reply, "comment_image", false},
	}, contentBackfillRows(t, db, comment, reply), "both direct and nested comments under deleted community posts must retain inactive references")
}

// B3 is a guard: existing Delete already preserves the unrelated subject.
func TestCommunityDeleteContentBlobReferences_UnrelatedCommunityStaysActive(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	repo := NewCommunityRepository(db, credentialciphertest.Fixed())
	deletedCommunity := visibilityCommunity(t, db, testkit.UniqueID(t))
	retainedCommunity := visibilityCommunity(t, db, testkit.UniqueID(t))
	author := "did:plc:" + testkit.UniqueID(t)
	createdAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	deletedPost := seedVisibilityPost(t, db, deletedCommunity, author, testkit.TID(), "deleted image", createdAt)
	retainedPost := seedVisibilityPost(t, db, retainedCommunity, author, testkit.TID(), "retained image", createdAt)
	// Sharing owner and CID makes overbroad deactivation observable.
	for _, subject := range []string{deletedPost, retainedPost} {
		_, err := db.Exec(`INSERT INTO blob_references (owner_did, blob_cid, subject_uri, kind, active)
			VALUES ($1, $2, $3, 'post_image', true)`, author, communityAvatarA, subject)
		require.NoError(t, err)
	}

	require.NoError(t, repo.Delete(context.Background(), deletedCommunity))
	require.Equal(t, []contentBackfillReference{{author, communityAvatarA, retainedPost, "post_image", true}},
		contentBackfillRows(t, db, retainedPost), "another community's subject must stay active even with the same owner and CID")
	var deletedRemaining, retainedRemaining int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM communities WHERE did = $1`, deletedCommunity).Scan(&deletedRemaining))
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM posts WHERE uri = $1`, retainedPost).Scan(&retainedRemaining))
	require.Equal(t, 0, deletedRemaining)
	require.Equal(t, 1, retainedRemaining)
}

func TestCommunityDeleteContentBlobReferences_DeactivationFailureRollsBackDelete(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	repo := NewCommunityRepository(db, credentialciphertest.Fixed())
	community := visibilityCommunity(t, db, testkit.UniqueID(t))
	author := "did:plc:" + testkit.UniqueID(t)
	post := seedVisibilityPost(t, db, community, author, testkit.TID(), "rollback image", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	_, err := db.Exec(`INSERT INTO blob_references (owner_did, blob_cid, subject_uri, kind, active)
		VALUES ($1, $2, $3, 'post_image', true)`, author, communityAvatarA, post)
	require.NoError(t, err)
	communityBefore := communityProfileSnapshot(t, db, community)
	referencesBefore := communityProfileReferencesSnapshot(t, db, post)
	var postBefore string
	require.NoError(t, db.QueryRow(`SELECT to_jsonb(p)::text FROM posts p WHERE uri = $1`, post).Scan(&postBefore))
	_, err = db.Exec(`CREATE FUNCTION reject_deleted_content_reference() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'reject content reference deactivation' USING ERRCODE = 'P4242'; END; $$`)
	require.NoError(t, err)
	_, err = db.Exec(fmt.Sprintf(`CREATE TRIGGER reject_deleted_content_reference BEFORE UPDATE ON blob_references
		FOR EACH ROW WHEN (OLD.subject_uri = %s)
		EXECUTE FUNCTION reject_deleted_content_reference()`, pq.QuoteLiteral(post)))
	require.NoError(t, err)

	err = repo.Delete(context.Background(), community)
	require.Error(t, err, "community deletion must report the content reference deactivation failure")
	var postgresError *pq.Error
	require.True(t, errors.As(err, &postgresError), "delete must preserve the PostgreSQL deactivation error, got %v", err)
	require.Equal(t, pqerror.Code("P4242"), postgresError.Code, "delete must return the injected content reference failure")
	require.Equal(t, communityBefore, communityProfileSnapshot(t, db, community), "failed deactivation must retain the community")
	var postAfter string
	require.NoError(t, db.QueryRow(`SELECT to_jsonb(p)::text FROM posts p WHERE uri = $1`, post).Scan(&postAfter), "failed deactivation must retain its post")
	require.Equal(t, postBefore, postAfter)
	require.Equal(t, referencesBefore, communityProfileReferencesSnapshot(t, db, post), "rollback must leave references unchanged")
	require.Equal(t, []contentBackfillReference{{author, communityAvatarA, post, "post_image", true}}, contentBackfillRows(t, db, post))
}
