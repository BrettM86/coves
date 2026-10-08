//go:build integration

package notification_test

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"Coves/internal/api/handlers/notification"
	"Coves/internal/api/middleware"
	"Coves/internal/core/notifications"
	"Coves/internal/core/posts"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func preferenceHandlerUsers(t *testing.T, db *sql.DB) (string, string) {
	t.Helper()
	id := testkit.UniqueID(t)
	caller, other := "did:plc:prefcaller"+id, "did:plc:prefother"+id
	for _, user := range []struct{ did, handle string }{
		{caller, "prefcaller" + id + ".test"},
		{other, "prefother" + id + ".test"},
	} {
		_, err := db.Exec(`INSERT INTO users (did, handle, pds_url) VALUES ($1, $2, $3)`, user.did, user.handle, "https://pds.test")
		require.NoError(t, err)
	}
	return caller, other
}

func preferenceHandlerState(t *testing.T, db *sql.DB, did string) []string {
	t.Helper()
	var disabled []string
	require.NoError(t, db.QueryRow(`SELECT disabled_reasons FROM notification_state WHERE did = $1`, did).Scan(pq.Array(&disabled)))
	sort.Strings(disabled)
	return disabled
}

func putPreferencesRequest(did, body string) (*httptest.ResponseRecorder, *http.Request) {
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.notification.putPreferences", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(middleware.SetTestUserDID(req.Context(), did))
	return httptest.NewRecorder(), req
}

func TestGetPreferences_ReturnsCallersPreferences(t *testing.T) {
	db := testkit.DB(t)
	caller, other := preferenceHandlerUsers(t, db)
	for _, state := range []struct {
		did      string
		disabled []string
	}{
		{caller, []string{"postReply"}},
		{other, []string{"mention"}},
	} {
		_, err := db.Exec(`INSERT INTO notification_state (did, disabled_reasons) VALUES ($1, $2)`, state.did, pq.Array(state.disabled))
		require.NoError(t, err)
	}
	repo := postgres.NewNotificationRepository(db)
	handler := notification.NewPreferencesHandler(notifications.NewPreferencesService(repo.(notifications.PreferencesRepository)))
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.notification.getPreferences", nil)
	req = req.WithContext(middleware.SetTestUserDID(req.Context(), caller))
	rec := httptest.NewRecorder()
	handler.HandleGetPreferences(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "getPreferences response: %s", rec.Body.String())
	require.JSONEq(t, `{"postReply":false,"commentReply":true,"mention":true,"upvote":true}`, rec.Body.String())
}

func TestPutPreferences_ReturnsFullPreferencesForCaller(t *testing.T) {
	db := testkit.DB(t)
	caller, other := preferenceHandlerUsers(t, db)
	_, err := db.Exec(`INSERT INTO notification_state (did, disabled_reasons) VALUES ($1, $2)`, other, pq.Array([]string{"upvote"}))
	require.NoError(t, err)
	repo := postgres.NewNotificationRepository(db)
	handler := notification.NewPreferencesHandler(notifications.NewPreferencesService(repo.(notifications.PreferencesRepository)))
	rec, req := putPreferencesRequest(caller, `{"commentReply":false}`)
	handler.HandlePutPreferences(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "putPreferences response: %s", rec.Body.String())
	require.JSONEq(t, `{"postReply":true,"commentReply":false,"mention":true,"upvote":true}`, rec.Body.String())
	require.Equal(t, []string{"commentReply"}, preferenceHandlerState(t, db, caller))
	require.Equal(t, []string{"upvote"}, preferenceHandlerState(t, db, other))
}

