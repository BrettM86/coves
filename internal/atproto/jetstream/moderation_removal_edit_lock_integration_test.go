//go:build integration

package jetstream

import (
	"context"
	"fmt"
	"testing"
	"time"

	"Coves/internal/core/moderation"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pausingModerationStore parks a mutation inside its transaction at
// InsertAction, after it has read the indexed subject and taken that read's
// content-row lock, until release is closed.
type pausingModerationStore struct {
	moderation.Store
	paused  chan struct{}
	release chan struct{}
}

func (s *pausingModerationStore) InTransaction(ctx context.Context, fn func(context.Context, moderation.Transaction) error) error {
	return s.Store.InTransaction(ctx, func(ctx context.Context, tx moderation.Transaction) error {
		return fn(ctx, &pausingModerationTransaction{Transaction: tx, store: s})
	})
}

type pausingModerationTransaction struct {
	moderation.Transaction
	store *pausingModerationStore
}

func (tx *pausingModerationTransaction) InsertAction(ctx context.Context, action moderation.Action) (*moderation.Action, error) {
	close(tx.store.paused)
	select {
	case <-tx.store.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return tx.Transaction.InsertAction(ctx, action)
}

// An author edit that adds image Y must not commit between the removal's read
// of the indexed subject and the removal's commit. If it could, the edit's
// media reconciliation would find no active removal yet, and the removal would
// block only the images it read, leaving Y served. The content-row share lock
// taken with that read orders the two: the edit waits for the removal, then its
// reconciliation sees the active removal and blocks Y.
func TestModerationRemovalHoldsContentRowAgainstAuthorEdit(t *testing.T) {
	imageY := postModerationCIDTwo
	for _, scenario := range []struct {
		name string
		// setup indexes the subject with one image and returns it with the
		// consumer and event that edit it to add imageY.
		setup func(t *testing.T, f postModerationConsumerFixture) (moderation.StrongRef, func(context.Context) error)
	}{
		{name: "postv2", setup: func(t *testing.T, f postModerationConsumerFixture) (moderation.StrongRef, func(context.Context) error) {
			update := pv2Event(pv2Author, "update", f.rkey, f.revs[1], editMediaCID("locked post edit"),
				f.createdAt+1_000_000, postModerationRecord(postModerationCIDOne, imageY))
			return moderation.StrongRef{URI: f.uri, CID: postModerationRecordCID},
				func(ctx context.Context) error { return f.consumer.HandleEvent(ctx, update) }
		}},
		{name: "comment", setup: func(t *testing.T, f postModerationConsumerFixture) (moderation.StrongRef, func(context.Context) error) {
			reconciler := moderation.NewMediaReconciler(postgres.NewModerationRepository(f.db), fixtures.InstanceDID(), nil)
			consumer := NewCommentEventConsumer(postgres.NewCommentRepository(f.db), f.db, WithCommentMediaReconciler(reconciler))
			root := moderation.StrongRef{URI: f.uri, CID: postModerationRecordCID}
			rkey := testkit.TID()
			created := editMediaCID("locked comment create")
			now := time.Now()
			require.NoError(t, consumer.HandleEvent(t.Context(),
				editMediaCommentEvent(root, "create", rkey, f.revs[2], created, now, postModerationCIDOne)))
			update := editMediaCommentEvent(root, "update", rkey, f.revs[3], editMediaCID("locked comment edit"),
				now.Add(time.Second), postModerationCIDOne, imageY)
			return moderation.StrongRef{URI: "at://" + pv2Author + "/" + moderation.CommentCollection + "/" + rkey, CID: created},
				func(ctx context.Context) error { return consumer.HandleEvent(ctx, update) }
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f := newPostModerationConsumerFixture(t)
			subject, edit := scenario.setup(t, f)
			// The pool is three connections: the removal, the edit and this one.
			observer, err := f.db.Conn(t.Context())
			require.NoError(t, err)
			t.Cleanup(func() { _ = observer.Close() })
			store := &pausingModerationStore{
				Store:  postgres.NewModerationRepository(f.db),
				paused: make(chan struct{}), release: make(chan struct{}),
			}
			moderator := moderation.NewService(
				moderation.NewRepositorySubjectReader(f.postRepository, postgres.NewCommentRepository(f.db)),
				store, moderation.Config{InstanceDID: fixtures.InstanceDID(), IdempotencyRetention: 24 * time.Hour, MaxLiveIdempotencyKeys: 1000},
			)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			released := false
			releaseRemoval := func() {
				if !released {
					released = true
					close(store.release)
				}
			}
			t.Cleanup(releaseRemoval)

			removalDone := make(chan error, 1)
			go func() {
				_, removeErr := moderator.RemoveContent(ctx, fixtures.DID("lockedremovaladmin"), moderation.RemoveContentRequest{
					Subject: subject, ExpectedVersion: "v0", IdempotencyKey: "locked-removal",
					Reason: "social.coves.moderation.defs#reasonSpam",
				})
				removalDone <- removeErr
			}()
			testkit.WaitFor(t, 5*time.Second, func() (bool, error) {
				select {
				case <-store.paused:
					return true, nil
				case removeErr := <-removalDone:
					return false, fmt.Errorf("removal finished before reaching InsertAction: %v", removeErr)
				default:
					return false, nil
				}
			}, testkit.WithDescription("removal paused after reading the indexed subject"))

			editDone := make(chan error, 1)
			go func() { editDone <- edit(ctx) }()
			// Either the edit waits on a row lock, or nothing orders it and it
			// commits while the removal is still paused.
			testkit.WaitFor(t, 5*time.Second, func() (bool, error) {
				if len(editDone) > 0 {
					return true, nil
				}
				var waiting int
				err := observer.QueryRowContext(t.Context(), `
					SELECT count(*) FROM pg_stat_activity
					WHERE datname = current_database() AND pid <> pg_backend_pid()
					  AND wait_event_type = 'Lock' AND wait_event <> 'advisory'
				`).Scan(&waiting)
				return waiting > 0, err
			}, testkit.WithDescription("author edit waiting on a lock or finished"))
			assert.Empty(t, editDone, "the author edit must not commit while the removal holds its read of the subject")
			var indexedCID string
			require.NoError(t, observer.QueryRowContext(t.Context(), `
				SELECT cid FROM posts WHERE uri = $1 UNION ALL SELECT cid FROM comments WHERE uri = $1
			`, subject.URI).Scan(&indexedCID))
			assert.Equal(t, subject.CID, indexedCID, "the edit must not be visible while the removal is paused")

			releaseRemoval()
			require.NoError(t, <-removalDone)
			require.NoError(t, <-editDone)
			require.NoError(t, observer.QueryRowContext(t.Context(), `
				SELECT cid FROM posts WHERE uri = $1 UNION ALL SELECT cid FROM comments WHERE uri = $1
			`, subject.URI).Scan(&indexedCID))
			assert.NotEqual(t, subject.CID, indexedCID, "the edit must be indexed after the removal commits")
			blocked, err := postgres.NewModerationRepository(f.db).IsBlocked(t.Context(), pv2Author, imageY)
			require.NoError(t, err)
			assert.True(t, blocked, "the image the edit added must be blocked under the removal")
		})
	}
}

var _ moderation.Store = (*pausingModerationStore)(nil)
