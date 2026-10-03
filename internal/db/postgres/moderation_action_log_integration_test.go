//go:build integration

package postgres_test

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"Coves/internal/core/moderation"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	actionLogSpam       = "social.coves.moderation.defs#reasonSpam"
	actionLogHarassment = "social.coves.moderation.defs#reasonHarassment"
	actionLogDoxing     = "social.coves.moderation.defs#reasonDoxing"
	actionLogIllegal    = "social.coves.moderation.defs#reasonIllegalContent"
	actionLogRule       = "social.coves.moderation.defs#reasonRuleViolation"
	actionLogDiscretion = "social.coves.moderation.defs#reasonModeratorDiscretion"
)

func actionLogRow(id string, createdAt time.Time) moderation.Action {
	return moderation.Action{
		ID: id, ActorDID: fixtures.DID("actor-a"), AuthorityDID: fixtures.DID("authority-a"),
		ScopeKind:           moderation.ScopeInstance,
		SubjectURI:          "at://" + fixtures.DID("subject-a") + "/" + moderation.CommentCollection + "/" + id,
		SubjectCollection:   moderation.CommentCollection,
		SubjectCommunityDID: fixtures.DID("community-a"),
		ObservedCID:         moderationCommentCID, Action: moderation.ActionRemove,
		Reason: actionLogSpam, PrivateNote: "private note for " + id,
		Origin: moderation.OriginLocal, CreatedAt: createdAt,
	}
}

func insertActionLogRow(t *testing.T, db *sql.DB, action moderation.Action) {
	t.Helper()
	_, err := db.ExecContext(t.Context(), `
		INSERT INTO moderation_actions
		    (id, actor_did, authority_did, scope_kind, scope_community_did,
		     subject_uri, subject_collection, subject_community_did, observed_cid,
		     action, label_value, reason, private_note, reverses_action_id, origin, created_at)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6, $7, NULLIF($8, ''), NULLIF($9, ''),
		        $10, NULLIF($11, ''), NULLIF($12, ''), NULLIF($13, ''), NULLIF($14, ''), $15, $16)
	`, action.ID, action.ActorDID, action.AuthorityDID, action.ScopeKind, action.ScopeCommunityDID,
		action.SubjectURI, action.SubjectCollection, action.SubjectCommunityDID, action.ObservedCID,
		action.Action, action.LabelValue, action.Reason, action.PrivateNote, action.ReversesActionID,
		action.Origin, action.CreatedAt)
	require.NoError(t, err)
}

func listActionLog(t *testing.T, db *sql.DB, query moderation.ActionListQuery) []moderation.Action {
	t.Helper()
	actions, err := postgres.NewModerationRepository(db).ListActions(t.Context(), query)
	require.NoError(t, err)
	return actions
}

func actionLogIDs(actions []moderation.Action) []string {
	ids := make([]string, 0, len(actions))
	for _, action := range actions {
		ids = append(ids, action.ID)
	}
	return ids
}

func TestModerationActionLogMigrationIndexes(t *testing.T) {
	db := testkit.DB(t)
	rows, err := db.QueryContext(t.Context(), `
		SELECT pg_get_indexdef(i.indexrelid), i.indisvalid
		FROM pg_index i
		JOIN pg_class relation ON relation.oid = i.indrelid
		JOIN pg_namespace namespace ON namespace.oid = relation.relnamespace
		WHERE namespace.nspname = 'public' AND relation.relname = 'moderation_actions'
	`)
	require.NoError(t, err)
	defer rows.Close()
	definitions := make(map[string]bool)
	for rows.Next() {
		var definition string
		var valid bool
		require.NoError(t, rows.Scan(&definition, &valid))
		for _, columns := range []string{
			"(created_at DESC, id DESC)",
			"(actor_did, created_at DESC, id DESC)",
			"(subject_community_did, created_at DESC, id DESC)",
		} {
			if strings.Contains(definition, "USING btree "+columns) {
				assert.True(t, valid, "index %s must be valid", columns)
				definitions[columns] = true
			}
		}
	}
	require.NoError(t, rows.Err())
	for _, columns := range []string{
		"(created_at DESC, id DESC)",
		"(actor_did, created_at DESC, id DESC)",
		"(subject_community_did, created_at DESC, id DESC)",
	} {
		assert.True(t, definitions[columns], "migration 051 must create moderation_actions index %s", columns)
	}
}