func TestPutPreferences_RejectsNonBooleanValues(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"string false", `{"mention":"false"}`},
		{"number", `{"mention":1}`},
		{"null", `{"mention":null}`},
		{"mixed valid and invalid", `{"mention":false,"upvote":"no"}`},
		{"truncated JSON", `{"mention":fal`},
		{"null body", `null`},
		{"trailing data", `{"mention":false}{"upvote":false}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testkit.DB(t)
			caller, _ := preferenceHandlerUsers(t, db)
			_, err := db.Exec(`INSERT INTO notification_state (did, disabled_reasons) VALUES ($1, $2)`, caller, pq.Array([]string{"upvote"}))
			require.NoError(t, err)
			repo := postgres.NewNotificationRepository(db)
			handler := notification.NewPreferencesHandler(notifications.NewPreferencesService(repo.(notifications.PreferencesRepository)))
			rec, req := putPreferencesRequest(caller, tc.body)
			handler.HandlePutPreferences(rec, req)

			require.Equal(t, http.StatusBadRequest, rec.Code, "putPreferences response: %s", rec.Body.String())
			var response map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
			require.Equal(t, "InvalidRequest", response["error"])
			require.Equal(t, []string{"upvote"}, preferenceHandlerState(t, db, caller))
		})
	}
}

func TestPutPreferences_RejectsOverLimitBodyAsPayloadTooLarge(t *testing.T) {
	db := testkit.DB(t)
	caller, _ := preferenceHandlerUsers(t, db)
	_, err := db.Exec(`INSERT INTO notification_state (did, disabled_reasons) VALUES ($1, $2)`, caller, pq.Array([]string{"upvote"}))
	require.NoError(t, err)
	repo := postgres.NewNotificationRepository(db)
	handler := notification.NewPreferencesHandler(notifications.NewPreferencesService(repo.(notifications.PreferencesRepository)))
	rec, req := putPreferencesRequest(caller, `{"mention":false,"padding":"`+strings.Repeat("x", 5000)+`"}`)
	handler.HandlePutPreferences(rec, req)

	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, "putPreferences response: %s", rec.Body.String())
	var response map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
	require.Equal(t, "PayloadTooLarge", response["error"])
	require.Equal(t, []string{"upvote"}, preferenceHandlerState(t, db, caller))
}

func TestPutPreferences_UnindexedCallerGetsAccountNotIndexed(t *testing.T) {
	db := testkit.DB(t)
	caller := "did:plc:prefunindexed" + testkit.UniqueID(t)
	repo := postgres.NewNotificationRepository(db)
	handler := notification.NewPreferencesHandler(notifications.NewPreferencesService(repo.(notifications.PreferencesRepository)))
	for _, body := range []string{`{}`, `{"mention":false}`} {
		rec, req := putPreferencesRequest(caller, body)
		handler.HandlePutPreferences(rec, req)

		require.Equal(t, http.StatusBadRequest, rec.Code, "putPreferences %s response: %s", body, rec.Body.String())
		var response map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
		require.Equal(t, "AccountNotIndexed", response["error"], "putPreferences %s", body)
	}
	var stateRows int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM notification_state WHERE did = $1`, caller).Scan(&stateRows))
	require.Zero(t, stateRows)
}

