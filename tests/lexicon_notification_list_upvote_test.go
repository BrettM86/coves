package tests

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"Coves/internal/core/notifications"

	"github.com/bluesky-social/indigo/atproto/atdata"
	lexicon "github.com/bluesky-social/indigo/atproto/lexicon"
	"github.com/stretchr/testify/require"
)

func TestNotificationListLexicon_UpvoteOutputContract(t *testing.T) {
	t.Run("rendered live and placeholder reasons", assertRenderedUpvoteOutput)
	t.Run("invalid aggregates", assertInvalidUpvoteAggregates)
}

func assertRenderedUpvoteOutput(t *testing.T) {
	catalog, recordID, _ := placeholderListLexicon(t)
	const root = "at://did:plc:owner/social.coves.community.postv2/root"
	const parent = "at://did:plc:owner/social.coves.community.comment/parent"
	const reply = "at://did:plc:actor/social.coves.community.comment/reply"
	const mentionPost = "at://did:plc:actor/social.coves.community.postv2/mention"
	const mentionComment = "at://did:plc:actor/social.coves.community.comment/mention"
	at := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	liveRoot := notifications.ListedReference{State: notifications.ReferenceLive, CID: "bafyroot"}
	liveParent := notifications.ListedReference{State: notifications.ReferenceLive, CID: "bafyparent"}
	liveReply := notifications.ListedReference{State: notifications.ReferenceLive, CID: "bafyreply"}
	rows := []notifications.ListedNotification{
		{Reason: notifications.ReasonPostReply, ActorDID: "did:plc:actor", RecordURI: reply, Record: liveReply, SubjectURI: root, Subject: liveRoot, RootPostURI: root, RootPost: liveRoot},
		{Reason: notifications.ReasonCommentReply, ActorDID: "did:plc:actor", RecordURI: reply, Record: liveReply, SubjectURI: parent, Subject: liveParent, RootPostURI: root, RootPost: liveRoot},
		{Reason: notifications.ReasonMention, ActorDID: "did:plc:actor", RecordURI: mentionComment, Record: notifications.ListedReference{State: notifications.ReferenceLive, CID: "bafymentioncomment"}, RootPostURI: root, RootPost: liveRoot},
		{Reason: notifications.ReasonMention, ActorDID: "did:plc:actor", RecordURI: mentionPost, Record: notifications.ListedReference{State: notifications.ReferenceLive, CID: "bafymentionpost"}, RootPostURI: mentionPost, RootPost: notifications.ListedReference{State: notifications.ReferenceLive, CID: "bafymentionpost"}},
		{Reason: notifications.ReasonUpvote, SubjectURI: root, Subject: liveRoot, RootPostURI: root, RootPost: liveRoot, UpvoteCount: 2, RecentUpvoterDIDs: []string{"did:plc:indexed", "did:plc:unindexed"}},
		{Reason: notifications.ReasonUpvote, SubjectURI: parent, Subject: liveParent, RootPostURI: root, RootPost: liveRoot, UpvoteCount: 1, RecentUpvoterDIDs: []string{"did:plc:unindexed"}},
	}
	for index := range rows {
		rows[index].RecordCreatedAt = at
		rows[index].SortAt = at.Add(time.Duration(index) * time.Second)
	}
	for _, original := range append([]notifications.ListedNotification(nil), rows...) {
		placeholder := original
		placeholder.RootPost.State = notifications.ReferenceDeleted
		if placeholder.Reason == notifications.ReasonMention {
			placeholder.Record.State = notifications.ReferenceDeleted
		} else {
			placeholder.Subject.State = notifications.ReferenceDeleted
			if placeholder.Reason != notifications.ReasonUpvote {
				placeholder.Record.State = notifications.ReferenceDeleted
			}
		}
		rows = append(rows, placeholder)
	}
	service := notifications.NewListService(mentionLexiconReader{page: notifications.ListPage{Notifications: rows}}, mentionLexiconProfiles{}, mentionLexiconPosts{
		root:        {URI: root, CID: "bafyroot", Record: map[string]interface{}{"title": "Root"}},
		mentionPost: {URI: mentionPost, CID: "bafymentionpost", Record: map[string]interface{}{"title": "Mention"}},
	}, mentionLexiconComments{
		parent:         {URI: parent, CID: "bafyparent", Content: "Parent"},
		reply:          {URI: reply, CID: "bafyreply", Content: "Reply"},
		mentionComment: {URI: mentionComment, CID: "bafymentioncomment", Content: "Mention"},
	})
	output, err := service.ListNotifications(context.Background(), "did:plc:owner", "", 50)
	require.NoError(t, err)
	require.Len(t, output.Notifications, 12, "six live and six placeholder rows must render")
	encoded, err := json.Marshal(output)
	require.NoError(t, err)
	var wire struct {
		Notifications []map[string]json.RawMessage `json:"notifications"`
	}
	require.NoError(t, json.Unmarshal(encoded, &wire))
	for _, index := range []int{4, 5, 10, 11} {
		require.Contains(t, wire.Notifications[index], "upvoteCount")
		require.Contains(t, wire.Notifications[index], "recentUpvoters")
	}
	data, err := atdata.UnmarshalJSON(encoded)
	require.NoError(t, err)
	data["$type"] = recordID
	require.NoError(t, lexicon.ValidateRecord(catalog, data, recordID, 0))
}

func assertInvalidUpvoteAggregates(t *testing.T) {
	catalog, recordID, _ := placeholderListLexicon(t)
	const row = `{"notifications":[{"reason":"upvote","sortAt":"2026-09-20T09:00:00Z","isRead":false,"rootPost":{"uri":"at://did:plc:owner/social.coves.community.postv2/root","cid":"bafyroot"},"subject":{"uri":"at://did:plc:owner/social.coves.community.postv2/root","cid":"bafyroot"},"upvoteCount":2,"recentUpvoters":[{"did":"did:plc:voter"}]}]}`
	valid, err := atdata.UnmarshalJSON([]byte(row))
	require.NoError(t, err)
	valid["$type"] = recordID
	require.NoError(t, lexicon.ValidateRecord(catalog, valid, recordID, 0), "unchanged upvote row must validate")
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"negative count", func(view map[string]any) { view["upvoteCount"] = -1 }},
		{"string count", func(view map[string]any) { view["upvoteCount"] = "2" }},
		{"four voters", func(view map[string]any) {
			view["recentUpvoters"] = []any{
				map[string]any{"did": "did:plc:v1"}, map[string]any{"did": "did:plc:v2"},
				map[string]any{"did": "did:plc:v3"}, map[string]any{"did": "did:plc:v4"},
			}
		}},
		{"voter without DID", func(view map[string]any) {
			view["recentUpvoters"] = []any{map[string]any{"handle": "voter.test"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := atdata.UnmarshalJSON([]byte(row))
			require.NoError(t, err)
			data["$type"] = recordID
			view := data["notifications"].([]any)[0].(map[string]any)
			tc.change(view)
			require.Error(t, lexicon.ValidateRecord(catalog, data, recordID, 0), "invalid upvote aggregate must be rejected")
		})
	}
}
