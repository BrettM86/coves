//go:build integration

package postgres_test

import (
	"database/sql"
	"encoding/json"
	"testing"

	"Coves/internal/core/moderation"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	snapshotReasonSpam       = "social.coves.moderation.defs#reasonSpam"
	snapshotReasonDoxing     = "social.coves.moderation.defs#reasonDoxing"
	snapshotReasonIllegal    = "social.coves.moderation.defs#reasonIllegalContent"
	snapshotReasonDiscretion = "social.coves.moderation.defs#reasonModeratorDiscretion"
	snapshotLinkURL          = "https://news.example/snapshot-story"
	snapshotLinkEmbed        = `{"$type":"social.coves.embed.external","external":{"uri":"https://news.example/snapshot-story","title":"Snapshot story card"}}`
	snapshotEditedCID        = "bafyreidfcytgqwl6jyx5nhsofbvq4dibimtbx3uwckcs2wtwulwsz7jvbm"
	snapshotMalformedEmbed   = `{"$type":"social.coves.embed.external","external":{"uri":"not a uri","title":"Malformed link card"}}`
)

// snapshotObject is the JSON object a log view serializes to; nil when the
// value is absent.
func snapshotObject(t *testing.T, value any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	var object map[string]any
	require.NoError(t, json.Unmarshal(encoded, &object))
	return object
}

// snapshotLogEntries returns an action's public log entry, nil when the public
// log does not list it, and its admin log entry.
func snapshotLogEntries(t *testing.T, service moderation.Service, actionID string) (public, admin map[string]any) {
	t.Helper()
	adminPage, err := service.ListAdminActions(t.Context(), moderation.ListAdminActionsParams{ActionID: actionID})
	require.NoError(t, err)
	require.Len(t, adminPage.Actions, 1)
	admin = snapshotObject(t, adminPage.Actions[0])
	publicPage, _ := restrictedLogPage(t, service, moderation.ListActionsParams{})
	for _, action := range publicPage.Actions {
		if action.Ref.ActionID == actionID {
			public = snapshotObject(t, action)
		}
	}
	return public, admin
}

// snapshotAdminSubject is the admin entry's subject reference, wherever the
// projection put it.
func snapshotAdminSubject(t *testing.T, admin map[string]any) map[string]any {
	t.Helper()
	if subject := snapshotObject(t, snapshotObject(t, admin["action"])["subject"]); subject != nil {
		return subject
	}
	return snapshotObject(t, admin["privateSubject"])
}

func snapshotRowCount(t *testing.T, db *sql.DB, actionID string) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRowContext(t.Context(), `
		SELECT count(*) FROM moderation_content_snapshots WHERE action_id = $1
	`, actionID).Scan(&count))
	return count
}

