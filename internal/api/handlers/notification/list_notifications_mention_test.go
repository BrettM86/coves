//go:build integration

package notification_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"Coves/internal/core/notifications"
	"Coves/internal/db/postgres"

	"github.com/stretchr/testify/require"
)

func (f *listHandlerFixture) addMention(recipient, recordURI, oldCID, actor, rootURI string, createdAt, sortAt time.Time) {
	f.t.Helper()
	_, err := f.db.ExecContext(context.Background(), `INSERT INTO notifications (recipient_did, reason, record_uri, record_cid, actor_did, subject_uri, root_post_uri, record_created_at, sort_at) VALUES ($1, 'mention', $2, $3, $4, NULL, $5, $6, $7)`, recipient, recordURI, oldCID, actor, rootURI, createdAt.UTC().Truncate(time.Microsecond), sortAt.UTC().Truncate(time.Microsecond))
	require.NoError(f.t, err)
	f.expected[recipient] = append(f.expected[recipient], recordURI)
}

func TestListNotifications_CommentMentionFullShape(t *testing.T) {
	for _, tc := range []struct{ name, content, want string }{
		{"long multi-codepoint excerpt", strings.Repeat("a", 139) + "👩‍👩‍👧‍👦" + strings.Repeat("b", 160), strings.Repeat("a", 139) + "👩‍👩‍👧‍👦"},
		{"short content", "Hello recipient", "Hello recipient"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newListHandlerFixture(t)
			thread := f.seedThread("Root title", "Root body")
			at := time.Date(2026, 9, 20, 9, 1, 0, 123456000, time.UTC)
			uri := "at://" + thread.actor + "/social.coves.community.comment/mention"
			f.addComment(uri, "bafycurrentcomment", "mention", thread.actor, thread.root, "bafystaleroot", thread.root, "bafystaleparent", tc.content, at)
			f.addMention(thread.caller, uri, "bafystoredcomment", thread.actor, thread.root, at.Add(time.Minute), at.Add(2*time.Minute))
			response := f.placeholderRequest(thread.caller, "")
			require.Equal(t, http.StatusOK, response.status, "response: %s", response.raw)
			rows := listRawRows(t, response)
			require.Len(t, rows, 1, "comment mention must be listed")
			row := response.body.Notifications[0]
			require.Equal(t, notifications.ReasonMention, row.Reason)
			require.Equal(t, &notifications.ProfileView{DID: thread.actor, Handle: "listactor" + f.id + ".test", DisplayName: stringPointer("Indexed Author")}, row.Author)
			require.Equal(t, &notifications.RecordView{URI: uri, CID: "bafycurrentcomment", CreatedAt: "2026-09-20T09:02:00.123456Z", Excerpt: tc.want}, row.Record)
			require.NotNil(t, row.RootPost)
			require.Equal(t, thread.root, row.RootPost.URI)
			require.Equal(t, "bafyroot", row.RootPost.CID)
			require.Equal(t, "Root title", row.RootPost.Title)
			require.NotNil(t, row.RootPost.Community)
			require.Equal(t, thread.community, row.RootPost.Community.DID)
			require.Equal(t, "Thread Community", row.RootPost.Community.Name)
			require.NotContains(t, rows[0], "subject")
			require.NotContains(t, rows[0], "upvoteCount")
		})
	}
}

func TestListNotifications_PostMentionTitleOrBodyExcerpt(t *testing.T) {
	for _, tc := range []struct{ name, title, body, want string }{
		{"title truncated on grapheme", strings.Repeat("T", 139) + "🇺🇸" + strings.Repeat("X", 160), "Other body", strings.Repeat("T", 139) + "🇺🇸"},
		{"untitled body truncated on grapheme", "", strings.Repeat("b", 139) + "👩‍👩‍👧‍👦" + strings.Repeat("c", 160), strings.Repeat("b", 139) + "👩‍👩‍👧‍👦"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newListHandlerFixture(t)
			thread := f.seedThread("Existing root", "Existing body")
			at := time.Date(2026, 9, 20, 9, 1, 0, 123456000, time.UTC)
			uri := "at://" + thread.actor + "/social.coves.community.postv2/mention"
			f.addPost(uri, "bafycurrentpost", "mention", thread.actor, thread.community, tc.title, tc.body, at)
			f.addMention(thread.caller, uri, "bafystoredpost", thread.actor, uri, at.Add(time.Minute), at.Add(2*time.Minute))
			response := f.placeholderRequest(thread.caller, "")
			require.Equal(t, http.StatusOK, response.status, "response: %s", response.raw)
			rows := listRawRows(t, response)
			require.Len(t, rows, 1, "post mention must be listed")
			row := response.body.Notifications[0]
			require.Equal(t, notifications.ReasonMention, row.Reason)
			require.Equal(t, &notifications.RecordView{URI: uri, CID: "bafycurrentpost", CreatedAt: "2026-09-20T09:02:00.123456Z", Excerpt: tc.want}, row.Record)
			require.NotNil(t, row.RootPost)
			require.Equal(t, uri, row.RootPost.URI)
			require.Equal(t, "bafycurrentpost", row.RootPost.CID)
			require.Equal(t, tc.title, row.RootPost.Title, "root title is complete")
			require.NotNil(t, row.RootPost.Community)
			require.Equal(t, thread.community, row.RootPost.Community.DID)
			require.NotContains(t, rows[0], "subject")
			require.NotContains(t, rows[0], "upvoteCount")
			if tc.title == "" {
				require.NotContains(t, placeholderObject(t, rows[0], "rootPost"), "title")
			}
		})
	}
}

