//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/core/posts"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

func removeNotificationReferencePost(t *testing.T, db *sql.DB, uri string) {
	t.Helper()
	var community string
	require.NoError(t, db.QueryRow(`SELECT community_did FROM posts WHERE uri = $1`, uri).Scan(&community))
	result, err := postgres.NewAdmissionRepository(db).ApplyRemoval(context.Background(), posts.ApplyRemovalCommand{
		CommunityDID: community, PostURI: uri, DecisionCode: string(posts.DecisionRuleViolation),
		Watermark: posts.CommunityWatermark{Rev: testkit.TID()},
	})
	require.NoError(t, err)
	require.Equal(t, posts.AdmissionApplied, result.Outcome)
}

func TestCommentConsumer_WithdrawnReferencesSuppressCreate(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"removed root", "deleted root", "deleted parent comment", "removed distinct parent post", "live distinct parent post", "pending root"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			f := newCommentAuthorDeleteFixture(t)
			parentURI, parentCID := f.postURI, f.postCID
			rootURI, rootCID := f.postURI, f.postCID
			if kind == "deleted parent comment" {
				parentURI, parentCID = f.parent(t, revTestAuthor)
				key := parentURI[len("at://"+revTestAuthor+"/"+CommentCollection+"/"):]
				require.NoError(t, f.consumer.HandleEvent(context.Background(), revCommitEvent(revTestAuthor, CommentCollection, "delete", key,
					testkit.TID(), "", time.Now().UnixMicro(), nil)))
				require.Equal(t, 1, countRows(t, f.db, `SELECT count(*) FROM comments WHERE uri = $1 AND deleted_at IS NOT NULL`, parentURI))
			}
			if kind == "removed distinct parent post" || kind == "live distinct parent post" {
				key := testkit.TID()
				parentURI = pv2URI(revTestAuthor, key)
				parentCID = "bafyreiwithdrawnparentpost"
				_, err := f.db.Exec(`INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, created_at)
					VALUES ($1, $2, $3, $4, $5, 'distinct parent', NOW())`, parentURI, parentCID, key, revTestAuthor, revTestCommunity)
				require.NoError(t, err)
			}
			if kind == "removed root" {
				removeNotificationReferencePost(t, f.db, rootURI)
			}
			if kind == "removed distinct parent post" {
				removeNotificationReferencePost(t, f.db, parentURI)
			}
			if kind == "deleted root" {
				_, err := f.db.Exec(`UPDATE posts SET deleted_at = NOW() WHERE uri = $1`, rootURI)
				require.NoError(t, err)
			}
			mentioned, handle := f.mentionRecipient(t)
			key := testkit.TID()
			uri := "at://" + revTestCommenter + "/" + CommentCollection + "/" + key
			content := "A mentions @" + handle
			record := revCommentRecord(content, rootURI, rootCID, parentURI, parentCID)
			record["createdAt"] = f.createdAt
			record["facets"] = []interface{}{commentMentionFacet(t, content, handle, mentioned)}
			require.NoError(t, f.consumer.HandleEvent(context.Background(), revCommitEvent(revTestCommenter, CommentCollection, "create", key,
				testkit.TID(), "bafyreiwithdrawncomment", time.Now().UnixMicro(), record)))
			require.Equal(t, 1, countRows(t, f.db, `SELECT count(*) FROM comments WHERE uri = $1`, uri))
			if kind == "pending root" {
				require.Equal(t, 1, countRows(t, f.db, `SELECT count(*) FROM notifications WHERE record_uri = $1 AND recipient_did = $2 AND reason = 'postReply'`, uri, revTestAuthor))
				return
			}
			if kind == "live distinct parent post" {
				require.Equal(t, 1, countRows(t, f.db, `SELECT count(*) FROM notifications WHERE record_uri = $1 AND recipient_did = $2 AND reason = 'mention'`, uri, mentioned))
				return
			}
			require.Zero(t, countRows(t, f.db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, uri),
				"withdrawn root or parent must suppress both reply and mention")
		})
	}
}

func TestCommentConsumer_RemovedRootSuppressesEditAndActiveRecreate(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"update", "active recreate"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			f := newMentionEditFixture(t)
			added := f.recipient(t)
			f.create(t)
			require.Equal(t, 1, mentionEditRows(t, f, f.postAuthor(t), "postReply"))
			removeNotificationReferencePost(t, f.gate.db, f.post.uri)
			var err error
			if operation == "update" {
				err = f.update(t, f.createdAt, time.Now().Add(time.Second).UnixMicro(), added)
			} else {
				event, content := activeRecreateEvent(t, f, f.createdAt, added)
				err = f.consumer.HandleEvent(context.Background(), event)
				require.NoError(t, err)
				requireActiveRecreateApplied(t, f, content)
			}
			require.NoError(t, err)
			if operation == "update" {
				var content string
				require.NoError(t, f.gate.db.QueryRow(`SELECT content FROM comments WHERE uri = $1`, f.uri).Scan(&content))
				require.Contains(t, content, added.handle)
			}
			require.Zero(t, mentionEditRows(t, f, added, "mention"), "removed root must suppress the new mention")
		})
	}
}

