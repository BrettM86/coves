//go:build integration

package postgres_test

import (
	"database/sql"
	"encoding/json"
	"testing"

	"Coves/internal/core/comments"
	"Coves/internal/core/moderation"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Once its author deletes a removed comment, the comment's snapshot stays out
// of the public log, even after a new record re-creates the comment's URI; the
// admin log keeps it (decided 2026-10-13).
func TestModerationContentSnapshotWithheldAfterAuthorDeletedCommentIsRecreated(t *testing.T) {
	db := testkit.DB(t)
	communityDID, authorDID := moderationPostActors(t, db)
	service := restrictedLogService(db)
	repository, ok := postgres.NewCommentRepository(db).(comments.RepositoryTx)
	require.True(t, ok)
	root := indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, "Recreated comment thread root", "")

	for _, test := range []struct {
		name          string
		inTransaction bool
	}{
		{name: "deleted inside the consumer's transaction", inTransaction: true},
		{name: "deleted without a transaction"},
	} {
		t.Run(test.name, func(t *testing.T) {
			subject := restrictedLogComment(t, db, authorDID, root)
			id := restrictedLogRemove(t, service, subject, snapshotReasonSpam)

			if test.inTransaction {
				tx, err := db.BeginTx(t.Context(), nil)
				require.NoError(t, err)
				defer tx.Rollback()
				deleted, err := repository.SoftDeleteWithReasonTx(t.Context(), tx, subject.URI, comments.DeletionReasonAuthor, authorDID)
				require.NoError(t, err)
				require.Equal(t, int64(1), deleted)
				require.NoError(t, tx.Commit())
			} else {
				deleted, err := repository.SoftDeleteWithReasonTx(t.Context(), nil, subject.URI, comments.DeletionReasonAuthor, authorDID)
				require.NoError(t, err)
				require.Equal(t, int64(1), deleted)
			}
			// The fields the comment consumer's resurrection sets when a new
			// record re-creates a deleted comment's URI.
			_, err := db.ExecContext(t.Context(), `
				UPDATE comments SET cid = $2, content = 'recreated comment text',
					deleted_at = NULL, deletion_reason = NULL, deleted_by = NULL
				WHERE uri = $1
			`, subject.URI, snapshotEditedCID)
			require.NoError(t, err)

			public, admin := snapshotLogEntries(t, service, id)
			assert.Equal(t, map[string]any{"text": "restricted log comment", "capturedCid": moderationCommentCID}, admin["contentSnapshot"],
				"the admin log keeps the snapshot")
			require.NotNil(t, public, "the removal stays in the public log")
			publicSubject := snapshotObject(t, public["subject"])
			require.NotNil(t, publicSubject)
			assert.Equal(t, subject.URI, publicSubject["uri"])
			assert.NotContains(t, public, "contentSnapshot", "the re-created comment must not bring back the deleted text")
			publicJSON, err := json.Marshal(public)
			require.NoError(t, err)
			assert.NotContains(t, string(publicJSON), "restricted log comment")
		})
	}
}

// The public log omits a comment's rootUri when the root post has a doxing or
// illegal-content action or is no longer indexed; the admin log carries it in
// privateSubject (decided 2026-10-13).
func TestModerationContentSnapshotRootURIWithheldForHiddenOrUnindexedRoot(t *testing.T) {
	db := testkit.DB(t)
	communityDID, authorDID := moderationPostActors(t, db)
	service := restrictedLogService(db)

	for _, test := range []struct {
		name         string
		withholdRoot func(t *testing.T, db *sql.DB, root moderation.StrongRef)
	}{
		{
			name: "root post removed for doxing",
			withholdRoot: func(t *testing.T, _ *sql.DB, root moderation.StrongRef) {
				restrictedLogRemove(t, service, root, snapshotReasonDoxing)
			},
		},
		{
			name: "root post no longer indexed",
			withholdRoot: func(t *testing.T, db *sql.DB, root moderation.StrongRef) {
				_, err := db.ExecContext(t.Context(), `DELETE FROM posts WHERE uri = $1`, root.URI)
				require.NoError(t, err)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := indexedModerationPost(t, db, moderation.PostV2Collection, communityDID, authorDID, "Withheld root title", "")
			subject := restrictedLogComment(t, db, authorDID, root)
			id := restrictedLogRemove(t, service, subject, snapshotReasonSpam)
			test.withholdRoot(t, db, root)

			public, admin := snapshotLogEntries(t, service, id)
			require.NotNil(t, public, "the comment's removal stays in the public log")
			publicSubject := snapshotObject(t, public["subject"])
			require.NotNil(t, publicSubject, "the public log still names the comment")
			assert.Equal(t, subject.URI, publicSubject["uri"])
			assert.NotContains(t, publicSubject, "rootUri")
			publicJSON, err := json.Marshal(public)
			require.NoError(t, err)
			assert.NotContains(t, string(publicJSON), root.URI, "the public log must not name the withheld root")

			privateSubject := snapshotObject(t, admin["privateSubject"])
			require.NotNil(t, privateSubject, "the admin log carries the withheld root")
			assert.Equal(t, subject.URI, privateSubject["uri"])
			assert.Equal(t, root.URI, privateSubject["rootUri"])
		})
	}
}