// A removal keeps the text of the version the moderator saw: title, body and
// external link of a post, the body of a comment. Doxing removals are kept for
// admins only; illegal-content removals are never kept. A comment action
// records the post that roots its thread.
func TestModerationContentSnapshotCapture(t *testing.T) {
	db := testkit.DB(t)
	communityDID, authorDID := moderationPostActors(t, db)
	service := restrictedLogService(db)
	post := func(title, embed string) moderation.StrongRef {
		return indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, title, embed)
	}
	root := post("Snapshot thread root", "")
	emptyPost := post("", "")
	_, err := db.ExecContext(t.Context(), `UPDATE posts SET title = NULL, content = NULL, embed = NULL WHERE uri = $1`, emptyPost.URI)
	require.NoError(t, err)
	// The comment consumer only loosely validates a root URI, so an index row
	// can hold one the lexicon's at-uri format rejects.
	malformedRootComment := restrictedLogComment(t, db, authorDID, root)
	_, err = db.ExecContext(t.Context(), `UPDATE comments SET root_uri = $2 WHERE uri = $1`,
		malformedRootComment.URI, "at://did:plc:rootauthor/social.coves.community.postv2/3abc/extra")
	require.NoError(t, err)

	for _, test := range []struct {
		name     string
		subject  moderation.StrongRef
		reason   string
		snapshot map[string]any
		rootURI  string
		public   bool
	}{
		{
			name: "spam post removal keeps title, body and link", subject: post("Snapshot spam title", snapshotLinkEmbed), reason: snapshotReasonSpam,
			snapshot: map[string]any{"title": "Snapshot spam title", "text": "indexed post content", "linkUrl": snapshotLinkURL, "capturedCid": moderationPostCID},
			public:   true,
		},
		{
			name: "comment removal keeps its body and records its root post", subject: restrictedLogComment(t, db, authorDID, root), reason: snapshotReasonSpam,
			snapshot: map[string]any{"text": "restricted log comment", "capturedCid": moderationCommentCID},
			rootURI:  root.URI, public: true,
		},
		{
			name: "comment whose indexed root is not a valid at-uri records no root", subject: malformedRootComment, reason: snapshotReasonSpam,
			snapshot: map[string]any{"text": "restricted log comment", "capturedCid": moderationCommentCID},
			public:   true,
		},
		{
			name: "doxing removal is kept for admins only", subject: post("Snapshot doxing title", snapshotLinkEmbed), reason: snapshotReasonDoxing,
			snapshot: map[string]any{"title": "Snapshot doxing title", "text": "indexed post content", "linkUrl": snapshotLinkURL, "capturedCid": moderationPostCID},
		},
		{
			name: "illegal-content removal is never kept", subject: post("Snapshot illegal title", snapshotLinkEmbed), reason: snapshotReasonIllegal,
		},
		{
			name: "post without title, body or embed keeps only its CID", subject: emptyPost, reason: snapshotReasonSpam,
			snapshot: map[string]any{"capturedCid": moderationPostCID}, public: true,
		},
		{
			name: "image post keeps its text and no blob reference", subject: post("Snapshot image title", moderationTwoImageEmbed()), reason: snapshotReasonSpam,
			snapshot: map[string]any{"title": "Snapshot image title", "text": "indexed post content", "capturedCid": moderationPostCID},
			public:   true,
		},
		{
			name: "external embed with a malformed uri keeps no link", subject: post("Snapshot malformed link title", snapshotMalformedEmbed), reason: snapshotReasonSpam,
			snapshot: map[string]any{"title": "Snapshot malformed link title", "text": "indexed post content", "capturedCid": moderationPostCID},
			public:   true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			id := restrictedLogRemove(t, service, test.subject, test.reason)
			public, admin := snapshotLogEntries(t, service, id)

			if test.snapshot == nil {
				assert.NotContains(t, admin, "contentSnapshot")
			} else {
				assert.Equal(t, test.snapshot, admin["contentSnapshot"])
			}
			adminSubject := snapshotAdminSubject(t, admin)
			require.NotNil(t, adminSubject)
			if test.rootURI == "" {
				assert.NotContains(t, adminSubject, "rootUri", "only a comment with a valid root carries a root")
			} else {
				assert.Equal(t, test.rootURI, adminSubject["rootUri"])
			}

			require.NotNil(t, public, "every removal is listed in the public log")
			if test.public {
				assert.Equal(t, test.snapshot, public["contentSnapshot"])
				publicSubject := snapshotObject(t, public["subject"])
				require.NotNil(t, publicSubject)
				if test.rootURI == "" {
					assert.NotContains(t, publicSubject, "rootUri")
				} else {
					assert.Equal(t, test.rootURI, publicSubject["rootUri"])
				}
			} else {
				assert.NotContains(t, public, "contentSnapshot")
				assert.NotContains(t, public, "subject")
			}

			state, err := service.GetSubjectState(t.Context(), test.subject.URI)
			require.NoError(t, err, "the subject state must still read a removal that carries a snapshot")
			require.NotNil(t, state)
			require.NotNil(t, state.LocalRemoval)
			assert.Equal(t, id, state.LocalRemoval.ActionID)
			assert.Equal(t, moderation.ModerationStateRemoved, state.Moderation.State)

			var stored string
			require.NoError(t, db.QueryRowContext(t.Context(), `
				SELECT stored_result::text FROM moderation_idempotency_keys WHERE stored_result->'Action'->>'ID' = $1
			`, id).Scan(&stored))
			for _, field := range []string{"title", "text", "linkUrl"} {
				if value, present := test.snapshot[field]; present {
					assert.NotContains(t, stored, value, "the stored idempotency result must not copy the removed %s", field)
				}
			}

			want := 1
			if test.snapshot == nil {
				want = 0
			}
			assert.Equal(t, want, snapshotRowCount(t, db, id))
		})
	}
}

