//go:build integration

package jetstream

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A community withdraws its removal of a comment by deleting the removal
// record from its own repo. The withdrawal ends that community's decision and
// logs a retract-removal that names the apply-removal it reverses. It touches
// nothing else: an instance removal of the same comment stands, an instance
// restore does not end the community's decision, and a delete commit from a
// different community's repo, even under the same rkey, retracts nothing.

// ccrCommunityDecisionActive reports whether the community-scope removal
// decision of did:plc:acccommunity on commentURI is active, and the id of the
// apply-removal action it points at.
func ccrCommunityDecisionActive(t *testing.T, db *sql.DB, commentURI string) (bool, string) {
	t.Helper()
	var activeCount int
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*) FROM moderation_decisions
		WHERE subject_uri = $1 AND authority_did = 'did:plc:acccommunity'
		  AND scope_kind = 'community' AND scope_community_did = 'did:plc:acccommunity'
		  AND kind = 'removal' AND active`, commentURI).Scan(&activeCount))
	var applyID sql.NullString
	require.NoError(t, db.QueryRow(`
		SELECT MAX(id) FROM moderation_actions
		WHERE subject_uri = $1 AND action = 'apply-removal' AND authority_did = 'did:plc:acccommunity'`,
		commentURI).Scan(&applyID))
	return activeCount == 1, applyID.String
}

// ccrInstanceDecisionActive reports whether an instance-scope removal decision
// on commentURI is active.
func ccrInstanceDecisionActive(t *testing.T, db *sql.DB, commentURI string) bool {
	t.Helper()
	return countRows(t, db, `
		SELECT COUNT(*) FROM moderation_decisions
		WHERE subject_uri = $1 AND scope_kind = 'instance' AND kind = 'removal' AND active`, commentURI) == 1
}

type ccrRetraction struct {
	action, actor, authority, scopeKind string
	scopeCommunity                      sql.NullString
	subject, origin                     string
	reverses                            sql.NullString
}

func ccrRetractions(t *testing.T, db *sql.DB, commentURI string) []ccrRetraction {
	t.Helper()
	rows, err := db.Query(`
		SELECT action, actor_did, authority_did, scope_kind, scope_community_did, subject_uri, origin, reverses_action_id
		FROM moderation_actions WHERE subject_uri = $1 AND action = 'retract-removal'`, commentURI)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var got []ccrRetraction
	for rows.Next() {
		var r ccrRetraction
		require.NoError(t, rows.Scan(&r.action, &r.actor, &r.authority, &r.scopeKind, &r.scopeCommunity,
			&r.subject, &r.origin, &r.reverses))
		got = append(got, r)
	}
	require.NoError(t, rows.Err())
	return got
}

func TestCommunityCommentRemoval_Retraction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// instanceRemoveFirst: an instance admin removes the comment before the
		// community removal arrives.
		instanceRemoveFirst bool
		// instanceRemoveThenRestore: after the community removal, an instance
		// admin removes and then restores the comment.
		instanceRemoveThenRestore bool
		// deleteFrom is the repo whose bodyless removal delete commit arrives
		// last; "" sends none.
		deleteFrom      string
		communityActive bool
		retracted       bool
		instanceActive  bool
	}{
		{name: "C deletes its removal record", deleteFrom: "did:plc:acccommunity", retracted: true},
		{
			name: "C deletes its removal record while an instance removal stands", instanceRemoveFirst: true,
			deleteFrom: "did:plc:acccommunity", retracted: true, instanceActive: true,
		},
		{name: "instance restore leaves the community removal", instanceRemoveThenRestore: true, communityActive: true},
		{name: "delete commit from another community under the same rkey", deleteFrom: ccrOtherCommunity, communityActive: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newCCRFixture(t)
			ctx := context.Background()
			base := time.Now().UnixMicro()
			revs := increasingTIDs(t, 4)
			moderator := moderation.NewService(
				moderation.NewRepositorySubjectReader(postgres.NewPostRepository(f.db), postgres.NewCommentRepository(f.db)),
				postgres.NewModerationRepository(f.db),
				moderation.Config{InstanceDID: fixtures.InstanceDID(), IdempotencyRetention: 24 * time.Hour, MaxLiveIdempotencyKeys: 1000},
			)
			adminDID := fixtures.DID(testkit.UniqueIDWithPrefix(t, "ccradmin"))

			const (
				rootRkey    = "ccrroot"
				rootCID     = "bafyreiccrroot"
				commentRkey = "ccrcomment"
				commentCID  = "bafyreiccrcomment"
			)
			rootURI := accPostURI(rootRkey)
			commentURI := ccrCommentURI(commentRkey)
			comment := moderation.StrongRef{URI: commentURI, CID: commentCID}

			require.NoError(t, f.posts.consumer.HandleEvent(ctx, pv2Event(
				accAuthor, "create", rootRkey, revs[0], rootCID, base+1_000_000,
				pv2Record(accCommunity, "thread root", "root body"))), "root post")
			require.NoError(t, f.comments.HandleEvent(ctx, revCommitEvent(
				ccrCommenter, CommentCollection, "create", commentRkey, revs[1], commentCID, base+2_000_000,
				revCommentRecord("the comment", rootURI, rootCID, rootURI, rootCID))), "comment")

			instanceRemove := func(key string) *moderation.MutationResult {
				t.Helper()
				state, err := moderator.GetSubjectState(ctx, commentURI)
				require.NoError(t, err)
				removed, err := moderator.RemoveContent(ctx, adminDID, moderation.RemoveContentRequest{
					Subject: comment, ExpectedVersion: state.Version, IdempotencyKey: key,
					Reason: "social.coves.moderation.defs#reasonSpam",
				})
				require.NoError(t, err, "instance removal")
				require.Equal(t, moderation.OutcomeApplied, removed.Outcome, "instance removal")
				require.True(t, ccrInstanceDecisionActive(t, f.db, commentURI), "precondition: the instance removal is active")
				return removed
			}

			if tt.instanceRemoveFirst {
				instanceRemove("ccr-instance-remove-first")
			}

			require.NoError(t, f.posts.consumer.HandleEvent(ctx, removalEvent(
				accCommunity, commentURI, commentCID, string(posts.DecisionRuleViolation), revs[2], base+3_000_000)),
				"removal from C")
			if tt.instanceRemoveFirst {
				active, _ := ccrCommunityDecisionActive(t, f.db, commentURI)
				require.True(t, active, "precondition: the community removal is applied alongside the instance removal")
			} else {
				assertCommunityCommentRemoval(t, f.db, commentURI, true)
				require.False(t, t.Failed(), "precondition: the community removal is applied")
			}
			_, applyID := ccrCommunityDecisionActive(t, f.db, commentURI)
			require.NotEmpty(t, applyID, "precondition: an apply-removal action exists")

			if tt.instanceRemoveThenRestore {
				removed := instanceRemove("ccr-instance-remove")
				state, err := moderator.GetSubjectState(ctx, commentURI)
				require.NoError(t, err)
				restored, err := moderator.RestoreContent(ctx, adminDID, moderation.RestoreContentRequest{
					ActionID: removed.Action.ID, ReviewedSubject: &comment, ExpectedVersion: state.Version,
					IdempotencyKey: "ccr-instance-restore", Reason: "social.coves.moderation.defs#reasonModeratorDiscretion",
				})
				require.NoError(t, err, "instance restore")
				require.Equal(t, moderation.OutcomeApplied, restored.Outcome, "instance restore")
			}

			if tt.deleteFrom != "" {
				require.NoError(t, f.posts.consumer.HandleEvent(ctx, removalDeleteEvent(
					tt.deleteFrom, commentURI, revs[3], base+4_000_000)), "removal delete")
			}

			active, _ := ccrCommunityDecisionActive(t, f.db, commentURI)
			assert.Equal(t, tt.communityActive, active, "community removal decision active")
			assert.Equal(t, tt.instanceActive, ccrInstanceDecisionActive(t, f.db, commentURI), "instance removal decision active")

			retractions := ccrRetractions(t, f.db, commentURI)
			if !tt.retracted {
				assert.Empty(t, retractions, "no retract-removal may be logged")
				return
			}
			require.Len(t, retractions, 1, "exactly one retract-removal is logged")
			assert.Equal(t, ccrRetraction{
				action:         "retract-removal",
				actor:          "did:plc:acccommunity",
				authority:      "did:plc:acccommunity",
				scopeKind:      "community",
				scopeCommunity: sql.NullString{String: "did:plc:acccommunity", Valid: true},
				subject:        commentURI,
				origin:         "inherited",
				reverses:       sql.NullString{String: applyID, Valid: true},
			}, retractions[0])
		})
	}
}