func TestListNotifications_MentionPlaceholders(t *testing.T) {
	for _, tc := range []struct {
		name               string
		postMention        bool
		withdrawal, status string
		deletedComment     bool
	}{
		{"post author deleted", true, "authorDelete", "deleted", false},
		{"post moderator removed", true, "communityWithdrawal", "removedByModerator", false},
		{"comment deleted", false, "", "deleted", true},
		{"comment under deleted root", false, "authorDelete", "deleted", false},
		{"comment under removed root", false, "communityWithdrawal", "removedByModerator", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newListHandlerFixture(t)
			thread := f.seedThread("Root title", "Private root body")
			at := time.Date(2026, 9, 20, 9, 1, 0, 123456000, time.UTC)
			recordURI, rootURI := "at://"+thread.actor+"/social.coves.community.comment/mention", thread.root
			recordCID := "bafycurrentcomment"
			if tc.postMention {
				recordURI = "at://" + thread.actor + "/social.coves.community.postv2/mention"
				rootURI = recordURI
				recordCID = "bafycurrentpost"
				f.addPost(recordURI, recordCID, "mention", thread.actor, thread.community, "Private post title", "Private post body", at)
			} else {
				f.addComment(recordURI, recordCID, "mention", thread.actor, rootURI, "bafystaleroot", rootURI, "bafystaleroot", "Comment visible when live", at)
			}
			f.addMention(thread.caller, recordURI, "bafystoredold", thread.actor, rootURI, at, at)
			if tc.deletedComment {
				f.softDeleteComment(recordURI)
			}
			if tc.withdrawal != "" {
				f.withdrawPost(rootURI, tc.withdrawal)
			}
			response := f.placeholderRequest(thread.caller, "")
			require.Equal(t, http.StatusOK, response.status, "response: %s", response.raw)
			rows := listRawRows(t, response)
			require.Len(t, rows, 1, "mention placeholder must remain listed")
			require.Equal(t, notifications.ReasonMention, response.body.Notifications[0].Reason)
			require.NotContains(t, rows[0], "subject")
			require.NotContains(t, rows[0], "upvoteCount")
			record := placeholderObject(t, rows[0], "record")
			root := placeholderObject(t, rows[0], "rootPost")
			require.JSONEq(t, `"`+recordURI+`"`, string(record["uri"]))
			require.JSONEq(t, `"`+recordCID+`"`, string(record["cid"]))
			require.JSONEq(t, `"2026-09-20T09:01:00.123456Z"`, string(record["createdAt"]))
			require.JSONEq(t, `"`+rootURI+`"`, string(root["uri"]))
			if tc.postMention {
				require.JSONEq(t, `"bafycurrentpost"`, string(root["cid"]))
				require.JSONEq(t, `"`+tc.status+`"`, string(record["status"]))
				require.NotContains(t, record, "excerpt")
				require.JSONEq(t, `"`+tc.status+`"`, string(root["status"]))
				require.NotContains(t, root, "title")
				require.NotContains(t, root, "community")
			} else if tc.deletedComment {
				require.JSONEq(t, `"deleted"`, string(record["status"]))
				require.NotContains(t, record, "excerpt")
				require.JSONEq(t, `"bafyroot"`, string(root["cid"]))
				require.JSONEq(t, `"Root title"`, string(root["title"]))
				require.Contains(t, root, "community")
				require.NotContains(t, root, "status")
			} else {
				require.JSONEq(t, `"Comment visible when live"`, string(record["excerpt"]))
				require.NotContains(t, record, "status")
				require.JSONEq(t, `"bafyroot"`, string(root["cid"]))
				require.JSONEq(t, `"`+tc.status+`"`, string(root["status"]))
				require.NotContains(t, root, "title")
				require.NotContains(t, root, "community")
			}
		})
	}
}