// Once its author deletes the subject, the public log still names it but no
// longer shows its snapshot; the admin log keeps it (decided 2026-10-13). A
// post already deleted by its author can still be removed, and its snapshot is
// kept for admins. A comment already deleted by its author has lost its text,
// so its removal keeps no snapshot.
func TestModerationContentSnapshotWithheldAfterAuthorDeletion(t *testing.T) {
	db := testkit.DB(t)
	communityDID, authorDID := moderationPostActors(t, db)
	service := restrictedLogService(db)
	root := indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, "Author deletion thread root", "")
	// The SQL the post and comment consumers run when an author deletes.
	const deletePost = `UPDATE posts SET deleted_at = NOW() WHERE uri = $1`
	const deleteComment = `
		UPDATE comments SET content = '', content_facets = NULL, embed = NULL, content_labels = NULL,
			deleted_at = NOW(), deletion_reason = 'author', deleted_by = commenter_did
		WHERE uri = $1`

	for _, test := range []struct {
		name          string
		subject       moderation.StrongRef
		deleteSQL     string
		deletedBefore bool
		snapshot      map[string]any
	}{
		{
			name:      "post deleted after its removal",
			subject:   indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, "Deleted after removal title", snapshotLinkEmbed),
			deleteSQL: deletePost,
			snapshot:  map[string]any{"title": "Deleted after removal title", "text": "indexed post content", "linkUrl": snapshotLinkURL, "capturedCid": moderationPostCID},
		},
		{
			name:      "post deleted before its removal",
			subject:   indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, "Deleted before removal title", snapshotLinkEmbed),
			deleteSQL: deletePost, deletedBefore: true,
			snapshot: map[string]any{"title": "Deleted before removal title", "text": "indexed post content", "linkUrl": snapshotLinkURL, "capturedCid": moderationPostCID},
		},
		{
			name:      "comment deleted after its removal",
			subject:   restrictedLogComment(t, db, authorDID, root),
			deleteSQL: deleteComment,
			snapshot:  map[string]any{"text": "restricted log comment", "capturedCid": moderationCommentCID},
		},
		{
			name:      "comment deleted before its removal",
			subject:   restrictedLogComment(t, db, authorDID, root),
			deleteSQL: deleteComment, deletedBefore: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			authorDelete := func() {
				_, err := db.ExecContext(t.Context(), test.deleteSQL, test.subject.URI)
				require.NoError(t, err)
			}
			if test.deletedBefore {
				authorDelete()
			}
			id := restrictedLogRemove(t, service, test.subject, snapshotReasonSpam)
			if !test.deletedBefore {
				authorDelete()
			}

			public, admin := snapshotLogEntries(t, service, id)
			if test.snapshot == nil {
				assert.NotContains(t, admin, "contentSnapshot")
				assert.Equal(t, 0, snapshotRowCount(t, db, id), "a removal of an author-deleted comment keeps no snapshot")
			} else {
				assert.Equal(t, test.snapshot, admin["contentSnapshot"], "the admin log keeps the snapshot")
			}

			require.NotNil(t, public, "the removal stays in the public log")
			publicSubject := snapshotObject(t, public["subject"])
			require.NotNil(t, publicSubject, "the public log still names an author-deleted subject")
			assert.Equal(t, test.subject.URI, publicSubject["uri"])
			assert.NotContains(t, public, "contentSnapshot", "the public log withholds an author-deleted subject's snapshot")
			publicJSON, err := json.Marshal(public)
			require.NoError(t, err)
			for _, field := range []string{"title", "text", "linkUrl"} {
				if value, present := test.snapshot[field]; present {
					assert.NotContains(t, string(publicJSON), value, "the public log must not show the deleted subject's %s", field)
				}
			}
		})
	}
}

