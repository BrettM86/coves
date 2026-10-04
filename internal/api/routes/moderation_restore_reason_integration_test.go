//go:build integration

package routes_test

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// restoreReasonSnapshot is everything a refused restore must leave unchanged:
// both action logs, the per-subject public history, the subject state, and
// the stored action and idempotency rows.
type restoreReasonSnapshot struct {
	adminLog, publicLog, publicSubjectLog, subjectState string
	actionRows, idempotencyRows                         int
}

func takeRestoreReasonSnapshot(t *testing.T, f *modlogFixture, uri string) restoreReasonSnapshot {
	t.Helper()
	adminLog := f.list(t, true, f.tokenA, nil)
	modlogPage(t, adminLog, true)
	publicLog := f.list(t, false, "", nil)
	modlogPage(t, publicLog, false)
	publicSubjectLog := f.list(t, false, "", url.Values{"subject": {uri}})
	modlogPage(t, publicSubjectLog, false)
	subjectState := requestSubjectState(t, f.client, f.server.URL, uri, f.tokenA)
	moderationAcceptanceBody(t, subjectState)
	snapshot := restoreReasonSnapshot{
		adminLog: string(adminLog.body), publicLog: string(publicLog.body),
		publicSubjectLog: string(publicSubjectLog.body), subjectState: string(subjectState.body),
	}
	require.NoError(t, f.db.QueryRowContext(t.Context(), `SELECT count(*) FROM moderation_actions`).Scan(&snapshot.actionRows))
	require.NoError(t, f.db.QueryRowContext(t.Context(), `SELECT count(*) FROM moderation_idempotency_keys`).Scan(&snapshot.idempotencyRows))
	return snapshot
}

func restoreReasonRequest(t *testing.T, f *modlogFixture, token, removalID, uri, cid, version, key, reason string) subjectStateHTTPResponse {
	t.Helper()
	return moderationAcceptanceRequest(t, f.client, http.MethodPost, f.server.URL+modlogRestorePath, token, map[string]any{
		"actionId": removalID, "reviewedSubject": map[string]string{"uri": uri, "cid": cid},
		"expectedVersion": version, "idempotencyKey": key, "reason": reason, "privateNote": "restore review note",
	})
}

// A restore with a hidden reason would hide the restore from the public log
// while its `reverses` still named a public removal, so the history for that
// subject would show it as removed after it was restored. Such a restore is
// refused before anything is written.
func TestModerationRestoreRejectsHiddenReasonWithoutChangingHistory(t *testing.T) {
	f := newModlogFixture(t)
	uri, cid := f.comment(t)
	removalID, version := f.remove(t, uri, cid, f.tokenA, modlogSpam, "spam removal")
	before := takeRestoreReasonSnapshot(t, f, uri)
	assert.Equal(t, 1, before.actionRows)
	_, subjectItems := modlogPage(t, f.list(t, false, "", url.Values{"subject": {uri}}), false)
	require.Len(t, subjectItems, 1)
	assert.Equal(t, removalID, modlogID(t, modlogAction(t, subjectItems[0], false)))
	restoreKey := "restore-reason-key"

	for _, reason := range []string{modlogDoxing, modlogIllegal} {
		t.Run(reason, func(t *testing.T) {
			response := restoreReasonRequest(t, f, f.tokenB, removalID, uri, cid, version, restoreKey, reason)
			requireXRPCError(t, response, http.StatusBadRequest, "UnsupportedReason")
			after := takeRestoreReasonSnapshot(t, f, uri)
			assert.Equal(t, before, after, "a refused restore must not change the logs, the subject state or any stored row")
			state := moderationAcceptanceObject(t, moderationAcceptanceBody(t, requestSubjectState(t, f.client, f.server.URL, uri, f.tokenA))["state"])
			assert.Equal(t, version, state["version"])
			assert.Equal(t, "removed", moderationAcceptanceObject(t, state["moderation"])["state"])
		})
	}

	// The refusals consumed no idempotency key: the same key restores with a public reason.
	response := restoreReasonRequest(t, f, f.tokenB, removalID, uri, cid, version, restoreKey, modlogDiscretion)
	require.Equal(t, "applied", moderationAcceptanceBody(t, response)["outcome"])
	_, subjectItems = modlogPage(t, f.list(t, false, "", url.Values{"subject": {uri}}), false)
	require.Len(t, subjectItems, 2)
	restore := modlogAction(t, subjectItems[0], false)
	assert.Equal(t, "restore", restore["action"])
	assert.Equal(t, modlogDiscretion, restore["reason"])
	assert.Equal(t, uri, moderationAcceptanceObject(t, restore["subject"])["uri"])
	assert.Equal(t, removalID, moderationAcceptanceObject(t, restore["reverses"])["actionId"])
	assert.Equal(t, removalID, modlogID(t, modlogAction(t, subjectItems[1], false)))
}

// The idempotent replay of a restore that reverses a hidden removal is read
// back from Postgres stored_result. It must keep the reversed removal's reason,
// or the replayed action would expose the subject the first response hid.
func TestModerationRestoreReplayOfHiddenRemovalKeepsHiddenProjection(t *testing.T) {
	f := newModlogFixture(t)
	uri, cid := f.comment(t)
	removalID, version := f.remove(t, uri, cid, f.tokenA, modlogIllegal, "illegal removal")
	first := restoreReasonRequest(t, f, f.tokenB, removalID, uri, cid, version, "replayed-restore-key", modlogDiscretion)
	require.Equal(t, "applied", moderationAcceptanceBody(t, first)["outcome"])
	var actionRows int
	require.NoError(t, f.db.QueryRowContext(t.Context(), `SELECT count(*) FROM moderation_actions`).Scan(&actionRows))
	require.Equal(t, 2, actionRows)

	replay := restoreReasonRequest(t, f, f.tokenB, removalID, uri, cid, version, "replayed-restore-key", modlogDiscretion)
	replayBody := moderationAcceptanceBody(t, replay)
	assert.Equal(t, string(first.body), string(replay.body), "the replay must return the stored mutation result byte for byte")
	require.NoError(t, f.db.QueryRowContext(t.Context(), `SELECT count(*) FROM moderation_actions`).Scan(&actionRows))
	assert.Equal(t, 2, actionRows, "the replay must not write another action")

	entry := moderationAcceptanceObject(t, replayBody["action"])
	action := moderationAcceptanceObject(t, entry["action"])
	assert.Equal(t, "restore", action["action"])
	assert.Equal(t, modlogDiscretion, action["reason"])
	assert.NotContains(t, action, "subject", "a restore of a hidden removal must not name its subject publicly")
	assert.Equal(t, removalID, moderationAcceptanceObject(t, action["reverses"])["actionId"])
	assert.Equal(t, uri, moderationAcceptanceObject(t, entry["privateSubject"])["uri"])
}
