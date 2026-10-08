//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"Coves/internal/core/notifications"
	"Coves/internal/core/posts"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedModerationDecision records one admin moderation decision on subjectURI in
// main's moderation tables: an action row, then the decision it made active. An
// empty scopeCommunityDID is an instance-scope decision; kind is 'removal' or
// 'label' (labels carry the value nsfw).
func seedModerationDecision(t *testing.T, db *sql.DB, subjectURI, kind, scopeCommunityDID string, active bool) {
	t.Helper()
	scopeKind, authority := "instance", "did:plc:notificationinstance"
	var scopeCommunity any
	if scopeCommunityDID != "" {
		scopeKind, authority, scopeCommunity = "community", scopeCommunityDID, scopeCommunityDID
	}
	action, value := "remove", any(nil)
	if kind == "label" {
		action, value = "label", "nsfw"
	}
	actionID := "notification-moderation-" + testkit.TID()
	_, err := db.ExecContext(context.Background(), `INSERT INTO moderation_actions
		(id, actor_did, authority_did, scope_kind, scope_community_did, subject_uri, subject_collection,
		 action, label_value, origin, created_at)
		VALUES ($1, 'did:plc:notificationmoderator', $2, $3, $4, $5, $6, $7, $8, 'local', NOW())`,
		actionID, authority, scopeKind, scopeCommunity, subjectURI, strings.Split(subjectURI, "/")[3], action, value)
	require.NoError(t, err)
	_, err = db.ExecContext(context.Background(), `INSERT INTO moderation_decisions
		(authority_did, scope_kind, scope_community_did, subject_uri, kind, value, active_action_id, active)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		authority, scopeKind, scopeCommunity, subjectURI, kind, value, actionID, active)
	require.NoError(t, err)
}

func TestNotificationLookups_ReferenceStates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	label := testkit.UniqueID(t)
	community := visibilityCommunity(t, db, label+"a")
	otherCommunity := visibilityCommunity(t, db, label+"b")
	author := "did:plc:referenceauthor" + label
	createdAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)

	rows := []struct {
		name                  string
		status                posts.AdmissionStatus
		deleted               bool
		otherCommunityRemoved bool
		mismatchedCID         bool
		removal               string // "instance", "community", "both", "inactive", "label"
		want                  notifications.ReferenceState
	}{
		{name: "deleted_post", deleted: true, want: notifications.ReferenceDeleted},
		{name: "removed_post", status: posts.AdmissionStatusRemoved, want: notifications.ReferenceRemovedByModerator},
		{name: "removed_and_deleted_post", status: posts.AdmissionStatusRemoved, deleted: true, want: notifications.ReferenceRemovedByModerator},
		{name: "other_community_removed", status: posts.AdmissionStatusAccepted, otherCommunityRemoved: true, want: notifications.ReferenceLive},
		{name: "pending", status: posts.AdmissionStatusPending, want: notifications.ReferenceLive},
		{name: "rejected", status: posts.AdmissionStatusRejected, want: notifications.ReferenceLive},
		{name: "pending_reacceptance", status: posts.AdmissionStatusPendingReacceptance, want: notifications.ReferenceLive},
		{name: "accepted", status: posts.AdmissionStatusAccepted, want: notifications.ReferenceLive},
		{name: "accepted_cid_mismatch", status: posts.AdmissionStatusAccepted, mismatchedCID: true, want: notifications.ReferenceLive},
		{name: "no_admission", want: notifications.ReferenceLive},
		{name: "instance_removed_post", status: posts.AdmissionStatusAccepted, removal: "instance", want: notifications.ReferenceRemovedByServerAdmin},
		{name: "community_scope_removed_post", status: posts.AdmissionStatusAccepted, removal: "community", want: notifications.ReferenceRemovedByModerator},
		{name: "instance_and_community_scope_removed_post", status: posts.AdmissionStatusAccepted, removal: "both", want: notifications.ReferenceRemovedByServerAdmin},
		{name: "instance_removed_and_community_removed_post", status: posts.AdmissionStatusRemoved, removal: "instance", want: notifications.ReferenceRemovedByServerAdmin},
		{name: "inactive_removal_post", status: posts.AdmissionStatusAccepted, removal: "inactive", want: notifications.ReferenceLive},
		{name: "label_only_post", status: posts.AdmissionStatusAccepted, removal: "label", want: notifications.ReferenceLive},
	}
	moderate := func(uri, removal string) {
		t.Helper()
		switch removal {
		case "instance":
			seedModerationDecision(t, db, uri, "removal", "", true)
		case "community":
			seedModerationDecision(t, db, uri, "removal", community, true)
		case "both":
			seedModerationDecision(t, db, uri, "removal", "", true)
			seedModerationDecision(t, db, uri, "removal", community, true)
		case "inactive":
			seedModerationDecision(t, db, uri, "removal", "", false)
		case "label":
			seedModerationDecision(t, db, uri, "label", "", true)
		}
	}
	type expectation struct {
		name, uri string
		want      notifications.ReferenceState
	}
	var expected []expectation
	var uris []string
	for _, row := range rows {
		uri := seedVisibilityPost(t, db, community, author, row.name, row.name, createdAt)
		if row.status != "" {
			if row.mismatchedCID {
				seedVisibilityAdmissionDriftedCID(t, db, community, uri)
			} else {
				seedVisibilityAdmission(t, db, community, uri, row.status, "", "")
			}
		}
		if row.otherCommunityRemoved {
			seedVisibilityAdmission(t, db, otherCommunity, uri, posts.AdmissionStatusRemoved, "", "")
		}
		if row.deleted {
			_, err := db.ExecContext(ctx, `UPDATE posts SET deleted_at = NOW() WHERE uri = $1`, uri)
			require.NoError(t, err)
		}
		moderate(uri, row.removal)
		uris = append(uris, uri)
		expected = append(expected, expectation{row.name, uri, row.want})
	}
	commenter := "did:plc:referencecommenter" + label
	for _, row := range []struct {
		name    string
		deleted bool
		removal string
		want    notifications.ReferenceState
	}{
		{"deleted_comment", true, "", notifications.ReferenceDeleted},
		{"live_comment", false, "", notifications.ReferenceLive},
		{"instance_removed_comment", false, "instance", notifications.ReferenceRemovedByServerAdmin},
		{"community_scope_removed_comment", false, "community", notifications.ReferenceRemovedByModerator},
		{"inactive_removal_comment", false, "inactive", notifications.ReferenceLive},
		{"label_only_comment", false, "label", notifications.ReferenceLive},
	} {
		uri := seedActorComment(t, db, commenter, uris[0], row.name, createdAt)
		if row.deleted {
			_, err := db.ExecContext(ctx, `UPDATE comments SET deleted_at = NOW() WHERE uri = $1`, uri)
			require.NoError(t, err)
		}
		moderate(uri, row.removal)
		uris = append(uris, uri)
		expected = append(expected, expectation{row.name, uri, row.want})
	}
	missingURI := postV2URI(author, "unindexed")
	uris = append(uris, missingURI)
	expected = append(expected, expectation{"unindexed", missingURI, notifications.ReferenceUnindexed})
	missingCommentURI := "at://" + commenter + "/social.coves.community.comment/unindexed"
	uris = append(uris, missingCommentURI)
	expected = append(expected, expectation{"unindexed_comment", missingCommentURI, notifications.ReferenceUnindexed})
	// A decision can predate indexing (or outlive a delete): the decision alone
	// classifies the URI, by scope.
	for _, row := range []struct {
		name, uri, removal string
		want               notifications.ReferenceState
	}{
		{"decision_only_instance_post", postV2URI(author, "decisiononlyinstance"), "instance", notifications.ReferenceRemovedByServerAdmin},
		{"decision_only_community_scope_post", postV2URI(author, "decisiononlycommunity"), "community", notifications.ReferenceRemovedByModerator},
		{"decision_only_instance_comment", "at://" + commenter + "/social.coves.community.comment/decisiononlyinstance", "instance", notifications.ReferenceRemovedByServerAdmin},
		{"decision_only_community_scope_comment", "at://" + commenter + "/social.coves.community.comment/decisiononlycommunity", "community", notifications.ReferenceRemovedByModerator},
		{"decision_only_inactive_post", postV2URI(author, "decisiononlyinactive"), "inactive", notifications.ReferenceUnindexed},
		{"decision_only_label_post", postV2URI(author, "decisiononlylabel"), "label", notifications.ReferenceUnindexed},
	} {
		moderate(row.uri, row.removal)
		uris = append(uris, row.uri)
		expected = append(expected, expectation{row.name, row.uri, row.want})
	}

	transaction, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer transaction.Rollback()
	states, err := NewNotificationRepository(db).LookupsTx(transaction).ReferenceStates(ctx, uris)
	require.NoError(t, err)
	for _, row := range expected {
		t.Run(row.name, func(t *testing.T) {
			assert.Equal(t, row.want, states[row.uri], "reference state for %s", row.uri)
		})
	}
}
