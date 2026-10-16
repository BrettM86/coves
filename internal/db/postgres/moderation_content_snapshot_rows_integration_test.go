//go:build integration

package postgres_test

import (
	"encoding/json"
	"testing"

	"Coves/internal/core/comments"
	"Coves/internal/core/moderation"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	snapshotRowsEditedLinkURL = "https://news.example/edited-story"
	snapshotRowsEditedEmbed   = `{"$type":"social.coves.embed.external","external":{"uri":"https://news.example/edited-story","title":"Edited story card"}}`
	snapshotRowsArrayEmbed    = `[{"$type":"social.coves.embed.external","external":{"uri":"https://news.example/array-story"}}]`
)

// snapshotRowsRemove removes subject at the given subject-state version and
// returns the removal's action ID.
func snapshotRowsRemove(t *testing.T, service moderation.Service, subject moderation.StrongRef, reason, version string) string {
	t.Helper()
	removed, err := service.RemoveContent(t.Context(), fixtures.DID("snapshotrowsadmin"), moderation.RemoveContentRequest{
		Subject: subject, ExpectedVersion: version, IdempotencyKey: testkit.UniqueIDWithPrefix(t, "remove"), Reason: reason,
	})
	require.NoError(t, err)
	require.Equal(t, moderation.OutcomeApplied, removed.Outcome)
	require.NotNil(t, removed.Action)
	return removed.Action.ID
}

// snapshotRowsRestore restores a removal and returns the restore's action ID.
func snapshotRowsRestore(t *testing.T, service moderation.Service, removalID string, reviewed *moderation.StrongRef, version string) string {
	t.Helper()
	restored, err := service.RestoreContent(t.Context(), fixtures.DID("snapshotrowsadmin"), moderation.RestoreContentRequest{
		ActionID: removalID, ReviewedSubject: reviewed, ExpectedVersion: version,
		IdempotencyKey: testkit.UniqueIDWithPrefix(t, "restore"), Reason: snapshotReasonDiscretion,
	})
	require.NoError(t, err)
	require.Equal(t, moderation.OutcomeApplied, restored.Outcome)
	require.NotNil(t, restored.Action)
	return restored.Action.ID
}

// snapshotRowsLabelAndRetract labels a fresh post NSFW and retracts the label,
// returning both action IDs; the subject is then at version v2.
func snapshotRowsLabelAndRetract(t *testing.T, service moderation.Service, subject moderation.StrongRef) (labelID, retractID string) {
	t.Helper()
	actor := fixtures.DID("snapshotrowsadmin")
	labelled := labelPost(t, service, actor, subject, "v0", testkit.UniqueIDWithPrefix(t, "label"))
	retracted, err := service.RetractContentLabel(t.Context(), actor, moderation.RetractContentLabelRequest{
		ActionID: labelled.Action.ID, ReviewedSubject: &subject, ExpectedVersion: "v1",
		IdempotencyKey: testkit.UniqueIDWithPrefix(t, "retract"),
	})
	require.NoError(t, err)
	require.Equal(t, moderation.OutcomeApplied, retracted.Outcome)
	require.NotNil(t, retracted.Action)
	return labelled.Action.ID, retracted.Action.ID
}

// A restore row shows the snapshot of the removal it reverses: the version the
// moderator saw at removal, not the post as later edited.
func TestModerationContentSnapshotRestoreShowsReversedRemovalSnapshot(t *testing.T) {
	db := testkit.DB(t)
	communityDID, authorDID := moderationPostActors(t, db)
	service := restrictedLogService(db)
	subject := indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, "Restored original title", snapshotLinkEmbed)
	removalID := snapshotRowsRemove(t, service, subject, snapshotReasonSpam, "v0")

	_, err := db.ExecContext(t.Context(), `
		UPDATE posts SET title = 'Restored edited title', content = 'restored edited content', embed = $2::jsonb, cid = $3 WHERE uri = $1
	`, subject.URI, snapshotRowsEditedEmbed, snapshotEditedCID)
	require.NoError(t, err)
	edited := moderation.StrongRef{URI: subject.URI, CID: snapshotEditedCID}
	restoreID := snapshotRowsRestore(t, service, removalID, &edited, "v1")

	public, admin := snapshotLogEntries(t, service, restoreID)
	original := map[string]any{"title": "Restored original title", "text": "indexed post content", "linkUrl": snapshotLinkURL, "capturedCid": moderationPostCID}
	assert.Equal(t, original, admin["contentSnapshot"], "the admin restore row shows the reversed removal's snapshot")
	require.NotNil(t, public, "the restore is listed in the public log")
	assert.Equal(t, original, public["contentSnapshot"], "the public restore row shows the reversed removal's snapshot")
}

