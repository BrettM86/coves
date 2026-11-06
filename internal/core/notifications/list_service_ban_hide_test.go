package notifications_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"Coves/internal/core/comments"
	"Coves/internal/core/notifications"
	"Coves/internal/core/posts"

	"github.com/stretchr/testify/require"
)

// TestListNotifications_OmitsHydratedCommentsWithHideReason covers a row
// selected while its comment was live, where the comment was re-created
// hidden (community ban, or a ban check still pending) before hydration. The
// hydrated comment has no DeletedAt, but its content must not render: the row
// is omitted exactly as a comment deleted before hydration is.
func TestListNotifications_OmitsHydratedCommentsWithHideReason(t *testing.T) {
	at := time.Date(2026, time.October, 22, 9, 0, 0, 0, time.UTC)
	const hiddenText = "hidden content that must not render"
	const postURI = "at://did:plc:owner/social.coves.community.postv2/root"
	const hiddenURI = "at://did:plc:actor/social.coves.community.comment/hidden"
	const healthyURI = "at://did:plc:actor/social.coves.community.comment/healthy"
	const replyURI = "at://did:plc:actor/social.coves.community.comment/reply"
	live := notifications.ListedReference{State: notifications.ReferenceLive, CID: "bafylive"}
	post := &posts.PostView{URI: postURI, CID: "bafyroot", Record: map[string]interface{}{"title": "Root"}, Community: &posts.CommunityRef{DID: "did:plc:community", Handle: "community.coves.social", Name: "Community"}}
	for _, hideReason := range []string{comments.HideReasonCommunityBan, "pending_ban_check"} {
		for _, tc := range []struct {
			name string
			row  notifications.ListedNotification
		}{
			{"postReply record", notifications.ListedNotification{Reason: notifications.ReasonPostReply, RecordURI: hiddenURI, Record: live, SubjectURI: postURI, Subject: live, RootPostURI: postURI, RootPost: live}},
			{"comment mention record", notifications.ListedNotification{Reason: notifications.ReasonMention, RecordURI: hiddenURI, Record: live, RootPostURI: postURI, RootPost: live}},
			{"commentReply subject", notifications.ListedNotification{Reason: notifications.ReasonCommentReply, RecordURI: replyURI, Record: live, SubjectURI: hiddenURI, Subject: live, RootPostURI: postURI, RootPost: live}},
			{"comment upvote subject", notifications.ListedNotification{Reason: notifications.ReasonUpvote, SubjectURI: hiddenURI, Subject: live, RootPostURI: postURI, RootPost: live, UpvoteCount: 1}},
		} {
			t.Run(hideReason+"/"+tc.name, func(t *testing.T) {
				reason := hideReason
				row := tc.row
				row.ActorDID, row.SortAt, row.RecordCreatedAt = "did:plc:actor", at, at
				healthy := notifications.ListedNotification{Reason: notifications.ReasonPostReply, RecordURI: healthyURI, Record: live, SubjectURI: postURI, Subject: live, RootPostURI: postURI, RootPost: live, ActorDID: "did:plc:actor", SortAt: at.Add(time.Minute), RecordCreatedAt: at}
				service := notifications.NewListService(
					cannedListReader{page: notifications.ListPage{Notifications: []notifications.ListedNotification{healthy, row}}},
					cannedProfiles{},
					anonymousPosts{postURI: post},
					cannedComments{
						hiddenURI:  {URI: hiddenURI, CID: "bafyhidden", CommenterDID: "did:plc:actor", Content: hiddenText, HideReason: &reason},
						healthyURI: {URI: healthyURI, CID: "bafyhealthy", Content: "Healthy reply"},
						replyURI:   {URI: replyURI, CID: "bafyreply", Content: "Reply"},
					},
				)
				got, err := service.ListNotifications(context.Background(), "did:plc:owner", "", 50)
				require.NoError(t, err)
				encoded, err := json.Marshal(got)
				require.NoError(t, err)
				require.NotContains(t, string(encoded), hiddenText, "a hidden comment's content must not render in a notification")
				require.Len(t, got.Notifications, 1, "the row whose comment is hidden is omitted like a deleted one")
				require.Equal(t, healthyURI, got.Notifications[0].Record.URI)
			})
		}
	}
}