func TestCommentConsumer_RemovedRootSuppressesSameParentResurrection(t *testing.T) {
	t.Parallel()
	f := newCommentAuthorDeleteFixture(t)
	parent, parentCID := f.parent(t, revTestAuthor)
	key := testkit.TID()
	firstRevision := testkit.TID()
	uri := f.createReply(t, key, firstRevision, "bafyreiwithdrawninitial", f.replyRecord("A replies", parent, parentCID))
	before := notificationRowsForRecordOrSubject(t, f.db, uri)
	require.Len(t, before, 1)
	deleteRevision := testkit.TID()
	require.Less(t, firstRevision, deleteRevision)
	require.NoError(t, f.consumer.HandleEvent(context.Background(), revCommitEvent(revTestCommenter, CommentCollection, "delete", key,
		deleteRevision, "", time.Now().UnixMicro(), nil)))
	removeNotificationReferencePost(t, f.db, f.postURI)
	added, handle := f.mentionRecipient(t)
	recreateRevision := testkit.TID()
	require.Less(t, deleteRevision, recreateRevision)
	require.NoError(t, f.consumer.HandleEvent(context.Background(), revCommitEvent(revTestCommenter, CommentCollection, "create", key,
		recreateRevision, "bafyreiwithdrawnrecreated", time.Now().UnixMicro(), f.mentionedReply(t, parent, parentCID, added, handle))))
	require.Equal(t, before, notificationRowsForRecordOrSubject(t, f.db, uri), "kept notification IDs and sort times must not change")
	require.Zero(t, countRows(t, f.db, `SELECT count(*) FROM notifications WHERE record_uri = $1 AND recipient_did = $2`, uri, added))
}

func TestCommentConsumer_RestoredPostOnlyNotifiesNewComments(t *testing.T) {
	t.Parallel()
	f := newCommentAuthorDeleteFixture(t)
	removeNotificationReferencePost(t, f.db, f.postURI)
	first := f.createReply(t, testkit.TID(), testkit.TID(), "bafyreiwithdrawnwhile", f.replyRecord("A replies while removed", f.postURI, f.postCID))
	var community string
	require.NoError(t, f.db.QueryRow(`SELECT community_did FROM posts WHERE uri = $1`, f.postURI).Scan(&community))
	admissions := postgres.NewAdmissionRepository(f.db)
	removed, err := admissions.ApplyRemovalDelete(context.Background(), posts.CommunityDeleteCommand{
		CommunityDID: community, PostURI: f.postURI, Watermark: posts.CommunityWatermark{Rev: testkit.TID()},
	})
	require.NoError(t, err)
	require.Equal(t, posts.AdmissionApplied, removed.Outcome)
	key := testkit.TID()
	accepted, err := admissions.ApplyAcceptance(context.Background(), posts.ApplyAcceptanceCommand{
		CommunityDID: community, PostURI: f.postURI, AcceptanceURI: "at://" + community + "/" + posts.AcceptanceCollection + "/" + key,
		AcceptanceRkey: key, PinnedCID: f.postCID, Watermark: posts.CommunityWatermark{Rev: testkit.TID()},
	})
	require.NoError(t, err)
	require.Equal(t, posts.AdmissionApplied, accepted.Outcome)
	state, err := admissions.Get(context.Background(), community, f.postURI)
	require.NoError(t, err)
	require.Equal(t, posts.AdmissionStatusAccepted, state.Status)
	second := f.createReply(t, testkit.TID(), testkit.TID(), "bafyreiwithdrawnafter", f.replyRecord("A replies after restore", f.postURI, f.postCID))
	require.Equal(t, 1, countRows(t, f.db, `SELECT count(*) FROM notifications WHERE record_uri = $1 AND recipient_did = $2 AND reason = 'postReply'`, second, revTestAuthor))
	require.Zero(t, countRows(t, f.db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, first))
}

// A thread reference with no indexed row cannot be checked for withdrawal, so
// it opens no fan-out: the comment still indexes as an orphan, but neither its
// create nor an edit that adds a mention notifies anyone, including the
// reference's authority and every mentioned user.
func TestCommentConsumer_UnindexedThreadReferenceSuppressesCreateAndEdit(t *testing.T) {
	t.Parallel()
	for _, missing := range []string{"root and parent postv2", "parent comment"} {
		t.Run(missing, func(t *testing.T) {
			t.Parallel()
			f := newMentionEditFixture(t)
			victim := f.postAuthor(t)
			if missing == "root and parent postv2" {
				f.post.uri = "at://" + victim.did + "/" + posts.PostV2Collection + "/" + testkit.TID()
				f.post.cid = "bafyreiunindexedroot"
				f.parentURI, f.parentCID = f.post.uri, f.post.cid
			} else {
				f.parentURI = "at://" + victim.did + "/" + CommentCollection + "/" + testkit.TID()
				f.parentCID = "bafyreiunindexedparent"
			}
			require.Zero(t, countRows(t, f.gate.db, `SELECT count(*) FROM posts WHERE uri = $1`, f.parentURI))
			require.Zero(t, countRows(t, f.gate.db, `SELECT count(*) FROM comments WHERE uri = $1`, f.parentURI))
			mentioned := f.recipient(t)
			f.create(t, mentioned)
			require.Zero(t, countRows(t, f.gate.db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, f.uri),
				"an unindexed thread reference must suppress the create's reply and mention rows")

			added := f.recipient(t)
			require.NoError(t, f.update(t, f.createdAt, time.Now().Add(time.Second).UnixMicro(), mentioned, added))
			var content string
			require.NoError(t, f.gate.db.QueryRow(`SELECT content FROM comments WHERE uri = $1`, f.uri).Scan(&content))
			require.Contains(t, content, added.handle, "the edit must still be indexed")
			require.Zero(t, countRows(t, f.gate.db, `SELECT count(*) FROM notifications WHERE record_uri = $1`, f.uri),
				"an unindexed thread reference must suppress the edit's new mention")
		})
	}
}
