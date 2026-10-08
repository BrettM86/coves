package notifications_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"Coves/internal/core/blobs"
	"Coves/internal/core/comments"
	"Coves/internal/core/notifications"
	"Coves/internal/core/posts"

	"github.com/stretchr/testify/require"
)

func TestListNotifications_PlaceholdersOmitLabelsAndThumbnail(t *testing.T) {
	blobs.ResetImageURLConfigForTesting()
	blobs.SetImageURLConfig(blobs.ImageURLConfig{ProxyEnabled: true, ProxyBaseURL: "https://img.example.test"})
	t.Cleanup(blobs.ResetImageURLConfigForTesting)
	const rootURI = "at://did:plc:owner/social.coves.community.postv2/root"
	const subjectPostURI = "at://did:plc:owner/social.coves.community.postv2/subject"
	const mentionPostURI = "at://did:plc:actor/social.coves.community.postv2/mention"
	const subjectCommentURI = "at://did:plc:owner/social.coves.community.comment/parent"
	const recordCommentURI = "at://did:plc:actor/social.coves.community.comment/reply"
	const rootCID = "bafkreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy"
	const subjectCID = "bafkreie5737gdxlw5i64vzichcalba3z2v5n6icifvx5xytvske7mr3hpm"
	const mentionCID = "bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku"
	type position struct{ name, labels, thumbnail, alt string }
	root := position{"rootPost", `{"values":[{"val":"nsfw"}]}`, "https://img.example.test/img/content_preview/plain/did:plc:owner/bafkreigh2akiscaildcqabsyg3dfr6chu3fgpregiymsck7e7aqa4s52zy", "Root image"}
	postSubject := position{"subject", `{"values":[{"val":"spoiler"}]}`, "https://img.example.test/img/content_preview/plain/did:plc:owner/bafkreie5737gdxlw5i64vzichcalba3z2v5n6icifvx5xytvske7mr3hpm", "Subject image"}
	commentSubject := position{"subject", `{"values":[{"val":"gore"}]}`, "", ""}
	commentRecord := position{"record", `{"values":[{"val":"violence"}]}`, "", ""}
	postRecord := position{"record", `{"values":[{"val":"spoiler"}]}`, "https://img.example.test/img/content_preview/plain/did:plc:actor/bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku", "Mention image"}
	type scenario struct {
		name, placeholder       string
		reason                  notifications.Reason
		postSubject, postRecord bool
	}
	scenarios := []scenario{
		{"postReply rootPost", "rootPost", notifications.ReasonPostReply, true, false},
		{"postReply subject", "subject", notifications.ReasonPostReply, true, false},
		{"postReply record", "record", notifications.ReasonPostReply, true, false},
		{"commentReply rootPost", "rootPost", notifications.ReasonCommentReply, false, false},
		{"commentReply subject", "subject", notifications.ReasonCommentReply, false, false},
		{"commentReply record", "record", notifications.ReasonCommentReply, false, false},
		{"comment mention rootPost", "rootPost", notifications.ReasonMention, false, false},
		{"comment mention record", "record", notifications.ReasonMention, false, false},
		{"post mention rootPost", "rootPost", notifications.ReasonMention, false, true},
		{"post mention record", "record", notifications.ReasonMention, false, true},
		{"post upvote rootPost", "rootPost", notifications.ReasonUpvote, true, false},
		{"post upvote subject", "subject", notifications.ReasonUpvote, true, false},
		{"comment upvote rootPost", "rootPost", notifications.ReasonUpvote, false, false},
		{"comment upvote subject", "subject", notifications.ReasonUpvote, false, false},
	}
	for _, state := range []struct {
		name   string
		value  notifications.ReferenceState
		status string
	}{{"deleted", notifications.ReferenceDeleted, "deleted"}, {"removedByModerator", notifications.ReferenceRemovedByModerator, "removedByModerator"}, {"removedByServerAdmin", notifications.ReferenceRemovedByServerAdmin, "removedByServerAdmin"}} {
		for _, scenario := range scenarios {
			t.Run(state.name+"/"+scenario.name, func(t *testing.T) {
				at := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
				post := func(uri, owner, imageCID, alt, label string) *posts.PostView {
					return &posts.PostView{URI: uri, CID: imageCID, Author: &posts.AuthorView{DID: owner}, Community: &posts.CommunityRef{DID: "did:plc:community", Handle: "community.coves.social", Name: "Community"},
						Record: map[string]interface{}{"title": "Image", "labels": posts.SelfLabels{Values: []posts.SelfLabel{{Val: label}}}},
						Embed:  map[string]interface{}{"$type": "social.coves.embed.images", "images": []interface{}{map[string]interface{}{"alt": alt, "image": map[string]interface{}{"$type": "blob", "ref": map[string]interface{}{"$link": imageCID}, "mimeType": "image/jpeg", "size": 123}}}}}
				}
				label := func(val string) *string { value := `{"values":[{"val":"` + val + `"}]}`; return &value }
				postsByURI := anonymousPosts{
					rootURI:        post(rootURI, "did:plc:owner", rootCID, "Root image", "nsfw"),
					subjectPostURI: post(subjectPostURI, "did:plc:owner", subjectCID, "Subject image", "spoiler"),
					mentionPostURI: post(mentionPostURI, "did:plc:actor", mentionCID, "Mention image", "spoiler"),
				}
				commentsByURI := cannedComments{
					subjectCommentURI: &comments.Comment{URI: subjectCommentURI, CID: "bafyparent", Content: "Parent", ContentLabels: label("gore")},
					recordCommentURI:  &comments.Comment{URI: recordCommentURI, CID: "bafyreply", Content: "Reply", ContentLabels: label("violence")},
				}
				row := notifications.ListedNotification{Reason: scenario.reason, ActorDID: "did:plc:actor", RootPostURI: rootURI, RootPost: notifications.ListedReference{CID: "bafyroot"}, RecordURI: recordCommentURI, Record: notifications.ListedReference{CID: "bafyreply"}, RecordCreatedAt: at, SortAt: at, UpvoteCount: 1}
				expected := []position{root}
				if scenario.reason == notifications.ReasonPostReply || scenario.reason == notifications.ReasonUpvote && scenario.postSubject {
					row.SubjectURI, row.Subject.CID = subjectPostURI, "bafysubject"
					expected = append(expected, postSubject)
				} else if scenario.reason == notifications.ReasonCommentReply || scenario.reason == notifications.ReasonUpvote {
					row.SubjectURI, row.Subject.CID = subjectCommentURI, "bafyparent"
					expected = append(expected, commentSubject)
				}
				if scenario.postRecord {
					row.RecordURI, row.Record.CID = mentionPostURI, "bafymention"
				}
				if scenario.reason != notifications.ReasonUpvote {
					if scenario.postRecord {
						expected = append(expected, postRecord)
					} else {
						expected = append(expected, commentRecord)
					}
				}
				switch scenario.placeholder {
				case "rootPost":
					row.RootPost.State = state.value
				case "subject":
					row.Subject.State = state.value
				case "record":
					row.Record.State = state.value
				}
				service := notifications.NewListService(cannedListReader{page: notifications.ListPage{Notifications: []notifications.ListedNotification{row}}}, cannedProfiles{}, postsByURI, commentsByURI)
				got, err := service.ListNotifications(context.Background(), "did:plc:owner", "", 50)
				require.NoError(t, err)
				require.Len(t, got.Notifications, 1)
				encoded, err := json.Marshal(got)
				require.NoError(t, err)
				var page struct {
					Notifications []map[string]json.RawMessage `json:"notifications"`
				}
				require.NoError(t, json.Unmarshal(encoded, &page))
				for _, sibling := range expected {
					var reference map[string]json.RawMessage
					require.NoError(t, json.Unmarshal(page.Notifications[0][sibling.name], &reference))
					if sibling.name == scenario.placeholder {
						require.Contains(t, reference, "status", sibling.name)
						require.JSONEq(t, `"`+state.status+`"`, string(reference["status"]), sibling.name)
						for _, field := range []string{"labels", "thumbnail", "thumbnailAlt", "title", "community", "preview", "excerpt"} {
							require.NotContains(t, reference, field, sibling.name)
						}
						continue
					}
					require.NotContains(t, reference, "status", sibling.name)
					require.Contains(t, reference, "labels", sibling.name+" labels")
					require.JSONEq(t, sibling.labels, string(reference["labels"]), sibling.name+" labels")
					if sibling.thumbnail != "" {
						require.Contains(t, reference, "thumbnail", sibling.name+" thumbnail")
						require.Contains(t, reference, "thumbnailAlt", sibling.name+" thumbnailAlt")
						require.JSONEq(t, `"`+sibling.thumbnail+`"`, string(reference["thumbnail"]), sibling.name+" thumbnail")
						require.JSONEq(t, `"`+sibling.alt+`"`, string(reference["thumbnailAlt"]), sibling.name+" thumbnailAlt")
					} else {
						require.NotContains(t, reference, "thumbnail", sibling.name)
						require.NotContains(t, reference, "thumbnailAlt", sibling.name)
					}
				}
			})
		}
	}
}