// A restore of a doxing removal shows its snapshot to admins only; a restore of
// an illegal-content removal has none to show.
func TestModerationContentSnapshotRestoreOfHiddenRemoval(t *testing.T) {
	db := testkit.DB(t)
	communityDID, authorDID := moderationPostActors(t, db)
	service := restrictedLogService(db)

	for _, test := range []struct {
		name     string
		title    string
		reason   string
		snapshot map[string]any
	}{
		{
			name: "doxing removal", title: "Restored doxing title", reason: snapshotReasonDoxing,
			snapshot: map[string]any{"title": "Restored doxing title", "text": "indexed post content", "linkUrl": snapshotLinkURL, "capturedCid": moderationPostCID},
		},
		{name: "illegal-content removal", title: "Restored illegal title", reason: snapshotReasonIllegal},
	} {
		t.Run(test.name, func(t *testing.T) {
			subject := indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, test.title, snapshotLinkEmbed)
			removalID := snapshotRowsRemove(t, service, subject, test.reason, "v0")
			restoreID := snapshotRowsRestore(t, service, removalID, &subject, "v1")

			public, admin := snapshotLogEntries(t, service, restoreID)
			if test.snapshot == nil {
				assert.NotContains(t, admin, "contentSnapshot")
			} else {
				assert.Equal(t, test.snapshot, admin["contentSnapshot"], "the admin restore row shows the hidden removal's snapshot")
			}
			assert.NotContains(t, public, "contentSnapshot", "the public log never shows a hidden removal's snapshot")
			publicJSON, err := json.Marshal(public)
			require.NoError(t, err)
			assert.NotContains(t, string(publicJSON), test.title)
		})
	}
}

