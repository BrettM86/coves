//go:build integration

package post_test

import (
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

func admissionSnapshot(t *testing.T, db *sql.DB, communityDID, postURI string) string {
	t.Helper()
	var row string
	require.NoError(t, db.QueryRowContext(t.Context(), `
		SELECT row_to_json(a)::text FROM community_post_admissions a
		WHERE community_did = $1 AND post_uri = $2
	`, communityDID, postURI).Scan(&row))
	return row
}

func TestGetStatus_InstancePostRemovalPreservesCommunityAcceptance(t *testing.T) {
	db := testkit.DB(t)
	stack := newStatusStack(db)
	subject := newStatusSubject(t, db) // fixtures.Community sets hosted_by_did to this instance.
	const postCID = "bafyreistatusseed"
	rkey := testkit.TID()
	acceptanceURI := "at://" + subject.CommunityDID + "/" + posts.AcceptanceCollection + "/" + rkey
	_, err := stack.admissions.UpsertPending(t.Context(), posts.UpsertPendingCommand{
		CommunityDID: subject.CommunityDID, PostURI: subject.PostURI, EvaluatedCID: postCID,
	})
	require.NoError(t, err)
	result, err := stack.admissions.ApplyAcceptance(t.Context(), posts.ApplyAcceptanceCommand{
		CommunityDID: subject.CommunityDID, PostURI: subject.PostURI,
		AcceptanceURI: acceptanceURI, AcceptanceRkey: rkey, PinnedCID: postCID,
		Watermark: posts.CommunityWatermark{Rev: testkit.TID()},
	})
	require.NoError(t, err)
	require.Equal(t, posts.AdmissionApplied, result.Outcome)
	var hostingDID string
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT hosted_by_did FROM communities WHERE did = $1`, subject.CommunityDID).Scan(&hostingDID))
	require.Equal(t, fixtures.InstanceDID(), hostingDID)
	before := admissionSnapshot(t, db, subject.CommunityDID, subject.PostURI)
	checkAcceptance := func(t *testing.T) {
		t.Helper()
		assert.Equal(t, before, admissionSnapshot(t, db, subject.CommunityDID, subject.PostURI),
			"instance moderation must not alter any admission column, including record URI, CID, watermarks and timestamps")
		body := decodeStatus(t, getStatus(t, stack.handler, subject.PostURI, subject.CommunityDID))
		assert.Equal(t, "accepted", body["status"])
		assert.Equal(t, acceptanceURI, body["acceptanceUri"])
		assert.NotContains(t, body, "decisionCode", "instance removal is not a community removal")
	}
	checkAcceptance(t)
	postRepository := postgres.NewPostRepository(db)
	commentRepository := postgres.NewCommentRepository(db)
	moderationService := moderation.NewService(
		moderation.NewRepositorySubjectReader(postRepository, commentRepository),
		postgres.NewModerationRepository(db),
		moderation.Config{InstanceDID: fixtures.InstanceDID(), IdempotencyRetention: 24 * time.Hour, MaxLiveIdempotencyKeys: 1000},
	)
	ref := moderation.StrongRef{URI: subject.PostURI, CID: postCID}
	removed, err := moderationService.RemoveContent(t.Context(), fixtures.DID(testkit.UniqueIDWithPrefix(t, "statusadmina")), moderation.RemoveContentRequest{
		Subject: ref, ExpectedVersion: "v0", IdempotencyKey: "remove-status-post", Reason: "social.coves.moderation.defs#reasonSpam",
	})
	require.NoError(t, err)
	require.Equal(t, moderation.OutcomeApplied, removed.Outcome)
	checkAcceptance(t)
	restored, err := moderationService.RestoreContent(t.Context(), fixtures.DID(testkit.UniqueIDWithPrefix(t, "statusadminb")), moderation.RestoreContentRequest{
		ActionID: removed.Action.ID, ReviewedSubject: &ref, ExpectedVersion: removed.State.Version,
		IdempotencyKey: "restore-status-post", Reason: "social.coves.moderation.defs#reasonModeratorDiscretion",
	})
	require.NoError(t, err)
	require.Equal(t, moderation.OutcomeApplied, restored.Outcome)
	checkAcceptance(t)
}
