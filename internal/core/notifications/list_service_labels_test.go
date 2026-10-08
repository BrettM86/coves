package notifications_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"Coves/internal/core/comments"
	"Coves/internal/core/notifications"
	"Coves/internal/core/posts"

	"github.com/stretchr/testify/require"
)

func TestListNotifications_LabelRules(t *testing.T) {
	const rootURI = "at://did:plc:owner/social.coves.community.postv2/root"
	const commentURI = "at://did:plc:actor/social.coves.community.comment/reply"
	const recordPostURI = "at://did:plc:actor/social.coves.community.postv2/mention"
	const tenUnknown = `{"values":[{"val":"unknown-1"},{"val":"unknown-2"},{"val":"unknown-3"},{"val":"unknown-4"},{"val":"unknown-5"},{"val":"unknown-6"},{"val":"unknown-7"},{"val":"unknown-8"},{"val":"unknown-9"},{"val":"unknown-10"}]}`
	longValue := strings.Repeat("x", 200)
	shortBoundary := strings.Repeat("é", 64)
	trueValue := true
	cases := []struct {
		name, rootLabels, commentLabels   string
		postLabels                        *posts.SelfLabels
		reason                            notifications.Reason
		wantRoot, wantSubject, wantRecord string
	}{
		{name: "unlabeled reply inherits nothing", reason: notifications.ReasonCommentReply, rootLabels: `{"values":[{"val":"nsfw"}]}`, wantRoot: `{"values":[{"val":"nsfw"}]}`},
		{name: "comment drops long value then keeps first ten", reason: notifications.ReasonPostReply,
			commentLabels: `{"values":[{"val":"nsfw"},{"val":"gore"},{"val":"` + longValue + `"},{"val":"one"},{"val":"two"},{"val":"three"},{"val":"four"},{"val":"five"},{"val":"six"},{"val":"seven"},{"val":"eight"},{"val":"nine"}]}`,
			wantRecord:    `{"values":[{"val":"nsfw"},{"val":"gore"},{"val":"one"},{"val":"two"},{"val":"three"},{"val":"four"},{"val":"five"},{"val":"six"},{"val":"seven"},{"val":"eight"}]}`},
		{name: "ten unknown post labels exclude nsfw", reason: notifications.ReasonMention,
			postLabels: &posts.SelfLabels{Values: []posts.SelfLabel{{Val: "unknown-1"}, {Val: "unknown-2"}, {Val: "unknown-3"}, {Val: "unknown-4"}, {Val: "unknown-5"}, {Val: "unknown-6"}, {Val: "unknown-7"}, {Val: "unknown-8"}, {Val: "unknown-9"}, {Val: "unknown-10"}, {Val: "nsfw"}}}, wantRecord: tenUnknown},
		{name: "128 UTF-8 bytes retained and 129 dropped", reason: notifications.ReasonPostReply,
			commentLabels: `{"values":[{"val":"` + shortBoundary + `"},{"val":"` + shortBoundary + `a"}]}`,
			wantRecord:    `{"values":[{"val":"` + shortBoundary + `"}]}`},
		{name: "negated post label is preserved", reason: notifications.ReasonMention,
			postLabels: &posts.SelfLabels{Values: []posts.SelfLabel{{Val: "spoiler", Neg: &trueValue}}}, wantRecord: `{"values":[{"val":"spoiler","neg":true}]}`},
		{name: "empty post labels omitted", reason: notifications.ReasonMention, postLabels: &posts.SelfLabels{Values: []posts.SelfLabel{}}},
		{name: "empty comment labels omitted", reason: notifications.ReasonPostReply, commentLabels: `{"values":[]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			at := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
			rootLabels := &posts.SelfLabels{}
			storedRoot := tc.rootLabels
			if storedRoot == "" {
				storedRoot = `{"values":[{"val":"nsfw"}]}`
			}
			require.NoError(t, json.Unmarshal([]byte(storedRoot), rootLabels))
			root := &posts.PostView{URI: rootURI, CID: "bafyroot", Record: map[string]interface{}{"title": "Root"}}
			root.Record.(map[string]interface{})["labels"] = *rootLabels
			comment := &comments.Comment{URI: commentURI, CID: "bafycomment", Content: "Reply"}
			if tc.commentLabels != "" {
				comment.ContentLabels = &tc.commentLabels
			}
			post := &posts.PostView{URI: recordPostURI, CID: "bafypost", Record: map[string]interface{}{"title": "Mention"}}
			if tc.postLabels != nil {
				post.Record.(map[string]interface{})["labels"] = *tc.postLabels
			}
			row := notifications.ListedNotification{Reason: tc.reason, ActorDID: "did:plc:actor", RootPostURI: rootURI, RootPost: notifications.ListedReference{CID: "bafyroot"}, RecordURI: commentURI, Record: notifications.ListedReference{CID: "bafycomment"}, SubjectURI: rootURI, Subject: notifications.ListedReference{CID: "bafyroot"}, RecordCreatedAt: at, SortAt: at}
			if tc.reason == notifications.ReasonCommentReply {
				row.SubjectURI = "at://did:plc:owner/social.coves.community.comment/parent"
				row.Subject.CID = "bafyparent"
			}
			if tc.reason == notifications.ReasonMention {
				row.RecordURI, row.Record.CID = recordPostURI, "bafypost"
				row.SubjectURI = ""
			}
			if tc.wantRoot == "" {
				tc.wantRoot = `{"values":[{"val":"nsfw"}]}`
			}
			if tc.reason == notifications.ReasonPostReply && tc.wantSubject == "" {
				tc.wantSubject = `{"values":[{"val":"nsfw"}]}`
			}
			service := notifications.NewListService(cannedListReader{page: notifications.ListPage{Notifications: []notifications.ListedNotification{row}}}, cannedProfiles{}, anonymousPosts{rootURI: root, recordPostURI: post}, cannedComments{commentURI: comment, row.SubjectURI: {URI: row.SubjectURI, CID: "bafyparent", Content: "Parent"}})
			got, err := service.ListNotifications(context.Background(), "did:plc:owner", "", 50)
			require.NoError(t, err)
			require.Len(t, got.Notifications, 1, "invalid labels must not lose the notification")
			encoded, err := json.Marshal(got)
			require.NoError(t, err)
			var output struct {
				Notifications []map[string]json.RawMessage `json:"notifications"`
			}
			require.NoError(t, json.Unmarshal(encoded, &output))
			for _, field := range []struct{ name, want string }{{"rootPost", tc.wantRoot}, {"subject", tc.wantSubject}, {"record", tc.wantRecord}} {
				if field.name == "subject" && tc.reason == notifications.ReasonMention {
					require.NotContains(t, output.Notifications[0], "subject")
					continue
				}
				var reference map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(output.Notifications[0][field.name], &reference))
				if field.want == "" {
					require.NotContains(t, reference, "labels", field.name)
				} else {
					require.Contains(t, reference, "labels", field.name)
					require.JSONEq(t, field.want, string(reference["labels"]), field.name)
				}
			}
		})
	}
}

// A live comment whose stored self-labels do not decode must not be shown
// with its labels silently dropped: the row is omitted and counted, while
// valid neighbours and placeholder references (never parsed) stay listed.
func TestListNotifications_MalformedLiveCommentLabelsOmitRow(t *testing.T) {
	const rootURI = "at://did:plc:owner/social.coves.community.postv2/root"
	const neighbourURI = "at://did:plc:actor/social.coves.community.comment/neighbour"
	const malformedURI = "at://did:plc:actor/social.coves.community.comment/malformed"
	const replyURI = "at://did:plc:actor/social.coves.community.comment/reply"
	const placeholderRecordURI = "at://did:plc:actor/social.coves.community.comment/removed"
	const placeholderParentURI = "at://did:plc:owner/social.coves.community.comment/deleted"
	const actorDID = "did:plc:actor"
	const validLabels = `{"values":[{"val":"nsfw"}]}`
	const wrongShape = `{"values":"bad"}`
	at := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	live := func(cid string) notifications.ListedReference {
		return notifications.ListedReference{State: notifications.ReferenceLive, CID: cid}
	}
	labels := func(raw string) *string { return &raw }
	root := &posts.PostView{URI: rootURI, CID: "bafyroot", Record: map[string]interface{}{"title": "Root"}}

	cases := []struct {
		name      string
		target    notifications.ListedNotification
		wantCause map[string]int64
	}{
		{
			name: "postReply record comment",
			target: notifications.ListedNotification{Reason: notifications.ReasonPostReply, ActorDID: actorDID, RootPostURI: rootURI, RootPost: live("bafyroot"),
				SubjectURI: rootURI, Subject: live("bafyroot"), RecordURI: malformedURI, Record: live("bafymalformed")},
			wantCause: map[string]int64{"omitted": 1, "by_reason.postReply": 1, "by_reason.commentReply": 0, "by_reason.other": 0},
		},
		{
			name: "comment mention record",
			target: notifications.ListedNotification{Reason: notifications.ReasonMention, ActorDID: actorDID, RootPostURI: rootURI, RootPost: live("bafyroot"),
				RecordURI: malformedURI, Record: live("bafymalformed")},
			wantCause: map[string]int64{"omitted": 1, "by_reason.postReply": 0, "by_reason.commentReply": 0, "by_reason.other": 1},
		},
		{
			name: "commentReply subject comment",
			target: notifications.ListedNotification{Reason: notifications.ReasonCommentReply, ActorDID: actorDID, RootPostURI: rootURI, RootPost: live("bafyroot"),
				SubjectURI: malformedURI, Subject: live("bafymalformed"), RecordURI: replyURI, Record: live("bafyreply")},
			wantCause: map[string]int64{"omitted": 1, "by_reason.postReply": 0, "by_reason.commentReply": 1, "by_reason.other": 0},
		},
		{
			name: "comment upvote subject",
			target: notifications.ListedNotification{Reason: notifications.ReasonUpvote, RootPostURI: rootURI, RootPost: live("bafyroot"),
				SubjectURI: malformedURI, Subject: live("bafymalformed"), UpvoteCount: 1, RecentUpvoterDIDs: []string{actorDID}},
			wantCause: map[string]int64{"omitted": 1, "by_reason.postReply": 0, "by_reason.commentReply": 0, "by_reason.other": 1},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			captured := captureDefaultLogs(t)
			neighbour := notifications.ListedNotification{Reason: notifications.ReasonPostReply, ActorDID: actorDID, RootPostURI: rootURI, RootPost: live("bafyroot"),
				SubjectURI: rootURI, Subject: live("bafyroot"), RecordURI: neighbourURI, Record: live("bafyneighbour"), RecordCreatedAt: at, SortAt: at.Add(time.Minute)}
			target := tc.target
			target.RecordCreatedAt, target.SortAt = at, at
			placeholder := notifications.ListedNotification{Reason: notifications.ReasonCommentReply, ActorDID: actorDID, RootPostURI: rootURI, RootPost: live("bafyroot"),
				SubjectURI: placeholderParentURI, Subject: notifications.ListedReference{State: notifications.ReferenceDeleted, CID: "bafyparentdeleted"},
				RecordURI: placeholderRecordURI, Record: notifications.ListedReference{State: notifications.ReferenceRemovedByModerator, CID: "bafyrecordremoved"},
				RecordCreatedAt: at, SortAt: at.Add(-time.Minute)}
			commentViews := cannedComments{
				neighbourURI:         {URI: neighbourURI, CID: "bafyneighbour", Content: "Neighbour", ContentLabels: labels(validLabels)},
				malformedURI:         {URI: malformedURI, CID: "bafymalformed", Content: "Malformed", ContentLabels: labels(wrongShape)},
				replyURI:             {URI: replyURI, CID: "bafyreply", Content: "Reply"},
				placeholderRecordURI: {URI: placeholderRecordURI, CID: "bafyrecordremoved", Content: "Must not leak", ContentLabels: labels(wrongShape)},
				placeholderParentURI: {URI: placeholderParentURI, CID: "bafyparentdeleted", Content: "Must not leak", ContentLabels: labels(wrongShape)},
			}
			page := notifications.ListPage{Notifications: []notifications.ListedNotification{neighbour, target, placeholder}}
			service := notifications.NewListService(cannedListReader{page: page}, cannedProfiles{}, anonymousPosts{rootURI: root}, commentViews)

			got, err := service.ListNotifications(context.Background(), "did:plc:owner", "", 50)
			require.NoError(t, err)

			listed := make([]string, 0, len(got.Notifications))
			for _, view := range got.Notifications {
				if view.Record == nil {
					listed = append(listed, string(view.Reason)+" subject "+view.Subject.URI)
					continue
				}
				listed = append(listed, string(view.Reason)+" "+view.Record.URI)
			}
			require.Equal(t, []string{"postReply " + neighbourURI, "commentReply " + placeholderRecordURI}, listed,
				"the malformed live row must be omitted; the valid neighbour and the placeholder must stay listed")
			neighbourLabels, err := json.Marshal(got.Notifications[0].Record.Labels)
			require.NoError(t, err)
			require.JSONEq(t, validLabels, string(neighbourLabels))
			require.Equal(t, "removedByModerator", got.Notifications[1].Record.Status)
			require.Equal(t, "deleted", got.Notifications[1].Subject.Status)

			var omission *slog.Record
			for _, record := range captured() {
				if record.Message == "notifications: omitted unhydratable rows" {
					require.Nil(t, omission, "expected one omission warning per page")
					record := record
					omission = &record
				}
			}
			require.NotNil(t, omission, "the malformed row must be counted in the omission warning")
			attrs := []slog.Attr{}
			omission.Attrs(func(attr slog.Attr) bool {
				attrs = append(attrs, attr)
				return true
			})
			flat := map[string]slog.Value{}
			flattenLogAttrs("", attrs, flat)
			counts := map[string]int64{}
			for key, value := range flat {
				counts[key] = value.Int64()
			}
			want := map[string]int64{
				"by_cause.missing_record":      0,
				"by_cause.deleted_record":      0,
				"by_cause.missing_root":        0,
				"by_cause.missing_subject":     0,
				"by_cause.deleted_subject":     0,
				"by_cause.unrecognized_reason": 0,
				"by_cause.malformed_labels":    1,
			}
			for key, value := range tc.wantCause {
				want[key] = value
			}
			require.Equal(t, want, counts)
		})
	}
}