// After a restore, the public restore row withholds the snapshot once the
// subject's author deletes it, even if a new record re-creates a deleted
// comment, and once the subject's row is gone; the admin row keeps it.
func TestModerationContentSnapshotRestoreWithheldAfterAuthorDeletion(t *testing.T) {
	db := testkit.DB(t)
	communityDID, authorDID := moderationPostActors(t, db)
	service := restrictedLogService(db)
	repository, ok := postgres.NewCommentRepository(db).(comments.RepositoryTx)
	require.True(t, ok)
	root := indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, "Restore deletion thread root", "")
	commentSnapshot := map[string]any{"text": "restricted log comment", "capturedCid": moderationCommentCID}

	for _, test := range []struct {
		name     string
		subject  moderation.StrongRef
		withhold func(t *testing.T, subject moderation.StrongRef)
		snapshot map[string]any
	}{
		{
			name:    "post deleted by its author after the restore",
			subject: indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, "Restored then deleted title", snapshotLinkEmbed),
			withhold: func(t *testing.T, subject moderation.StrongRef) {
				_, err := db.ExecContext(t.Context(), `UPDATE posts SET deleted_at = NOW() WHERE uri = $1`, subject.URI)
				require.NoError(t, err)
			},
			snapshot: map[string]any{"title": "Restored then deleted title", "text": "indexed post content", "linkUrl": snapshotLinkURL, "capturedCid": moderationPostCID},
		},
		{
			name:    "comment deleted by its author after the restore, then re-created",
			subject: restrictedLogComment(t, db, authorDID, root),
			withhold: func(t *testing.T, subject moderation.StrongRef) {
				deleted, err := repository.SoftDeleteWithReasonTx(t.Context(), nil, subject.URI, comments.DeletionReasonAuthor, authorDID)
				require.NoError(t, err)
				require.Equal(t, int64(1), deleted)
				// The fields the comment consumer's resurrection sets.
				_, err = db.ExecContext(t.Context(), `
					UPDATE comments SET cid = $2, content = 'recreated comment text',
						deleted_at = NULL, deletion_reason = NULL, deleted_by = NULL
					WHERE uri = $1
				`, subject.URI, snapshotEditedCID)
				require.NoError(t, err)
			},
			snapshot: commentSnapshot,
		},
		{
			name:    "comment row gone after the restore",
			subject: restrictedLogComment(t, db, authorDID, root),
			withhold: func(t *testing.T, subject moderation.StrongRef) {
				_, err := db.ExecContext(t.Context(), `DELETE FROM comments WHERE uri = $1`, subject.URI)
				require.NoError(t, err)
			},
			snapshot: commentSnapshot,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			removalID := snapshotRowsRemove(t, service, test.subject, snapshotReasonSpam, "v0")
			restoreID := snapshotRowsRestore(t, service, removalID, &test.subject, "v1")
			test.withhold(t, test.subject)

			public, admin := snapshotLogEntries(t, service, restoreID)
			assert.Equal(t, test.snapshot, admin["contentSnapshot"], "the admin restore row keeps the snapshot")
			require.NotNil(t, public, "the restore stays in the public log")
			assert.NotContains(t, public, "contentSnapshot", "the public restore row withholds the snapshot")
			publicJSON, err := json.Marshal(public)
			require.NoError(t, err)
			for _, field := range []string{"title", "text", "linkUrl"} {
				if value, present := test.snapshot[field]; present {
					assert.NotContains(t, string(publicJSON), value, "the public log must not show the withheld %s", field)
				}
			}
		})
	}
}

// Label and retract-label rows in the admin log show the post as it is indexed
// now, with its current CID, and follow later edits.
func TestModerationContentSnapshotLabelRowsShowCurrentPost(t *testing.T) {
	db := testkit.DB(t)
	communityDID, authorDID := moderationPostActors(t, db)
	service := restrictedLogService(db)
	subject := indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, "Labelled current title", snapshotLinkEmbed)
	labelID, retractID := snapshotRowsLabelAndRetract(t, service, subject)
	rows := map[string]string{moderation.ActionLabel: labelID, moderation.ActionRetractLabel: retractID}

	current := map[string]any{"title": "Labelled current title", "text": "indexed post content", "linkUrl": snapshotLinkURL, "capturedCid": moderationPostCID}
	for kind, id := range rows {
		_, admin := snapshotLogEntries(t, service, id)
		assert.Equal(t, current, admin["contentSnapshot"], "the %s row shows the indexed post", kind)
	}

	_, err := db.ExecContext(t.Context(), `
		UPDATE posts SET title = 'Labelled edited title', content = 'labelled edited content', embed = $2::jsonb, cid = $3 WHERE uri = $1
	`, subject.URI, snapshotRowsEditedEmbed, snapshotEditedCID)
	require.NoError(t, err)

	edited := map[string]any{"title": "Labelled edited title", "text": "labelled edited content", "linkUrl": snapshotRowsEditedLinkURL, "capturedCid": snapshotEditedCID}
	for kind, id := range rows {
		_, admin := snapshotLogEntries(t, service, id)
		assert.Equal(t, edited, admin["contentSnapshot"], "the %s row follows the post's edit", kind)
	}
}

