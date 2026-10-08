package notifications_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"Coves/internal/core/notifications"

	"github.com/stretchr/testify/require"
)

func TestListNotifications_MentionHydrationOmissions(t *testing.T) {
	const rootURI = "at://did:plc:owner/social.coves.community.postv2/root"
	const missingPost = "at://did:plc:actor/social.coves.community.postv2/missing"
	const livePost = "at://did:plc:actor/social.coves.community.postv2/live"
	const deletedComment = "at://did:plc:actor/social.coves.community.comment/deleted"
	const missingComment = "at://did:plc:actor/social.coves.community.comment/missing"
	const replyURI = "at://did:plc:actor/social.coves.community.comment/reply"
	at := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	deletedAt := at.Add(time.Minute)
	live := notifications.ListedReference{State: notifications.ReferenceLive, CID: "bafylistedroot"}
	rows := []struct {
		reason                notifications.Reason
		uri, root             string
		record, rootReference notifications.ListedReference
	}{
		{notifications.ReasonMention, missingPost, missingPost, notifications.ListedReference{State: notifications.ReferenceLive, CID: "bafylistedmissing"}, notifications.ListedReference{State: notifications.ReferenceLive, CID: "bafylistedmissingroot"}},
		{notifications.ReasonMention, livePost, livePost, notifications.ListedReference{State: notifications.ReferenceLive, CID: "bafylistedpost"}, notifications.ListedReference{State: notifications.ReferenceLive, CID: "bafylistedpostroot"}},
		{notifications.ReasonMention, deletedComment, rootURI, notifications.ListedReference{State: notifications.ReferenceLive, CID: "bafylisteddeleted"}, live},
		{notifications.ReasonMention, missingComment, rootURI, notifications.ListedReference{State: notifications.ReferenceLive, CID: "bafylistedcomment"}, live},
		{notifications.ReasonPostReply, replyURI, rootURI, notifications.ListedReference{State: notifications.ReferenceLive, CID: "bafylistedreply"}, live},
	}
	listed := make([]notifications.ListedNotification, 0, len(rows))
	for index, row := range rows {
		item := notifications.ListedNotification{Reason: row.reason, ActorDID: "did:plc:actor", RecordURI: row.uri, RootPostURI: row.root, Record: row.record, RootPost: row.rootReference, RecordCreatedAt: at, SortAt: at.Add(time.Duration(index) * time.Second)}
		if row.reason == notifications.ReasonPostReply {
			item.SubjectURI, item.Subject = rootURI, live
		}
		listed = append(listed, item)
	}
	logs := captureDefaultLogs(t)
	service := notifications.NewListService(cannedListReader{page: notifications.ListPage{Notifications: listed}}, cannedProfiles{}, anonymousPosts{
		rootURI:  {URI: rootURI, CID: "bafylookuproot", Record: map[string]interface{}{"title": "Root"}},
		livePost: {URI: livePost, CID: "bafylookuppost", Record: map[string]interface{}{"title": "Post title"}},
	}, cannedComments{
		deletedComment: {URI: deletedComment, CID: "bafylookupdeleted", Content: "Private", DeletedAt: &deletedAt},
		replyURI:       {URI: replyURI, CID: "bafylookupreply", Content: "Reply"},
	})
	got, err := service.ListNotifications(context.Background(), "did:plc:owner", "", 50)
	require.NoError(t, err)
	require.Len(t, got.Notifications, 2, "only the three unhydratable mentions are omitted")
	var uris []string
	for _, notification := range got.Notifications {
		uris = append(uris, notification.Record.URI)
	}
	require.Equal(t, []string{livePost, replyURI}, uris)
	for _, reference := range []struct {
		value any
		want  string
	}{
		{got.Notifications[0].Record, `{"uri":"` + livePost + `","cid":"bafylistedpost","createdAt":"2026-09-20T09:00:00Z","excerpt":"Post title"}`},
		{got.Notifications[0].RootPost, `{"uri":"` + livePost + `","cid":"bafylistedpostroot","title":"Post title"}`},
	} {
		encoded, err := json.Marshal(reference.value)
		require.NoError(t, err)
		require.JSONEq(t, reference.want, string(encoded))
	}
	require.Len(t, logs(), 1)
	require.Equal(t, slog.LevelWarn, logs()[0].Level)
	attrs := map[string]slog.Value{}
	logs()[0].Attrs(func(attr slog.Attr) bool { flattenLogAttrs("", []slog.Attr{attr}, attrs); return true })
	require.Equal(t, int64(3), attrs["omitted"].Int64())
	require.Equal(t, int64(0), attrs["by_cause.unrecognized_reason"].Int64())
	require.Equal(t, int64(2), attrs["by_cause.missing_record"].Int64())
	require.Equal(t, int64(1), attrs["by_cause.deleted_record"].Int64())
}
