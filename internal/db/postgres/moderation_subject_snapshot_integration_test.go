//go:build integration

package postgres_test

import (
	"testing"
	"time"

	"Coves/internal/core/moderation"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type subjectStateOutcome struct {
	state *moderation.SubjectState
	err   error
}

// getSubjectState reads one consistent snapshot. A retraction that holds the
// label decision row and commits while the read is in flight must neither
// block the read nor fail it with a serialization error: the read returns the
// state as of its snapshot.
func TestModerationSubjectStateReadSurvivesConcurrentLabelRetraction(t *testing.T) {
	db := testkit.DB(t)
	subject := indexedLabelPost(t, db)
	service := newPostgresModerationService(db)
	label := labelPost(t, service, fixtures.DID("snapshotlabeladmin"), subject, "v0", "snapshot-label")

	// Hold the writes a retraction makes, uncommitted, on a second connection.
	retraction, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = retraction.Rollback() })
	result, err := retraction.ExecContext(t.Context(), `
		UPDATE moderation_decisions SET active = FALSE
		WHERE subject_uri = $1 AND kind = 'label' AND active_action_id = $2 AND active
	`, subject.URI, label.Action.ID)
	require.NoError(t, err)
	updated, err := result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, updated)
	_, err = retraction.ExecContext(t.Context(), `UPDATE moderation_subjects SET version = 2 WHERE subject_uri = $1`, subject.URI)
	require.NoError(t, err)

	outcomes := make(chan subjectStateOutcome, 1)
	go func() {
		state, err := service.GetSubjectState(t.Context(), subject.URI)
		outcomes <- subjectStateOutcome{state: state, err: err}
	}()

	// Let the read either finish or reach a lock wait on the held decision row
	// before the retraction commits, so a locking label read sees a row
	// updated after its snapshot.
	var outcome *subjectStateOutcome
	testkit.WaitFor(t, 5*time.Second, func() (bool, error) {
		select {
		case finished := <-outcomes:
			outcome = &finished
			return true, nil
		default:
		}
		var waiting int
		err := db.QueryRowContext(t.Context(), `
			SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND pid <> pg_backend_pid()
			  AND wait_event_type = 'Lock' AND query LIKE '%moderation_decisions%'
		`).Scan(&waiting)
		return waiting == 1, err
	}, testkit.WithDescription("subject state read finished or waiting on the held label decision row"))
	require.NoError(t, retraction.Commit())
	if outcome == nil {
		select {
		case finished := <-outcomes:
			outcome = &finished
		case <-time.After(5 * time.Second):
			t.Fatal("subject state read did not finish after the retraction committed")
		}
	}

	require.NoError(t, outcome.err)
	require.NotNil(t, outcome.state)
	requirePersistedLabelState(t, *outcome.state, label.Action.ID, "v1", moderation.ModerationStateClear)

	after, err := service.GetSubjectState(t.Context(), subject.URI)
	require.NoError(t, err)
	require.NotNil(t, after)
	assert.Equal(t, "v2", after.Version)
	assert.Empty(t, after.LocalLabels)
	assert.Empty(t, after.Moderation.ContentLabels)
}
