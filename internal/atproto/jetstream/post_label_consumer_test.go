//go:build integration

package jetstream

import (
	"encoding/json"
	"testing"

	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func postLabelSelfLabel() map[string]interface{} {
	return map[string]interface{}{
		"$type":  "com.atproto.label.defs#selfLabels",
		"values": []interface{}{map[string]interface{}{"val": "nsfw"}},
	}
}

func postLabelAcceptRevision(t *testing.T, f postModerationConsumerFixture, cid string) {
	t.Helper()
	rkey := testkit.TID()
	accepted, err := f.admissions.ApplyAcceptance(t.Context(), posts.ApplyAcceptanceCommand{
		CommunityDID: pv2Community, PostURI: f.uri,
		AcceptanceURI:  "at://" + pv2Community + "/" + posts.AcceptanceCollection + "/" + rkey,
		AcceptanceRkey: rkey, PinnedCID: cid,
		Watermark: posts.CommunityWatermark{Rev: testkit.TID()},
	})
	require.NoError(t, err)
	require.Equal(t, posts.AdmissionApplied, accepted.Outcome)
}

func postLabelView(t *testing.T, f postModerationConsumerFixture, viewerDID string) *posts.PostView {
	t.Helper()
	results, err := f.postService.GetPosts(t.Context(), posts.GetPostsRequest{URIs: []string{f.uri}, ViewerDID: viewerDID})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.NotNil(t, results[0].Post, "post must be visible to %q after the expected acceptance", viewerDID)
	return results[0].Post
}

func postLabelRecordJSON(t *testing.T, record interface{}) string {
	t.Helper()
	encoded, err := json.Marshal(record)
	require.NoError(t, err)
	return string(encoded)
}

func requirePostLabelEditedRecord(t *testing.T, view *posts.PostView, edited map[string]interface{}) {
	t.Helper()
	require.Equal(t, postModerationCIDTwo, view.CID)
	var record map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(postLabelRecordJSON(t, view.Record)), &record))
	assert.Equal(t, edited["title"], record["title"])
	assert.Equal(t, edited["content"], record["content"])
	labels, ok := record["labels"].(map[string]interface{})
	require.True(t, ok, "the served record must retain author self-labels")
	values, ok := labels["values"].([]interface{})
	require.True(t, ok)
	require.Equal(t, []interface{}{map[string]interface{}{"val": "nsfw"}}, values)
}