func TestListNotifications_MixedMentionsBatchHydration(t *testing.T) {
	for _, perKind := range []int{2, 12} {
		t.Run(fmt.Sprintf("%d of each kind", perKind), func(t *testing.T) {
			f := newListHandlerFixture(t)
			thread := f.seedThread("Base root", "Base body")
			second := "did:plc:mentionsecond" + f.id
			f.addUser(second, "mentionsecond"+f.id+".test", "Second Author")
			actors := []string{thread.actor, second}
			at := time.Date(2026, 9, 20, 9, 1, 0, 0, time.UTC)
			want := map[string]notifications.Reason{}
			for index := 0; index < perKind; index++ {
				actor := actors[index%2]
				rootKey := fmt.Sprintf("mixroot%d", index)
				rootURI := "at://" + thread.caller + "/social.coves.community.postv2/" + rootKey
				rootCID := "bafy" + rootKey
				f.addPost(rootURI, rootCID, rootKey, thread.caller, thread.community, "Root "+rootKey, "Root body", at)
				parentKey := fmt.Sprintf("mixparent%d", index)
				parentURI := "at://" + thread.caller + "/social.coves.community.comment/" + parentKey
				f.addComment(parentURI, "bafy"+parentKey, parentKey, thread.caller, rootURI, rootCID, rootURI, rootCID, "Parent "+parentKey, at)
				for _, reason := range []notifications.Reason{notifications.ReasonPostReply, notifications.ReasonCommentReply, notifications.ReasonMention} {
					key := fmt.Sprintf("mixed%s%d", reason, index)
					uri := "at://" + actor + "/social.coves.community.comment/" + key
					f.addComment(uri, "bafy"+key, key, actor, rootURI, rootCID, rootURI, rootCID, "Text "+key, at)
					if reason == notifications.ReasonMention {
						f.addMention(thread.caller, uri, "bafystored"+key, actor, rootURI, at, at)
					} else {
						subject := rootURI
						if reason == notifications.ReasonCommentReply {
							subject = parentURI
						}
						f.addReply(thread.caller, reason, uri, "bafystored"+key, actor, subject, rootURI, at, at)
					}
					want[uri] = reason
				}
				key := fmt.Sprintf("mixedpost%d", index)
				uri := "at://" + actor + "/social.coves.community.postv2/" + key
				f.addPost(uri, "bafy"+key, key, actor, thread.community, "Title "+key, "Body", at)
				f.addMention(thread.caller, uri, "bafystored"+key, actor, uri, at, at)
				want[uri] = notifications.ReasonMention
			}
			repo := &countedNotificationReads{ReadRepository: f.repo}
			profiles := &countedProfiles{ProfileLookup: postgres.NewUserRepository(f.db)}
			posts := &countedPosts{PostViewLookup: postgres.NewPostRepository(f.db)}
			comments := &countedComments{CommentLookup: postgres.NewCommentRepository(f.db)}
			response := f.requestWithListDependencies(thread.caller, fmt.Sprintf("limit=%d", len(want)), repo, profiles, posts, comments)
			require.Equal(t, http.StatusOK, response.status, "response: %s", response.raw)
			rows := listRawRows(t, response)
			require.Equal(t, len(want), len(rows), "all four kinds fill the page")
			seen := map[string]bool{}
			for _, row := range rows {
				var record map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(row["record"], &record))
				var uri string
				require.NoError(t, json.Unmarshal(record["uri"], &uri))
				require.Equal(t, want[uri], notifications.Reason(strings.Trim(string(row["reason"]), `"`)))
				require.NotEmpty(t, record["excerpt"], "record %s has excerpt", uri)
				require.NotEmpty(t, placeholderObject(t, row, "rootPost")["title"], "record %s has root title", uri)
				require.NotEmpty(t, row["author"])
				seen[uri] = true
			}
			require.Len(t, seen, len(want))
			require.Equal(t, 1, repo.lists)
			require.Equal(t, 1, profiles.calls)
			require.Equal(t, 1, posts.calls)
			require.Equal(t, 1, comments.calls)
		})
	}
}