// A doxing or illegal-content removal deletes every earlier snapshot of its
// subject, for admins too (decided 2026-10-13): a subject first removed as spam,
// restored and removed again for doxing no longer shows the spam removal's
// text. The doxing removal keeps its own admin-only snapshot.
func TestModerationContentSnapshotHiddenRemovalDeletesEarlierSnapshots(t *testing.T) {
	db := testkit.DB(t)
	communityDID, authorDID := moderationPostActors(t, db)
	service := restrictedLogService(db)
	actor := fixtures.DID("snapshothiddenadmin")

	for _, test := range []struct {
		name     string
		title    string
		reason   string
		snapshot map[string]any
		rows     int
	}{
		{
			name: "doxing removal", title: "Spam then doxing title", reason: snapshotReasonDoxing,
			snapshot: map[string]any{"title": "Spam then doxing title", "text": "indexed post content", "linkUrl": snapshotLinkURL, "capturedCid": moderationPostCID},
			rows:     1,
		},
		{name: "illegal-content removal", title: "Spam then illegal title", reason: snapshotReasonIllegal},
	} {
		t.Run(test.name, func(t *testing.T) {
			subject := indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, test.title, snapshotLinkEmbed)
			spamID := restrictedLogRemove(t, service, subject, snapshotReasonSpam)
			restored, err := service.RestoreContent(t.Context(), actor, moderation.RestoreContentRequest{
				ActionID: spamID, ReviewedSubject: &subject, ExpectedVersion: "v1",
				IdempotencyKey: testkit.UniqueIDWithPrefix(t, "restore"), Reason: snapshotReasonDiscretion,
			})
			require.NoError(t, err)
			require.Equal(t, moderation.OutcomeApplied, restored.Outcome)
			removed, err := service.RemoveContent(t.Context(), actor, moderation.RemoveContentRequest{
				Subject: subject, ExpectedVersion: "v2", IdempotencyKey: testkit.UniqueIDWithPrefix(t, "remove"), Reason: test.reason,
			})
			require.NoError(t, err)
			require.Equal(t, moderation.OutcomeApplied, removed.Outcome)
			require.NotNil(t, removed.Action)

			spamPublic, spamAdmin := snapshotLogEntries(t, service, spamID)
			assert.NotContains(t, spamAdmin, "contentSnapshot", "the spam removal's snapshot is gone for admins")
			require.NotNil(t, spamPublic, "the spam removal stays in the public log")
			assert.NotContains(t, spamPublic, "contentSnapshot", "the spam removal's snapshot is gone from the public log")
			assert.Equal(t, 0, snapshotRowCount(t, db, spamID))

			_, hiddenAdmin := snapshotLogEntries(t, service, removed.Action.ID)
			if test.snapshot == nil {
				assert.NotContains(t, hiddenAdmin, "contentSnapshot")
			} else {
				assert.Equal(t, test.snapshot, hiddenAdmin["contentSnapshot"], "the doxing removal keeps its own admin-only snapshot")
			}

			var subjectRows int
			require.NoError(t, db.QueryRowContext(t.Context(), `
				SELECT count(*) FROM moderation_content_snapshots s
				JOIN moderation_actions a ON a.id = s.action_id
				WHERE a.subject_uri = $1
			`, subject.URI).Scan(&subjectRows))
			assert.Equal(t, test.rows, subjectRows)
		})
	}
}

