//go:build integration

package jetstream

import (
	"context"
	"testing"
	"time"

	"Coves/internal/core/posts"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

func TestPostConsumer_RemovedBeforeJetstreamInsertSuppressesMention(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	f := newPV2Fixture(t, db)
	handles, recipients := postMentionRecipients(t, db, 1)
	createdAt := activatedCommentNotificationTime(t, db, ctx)
	key := testkit.TID()
	uri := pv2URI(pv2Author, key)
	result, err := f.admissions.ApplyRemoval(ctx, posts.ApplyRemovalCommand{
		CommunityDID: pv2Community, PostURI: uri, DecisionCode: string(posts.DecisionRuleViolation),
		Watermark: posts.CommunityWatermark{Rev: testkit.TID()},
	})
	require.NoError(t, err)
	require.Equal(t, posts.AdmissionApplied, result.Outcome)
	require.NoError(t, postMentionConsumer(db, f, postgres.NewNotificationRepository(db)).HandleEvent(ctx,
		pv2Event(pv2Author, "create", key, testkit.TID(), "bafyreiwithdrawninsert", time.Now().UnixMicro(),
			postMentionRecord(t, createdAt, handles, recipients))))
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM posts WHERE uri = $1`, uri))
	requirePostMentionFacets(t, db, uri, recipients[0])
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM notifications WHERE record_uri = $1 AND recipient_did = $2`, uri, recipients[0]))
}

func TestPostConsumer_RemovedBeforeAcceptanceDirectFetchSuppressesMention(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	f := newRealRepoFixture(t, db)
	handles, recipients := postMentionRecipients(t, db, 1)
	record := postMentionRecord(t, activatedCommentNotificationTime(t, db, ctx), handles, recipients)
	record["community"] = accCommunity
	created := f.author.CreateRecord(t, PostV2Collection, record)
	revisions := increasingTIDs(t, 2)
	result, err := f.admissions.ApplyRemoval(ctx, posts.ApplyRemovalCommand{
		CommunityDID: accCommunity, PostURI: created.URI, DecisionCode: string(posts.DecisionRuleViolation),
		Watermark: posts.CommunityWatermark{Rev: revisions[1]},
	})
	require.NoError(t, err)
	require.Equal(t, posts.AdmissionApplied, result.Outcome)
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM posts WHERE uri = $1`, created.URI))
	consumer := newRealPostMentionConsumer(db, f, postgres.NewNotificationRepository(db), nil)
	require.NoError(t, consumer.HandleEvent(ctx, acceptanceEvent(accCommunity, created.URI, created.CID,
		revisions[0], time.Now().UnixMicro())))
	require.Equal(t, 1, countRows(t, db, `SELECT count(*) FROM posts WHERE uri = $1`, created.URI))
	requirePostMentionFacets(t, db, created.URI, recipients[0])
	admission, err := f.admissions.Get(ctx, accCommunity, created.URI)
	require.NoError(t, err)
	require.Equal(t, posts.AdmissionStatusRemoved, admission.Status)
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM notifications WHERE record_uri = $1 AND recipient_did = $2`, created.URI, recipients[0]))
}

func TestPostConsumer_RemovedPostEditDoesNotMentionNewRecipient(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	f := newPV2Fixture(t, db)
	handles, recipients := postMentionRecipients(t, db, 1)
	createdAt := activatedCommentNotificationTime(t, db, ctx)
	key := testkit.TID()
	uri := pv2URI(pv2Author, key)
	revisions := increasingTIDs(t, 2)
	consumer := postMentionConsumer(db, f, postgres.NewNotificationRepository(db))
	record := pv2Record(pv2Community, "Before removal", "No mentions")
	record["createdAt"] = createdAt
	require.NoError(t, consumer.HandleEvent(ctx, pv2Event(pv2Author, "create", key, revisions[0],
		"bafyreiwithdrawnoriginal", time.Now().UnixMicro(), record)))
	removeNotificationReferencePost(t, db, uri)
	require.NoError(t, consumer.HandleEvent(ctx, pv2Event(pv2Author, "update", key, revisions[1],
		"bafyreiwithdrawnedit", time.Now().Add(time.Second).UnixMicro(), postMentionRecord(t, createdAt, handles, recipients))))
	_, _, indexedCID, _, _ := readPV2Post(t, db, uri)
	require.Equal(t, "bafyreiwithdrawnedit", indexedCID)
	requirePostMentionFacets(t, db, uri, recipients[0])
	require.Zero(t, countRows(t, db, `SELECT count(*) FROM notifications WHERE record_uri = $1 AND recipient_did = $2`, uri, recipients[0]))
}
