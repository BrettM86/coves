//go:build integration

package routes_test

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"Coves/internal/core/moderation"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	modlogLabelPath   = "/xrpc/social.coves.moderation.labelContent"
	modlogRetractPath = "/xrpc/social.coves.moderation.retractContentLabel"
	// modlogRuleViolation is a public reason that labelContent accepts.
	modlogRuleViolation = "social.coves.moderation.defs#reasonRuleViolation"
)

func (f *modlogFixture) labelPost(t *testing.T) moderation.StrongRef {
	t.Helper()
	rkey := testkit.TID()
	subject := moderation.StrongRef{
		URI: "at://" + f.authorDID + "/" + moderation.PostV2Collection + "/" + rkey,
		CID: f.postCID,
	}
	_, err := f.db.ExecContext(t.Context(), `
		INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, content, created_at)
		VALUES ($1, $2, $3, $4, $5, 'modlog labelled post', 'indexed body', $6)
	`, subject.URI, subject.CID, rkey, f.authorDID, f.communityDID, time.Now())
	require.NoError(t, err)
	// Accepted, so the post is public and the admin log names it as the
	// action's subject rather than as a restricted privateSubject.
	_, err = f.db.ExecContext(t.Context(), `
		INSERT INTO community_post_admissions
			(community_did, post_uri, status, accepted_cid, evaluated_cid, last_community_rev, last_community_op_rank, created_at, updated_at)
		VALUES ($1, $2, 'accepted', $3, $3, '3lqqqqqqqqqq2', 1, NOW(), NOW())
	`, f.communityDID, subject.URI, subject.CID)
	require.NoError(t, err)
	return subject
}

func (f *modlogFixture) label(t *testing.T, subject moderation.StrongRef, reason, note string) (string, string) {
	t.Helper()
	response := moderationAcceptanceRequest(t, f.client, http.MethodPost, f.server.URL+modlogLabelPath, f.tokenA, map[string]any{
		"subject": map[string]string{"uri": subject.URI, "cid": subject.CID}, "labelValue": "nsfw",
		"expectedVersion": "v0", "idempotencyKey": testkit.UniqueIDWithPrefix(t, "label"),
		"reason": reason, "privateNote": note,
	})
	body := moderationAcceptanceBody(t, response)
	require.Equal(t, "applied", body["outcome"])
	action := modlogAction(t, body["action"], true)
	require.Equal(t, "label", action["action"])
	version, ok := moderationAcceptanceObject(t, body["state"])["version"].(string)
	require.True(t, ok)
	return modlogID(t, action), version
}

func (f *modlogFixture) retractLabel(t *testing.T, subject moderation.StrongRef, labelID, version, reason, note string) string {
	t.Helper()
	response := moderationAcceptanceRequest(t, f.client, http.MethodPost, f.server.URL+modlogRetractPath, f.tokenB, map[string]any{
		"actionId": labelID, "reviewedSubject": map[string]string{"uri": subject.URI, "cid": subject.CID},
		"expectedVersion": version, "idempotencyKey": testkit.UniqueIDWithPrefix(t, "retract"),
		"reason": reason, "privateNote": note,
	})
	body := moderationAcceptanceBody(t, response)
	require.Equal(t, "applied", body["outcome"])
	action := modlogAction(t, body["action"], true)
	require.Equal(t, "retract-label", action["action"])
	return modlogID(t, action)
}