func TestModerationActionLogOrderLimitAndKeyset(t *testing.T) {
	db := testkit.DB(t)
	base := time.Date(2026, 9, 28, 12, 0, 0, 123456000, time.UTC)
	for _, row := range []moderation.Action{
		actionLogRow("b", base.Add(-time.Microsecond)),
		actionLogRow("a", base),
		actionLogRow("c", base),
		actionLogRow("d", base.Add(time.Microsecond)),
	} {
		insertActionLogRow(t, db, row)
	}
	require.Equal(t, []string{"d", "c", "a", "b"}, actionLogIDs(listActionLog(t, db, moderation.ActionListQuery{Limit: 10})))
	assert.Equal(t, []string{"d", "c"}, actionLogIDs(listActionLog(t, db, moderation.ActionListQuery{Limit: 2})))
	for _, test := range []struct {
		name   string
		before moderation.ActionKey
		want   []string
	}{
		{"after newest", moderation.ActionKey{CreatedAt: base.Add(time.Microsecond), ID: "d"}, []string{"c", "a", "b"}},
		{"same timestamp smaller id", moderation.ActionKey{CreatedAt: base, ID: "c"}, []string{"a", "b"}},
		{"after oldest", moderation.ActionKey{CreatedAt: base.Add(-time.Microsecond), ID: "b"}, []string{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, actionLogIDs(listActionLog(t, db, moderation.ActionListQuery{Limit: 10, Before: &test.before})))
		})
	}
}

func TestModerationActionLogFiltersAndMicrosecondBounds(t *testing.T) {
	db := testkit.DB(t)
	base := time.Date(2026, 9, 28, 12, 0, 0, 123456000, time.UTC)
	first := actionLogRow("first", base)
	second := actionLogRow("second", base.Add(time.Microsecond))
	second.ActorDID = fixtures.DID("actor-b")
	second.AuthorityDID = fixtures.DID("authority-b")
	second.SubjectURI = "at://" + fixtures.DID("subject-b") + "/" + moderation.PostV2Collection + "/second"
	second.SubjectCollection = moderation.PostV2Collection
	second.SubjectCommunityDID = fixtures.DID("community-b")
	second.Action = moderation.ActionRestore
	second.ReversesActionID = first.ID
	second.Origin = "inherited"
	third := actionLogRow("third", base.Add(2*time.Microsecond))
	third.SubjectURI = first.SubjectURI
	third.SubjectCollection = moderation.PostV2Collection
	third.ActorDID = second.ActorDID
	third.SubjectCommunityDID = second.SubjectCommunityDID
	third.Origin = second.Origin
	for _, row := range []moderation.Action{first, second, third} {
		insertActionLogRow(t, db, row)
	}
	require.Equal(t, []string{"third", "second", "first"}, actionLogIDs(listActionLog(t, db, moderation.ActionListQuery{Limit: 10})))
	for _, test := range []struct {
		name  string
		query moderation.ActionListQuery
		want  []string
	}{
		{"subject URI", moderation.ActionListQuery{SubjectURI: first.SubjectURI}, []string{"third", "first"}},
		{"collection", moderation.ActionListQuery{SubjectCollection: moderation.CommentCollection}, []string{"first"}},
		{"action", moderation.ActionListQuery{Action: moderation.ActionRestore}, []string{"second"}},
		{"origin", moderation.ActionListQuery{Origin: "inherited"}, []string{"third", "second"}},
		{"authority", moderation.ActionListQuery{AuthorityDID: second.AuthorityDID}, []string{"second"}},
		{"actor", moderation.ActionListQuery{ActorDID: second.ActorDID}, []string{"third", "second"}},
		{"community without a content row", moderation.ActionListQuery{CommunityDID: second.SubjectCommunityDID}, []string{"third", "second"}},
		{"action ID", moderation.ActionListQuery{ActionID: first.ID}, []string{"first"}},
		{"combined", moderation.ActionListQuery{SubjectURI: first.SubjectURI, ActorDID: second.ActorDID, CommunityDID: second.SubjectCommunityDID, Origin: "inherited", Action: moderation.ActionRemove}, []string{"third"}},
		{"since inclusive", moderation.ActionListQuery{Since: &second.CreatedAt}, []string{"third", "second"}},
		{"until exclusive", moderation.ActionListQuery{Until: &second.CreatedAt}, []string{"first"}},
		{"bounded exact microsecond", moderation.ActionListQuery{Since: &second.CreatedAt, Until: &third.CreatedAt}, []string{"second"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.query.Limit = 10
			assert.Equal(t, test.want, actionLogIDs(listActionLog(t, db, test.query)))
		})
	}
}