func TestModerationLabelPostConsumerEditReplayAndSelfLabels(t *testing.T) {
	f := newPostModerationConsumerFixture(t)
	// The fixture initially indexes an unlabelled post. An author update adds
	// real self-labels, then the community accepts that revision before the
	// moderator acts; the second update is the edit under the active decision.
	initial := postModerationRecord(postModerationCIDOne)
	initial["labels"] = postLabelSelfLabel()
	initialUpdate := pv2Event(pv2Author, "update", f.rkey, f.revs[1], postModerationCIDOne, f.createdAt+1_000_000, initial)
	require.NoError(t, f.consumer.HandleEvent(t.Context(), initialUpdate))
	postLabelAcceptRevision(t, f, postModerationCIDOne)
	initialView := postLabelView(t, f, "")
	require.Nil(t, initialView.Moderation)
	require.Contains(t, postLabelRecordJSON(t, initialView.Record), `"labels"`, "the fixture must index an author self-label before admin labelling")
	beforeApply := postLabelRecordJSON(t, initialView.Record)
	actor := fixtures.DID("labelconsumeradmin")
	initialSubject := moderation.StrongRef{URI: f.uri, CID: postModerationCIDOne}
	label, err := f.moderator.LabelContent(t.Context(), actor, moderation.LabelContentRequest{
		Subject: initialSubject, LabelValue: moderation.LabelNSFW, ExpectedVersion: "v0", IdempotencyKey: "consumer-label",
	})
	require.NoError(t, err)
	require.NotNil(t, label.Action)
	require.Equal(t, moderation.OutcomeApplied, label.Outcome)
	labelID := label.Action.ID
	assert.Equal(t, beforeApply, postLabelRecordJSON(t, postLabelView(t, f, "").Record),
		"applying an admin label must not change the served author record")

	edited := postModerationRecord(postModerationCIDOne)
	edited["title"] = "edited author title"
	edited["content"] = "new author text with unchanged self-label"
	edited["labels"] = postLabelSelfLabel()
	update := pv2Event(pv2Author, "update", f.rkey, f.revs[2], postModerationCIDTwo, f.createdAt+2_000_000, edited)
	require.NoError(t, f.consumer.HandleEvent(t.Context(), update))
	indexed, err := f.postRepository.GetRawIndexedRow(t.Context(), f.uri)
	require.NoError(t, err)
	require.Equal(t, postModerationCIDTwo, indexed.CID)
	state, err := f.moderator.GetSubjectState(t.Context(), f.uri)
	require.NoError(t, err)
	require.Len(t, state.LocalLabels, 1)
	assert.Equal(t, labelID, state.LocalLabels[0].Action.ActionID, "the decision is URI-scoped across the edit")
	assert.Equal(t, "v1", state.Version)
	var admission string
	require.NoError(t, f.db.QueryRowContext(t.Context(), `SELECT status FROM community_post_admissions WHERE post_uri = $1`, f.uri).Scan(&admission))
	require.Equal(t, "pending_reacceptance", admission, "the author sees the edit before public reacceptance")
	authorView := postLabelView(t, f, pv2Author)
	assert.Equal(t, &posts.ModerationView{State: "clear", ContentLabels: []posts.ContentLabelView{{
		Value: "nsfw", Sources: []posts.ModerationSourceView{{AuthorityDID: fixtures.InstanceDID(), Scope: posts.ModerationScopeView{Kind: "instance"}}},
	}}}, authorView.Moderation)
	requirePostLabelEditedRecord(t, authorView, edited)
	postLabelAcceptRevision(t, f, postModerationCIDTwo)
	publicView := postLabelView(t, f, "")
	assert.Equal(t, &posts.ModerationView{State: "clear", ContentLabels: []posts.ContentLabelView{{
		Value: "nsfw", Sources: []posts.ModerationSourceView{{AuthorityDID: fixtures.InstanceDID(), Scope: posts.ModerationScopeView{Kind: "instance"}}},
	}}}, publicView.Moderation)
	requirePostLabelEditedRecord(t, publicView, edited)

	// A duplicate delivery of the exact update must not replace the active
	// action, rewrite the indexed record, or manufacture a second label action.
	var rowBefore, rowAfter string
	require.NoError(t, f.db.QueryRowContext(t.Context(), `SELECT row_to_json(p)::text FROM posts p WHERE uri = $1`, f.uri).Scan(&rowBefore))
	require.NoError(t, f.consumer.HandleEvent(t.Context(), update))
	require.NoError(t, f.db.QueryRowContext(t.Context(), `SELECT row_to_json(p)::text FROM posts p WHERE uri = $1`, f.uri).Scan(&rowAfter))
	assert.Equal(t, rowBefore, rowAfter, "a duplicate update must not rewrite any indexed post fields")
	state, err = f.moderator.GetSubjectState(t.Context(), f.uri)
	require.NoError(t, err)
	require.Equal(t, "v1", state.Version)
	require.Len(t, state.LocalLabels, 1)
	assert.Equal(t, labelID, state.LocalLabels[0].Action.ActionID)
	assert.Equal(t, 1, countRows(t, f.db, `SELECT count(*) FROM moderation_actions WHERE subject_uri = $1 AND action = 'label'`, f.uri))
	assert.Equal(t, postModerationCIDTwo, postLabelView(t, f, "").CID)
	beforeRetract := postLabelRecordJSON(t, postLabelView(t, f, "").Record)

	withoutReview, err := f.moderator.RetractContentLabel(t.Context(), actor, moderation.RetractContentLabelRequest{
		ActionID: labelID, ExpectedVersion: "v1", IdempotencyKey: "consumer-no-review",
	})
	assert.ErrorIs(t, err, moderation.ErrInvalidRequest)
	assert.Nil(t, withoutReview)
	withStaleCID, err := f.moderator.RetractContentLabel(t.Context(), actor, moderation.RetractContentLabelRequest{
		ActionID: labelID, ReviewedSubject: &initialSubject, ExpectedVersion: "v1", IdempotencyKey: "consumer-stale-review",
	})
	assert.ErrorIs(t, err, moderation.ErrContentChanged)
	assert.Nil(t, withStaleCID)
	currentSubject := moderation.StrongRef{URI: f.uri, CID: postModerationCIDTwo}
	retracted, err := f.moderator.RetractContentLabel(t.Context(), actor, moderation.RetractContentLabelRequest{
		ActionID: labelID, ReviewedSubject: &currentSubject, ExpectedVersion: "v1", IdempotencyKey: "consumer-retract",
	})
	require.NoError(t, err)
	require.Equal(t, moderation.OutcomeApplied, retracted.Outcome)
	assert.Empty(t, retracted.State.LocalLabels)
	view := postLabelView(t, f, "")
	assert.Nil(t, view.Moderation, "retraction removes only the admin label")
	assert.Equal(t, beforeRetract, postLabelRecordJSON(t, view.Record),
		"retracting an admin label must not change the served author record")
	requirePostLabelEditedRecord(t, view, edited)
	assert.Equal(t, 1, countRows(t, f.db, `SELECT count(*) FROM moderation_actions WHERE subject_uri = $1 AND action = 'label'`, f.uri))
	// newPostModerationConsumerFixture has no PDS URL parameter: its consumer
	// and moderation service use only Postgres, so there is no PDS seam to
	// redirect to a recording httptest server in this fixture.
}
