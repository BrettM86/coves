package notifications_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"Coves/internal/core/comments"
	"Coves/internal/core/notifications"
	"Coves/internal/core/posts"

	"github.com/stretchr/testify/require"
)

func TestListNotifications_BlankPostTitles(t *testing.T) {
	const bodyExcerpt = "ABCDEFGHIJABCDEFGHIJ" +
		"ABCDEFGHIJABCDEFGHIJ" +
		"ABCDEFGHIJABCDEFGHIJ" +
		"ABCDEFGHIJABCDEFGHIJ" +
		"ABCDEFGHIJABCDEFGHIJ" +
		"ABCDEFGHIJABCDEFGHIJ" +
		"ABCDEFGHIJABCDEFGHIJ"
	const titleExcerpt = "abcdefghijabcdefghij" +
		"abcdefghijabcdefghij" +
		"abcdefghijabcdefghij" +
		"abcdefghijabcdefghij" +
		"abcdefghijabcdefghij" +
		"abcdefghijabcdefghij" +
		"abcdefghijabcdefghij"
	const wholeTitle = titleExcerpt + "zzzzzzzzzzzzzzzzzzzz" +
		"zzzzzzzzzzzzzzzzzzzz" + "zzzzzzzzzzzzzzzzzzzz"
	const postURI = "at://did:plc:owner/social.coves.community.postv2/subject"
	const replyURI = "at://did:plc:actor/social.coves.community.comment/reply"
	for _, tc := range []struct {
		name, title, body, wantRoot, wantSubject, wantExcerpt string
	}{
		{"ASCII spaces", "   ", bodyExcerpt + "BODY-TAIL", "", bodyExcerpt, bodyExcerpt},
		{"tab and newline", "\t\n", bodyExcerpt + "BODY-TAIL", "", bodyExcerpt, bodyExcerpt},
		{"ideographic space", "　", bodyExcerpt + "BODY-TAIL", "", bodyExcerpt, bodyExcerpt},
		{"nonblank title retains surrounding whitespace", "  Hello  ", "Body", "  Hello  ", "  Hello  ", "  Hello  "},
		{"long title is whole except mention excerpt", titleExcerpt + strings.Repeat("z", 60), "Body", wholeTitle, wholeTitle, titleExcerpt},
		{"blank title and empty body omit text", "   ", "", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
			post := &posts.PostView{URI: postURI, CID: "bafypost", Record: map[string]interface{}{"title": tc.title, "content": tc.body}}
			rows := []notifications.ListedNotification{
				{Reason: notifications.ReasonPostReply, RootPostURI: postURI, RootPost: notifications.ListedReference{CID: "bafypost"}, SubjectURI: postURI, Subject: notifications.ListedReference{CID: "bafypost"}, RecordURI: replyURI, Record: notifications.ListedReference{CID: "bafyreply"}, ActorDID: "did:plc:actor", SortAt: at, RecordCreatedAt: at},
				{Reason: notifications.ReasonUpvote, RootPostURI: postURI, RootPost: notifications.ListedReference{CID: "bafypost"}, SubjectURI: postURI, Subject: notifications.ListedReference{CID: "bafypost"}, SortAt: at, UpvoteCount: 1},
				{Reason: notifications.ReasonMention, RootPostURI: postURI, RootPost: notifications.ListedReference{CID: "bafypost"}, RecordURI: postURI, Record: notifications.ListedReference{CID: "bafypost"}, ActorDID: "did:plc:actor", SortAt: at, RecordCreatedAt: at},
			}
			service := notifications.NewListService(cannedListReader{page: notifications.ListPage{Notifications: rows}}, cannedProfiles{}, anonymousPosts{postURI: post}, cannedComments{replyURI: &comments.Comment{URI: replyURI, CID: "bafyreply", Content: "Reply"}})
			got, err := service.ListNotifications(context.Background(), "did:plc:owner", "", 50)
			require.NoError(t, err)
			require.Len(t, got.Notifications, 3)
			encoded, err := json.Marshal(got)
			require.NoError(t, err)
			var page struct {
				Notifications []map[string]json.RawMessage `json:"notifications"`
			}
			require.NoError(t, json.Unmarshal(encoded, &page))
			for index, row := range page.Notifications {
				var root map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(row["rootPost"], &root))
				assertPreviewText(t, root, "title", tc.wantRoot)
				if index < 2 {
					var subject map[string]json.RawMessage
					require.NoError(t, json.Unmarshal(row["subject"], &subject))
					assertPreviewText(t, subject, "preview", tc.wantSubject)
				} else {
					var record map[string]json.RawMessage
					require.NoError(t, json.Unmarshal(row["record"], &record))
					assertPreviewText(t, record, "excerpt", tc.wantExcerpt)
				}
			}
		})
	}
}

func assertPreviewText(t *testing.T, reference map[string]json.RawMessage, field, want string) {
	t.Helper()
	if want == "" {
		require.NotContains(t, reference, field)
		return
	}
	require.Contains(t, reference, field)
	var value string
	require.NoError(t, json.Unmarshal(reference[field], &value))
	require.Equal(t, want, value, field)
}
