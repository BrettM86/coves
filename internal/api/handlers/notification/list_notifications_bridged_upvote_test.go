//go:build integration

package notification_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"Coves/internal/api/handlers/notification"
	"Coves/internal/api/middleware"
	"Coves/internal/core/notifications"
	"Coves/internal/db/postgres"

	"github.com/stretchr/testify/require"
)

func newBridgedUpvoteListFixture(t *testing.T) *listHandlerFixture {
	t.Helper()
	f := newListHandlerFixture(t)
	f.repo = postgres.NewNotificationRepository(f.db, postgres.WithBridgedUpvoteTotals()).(notifications.ReadRepository)
	return f
}

func (f *listHandlerFixture) setBridgedUpvotes(table, uri string, upvotes, downvotes int) {
	f.t.Helper()
	require.Contains(f.t, []string{"posts", "comments"}, table)
	_, err := f.db.ExecContext(context.Background(), "UPDATE "+table+" SET bridged_upvote_count = $1, bridged_downvote_count = $2, bridged_stats_as_of = NOW() WHERE uri = $3", upvotes, downvotes, uri)
	require.NoError(f.t, err)
}

func (f *listHandlerFixture) bridgedUnreadCount(recipient string) string {
	f.t.Helper()
	handler := notification.NewHandler(notifications.NewService(f.repo))
	request := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.notification.getUnreadCount", nil)
	request = request.WithContext(middleware.SetTestUserDID(request.Context(), recipient))
	response := httptest.NewRecorder()
	handler.HandleGetUnreadCount(response, request)
	require.Equal(f.t, http.StatusOK, response.Code, "getUnreadCount response: %s", response.Body.String())
	return response.Body.String()
}

func (f *listHandlerFixture) setBridgedSeenAt(recipient string, at time.Time) {
	f.t.Helper()
	_, err := f.db.ExecContext(context.Background(), `INSERT INTO notification_state (did, seen_at) VALUES ($1, $2)`, recipient, at.UTC().Truncate(time.Microsecond))
	require.NoError(f.t, err)
}

func TestListNotifications_BridgedMixedUpvotesKeepNativeVoters(t *testing.T) {
	f := newBridgedUpvoteListFixture(t)
	thread := f.seedThread("Root", "Body")
	at := time.Date(2026, 9, 20, 9, 1, 0, 0, time.UTC)
	f.addUpvoteGroup(thread.caller, thread.root, thread.root, at)
	f.setBridgedUpvotes("posts", thread.root, 5, 4)
	older, newer, blocked := "did:plc:older"+f.id, "did:plc:newer"+f.id, "did:plc:blocked"+f.id
	for _, voter := range []struct{ did, handle string }{
		{older, "older" + f.id + ".test"}, {newer, "newer" + f.id + ".test"}, {blocked, "blocked" + f.id + ".test"},
	} {
		f.addUser(voter.did, voter.handle, "Voter")
	}
	f.addUpvoteVote(older, thread.root, "up", at.Add(time.Second))
	f.addUpvoteVote(newer, thread.root, "up", at.Add(2*time.Second))
	f.addUpvoteVote(blocked, thread.root, "up", at.Add(3*time.Second))
	f.addUpvoteVote(thread.caller, thread.root, "up", at.Add(4*time.Second))
	_, err := f.db.ExecContext(context.Background(), `INSERT INTO user_blocks (blocker_did, blocked_did, record_uri, record_cid) VALUES ($1, $2, $3, 'bafyblock')`, thread.caller, blocked, "at://"+thread.caller+"/social.coves.actor.block/blocked")
	require.NoError(t, err)

	response := f.placeholderRequest(thread.caller, "")
	require.Equal(t, http.StatusOK, response.status, "response: %s", response.raw)
	rows := listRawRows(t, response)
	require.Len(t, rows, 1)
	require.JSONEq(t, `7`, string(rows[0]["upvoteCount"]))
	require.Equal(t, []string{newer, older}, upvoteVoterDIDs(t, rows[0]))
}