func TestModerationActionLogHiddenReasonsAndFullRowMapping(t *testing.T) {
	db := testkit.DB(t)
	base := time.Date(2026, 9, 28, 12, 0, 0, 123456000, time.UTC)
	reasons := []string{actionLogSpam, actionLogHarassment, actionLogRule, actionLogDiscretion, actionLogIllegal, actionLogDoxing}
	rows := make([]moderation.Action, 0, len(reasons)+4)
	for index, reason := range reasons {
		row := actionLogRow(string(rune('a'+index)), base.Add(time.Duration(index)*time.Microsecond))
		row.Reason = reason
		rows = append(rows, row)
	}
	for index, removal := range []moderation.Action{rows[0], rows[4], rows[5]} {
		restore := actionLogRow(string(rune('g'+index)), base.Add(time.Duration(len(reasons)+index)*time.Microsecond))
		restore.Action = moderation.ActionRestore
		restore.ReversesActionID = removal.ID
		restore.Reason = actionLogDiscretion
		rows = append(rows, restore)
	}
	publicRestore := actionLogRow("j", base.Add(9*time.Microsecond))
	publicRestore.Action = moderation.ActionRestore
	publicRestore.ReversesActionID = rows[1].ID
	publicRestore.Reason = actionLogDiscretion
	rows = append(rows, publicRestore)
	rows[7].ScopeKind = "community"
	rows[7].ScopeCommunityDID = fixtures.DID("community-a")
	rows[7].LabelValue = "fixture-label"
	rows[7].CreatedAt = rows[7].CreatedAt.In(time.FixedZone("fixture-offset", 2*60*60))
	for _, row := range rows {
		insertActionLogRow(t, db, row)
	}
	all := listActionLog(t, db, moderation.ActionListQuery{Limit: 20})
	require.Equal(t, []string{"j", "i", "h", "g", "f", "e", "d", "c", "b", "a"}, actionLogIDs(all))
	assert.Equal(t, []string{"j", "g", "d", "c", "b", "a"}, actionLogIDs(listActionLog(t, db, moderation.ActionListQuery{Limit: 20, ExcludeHidden: true})))
	byID := make(map[string]moderation.Action, len(all))
	for _, row := range all {
		byID[row.ID] = row
	}
	for _, test := range []struct {
		id             string
		reversedReason string
	}{
		{"g", actionLogSpam}, {"h", actionLogIllegal}, {"i", actionLogDoxing}, {"j", actionLogHarassment},
	} {
		assert.Equal(t, test.reversedReason, byID[test.id].ReversedActionReason, test.id)
	}
	for _, row := range rows[:6] {
		assert.Empty(t, byID[row.ID].ReversedActionReason, row.ID)
	}
	// No users, posts, comments, or communities were indexed. Compare every
	// stored column, including nullable fields, against its returned value.
	for _, want := range rows {
		if want.ReversesActionID != "" {
			want.ReversedActionReason = byID[want.ReversesActionID].Reason
		}
		want.CreatedAt = want.CreatedAt.UTC()
		// With no indexed content to decide from, every subject is restricted.
		want.SubjectAccess = moderation.SubjectAccessRestricted
		assert.Equal(t, want, byID[want.ID], want.ID)
		assert.Equal(t, time.UTC, byID[want.ID].CreatedAt.Location(), want.ID)
	}
	assert.Equal(t, []string{"h"}, actionLogIDs(listActionLog(t, db, moderation.ActionListQuery{Limit: 20, ActionID: "h", CommunityDID: rows[7].SubjectCommunityDID})))
}

// TestModerationActionLogExcludesLabelActions pins the SQL half of the public
// label exclusion: ExcludeLabelActions omits label and retract-label rows on
// every page and filter, and leaves every other kind in place.
func TestModerationActionLogExcludesLabelActions(t *testing.T) {
	db := testkit.DB(t)
	base := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	removal := actionLogRow("a", base)
	label := actionLogRow("b", base.Add(time.Microsecond))
	label.Action, label.LabelValue, label.Reason = moderation.ActionLabel, moderation.LabelNSFW, actionLogRule
	retraction := actionLogRow("c", base.Add(2*time.Microsecond))
	retraction.Action, retraction.LabelValue, retraction.Reason = moderation.ActionRetractLabel, moderation.LabelNSFW, ""
	retraction.ReversesActionID = label.ID
	restore := actionLogRow("d", base.Add(3*time.Microsecond))
	restore.Action, restore.ReversesActionID, restore.Reason = moderation.ActionRestore, removal.ID, actionLogDiscretion
	for _, row := range []moderation.Action{removal, label, retraction, restore} {
		insertActionLogRow(t, db, row)
	}
	assert.Equal(t, []string{"d", "c", "b", "a"}, actionLogIDs(listActionLog(t, db, moderation.ActionListQuery{Limit: 20})))
	assert.Equal(t, []string{"d", "a"}, actionLogIDs(listActionLog(t, db, moderation.ActionListQuery{Limit: 20, ExcludeLabelActions: true})))
	assert.Equal(t, []string{"a"}, actionLogIDs(listActionLog(t, db, moderation.ActionListQuery{
		Limit: 20, ExcludeLabelActions: true, Before: &moderation.ActionKey{CreatedAt: restore.CreatedAt, ID: restore.ID},
	})))
	for _, kind := range moderation.PublicExcludedActions() {
		assert.Empty(t, listActionLog(t, db, moderation.ActionListQuery{Limit: 20, Action: kind, ExcludeLabelActions: true}), kind)
		assert.Len(t, listActionLog(t, db, moderation.ActionListQuery{Limit: 20, Action: kind}), 1, kind)
	}
	assert.Equal(t, []string{"d", "a"}, actionLogIDs(listActionLog(t, db, moderation.ActionListQuery{
		Limit: 20, ExcludeLabelActions: true, ExcludeHidden: true, SubjectCollection: moderation.CommentCollection,
	})))
}
