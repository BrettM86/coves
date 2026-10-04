//go:build integration

package postgres_test

import (
	"fmt"
	"testing"

	"Coves/internal/core/imageproxy"
	"Coves/internal/core/moderation"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestModerationRepositoryListsActiveBlockedBlobs(t *testing.T) {
	db := testkit.DB(t)
	service := newPostgresModerationService(db)
	remove := func(subject moderation.StrongRef, reason string) *moderation.MutationResult {
		t.Helper()
		result, err := service.RemoveContent(t.Context(), fixtures.DID(testkit.UniqueIDWithPrefix(t, "modadmin")), moderation.RemoveContentRequest{
			Subject: subject, ExpectedVersion: "v0", IdempotencyKey: "remove-" + testkit.UniqueID(t), Reason: reason,
		})
		require.NoError(t, err)
		require.Equal(t, moderation.OutcomeApplied, result.Outcome)
		return result
	}
	const spam = "social.coves.moderation.defs#reasonSpam"
	embed := fmt.Sprintf(`{"$type":"social.coves.embed.images","images":[{"image":{"$type":"blob","ref":{"$link":"%s"},"mimeType":"image/png","size":10},"alt":""}]}`, moderationImageCIDOne)

	spamSubject, spamOwner, _ := indexedModerationComment(t, db, false, embed)
	remove(spamSubject, spam)
	// A second removal of the same owner's images adds duplicate block rows.
	rkey := testkit.TID()
	duplicateSubject := moderation.StrongRef{URI: "at://" + spamOwner + "/" + moderation.CommentCollection + "/" + rkey, CID: moderationCommentCID}
	_, err := db.ExecContext(t.Context(), `
		INSERT INTO comments (uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid, content, embed, created_at)
		VALUES ($1, $2, $3, $4, $5, 'bafyreiunindexedroot', $5, 'bafyreiunindexedroot', 'second comment', $6::jsonb, NOW())
	`, duplicateSubject.URI, duplicateSubject.CID, rkey, spamOwner,
		"at://"+fixtures.DID(testkit.UniqueIDWithPrefix(t, "modroot"))+"/"+moderation.PostV2Collection+"/"+testkit.TID(), embed)
	require.NoError(t, err)
	remove(duplicateSubject, spam)

	illegalSubject, illegalOwner, _ := indexedModerationComment(t, db, false, embed)
	remove(illegalSubject, "social.coves.moderation.defs#reasonIllegalContent")

	inactiveSubject, _, _ := indexedModerationComment(t, db, false, embed)
	inactive := remove(inactiveSubject, spam)
	_, err = db.ExecContext(t.Context(), `UPDATE moderation_media_blocks SET active = FALSE WHERE action_id = $1`, inactive.Action.ID)
	require.NoError(t, err)

	blobs, err := postgres.NewModerationRepository(db).ListActiveBlockedBlobs(t.Context())
	require.NoError(t, err)
	assert.ElementsMatch(t, []imageproxy.BlockedBlob{
		{OwnerDID: spamOwner, CID: moderationImageCIDOne},
		{OwnerDID: illegalOwner, CID: moderationImageCIDOne},
		{CID: moderationImageCIDOne},
	}, blobs, "each active block once, every-owner blocks with an empty owner, inactive blocks left out")
}
