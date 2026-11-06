//go:build integration

package postgres_test

import (
	"database/sql"
	"slices"
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

// activateInstanceRemoval writes this instance's active removal decision on
// subject, pointing at actionID.
func activateInstanceRemoval(t *testing.T, db *sql.DB, subject, actionID string) {
	t.Helper()
	_, err := db.ExecContext(t.Context(), `
		INSERT INTO moderation_decisions
		    (authority_did, scope_kind, scope_community_did, subject_uri, kind, value, active_action_id, active)
		VALUES ($1, 'instance', NULL, $2, 'removal', NULL, $3, TRUE)
	`, fixtures.InstanceDID(), subject, actionID)
	require.NoError(t, err)
}

// The account-ban reads (ActiveBan, the InForceSQL account restriction on
// served posts) and the community-removal read key on the active action's
// recorded subject_kind, whether or not the DID has a communities row.
func TestModerationReadsKeyOnRecordedSubjectKind(t *testing.T) {
	now := time.Date(2026, time.November, 5, 12, 0, 0, 0, time.UTC)
	future := time.Date(2099, time.January, 1, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name                 string
		subjectKind          moderation.SubjectKind
		expiresAt            *time.Time
		wantBan              bool
		wantCommunityRemoved bool
		wantPostServed       bool
	}{
		{
			name:        "permanent account ban on a DID indexed as a community",
			subjectKind: moderation.SubjectKindAccount, wantBan: true,
		},
		{
			name:        "community removal",
			subjectKind: moderation.SubjectKindCommunity, wantCommunityRemoved: true, wantPostServed: true,
		},
		{
			name:        "timed account ban on an indexed community DID",
			subjectKind: moderation.SubjectKindAccount, expiresAt: &future, wantBan: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := testkit.DB(t)
			did := fixtures.DID(testkit.UniqueIDWithPrefix(t, "kindread"))
			insertCommunityRowForDID(t, db, did)
			containerName := testkit.UniqueIDWithPrefix(t, "kindplace")
			container, err := fixtures.Community(t.Context(), db, containerName, "owner"+containerName)
			require.NoError(t, err)
			post := indexedModerationPost(t, db, moderation.PostV2Collection, container, did, "post by the subject", "")
			actionID := "kind-read-" + testkit.UniqueID(t)
			require.NoError(t, insertSubjectKindAction(t, db, moderationKindAction{
				id: actionID, subject: "at://" + did, action: moderation.ActionRemove,
				subjectKind: string(test.subjectKind), expiresAt: test.expiresAt,
			}))
			activateInstanceRemoval(t, db, "at://"+did, actionID)

			ban, err := postgres.NewAccountBanReader(db, fixtures.InstanceDID()).ActiveBan(t.Context(), did)
			require.NoError(t, err)
			if test.wantBan {
				require.NotNil(t, ban, "an account-kind decision is an account ban")
				assert.Equal(t, actionID, ban.ActionID)
				assert.Equal(t, test.expiresAt, ban.ExpiresAt)
				assert.True(t, ban.InForce(now))
			} else {
				assert.Nil(t, ban, "a community-kind decision is no account ban")
			}

			removed, err := postgres.NewCommunityRemovalReader(db, fixtures.InstanceDID()).CommunityRemoved(t.Context(), did)
			require.NoError(t, err)
			assert.Equal(t, test.wantCommunityRemoved, removed)

			views, _, err := postgres.NewPostRepository(db, postgres.WithAccountRestrictionAuthority(fixtures.InstanceDID())).
				GetByAuthor(t.Context(), posts.GetAuthorPostsRequest{ActorDID: did, Limit: 50})
			require.NoError(t, err)
			var served []string
			for _, view := range views {
				served = append(served, view.URI)
			}
			assert.Equal(t, test.wantPostServed, slices.Contains(served, post.URI),
				"the account restriction on served posts follows the recorded kind")
		})
	}
}