func TestNotificationPreferences_DisablingUpvoteHidesGroupsFromUnreadCount(t *testing.T) {
	db := testkit.DB(t)
	caller, other := preferenceHandlerUsers(t, db)
	id := testkit.UniqueID(t)
	base := time.Date(2026, time.September, 30, 12, 0, 0, 0, time.UTC).Truncate(time.Microsecond)
	_, err := db.Exec(`INSERT INTO notification_state (did, seen_at) VALUES ($1, $2)`, caller, base.Add(-time.Second))
	require.NoError(t, err)
	community := "did:plc:prefcommunity" + id
	_, err = db.Exec(`INSERT INTO communities
		(did, handle, name, owner_did, created_by_did, hosted_by_did, created_at)
		VALUES ($1, $2, 'preferences', $3, $3, $3, $4)`, community, "pref"+id+".coves.social", caller, base)
	require.NoError(t, err)
	root := "at://" + caller + "/social.coves.community.postv2/" + id
	rootCID := "bafyprefpost" + id
	_, err = db.Exec(`INSERT INTO posts
		(uri, cid, rkey, author_did, community_did, title, created_at, score, upvote_count, downvote_count)
		VALUES ($1, $2, $3, $4, $5, 'preferences root', $6, 1, 1, 0)`, root, rootCID, id, caller, community, base)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO community_post_admissions
		(community_did, post_uri, status, acceptance_uri, acceptance_rkey, accepted_cid, evaluated_cid,
		 last_community_rev, last_community_op_rank, created_at, updated_at)
		VALUES ($1, $2, 'accepted', $3, $4, $5, $5, '3lqqqqqqqqqq2', $7, $6, $6)`,
		community, root, "at://"+community+"/social.coves.community.acceptance/"+id, id, rootCID, base, int16(posts.CommunityOpPut))
	require.NoError(t, err)
	for _, suffix := range []string{"subject", "reply"} {
		rkey := id + suffix
		parent := root
		if suffix == "reply" {
			parent = "at://" + other + "/social.coves.community.comment/" + id + "subject"
		}
		_, err := db.Exec(`INSERT INTO comments
			(uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid, content, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $6, 'reply', $8)`,
			"at://"+other+"/social.coves.community.comment/"+rkey, "bafyprefcomment"+rkey, rkey, other, root, rootCID, parent, base)
		require.NoError(t, err)
	}
	subject := "at://" + other + "/social.coves.community.comment/" + id + "subject"
	reply := "at://" + other + "/social.coves.community.comment/" + id + "reply"
	_, err = db.Exec(`INSERT INTO notifications
		(recipient_did, reason, record_uri, record_cid, actor_did, subject_uri, root_post_uri, record_created_at, sort_at)
		VALUES ($1, 'commentReply', $2, $3, $4, $5, $6, $7, $7)`,
		caller, reply, "bafyprefcomment"+id+"reply", other, subject, root, base.Add(time.Second))
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO votes
		(uri, cid, rkey, voter_did, subject_uri, subject_cid, direction, created_at)
		VALUES ($1, 'bafyprefvote', $2, $3, $4, $5, 'up', $6)`,
		"at://"+other+"/social.coves.feed.vote/"+id, id, other, root, rootCID, base)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO notifications
		(recipient_did, reason, subject_uri, root_post_uri, sort_at)
		VALUES ($1, 'upvote', $2, $2, $3)`, caller, root, base.Add(2*time.Second))
	require.NoError(t, err)

	repo := postgres.NewNotificationRepository(db)
	unread := notification.NewHandler(notifications.NewService(repo.(notifications.ReadRepository)))
	preferences := notification.NewPreferencesHandler(notifications.NewPreferencesService(repo.(notifications.PreferencesRepository)))
	getCount := func() *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.notification.getUnreadCount", nil)
		req = req.WithContext(middleware.SetTestUserDID(req.Context(), caller))
		rec := httptest.NewRecorder()
		unread.HandleGetUnreadCount(rec, req)
		return rec
	}
	before := getCount()
	require.Equal(t, http.StatusOK, before.Code, "getUnreadCount response: %s", before.Body.String())
	require.JSONEq(t, `{"count":2}`, before.Body.String(), "fixture must start with a qualifying upvote group and a visible reply")

	putResponse, putRequest := putPreferencesRequest(caller, `{"upvote":false}`)
	preferences.HandlePutPreferences(putResponse, putRequest)
	require.Equal(t, http.StatusOK, putResponse.Code, "putPreferences response: %s", putResponse.Body.String())
	require.JSONEq(t, `{"postReply":true,"commentReply":true,"mention":true,"upvote":false}`, putResponse.Body.String())
	after := getCount()
	require.Equal(t, http.StatusOK, after.Code, "getUnreadCount response: %s", after.Body.String())
	require.JSONEq(t, `{"count":1}`, after.Body.String())
}
