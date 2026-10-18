package notifications_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"Coves/internal/core/notifications"
	"Coves/internal/core/posts"
	"Coves/internal/core/users"

	"github.com/stretchr/testify/require"
)

const (
	rootCardRootURI   = "at://did:plc:rootauthor/social.coves.community.postv2/root"
	rootCardParentURI = "at://did:plc:owner/social.coves.community.comment/parent"
	rootCardReplyURI  = "at://did:plc:actor/social.coves.community.comment/reply"
	rootCardActorDID  = "did:plc:actor"
	rootCardOtherURI  = "at://did:plc:otherauthor/social.coves.community.postv2/other"
)

func rootCardPostView() *posts.PostView {
	displayName := "Root Author"
	avatar := "https://img.example/avatar.jpg"
	return &posts.PostView{
		URI:       rootCardRootURI,
		CID:       "bafyroot",
		Record:    map[string]interface{}{"title": "Root title", "content": "Root body"},
		Community: &posts.CommunityRef{DID: "did:plc:community", Handle: "community.coves.social", Name: "Community"},
		Author:    &posts.AuthorView{DID: "did:plc:rootauthor", Handle: "rootauthor.coves.social", DisplayName: &displayName, Avatar: &avatar},
		CreatedAt: time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC),
		Stats:     &posts.PostStats{Upvotes: 7, Downvotes: 2, Score: 5, CommentCount: 3},
	}
}

func rootCardOtherPostView() *posts.PostView {
	return &posts.PostView{
		URI:       rootCardOtherURI,
		CID:       "bafyother",
		Record:    map[string]interface{}{"title": "Other title"},
		Author:    &posts.AuthorView{DID: "did:plc:otherauthor", Handle: "otherauthor.coves.social"},
		CreatedAt: time.Date(2026, 9, 19, 8, 0, 0, 0, time.UTC),
		Stats:     &posts.PostStats{Upvotes: 1, Score: 1},
	}
}

func rootCardRows(root notifications.ListedReference) []notifications.ListedNotification {
	at := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	live := notifications.ListedReference{State: notifications.ReferenceLive, CID: "bafyroot"}
	parent := notifications.ListedReference{State: notifications.ReferenceLive, CID: "bafyparent"}
	reply := notifications.ListedReference{State: notifications.ReferenceLive, CID: "bafyreply"}
	subject := live
	if root.State != notifications.ReferenceLive {
		subject = root
	}
	return []notifications.ListedNotification{
		{Reason: notifications.ReasonPostReply, RootPostURI: rootCardRootURI, RootPost: root, SubjectURI: rootCardRootURI, Subject: subject, RecordURI: rootCardReplyURI, Record: reply, ActorDID: rootCardActorDID, SortAt: at, RecordCreatedAt: at},
		{Reason: notifications.ReasonUpvote, RootPostURI: rootCardRootURI, RootPost: root, SubjectURI: rootCardRootURI, Subject: subject, SortAt: at, UpvoteCount: 2},
		{Reason: notifications.ReasonCommentReply, RootPostURI: rootCardRootURI, RootPost: root, SubjectURI: rootCardParentURI, Subject: parent, RecordURI: rootCardReplyURI, Record: reply, ActorDID: rootCardActorDID, SortAt: at, RecordCreatedAt: at},
		{Reason: notifications.ReasonMention, RootPostURI: rootCardRootURI, RootPost: root, RecordURI: rootCardReplyURI, Record: reply, ActorDID: rootCardActorDID, SortAt: at, RecordCreatedAt: at},
	}
}

func rootCardListJSON(t *testing.T, recipientDID string, rows []notifications.ListedNotification) []map[string]json.RawMessage {
	t.Helper()
	commentViews := cannedComments{
		rootCardParentURI: {URI: rootCardParentURI, CID: "bafyparent", Content: "Parent"},
		rootCardReplyURI:  {URI: rootCardReplyURI, CID: "bafyreply", Content: "Reply"},
	}
	profiles := cannedProfiles{rootCardActorDID: &users.User{DID: rootCardActorDID, Handle: "actor.test"}}
	service := notifications.NewListService(cannedListReader{page: notifications.ListPage{Notifications: rows}}, profiles, anonymousPosts{rootCardRootURI: rootCardPostView(), rootCardOtherURI: rootCardOtherPostView()}, commentViews)
	got, err := service.ListNotifications(context.Background(), recipientDID, "", 50)
	require.NoError(t, err)
	encoded, err := json.Marshal(got)
	require.NoError(t, err)
	var page struct {
		Notifications []map[string]json.RawMessage `json:"notifications"`
	}
	require.NoError(t, json.Unmarshal(encoded, &page))
	return page.Notifications
}

