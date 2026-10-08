//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

func TestPostConsumer_CreatePostV2NotifiesEligibleMentionedUsers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	fixture := newPV2Fixture(t, db)
	uniqueID := testkit.UniqueID(t)
	firstRecipientDID := "did:plc:" + uniqueID + "first"
	secondRecipientDID := "did:plc:" + uniqueID + "second"
	firstHandle := uniqueID + "first.test"
	secondHandle := uniqueID + "second.test"
	insertBridgedUserOnPDS(t, db, firstRecipientDID, firstHandle, bridgedTestNativePDS)
	insertBridgedUserOnPDS(t, db, secondRecipientDID, secondHandle, bridgedTestNativePDS)
	createdAt := activatedCommentNotificationTime(t, db, ctx)

	consumer := NewPostEventConsumer(
		postgres.NewPostRepository(db),
		postgres.NewCommunityRepository(db, credentialciphertest.Fixed()),
		fixture.users, db,
		WithAdmissions(fixture.admissions),
		WithDeletedAccounts(postgres.NewDeletedAccountRepository(db)),
		WithPostNotifications(postgres.NewNotificationRepository(db)),
	)
	const communityHandle = "pv2community.test"
	const authorHandle = "pv2author.test"
	// The community DID is also an indexed account, so only the community rule can exclude it.
	insertBridgedUserOnPDS(t, db, pv2Community, communityHandle, bridgedTestNativePDS)
	content := "A mentions @" + firstHandle + " @" + secondHandle + " @" + communityHandle + " @" + authorHandle
	record := pv2Record(pv2Community, "Mentioned users", content)
	record["createdAt"] = createdAt
	record["facets"] = []interface{}{
		commentMentionFacet(t, content, firstHandle, firstRecipientDID),
		commentMentionFacet(t, content, secondHandle, secondRecipientDID),
		commentMentionFacet(t, content, communityHandle, pv2Community),
		commentMentionFacet(t, content, authorHandle, pv2Author),
	}
	postKey := testkit.TID()
	postURI := pv2URI(pv2Author, postKey)
	const postCID = "bafyreipostmentioncreate"
	require.NoError(t, consumer.HandleEvent(ctx, pv2Event(
		pv2Author, "create", postKey, testkit.TID(), postCID, time.Now().UnixMicro(), record,
	)))

	// Prove the event was admitted and the facets survived before inspecting fan-out.
	authorDID, communityDID, indexedCID, _, deletedAt := readPV2Post(t, db, postURI)
	require.Equal(t, pv2Author, authorDID)
	require.Equal(t, pv2Community, communityDID)
	require.Equal(t, postCID, indexedCID)
	require.Nil(t, deletedAt)
	var storedFacets sql.NullString
	require.NoError(t, db.QueryRowContext(ctx, `SELECT content_facets FROM posts WHERE uri = $1`, postURI).Scan(&storedFacets))
	require.True(t, storedFacets.Valid, "fixture: valid mention facets must survive post indexing")
	for _, mentionedDID := range []string{firstRecipientDID, secondRecipientDID, pv2Community, pv2Author} {
		require.Contains(t, storedFacets.String, mentionedDID, "fixture: every mention must survive sanitization")
	}

	for _, recipientDID := range []string{firstRecipientDID, secondRecipientDID} {
		require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM notifications
			WHERE record_uri = $1 AND recipient_did = $2 AND reason = 'mention'`, postURI, recipientDID),
			"each eligible mentioned user must receive exactly one mention for the indexed post")
		var actorDID, recordURI, recordCID, rootPostURI string
		var subjectURI sql.NullString
		require.NoError(t, db.QueryRowContext(ctx, `SELECT actor_did, record_uri, record_cid, subject_uri, root_post_uri
			FROM notifications WHERE record_uri = $1 AND recipient_did = $2 AND reason = 'mention'`,
			postURI, recipientDID).Scan(&actorDID, &recordURI, &recordCID, &subjectURI, &rootPostURI))
		require.Equal(t, pv2Author, actorDID)
		require.Equal(t, postURI, recordURI)
		require.Equal(t, postCID, recordCID)
		require.False(t, subjectURI.Valid, "a post mention has no subject URI")
		require.Equal(t, postURI, rootPostURI)
	}
	for _, excludedDID := range []string{pv2Community, pv2Author} {
		require.Zero(t, countRows(t, db, `SELECT count(*) FROM notifications
			WHERE record_uri = $1 AND recipient_did = $2`, postURI, excludedDID),
			"a mentioned community or the post author must receive no notification")
	}
	require.Equal(t, 2, countRows(t, db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, postURI),
		"the post must produce only the two eligible mention notifications")
}