// A label or retract-label row stops showing the post once its author deletes
// it, its row is erased, or the subject has ever had a doxing or
// illegal-content action, whose snapshots are deleted even for admins.
func TestModerationContentSnapshotLabelRowsWithheld(t *testing.T) {
	db := testkit.DB(t)
	service := restrictedLogService(db)

	for _, test := range []struct {
		name     string
		withhold func(t *testing.T, subject moderation.StrongRef, authorDID string)
	}{
		{
			name: "post deleted by its author",
			withhold: func(t *testing.T, subject moderation.StrongRef, _ string) {
				_, err := db.ExecContext(t.Context(), `UPDATE posts SET deleted_at = NOW() WHERE uri = $1`, subject.URI)
				require.NoError(t, err)
			},
		},
		{
			name: "post row erased with its author's account",
			withhold: func(t *testing.T, subject moderation.StrongRef, authorDID string) {
				require.NoError(t, postgres.NewUserRepository(db).Delete(t.Context(), authorDID))
				var remaining int
				require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM posts WHERE uri = $1`, subject.URI).Scan(&remaining))
				require.Zero(t, remaining, "erasure removes the post row")
			},
		},
		{
			name: "post removed for doxing",
			withhold: func(t *testing.T, subject moderation.StrongRef, _ string) {
				snapshotRowsRemove(t, service, subject, snapshotReasonDoxing, "v2")
			},
		},
		{
			name: "post removed for doxing and later restored",
			withhold: func(t *testing.T, subject moderation.StrongRef, _ string) {
				removalID := snapshotRowsRemove(t, service, subject, snapshotReasonDoxing, "v2")
				snapshotRowsRestore(t, service, removalID, &subject, "v3")
			},
		},
		{
			name: "post removed for illegal content",
			withhold: func(t *testing.T, subject moderation.StrongRef, _ string) {
				snapshotRowsRemove(t, service, subject, snapshotReasonIllegal, "v2")
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			communityDID, authorDID := moderationPostActors(t, db)
			subject := indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, "Withheld label title", snapshotLinkEmbed)
			labelID, retractID := snapshotRowsLabelAndRetract(t, service, subject)
			rows := map[string]string{moderation.ActionLabel: labelID, moderation.ActionRetractLabel: retractID}
			current := map[string]any{"title": "Withheld label title", "text": "indexed post content", "linkUrl": snapshotLinkURL, "capturedCid": moderationPostCID}
			for kind, id := range rows {
				_, admin := snapshotLogEntries(t, service, id)
				require.Equal(t, current, admin["contentSnapshot"], "the %s row shows the post before it is withheld", kind)
			}

			test.withhold(t, subject, authorDID)

			for kind, id := range rows {
				_, admin := snapshotLogEntries(t, service, id)
				assert.NotContains(t, admin, "contentSnapshot", "the %s row no longer shows the post", kind)
			}
		})
	}
}

// A label row on a post whose title, body or link is unavailable shows the
// current CID and omits the rest, and the listing still succeeds.
func TestModerationContentSnapshotLabelRowsOmitUnavailableFields(t *testing.T) {
	db := testkit.DB(t)
	communityDID, authorDID := moderationPostActors(t, db)
	service := restrictedLogService(db)

	for _, test := range []struct {
		name     string
		title    string
		embed    string
		clearAll bool
		snapshot map[string]any
	}{
		{
			name: "no title, body or embed", clearAll: true,
			snapshot: map[string]any{"capturedCid": moderationPostCID},
		},
		{
			name: "embed is a JSON array", title: "Array embed label title", embed: snapshotRowsArrayEmbed,
			snapshot: map[string]any{"title": "Array embed label title", "text": "indexed post content", "capturedCid": moderationPostCID},
		},
		{
			name: "external embed with an invalid uri", title: "Malformed link label title", embed: snapshotMalformedEmbed,
			snapshot: map[string]any{"title": "Malformed link label title", "text": "indexed post content", "capturedCid": moderationPostCID},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			subject := indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, test.title, test.embed)
			if test.clearAll {
				_, err := db.ExecContext(t.Context(), `UPDATE posts SET title = NULL, content = NULL, embed = NULL WHERE uri = $1`, subject.URI)
				require.NoError(t, err)
			}
			labelled := labelPost(t, service, fixtures.DID("snapshotrowsadmin"), subject, "v0", testkit.UniqueIDWithPrefix(t, "label"))

			_, admin := snapshotLogEntries(t, service, labelled.Action.ID)
			assert.Equal(t, test.snapshot, admin["contentSnapshot"])
		})
	}
}