func TestListNotifications_BridgedOnlyGroupVisibilityAndUnread(t *testing.T) {
	f := newBridgedUpvoteListFixture(t)
	thread := f.seedThread("Root", "Body")
	at := time.Date(2026, 9, 20, 9, 1, 0, 0, time.UTC)
	f.setBridgedSeenAt(thread.caller, at.Add(-time.Second))
	f.addUpvoteGroup(thread.caller, thread.root, thread.root, at)
	f.setBridgedUpvotes("posts", thread.root, 5, 4)
	zero := "at://" + thread.caller + "/social.coves.community.postv2/zero"
	f.addPost(zero, "bafyzero", "zero", thread.caller, thread.community, "Zero", "Body", at)
	f.addUpvoteGroup(thread.caller, zero, zero, at.Add(time.Second))
	f.setBridgedUpvotes("posts", zero, 0, 4)

	t.Run("list includes positive total and omits zero total", func(t *testing.T) {
		response := f.placeholderRequest(thread.caller, "")
		require.Equal(t, http.StatusOK, response.status, "response: %s", response.raw)
		rows := listRawRows(t, response)
		require.Len(t, rows, 1)
		require.JSONEq(t, `"upvote"`, string(rows[0]["reason"]))
		require.JSONEq(t, `"`+thread.root+`"`, string(placeholderObject(t, rows[0], "subject")["uri"]))
		require.JSONEq(t, `5`, string(rows[0]["upvoteCount"]))
		require.NotContains(t, rows[0], "recentUpvoters")
	})
	t.Run("unread includes positive total but not zero total", func(t *testing.T) {
		require.JSONEq(t, `{"count":1}`, f.bridgedUnreadCount(thread.caller))
	})
}

func TestListNotifications_BridgedOnlyPlaceholdersAndUnread(t *testing.T) {
	f := newBridgedUpvoteListFixture(t)
	thread := f.seedThread("Root", "Body")
	at := time.Date(2026, 9, 20, 9, 1, 0, 0, time.UTC)
	f.setBridgedSeenAt(thread.caller, at.Add(-time.Second))
	comment := "at://" + thread.caller + "/social.coves.community.comment/own"
	f.addComment(comment, "bafyowncomment", "own", thread.caller, thread.root, "bafyroot", thread.root, "bafyroot", "Private comment", at)
	f.addUpvoteGroup(thread.caller, comment, thread.root, at)
	f.setBridgedUpvotes("comments", comment, 4, 0)
	f.softDeleteComment(comment)
	removed := "at://" + thread.caller + "/social.coves.community.postv2/removed"
	f.addPost(removed, "bafyremoved", "removed", thread.caller, thread.community, "Private title", "Private body", at)
	f.addUpvoteGroup(thread.caller, removed, removed, at.Add(time.Second))
	f.setBridgedUpvotes("posts", removed, 4, 0)
	f.withdrawPost(removed, "communityWithdrawal")
	deleted := "at://" + thread.caller + "/social.coves.community.postv2/deleted"
	f.addPost(deleted, "bafydeleted", "deleted", thread.caller, thread.community, "Private title", "Private body", at)
	f.addUpvoteGroup(thread.caller, deleted, deleted, at.Add(2*time.Second))
	f.setBridgedUpvotes("posts", deleted, 4, 0)
	f.withdrawPost(deleted, "authorDelete")

	t.Run("list contains deleted and moderator-removed placeholders", func(t *testing.T) {
		response := f.placeholderRequest(thread.caller, "")
		require.Equal(t, http.StatusOK, response.status, "response: %s", response.raw)
		rows := listRawRows(t, response)
		require.Len(t, rows, 3)
		bySubject := make(map[string]map[string]json.RawMessage)
		for _, row := range rows {
			var uri string
			require.NoError(t, json.Unmarshal(placeholderObject(t, row, "subject")["uri"], &uri))
			bySubject[uri] = row
		}
		for _, tc := range []struct{ uri, status string }{
			{comment, "deleted"}, {removed, "removedByModerator"}, {deleted, "deleted"},
		} {
			row, ok := bySubject[tc.uri]
			require.True(t, ok, "missing upvote group for %s", tc.uri)
			require.JSONEq(t, `4`, string(row["upvoteCount"]))
			subject := placeholderObject(t, row, "subject")
			require.JSONEq(t, `"`+tc.status+`"`, string(subject["status"]))
			require.NotContains(t, subject, "preview")
			require.NotContains(t, row, "recentUpvoters")
		}
	})
	t.Run("unread counts all three placeholders", func(t *testing.T) {
		require.JSONEq(t, `{"count":3}`, f.bridgedUnreadCount(thread.caller))
	})
}
