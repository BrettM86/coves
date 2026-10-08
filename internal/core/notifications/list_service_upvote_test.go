package notifications_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"Coves/internal/core/notifications"

	"github.com/stretchr/testify/require"
)

func TestListNotifications_UpvoteHydrationOmissions(t *testing.T) {
	const root = "at://did:plc:owner/social.coves.community.postv2/root"
	const present = "at://did:plc:owner/social.coves.community.postv2/present"
	const missingPost = "at://did:plc:owner/social.coves.community.postv2/missing"
	const missingComment = "at://did:plc:owner/social.coves.community.comment/missing"
	const deletedLive = "at://did:plc:owner/social.coves.community.comment/deletedlive"
	const missingRoot = "at://did:plc:owner/social.coves.community.postv2/missingroot"
	const orphan = "at://did:plc:owner/social.coves.community.comment/orphan"
	const presentComment = "at://did:plc:owner/social.coves.community.comment/presentcomment"
	const placeholder = "at://did:plc:owner/social.coves.community.comment/placeholder"
	const reply = "at://did:plc:actor/social.coves.community.comment/reply"
	at := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	deleted := at.Add(time.Minute)
	livePost := notifications.ListedReference{State: notifications.ReferenceLive, CID: "bafyroot"}
	liveSubject := notifications.ListedReference{State: notifications.ReferenceLive, CID: "bafysubject"}
	group := func(subject, rootURI string) notifications.ListedNotification {
		subjectReference := liveSubject
		if subject == rootURI {
			subjectReference = livePost
		}
		return notifications.ListedNotification{Reason: notifications.ReasonUpvote, SubjectURI: subject, RootPostURI: rootURI,
			Subject: subjectReference, RootPost: livePost, UpvoteCount: 2, RecentUpvoterDIDs: []string{"did:plc:voter"}, SortAt: at}
	}
	rows := []notifications.ListedNotification{
		group(missingPost, missingPost),
		group(missingComment, root),
		group(deletedLive, root),
		group(orphan, missingRoot),
		group(present, present),
		group(placeholder, root),
		group(presentComment, missingRoot),
		{Reason: notifications.ReasonPostReply, RecordURI: reply, ActorDID: "did:plc:actor", SubjectURI: root, RootPostURI: root,
			Record: notifications.ListedReference{State: notifications.ReferenceLive, CID: "bafyreply"}, Subject: livePost, RootPost: livePost, RecordCreatedAt: at, SortAt: at},
	}
	rows[5].Subject = notifications.ListedReference{State: notifications.ReferenceDeleted, CID: "bafydeletedcomment"}
	logs := captureDefaultLogs(t)
	service := notifications.NewListService(cannedListReader{page: notifications.ListPage{Notifications: rows}}, cannedProfiles{}, anonymousPosts{
		root:    {URI: root, CID: "bafyroot", Record: map[string]interface{}{"title": "Root"}},
		present: {URI: present, CID: "bafypresent", Record: map[string]interface{}{"title": "Present"}},
	}, cannedComments{
		deletedLive:    {URI: deletedLive, CID: "bafyleak", Content: "Must not render", DeletedAt: &deleted},
		presentComment: {URI: presentComment, CID: "bafypresentcomment", Content: "Live comment under a missing root"},
		reply:          {URI: reply, CID: "bafyreply", Content: "Reply"},
	})
	got, err := service.ListNotifications(context.Background(), "did:plc:owner", "", 50)
	require.NoError(t, err)
	require.Len(t, got.Notifications, 3, "five unhydratable upvote groups must be omitted; the placeholder and reply survive")
	require.Equal(t, []notifications.Reason{notifications.ReasonUpvote, notifications.ReasonUpvote, notifications.ReasonPostReply},
		[]notifications.Reason{got.Notifications[0].Reason, got.Notifications[1].Reason, got.Notifications[2].Reason})
	require.Equal(t, []string{present, placeholder, root}, []string{got.Notifications[0].Subject.URI, got.Notifications[1].Subject.URI, got.Notifications[2].Subject.URI})
	require.Len(t, logs(), 1)
	require.Equal(t, slog.LevelWarn, logs()[0].Level)
	attrs := map[string]slog.Value{}
	logs()[0].Attrs(func(attr slog.Attr) bool { flattenLogAttrs("", []slog.Attr{attr}, attrs); return true })
	require.Equal(t, int64(5), attrs["omitted"].Int64())
	require.Equal(t, int64(5), attrs["by_reason.other"].Int64())
	require.Equal(t, int64(3), attrs["by_cause.missing_root"].Int64(), "a live root absent from the post lookup omits the group even when its subject is present")
	require.Equal(t, int64(1), attrs["by_cause.missing_subject"].Int64())
	require.Equal(t, int64(1), attrs["by_cause.deleted_subject"].Int64())
	require.Zero(t, attrs["by_cause.missing_record"].Int64())
	require.Zero(t, attrs["by_cause.deleted_record"].Int64())
	require.Zero(t, attrs["by_reason.postReply"].Int64())
	require.Zero(t, attrs["by_reason.commentReply"].Int64())
	require.Zero(t, attrs["by_cause.unrecognized_reason"].Int64())
}
