package notifications_test

import (
	"context"
	"testing"
	"time"

	"Coves/internal/core/notifications"
	"Coves/internal/core/posts"

	"github.com/stretchr/testify/require"
)

func TestListNotifications_CommunityReportView(t *testing.T) {
	sortAt := time.Date(2026, time.November, 16, 9, 0, 0, 0, time.UTC)
	reader := cannedListReader{page: notifications.ListPage{Notifications: []notifications.ListedNotification{{
		ID: 1, Reason: notifications.ReasonCommunityReport, SubjectURI: "at://did:plc:reportedcommunity",
		SortAt: sortAt, ReportCount: 5,
		Community: &notifications.ListedCommunity{DID: "did:plc:reportedcommunity", Handle: "reported.coves.social", Name: "Reported"},
	}}}}
	service := notifications.NewListService(reader, cannedProfiles{}, anonymousPosts{}, cannedComments{})

	output, err := service.ListNotifications(context.Background(), "did:plc:reportmoderator", "", 50)
	require.NoError(t, err)
	require.Equal(t, []notifications.NotificationView{{
		Reason:      "communityReport",
		SortAt:      "2026-11-16T09:00:00Z",
		IsRead:      false,
		Community:   &posts.CommunityRef{DID: "did:plc:reportedcommunity", Handle: "reported.coves.social", Name: "Reported"},
		ReportCount: 5,
	}}, output.Notifications, "a communityReport names the community and its report count, with no author, root post, subject or record")
}