func TestListNotifications_RootCardLiveRoot(t *testing.T) {
	const wantAuthor = `{"did":"did:plc:rootauthor","handle":"rootauthor.coves.social","displayName":"Root Author","avatar":"https://img.example/avatar.jpg"}`
	const wantStats = `{"upvotes":7,"downvotes":2,"score":5,"commentCount":3}`
	for _, tc := range []struct {
		name, recipientDID string
	}{
		{"recipient is not the root author", "did:plc:owner"},
		{"recipient is the root author", "did:plc:rootauthor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live := notifications.ListedReference{State: notifications.ReferenceLive, CID: "bafyroot"}
			rows := rootCardListJSON(t, tc.recipientDID, rootCardRows(live))
			require.Len(t, rows, 4)
			wantReasons := []string{"postReply", "upvote", "commentReply", "mention"}
			for index, row := range rows {
				var reason string
				require.NoError(t, json.Unmarshal(row["reason"], &reason))
				require.Equal(t, wantReasons[index], reason)
				require.JSONEq(t, `"2026-09-20T09:00:00Z"`, string(row["sortAt"]), reason)
				var root map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(row["rootPost"], &root))
				require.Contains(t, rootCardKeys(root), "author", reason)
				require.JSONEq(t, wantAuthor, string(root["author"]), reason)
				require.Contains(t, rootCardKeys(root), "createdAt", reason)
				require.JSONEq(t, `"2026-09-20T08:00:00Z"`, string(root["createdAt"]), reason)
				require.Contains(t, rootCardKeys(root), "stats", reason)
				require.JSONEq(t, wantStats, string(root["stats"]), reason)
				if reason == "upvote" {
					require.JSONEq(t, `2`, string(row["upvoteCount"]))
				}
			}
		})
	}
}

func TestListNotifications_RootCardOmittedForPlaceholderRoot(t *testing.T) {
	for _, tc := range []struct {
		name, wantStatus string
		state            notifications.ReferenceState
	}{
		{"deleted root", "deleted", notifications.ReferenceDeleted},
		{"moderator removed root", "removedByModerator", notifications.ReferenceRemovedByModerator},
		{"server admin removed root", "removedByServerAdmin", notifications.ReferenceRemovedByServerAdmin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := notifications.ListedReference{State: tc.state, CID: "bafyrootplaceholder"}
			// A live upvote on another post forces the page to load posts; the
			// post fake then also returns the populated PostView for the
			// placeholder root, which must still not reach the root card.
			otherLive := notifications.ListedReference{State: notifications.ReferenceLive, CID: "bafyother"}
			otherUpvote := notifications.ListedNotification{Reason: notifications.ReasonUpvote, RootPostURI: rootCardOtherURI, RootPost: otherLive, SubjectURI: rootCardOtherURI, Subject: otherLive, SortAt: time.Date(2026, 9, 20, 8, 30, 0, 0, time.UTC), UpvoteCount: 1}
			rows := rootCardListJSON(t, "did:plc:owner", []notifications.ListedNotification{rootCardRows(root)[0], otherUpvote})
			require.Len(t, rows, 2, "placeholder root notification and live upvote must both be listed")
			var rootPost map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(rows[0]["rootPost"], &rootPost))
			require.JSONEq(t, `"`+tc.wantStatus+`"`, string(rootPost["status"]))
			for _, key := range []string{"author", "createdAt", "stats"} {
				require.NotContains(t, rootCardKeys(rootPost), key)
			}
			var liveRootPost map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(rows[1]["rootPost"], &liveRootPost))
			require.JSONEq(t, `"at://did:plc:otherauthor/social.coves.community.postv2/other"`, string(liveRootPost["uri"]))
			require.Contains(t, rootCardKeys(liveRootPost), "author", "the live row proves the page loaded posts")
			require.JSONEq(t, `{"did":"did:plc:otherauthor","handle":"otherauthor.coves.social"}`, string(liveRootPost["author"]))
		})
	}
}

func rootCardKeys(object map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	return keys
}