// Only removals keep a snapshot, and a removal's snapshot outlives its restore
// and later edits of the indexed post: it stays the version the moderator saw.
func TestModerationContentSnapshotOnlyRemovalsKeepItAndEditsDoNotChangeIt(t *testing.T) {
	db := testkit.DB(t)
	communityDID, authorDID := moderationPostActors(t, db)
	service := restrictedLogService(db)
	actor := fixtures.DID("snapshotlifecycleadmin")
	subject := indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, "Original snapshot title", snapshotLinkEmbed)
	removalID := restrictedLogRemove(t, service, subject, snapshotReasonSpam)

	_, err := db.ExecContext(t.Context(), `
		UPDATE posts SET title = 'Edited snapshot title', content = 'edited post content', embed = NULL, cid = $2 WHERE uri = $1
	`, subject.URI, snapshotEditedCID)
	require.NoError(t, err)
	edited := moderation.StrongRef{URI: subject.URI, CID: snapshotEditedCID}
	restored, err := service.RestoreContent(t.Context(), actor, moderation.RestoreContentRequest{
		ActionID: removalID, ReviewedSubject: &edited, ExpectedVersion: "v1",
		IdempotencyKey: "snapshot-restore", Reason: snapshotReasonDiscretion,
	})
	require.NoError(t, err)
	require.Equal(t, moderation.OutcomeApplied, restored.Outcome)
	require.NotNil(t, restored.Action)
	labelled := labelPost(t, service, actor, edited, "v2", "snapshot-label")
	retracted, err := service.RetractContentLabel(t.Context(), actor, moderation.RetractContentLabelRequest{
		ActionID: labelled.Action.ID, ReviewedSubject: &edited, ExpectedVersion: "v3", IdempotencyKey: "snapshot-retract",
	})
	require.NoError(t, err)
	require.Equal(t, moderation.OutcomeApplied, retracted.Outcome)
	require.NotNil(t, retracted.Action)

	public, admin := snapshotLogEntries(t, service, removalID)
	original := map[string]any{"title": "Original snapshot title", "text": "indexed post content", "linkUrl": snapshotLinkURL, "capturedCid": moderationPostCID}
	assert.Equal(t, original, admin["contentSnapshot"], "a restore keeps the snapshot and later edits do not change it")
	assert.Equal(t, original, public["contentSnapshot"])

	others := map[string]string{
		moderation.ActionRestore: restored.Action.ID, moderation.ActionLabel: labelled.Action.ID,
		moderation.ActionRetractLabel: retracted.Action.ID,
	}
	for kind, id := range others {
		_, admin := snapshotLogEntries(t, service, id)
		assert.NotContains(t, admin, "contentSnapshot", kind)
	}
	assert.Equal(t, 1, snapshotRowCount(t, db, removalID))
	for kind, id := range others {
		assert.Equal(t, 0, snapshotRowCount(t, db, id), "a %s action keeps no snapshot", kind)
	}
}

// A restore records the root of the comment it restores, read from its
// removal, so it keeps the root after the comment row is gone.
func TestModerationContentSnapshotRestoreKeepsCommentRoot(t *testing.T) {
	db := testkit.DB(t)
	communityDID, authorDID := moderationPostActors(t, db)
	service := restrictedLogService(db)
	root := indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, "Restore root post", "")
	comment := restrictedLogComment(t, db, authorDID, root)
	removalID := restrictedLogRemove(t, service, comment, snapshotReasonSpam)
	_, err := db.ExecContext(t.Context(), `DELETE FROM comments WHERE uri = $1`, comment.URI)
	require.NoError(t, err)

	restored, err := service.RestoreContent(t.Context(), fixtures.DID("snapshotrootadmin"), moderation.RestoreContentRequest{
		ActionID: removalID, ExpectedVersion: "v1", IdempotencyKey: "snapshot-root-restore", Reason: snapshotReasonDiscretion,
	})
	require.NoError(t, err)
	require.Equal(t, moderation.OutcomeApplied, restored.Outcome)
	require.NotNil(t, restored.Action)

	_, admin := snapshotLogEntries(t, service, restored.Action.ID)
	subject := snapshotAdminSubject(t, admin)
	require.NotNil(t, subject)
	assert.Equal(t, comment.URI, subject["uri"])
	assert.Equal(t, root.URI, subject["rootUri"])
	assert.NotContains(t, admin, "contentSnapshot")
}