// TestModerationLabelModlogPublicLogOmitsLabelActions pins the 2026-09-30
// decision: NSFW label applies and retractions are left out of the public
// modlog under every filter and cursor page; the admin log keeps them.
func TestModerationLabelModlogPublicLogOmitsLabelActions(t *testing.T) {
	f := newModlogFixture(t)
	firstURI, firstCID := f.comment(t)
	secondURI, secondCID := f.comment(t)
	firstRemove, _ := f.remove(t, firstURI, firstCID, f.tokenA, modlogSpam, "first removal note")
	subject := f.labelPost(t)
	labelID, version := f.label(t, subject, modlogRuleViolation, "private label review")
	retractID := f.retractLabel(t, subject, labelID, version, "", "private retraction review")
	secondRemove, _ := f.remove(t, secondURI, secondCID, f.tokenB, modlogSpam, "second removal note")
	removals := []string{secondRemove, firstRemove}

	publicIDs := func(query url.Values) []string {
		t.Helper()
		response := f.list(t, false, "", query)
		require.Equal(t, http.StatusOK, response.status, "%s", query.Encode())
		for _, leaked := range []string{subject.URI, labelID, retractID, "private label review", "private retraction review"} {
			assert.NotContains(t, string(response.body), leaked, "%s", query.Encode())
		}
		_, items := modlogPage(t, response, false)
		ids := make([]string, 0, len(items))
		for _, item := range items {
			action := modlogAction(t, item, false)
			assert.NotContains(t, []any{"label", "retract-label"}, action["action"], "%s", query.Encode())
			ids = append(ids, modlogID(t, action))
		}
		return ids
	}
	window := url.Values{
		"since": {time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)},
		"until": {time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)},
	}
	for _, test := range []struct {
		query url.Values
		want  []string
	}{
		{nil, removals},
		{url.Values{"subject": {subject.URI}}, []string{}},
		{url.Values{"collection": {moderation.PostV2Collection}}, []string{}},
		{url.Values{"community": {f.communityDID}}, removals},
		{url.Values{"actor": {f.adminA}}, []string{firstRemove}},
		{url.Values{"actor": {f.adminB}}, []string{secondRemove}},
		{url.Values{"authority": {fixtures.InstanceDID()}}, removals},
		{url.Values{"origin": {"local"}}, removals},
		{window, removals},
	} {
		assert.Equal(t, test.want, publicIDs(test.query), "%s", test.query.Encode())
	}
	for _, kind := range []string{"label", "retract-label"} {
		requireXRPCError(t, f.list(t, false, "", url.Values{"action": {kind}}), http.StatusBadRequest, "InvalidRequest")
	}

	var walked []string
	pages := modlogWalk(t, f, url.Values{"limit": {"1"}})
	for _, page := range pages {
		for _, leaked := range []string{subject.URI, labelID, retractID} {
			assert.NotContains(t, string(page), leaked)
		}
		_, items := modlogPage(t, subjectStateHTTPResponse{status: http.StatusOK, body: page}, false)
		for _, item := range items {
			walked = append(walked, modlogID(t, modlogAction(t, item, false)))
		}
	}
	assert.Equal(t, removals, walked)

	want := map[string]struct{ kind, note, actor string }{
		labelID:   {"label", "private label review", f.adminA},
		retractID: {"retract-label", "private retraction review", f.adminB},
	}
	_, admin := modlogPage(t, f.list(t, true, f.tokenA, url.Values{"subject": {subject.URI}}), true)
	require.Len(t, admin, 2)
	for _, item := range admin {
		entry := moderationAcceptanceObject(t, item)
		action := modlogAction(t, item, true)
		expected, ok := want[modlogID(t, action)]
		require.True(t, ok)
		assert.Equal(t, expected.kind, action["action"])
		assert.Equal(t, "nsfw", action["labelValue"])
		assert.Equal(t, subject.URI, moderationAcceptanceObject(t, action["subject"])["uri"])
		assert.Equal(t, expected.actor, entry["actorDid"])
		assert.Equal(t, expected.note, entry["privateNote"])
		if expected.kind == "label" {
			assert.Equal(t, modlogRuleViolation, action["reason"])
		} else {
			assert.Equal(t, labelID, moderationAcceptanceObject(t, action["reverses"])["actionId"])
		}
	}
	for kind, id := range map[string]string{"label": labelID, "retract-label": retractID} {
		_, filtered := modlogPage(t, f.list(t, true, f.tokenA, url.Values{"action": {kind}}), true)
		require.Len(t, filtered, 1, kind)
		assert.Equal(t, id, modlogID(t, modlogAction(t, filtered[0], true)))
	}
	_, everything := modlogPage(t, f.list(t, true, f.tokenA, nil), true)
	assert.Len(t, everything, 4)
}
