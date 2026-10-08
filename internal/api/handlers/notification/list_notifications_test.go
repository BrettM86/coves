//go:build integration

package notification_test

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"Coves/internal/api/handlers/notification"
	"Coves/internal/api/middleware"
	"Coves/internal/core/blobs"
	"Coves/internal/core/notifications"
	"Coves/internal/core/posts"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

type listHandlerFixture struct {
	t        *testing.T
	id       string
	db       *sql.DB
	repo     notifications.ReadRepository
	expected map[string][]string
}

func newListHandlerFixture(t *testing.T) *listHandlerFixture {
	t.Helper()
	db := testkit.DB(t)
	return &listHandlerFixture{t: t, id: testkit.UniqueID(t), db: db, repo: postgres.NewNotificationRepository(db).(notifications.ReadRepository), expected: make(map[string][]string)}
}

func (f *listHandlerFixture) addUser(did, handle, displayName string) {
	f.t.Helper()
	_, err := f.db.ExecContext(context.Background(), `INSERT INTO users (did, handle, pds_url, display_name) VALUES ($1, $2, $3, $4)`, did, handle, "https://pds.test", displayName)
	require.NoError(f.t, err)
}

func (f *listHandlerFixture) addCommunity(did, ownerDID, handle, name string, at time.Time) {
	f.t.Helper()
	_, err := f.db.ExecContext(context.Background(), `INSERT INTO communities (did, handle, name, owner_did, created_by_did, hosted_by_did, created_at) VALUES ($1, $2, $3, $4, $4, $4, $5)`, did, handle, name, ownerDID, at.UTC().Truncate(time.Microsecond))
	require.NoError(f.t, err)
}

func (f *listHandlerFixture) addPost(uri, cid, rkey, authorDID, communityDID, title, content string, at time.Time) {
	f.t.Helper()
	_, err := f.db.ExecContext(context.Background(), `INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, content, created_at, score, upvote_count, downvote_count) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 1, 1, 0)`, uri, cid, rkey, authorDID, communityDID, title, content, at.UTC().Truncate(time.Microsecond))
	require.NoError(f.t, err)
	_, err = f.db.ExecContext(context.Background(), `INSERT INTO community_post_admissions (community_did, post_uri, status, acceptance_uri, acceptance_rkey, accepted_cid, evaluated_cid, last_community_rev, last_community_op_rank, created_at, updated_at) VALUES ($1, $2, 'accepted', $3, $4, $5, $5, '3lqqqqqqqqqq2', $6, $7, $7)`, communityDID, uri, "at://"+communityDID+"/social.coves.community.acceptance/"+rkey, rkey, cid, int16(posts.CommunityOpPut), at.UTC().Truncate(time.Microsecond))
	require.NoError(f.t, err)
}

func (f *listHandlerFixture) addComment(uri, cid, rkey, authorDID, rootURI, rootCID, parentURI, parentCID, content string, at time.Time) {
	f.t.Helper()
	_, err := f.db.ExecContext(context.Background(), `INSERT INTO comments (uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid, content, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`, uri, cid, rkey, authorDID, rootURI, rootCID, parentURI, parentCID, content, at.UTC().Truncate(time.Microsecond))
	require.NoError(f.t, err)
}

func (f *listHandlerFixture) addReply(recipientDID string, reason notifications.Reason, recordURI, recordCID, actorDID, subjectURI, rootURI string, recordCreatedAt, sortAt time.Time) {
	f.t.Helper()
	_, err := f.db.ExecContext(context.Background(), `INSERT INTO notifications (recipient_did, reason, record_uri, record_cid, actor_did, subject_uri, root_post_uri, record_created_at, sort_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, recipientDID, reason, recordURI, recordCID, actorDID, subjectURI, rootURI, recordCreatedAt.UTC().Truncate(time.Microsecond), sortAt.UTC().Truncate(time.Microsecond))
	require.NoError(f.t, err)
	f.expected[recipientDID] = append(f.expected[recipientDID], recordURI)
}

type listThread struct {
	caller, actor, community, root string
}

func (f *listHandlerFixture) seedThread(title, content string) listThread {
	f.t.Helper()
	thread := listThread{
		caller:    "did:plc:listcaller" + f.id,
		actor:     "did:plc:listactor" + f.id,
		community: "did:plc:listcommunity" + f.id,
	}
	thread.root = "at://" + thread.caller + "/social.coves.community.postv2/root"
	f.addUser(thread.caller, "listcaller"+f.id+".test", "Caller")
	f.addUser(thread.actor, "listactor"+f.id+".test", "Indexed Author")
	base := time.Date(2026, time.September, 20, 9, 0, 0, 0, time.UTC)
	f.addCommunity(thread.community, thread.caller, "listcommunity"+f.id+".coves.social", "Thread Community", base)
	f.addPost(thread.root, "bafyroot", "root", thread.caller, thread.community, title, content, base)
	return thread
}

func (f *listHandlerFixture) seedListedReplies(count int) (listThread, []string) {
	f.t.Helper()
	thread := f.seedThread("Thread title", "Thread body")
	base := time.Date(2026, time.September, 20, 9, 0, 0, 0, time.UTC)
	var uris []string
	for i := 0; i < count; i++ {
		rkey := fmt.Sprintf("reply%d", i)
		uri := "at://" + thread.actor + "/social.coves.community.comment/" + rkey
		at := base.Add(time.Duration(i+1) * time.Second)
		f.addComment(uri, "bafy"+rkey, rkey, thread.actor, thread.root, "bafyroot", thread.root, "bafyroot", "Reply", at)
		f.addReply(thread.caller, notifications.ReasonPostReply, uri, "bafy"+rkey, thread.actor, thread.root, thread.root, at, at)
		uris = append(uris, uri)
	}
	return thread, uris
}

func listRawRows(t *testing.T, response listHandlerResponse) []map[string]json.RawMessage {
	t.Helper()
	var body struct {
		Notifications []map[string]json.RawMessage `json:"notifications"`
	}
	require.NoError(t, json.Unmarshal(response.raw, &body))
	return body.Notifications
}

func listXRPCError(t *testing.T, response listHandlerResponse) (string, string) {
	t.Helper()
	var body struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	require.NoError(t, json.Unmarshal(response.raw, &body))
	return body.Error, body.Message
}

type listHandlerResponse struct {
	status int
	body   notifications.ListNotificationsOutput
	raw    []byte
}

func (f *listHandlerFixture) request(callerDID, query string) listHandlerResponse {
	f.t.Helper()
	// A missing row in the real chunk-16 reader is a fixture defect, not a
	// listNotifications handler failure. Check every seeded recipient.
	for did, expected := range f.expected {
		found := make(map[string]bool, len(expected))
		cursor := ""
		for {
			page, err := f.repo.List(context.Background(), did, cursor, 100)
			require.NoError(f.t, err)
			for _, row := range page.Notifications {
				found[row.RecordURI] = true
			}
			if page.Cursor == "" {
				break
			}
			cursor = page.Cursor
		}
		for _, uri := range expected {
			if !found[uri] {
				f.t.Fatalf("list fixture: ReadRepository.List hid seeded record %s for %s", uri, did)
			}
		}
	}
	handler := notification.NewListHandler(notifications.NewListService(f.repo, postgres.NewUserRepository(f.db), postgres.NewPostRepository(f.db), postgres.NewCommentRepository(f.db)))
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.notification.listNotifications?"+query, nil)
	req = req.WithContext(middleware.SetTestUserDID(req.Context(), callerDID))
	rec := httptest.NewRecorder()
	handler.HandleListNotifications(rec, req)
	result := listHandlerResponse{status: rec.Code, raw: rec.Body.Bytes()}
	if rec.Code == http.StatusOK && len(result.raw) > 0 {
		require.NoError(f.t, json.Unmarshal(result.raw, &result.body), "response: %s", result.raw)
	}
	return result
}

func TestListNotifications_PostReplyFullShape(t *testing.T) {
	f := newListHandlerFixture(t)
	caller, other, replier := "did:plc:listcaller"+f.id, "did:plc:listother"+f.id, "did:plc:listreplier"+f.id
	f.addUser(caller, "listcaller"+f.id+".test", "Caller")
	f.addUser(other, "listother"+f.id+".test", "Other")
	f.addUser(replier, "listreplier"+f.id+".test", "Reply Author")
	base := time.Date(2026, time.September, 20, 9, 0, 0, 0, time.UTC)
	community := "did:plc:listcommunity" + f.id
	f.addCommunity(community, caller, "listcommunity"+f.id+".coves.social", "List Community", base)
	postURI := "at://" + caller + "/social.coves.community.postv2/root"
	otherPostURI := "at://" + other + "/social.coves.community.postv2/other"
	f.addPost(postURI, "bafycurrentpost", "root", caller, community, "Current post title", "Current post content", base)
	f.addPost(otherPostURI, "bafyotherpost", "other", other, community, "Other title", "Other content", base)
	replyURI := "at://" + replier + "/social.coves.community.comment/reply"
	otherReplyURI := "at://" + replier + "/social.coves.community.comment/otherreply"
	f.addComment(replyURI, "bafycurrentreply", "reply", replier, postURI, "bafystaleroot", postURI, "bafystaleparent", "A clear answer.", base.Add(time.Minute))
	f.addComment(otherReplyURI, "bafyotherreply", "otherreply", replier, otherPostURI, "bafyotherpost", otherPostURI, "bafyotherpost", "Not for caller", base.Add(time.Minute))
	f.addReply(caller, notifications.ReasonPostReply, replyURI, "bafystalereply", replier, postURI, postURI, base.Add(2*time.Minute), base.Add(3*time.Minute))
	f.addReply(other, notifications.ReasonPostReply, otherReplyURI, "bafyotherreply", replier, otherPostURI, otherPostURI, base.Add(2*time.Minute), base.Add(4*time.Minute))

	response := f.request(caller, "did="+other+"&recipient="+other)
	require.Equal(t, http.StatusOK, response.status, "listNotifications response: %s", response.raw)
	require.Contains(t, string(response.raw), `"notifications"`, "listNotifications must return a JSON response")
	require.Len(t, response.body.Notifications, 1, "only the authenticated caller's reply is returned")
	row := response.body.Notifications[0]
	require.Equal(t, notifications.ReasonPostReply, row.Reason)
	require.Equal(t, "2026-09-20T09:03:00Z", row.SortAt)
	require.Equal(t, &notifications.ProfileView{DID: replier, Handle: "listreplier" + f.id + ".test", DisplayName: stringPointer("Reply Author")}, row.Author)
	require.Equal(t, &notifications.RecordView{URI: replyURI, CID: "bafycurrentreply", Excerpt: "A clear answer.", CreatedAt: "2026-09-20T09:02:00Z"}, row.Record)
	require.Equal(t, &notifications.SubjectView{URI: postURI, CID: "bafycurrentpost", Preview: "Current post title"}, row.Subject)
	require.NotNil(t, row.RootPost)
	require.Equal(t, postURI, row.RootPost.URI)
	require.Equal(t, "bafycurrentpost", row.RootPost.CID)
	require.Equal(t, "Current post title", row.RootPost.Title)
	require.NotNil(t, row.RootPost.Community)
	require.Equal(t, community, row.RootPost.Community.DID)
	require.Equal(t, "List Community", row.RootPost.Community.Name)
	var raw struct {
		Notifications []map[string]json.RawMessage `json:"notifications"`
	}
	require.NoError(t, json.Unmarshal(response.raw, &raw))
	for _, key := range []string{"upvoteCount", "recentUpvoters", "status"} {
		require.NotContains(t, raw.Notifications[0], key)
	}
}

func stringPointer(value string) *string { return &value }

func TestListNotifications_CommentReplySubjectAndRoot(t *testing.T) {
	f := newListHandlerFixture(t)
	thread := f.seedThread("A different root", "Root body")
	base := time.Date(2026, time.September, 20, 9, 0, 0, 0, time.UTC)
	parentURI := "at://" + thread.caller + "/social.coves.community.comment/parent"
	replyURI := "at://" + thread.actor + "/social.coves.community.comment/child"
	f.addComment(parentURI, "bafyparent", "parent", thread.caller, thread.root, "bafyroot", thread.root, "bafyroot", "A parent's own words", base.Add(time.Minute))
	f.addComment(replyURI, "bafychild", "child", thread.actor, thread.root, "bafyroot", parentURI, "bafyparent", "Child reply", base.Add(2*time.Minute))
	f.addReply(thread.caller, notifications.ReasonCommentReply, replyURI, "bafychild", thread.actor, parentURI, thread.root, base.Add(2*time.Minute), base.Add(3*time.Minute))

	response := f.request(thread.caller, "")
	require.Equal(t, http.StatusOK, response.status, "response: %s", response.raw)
	require.Len(t, response.body.Notifications, 1)
	row := response.body.Notifications[0]
	require.Equal(t, notifications.ReasonCommentReply, row.Reason)
	require.Equal(t, &notifications.SubjectView{URI: parentURI, CID: "bafyparent", Preview: "A parent's own words"}, row.Subject)
	require.NotNil(t, row.RootPost)
	require.Equal(t, thread.root, row.RootPost.URI)
	require.Equal(t, "bafyroot", row.RootPost.CID)
	require.Equal(t, "A different root", row.RootPost.Title)
	require.NotNil(t, row.RootPost.Community)
	require.Equal(t, thread.community, row.RootPost.Community.DID)
	require.Equal(t, "Thread Community", row.RootPost.Community.Name)
}

func TestListNotifications_PostPreviewTitleOrBodyExcerpt(t *testing.T) {
	for _, tc := range []struct {
		name, title, content, want string
	}{
		{"long title is complete", strings.Repeat("T", 149) + "🇺🇸", "Other body", strings.Repeat("T", 149) + "🇺🇸"},
		{"untitled body is truncated", "", strings.Repeat("b", 139) + "👩‍👩‍👧‍👦" + strings.Repeat("c", 160), strings.Repeat("b", 139) + "👩‍👩‍👧‍👦"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newListHandlerFixture(t)
			thread := f.seedThread(tc.title, tc.content)
			replyURI := "at://" + thread.actor + "/social.coves.community.comment/reply"
			at := time.Date(2026, time.September, 20, 9, 1, 0, 0, time.UTC)
			f.addComment(replyURI, "bafyreply", "reply", thread.actor, thread.root, "bafyroot", thread.root, "bafyroot", "Reply", at)
			f.addReply(thread.caller, notifications.ReasonPostReply, replyURI, "bafyreply", thread.actor, thread.root, thread.root, at, at)

			response := f.request(thread.caller, "")
			require.Equal(t, http.StatusOK, response.status, "response: %s", response.raw)
			require.Len(t, response.body.Notifications, 1)
			require.Equal(t, &notifications.SubjectView{URI: thread.root, CID: "bafyroot", Preview: tc.want}, response.body.Notifications[0].Subject)
		})
	}
}

func TestListNotifications_GraphemeTruncation(t *testing.T) {
	for _, tc := range []struct {
		name, parent, reply, wantParent, wantReply string
	}{
		{
			"300 clusters cut on multi-codepoint cluster",
			strings.Repeat("a", 139) + "👩‍👩‍👧‍👦" + strings.Repeat("b", 160),
			strings.Repeat("c", 139) + "🇺🇸" + strings.Repeat("d", 160),
			strings.Repeat("a", 139) + "👩‍👩‍👧‍👦",
			strings.Repeat("c", 139) + "🇺🇸",
		},
		{
			"exactly 140 clusters unchanged",
			strings.Repeat("e", 139) + "👩‍👩‍👧‍👦",
			strings.Repeat("f", 139) + "🇺🇸",
			strings.Repeat("e", 139) + "👩‍👩‍👧‍👦",
			strings.Repeat("f", 139) + "🇺🇸",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newListHandlerFixture(t)
			thread := f.seedThread("Root", "Root body")
			parentURI := "at://" + thread.caller + "/social.coves.community.comment/parent"
			replyURI := "at://" + thread.actor + "/social.coves.community.comment/reply"
			at := time.Date(2026, time.September, 20, 9, 0, 0, 0, time.UTC)
			f.addComment(parentURI, "bafyparent", "parent", thread.caller, thread.root, "bafyroot", thread.root, "bafyroot", tc.parent, at)
			f.addComment(replyURI, "bafyreply", "reply", thread.actor, thread.root, "bafyroot", parentURI, "bafyparent", tc.reply, at.Add(time.Minute))
			f.addReply(thread.caller, notifications.ReasonCommentReply, replyURI, "bafyreply", thread.actor, parentURI, thread.root, at.Add(time.Minute), at.Add(time.Minute))

			response := f.request(thread.caller, "")
			require.Equal(t, http.StatusOK, response.status, "response: %s", response.raw)
			require.Len(t, response.body.Notifications, 1)
			require.NotNil(t, response.body.Notifications[0].Subject)
			require.NotNil(t, response.body.Notifications[0].Record)
			require.Equal(t, tc.wantParent, response.body.Notifications[0].Subject.Preview)
			require.Equal(t, tc.wantReply, response.body.Notifications[0].Record.Excerpt)
		})
	}
}

func TestListNotifications_UnindexedAuthorIsDIDOnly(t *testing.T) {
	f := newListHandlerFixture(t)
	thread := f.seedThread("Root", "Body")
	unindexed := "did:plc:listunindexed" + f.id
	base := time.Date(2026, time.September, 20, 9, 0, 0, 0, time.UTC)
	for index, actor := range []string{thread.actor, unindexed} {
		rkey := fmt.Sprintf("reply%d", index)
		uri := "at://" + actor + "/social.coves.community.comment/" + rkey
		at := base.Add(time.Duration(index+1) * time.Minute)
		f.addComment(uri, "bafy"+rkey, rkey, actor, thread.root, "bafyroot", thread.root, "bafyroot", "Reply", at)
		f.addReply(thread.caller, notifications.ReasonPostReply, uri, "bafy"+rkey, actor, thread.root, thread.root, at, at)
	}
	response := f.request(thread.caller, "")
	require.Equal(t, http.StatusOK, response.status, "response: %s", response.raw)
	require.Len(t, response.body.Notifications, 2)
	rows := listRawRows(t, response)
	require.Len(t, rows, 2)
	var unindexedAuthor, indexedAuthor map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rows[0]["author"], &unindexedAuthor))
	require.Equal(t, map[string]json.RawMessage{"did": json.RawMessage(`"` + unindexed + `"`)}, unindexedAuthor)
	require.NoError(t, json.Unmarshal(rows[1]["author"], &indexedAuthor))
	require.JSONEq(t, `"`+thread.actor+`"`, string(indexedAuthor["did"]))
	require.JSONEq(t, `"listactor`+f.id+`.test"`, string(indexedAuthor["handle"]))
	require.JSONEq(t, `"Indexed Author"`, string(indexedAuthor["displayName"]))
}

func TestListNotifications_IndexedAuthorAvatar(t *testing.T) {
	blobs.ResetImageURLConfigForTesting()
	f := newListHandlerFixture(t)
	thread := f.seedThread("Root", "Body")
	avatarDID := "did:plc:listavatarauthor"
	f.addUser(avatarDID, "listavatar"+f.id+".test", "Avatar Author")
	_, err := f.db.ExecContext(context.Background(), `UPDATE users SET pds_url = $1, avatar_cid = $2 WHERE did = $3`, "https://avatars.test", "bafyavatar", avatarDID)
	require.NoError(t, err)

	base := time.Date(2026, time.September, 20, 9, 0, 0, 0, time.UTC)
	for index, actor := range []string{avatarDID, thread.actor} {
		rkey := fmt.Sprintf("avatarreply%d", index)
		uri := "at://" + actor + "/social.coves.community.comment/" + rkey
		at := base.Add(time.Duration(index+1) * time.Minute)
		f.addComment(uri, "bafy"+rkey, rkey, actor, thread.root, "bafyroot", thread.root, "bafyroot", "Reply", at)
		f.addReply(thread.caller, notifications.ReasonPostReply, uri, "bafy"+rkey, actor, thread.root, thread.root, at, at)
	}

	response := f.request(thread.caller, "")
	require.Equal(t, http.StatusOK, response.status, "response: %s", response.raw)
	rows := listRawRows(t, response)
	require.Len(t, rows, 2)
	var withoutAvatar, withAvatar map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rows[0]["author"], &withoutAvatar))
	require.JSONEq(t, `"`+thread.actor+`"`, string(withoutAvatar["did"]))
	require.NotContains(t, withoutAvatar, "avatar")
	require.NoError(t, json.Unmarshal(rows[1]["author"], &withAvatar))
	require.JSONEq(t, `"did:plc:listavatarauthor"`, string(withAvatar["did"]))
	require.JSONEq(t, `"https://avatars.test/xrpc/com.atproto.sync.getBlob?did=did%3Aplc%3Alistavatarauthor&cid=bafyavatar"`, string(withAvatar["avatar"]))
}

func TestListNotifications_LimitValidation(t *testing.T) {
	f := newListHandlerFixture(t)
	thread, _ := f.seedListedReplies(101)
	for _, tc := range []struct {
		name, query string
		count       int
		valid       bool
	}{
		{"one", "limit=1", 1, true},
		{"hundred", "limit=100", 100, true},
		{"default", "", 50, true},
		{"zero", "limit=0", 0, false},
		{"over maximum", "limit=101", 0, false},
		{"not integer", "limit=abc", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := f.request(thread.caller, tc.query)
			if !tc.valid {
				require.Equal(t, http.StatusBadRequest, response.status, "response: %s", response.raw)
				name, _ := listXRPCError(t, response)
				require.Equal(t, "InvalidRequest", name)
				return
			}
			require.Equal(t, http.StatusOK, response.status, "response: %s", response.raw)
			require.Len(t, response.body.Notifications, tc.count, "response: %s", response.raw)
			require.NotEmpty(t, response.body.Cursor)
		})
	}
}

func TestListNotifications_CursorPaging(t *testing.T) {
	f := newListHandlerFixture(t)
	thread, uris := f.seedListedReplies(3)
	malformed := []string{"not-base64!", base64.RawURLEncoding.EncodeToString([]byte("notatime|xyz"))}
	var messages []string
	for _, tc := range []struct{ name, cursor string }{
		{"non-base64", malformed[0]},
		{"decoded garbage", malformed[1]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := f.request(thread.caller, "cursor="+url.QueryEscape(tc.cursor))
			require.Equal(t, http.StatusBadRequest, response.status, "response: %s", response.raw)
			name, message := listXRPCError(t, response)
			require.Equal(t, "InvalidCursor", name)
			require.NotEmpty(t, message)
			for _, cursor := range malformed {
				require.NotContains(t, message, cursor)
			}
			require.NotContains(t, message, "notatime")
			require.NotContains(t, message, "xyz")
			messages = append(messages, message)
		})
	}
	if len(messages) == 2 {
		require.Equal(t, messages[0], messages[1], "malformed cursor responses must use the same fixed message")
	}
	t.Run("two pages without duplicates", func(t *testing.T) {
		first := f.request(thread.caller, "limit=2")
		require.Equal(t, http.StatusOK, first.status, "response: %s", first.raw)
		require.Len(t, first.body.Notifications, 2)
		require.NotEmpty(t, first.body.Cursor)
		require.NotNil(t, first.body.Notifications[0].Record)
		require.NotNil(t, first.body.Notifications[1].Record)
		require.Equal(t, uris[2], first.body.Notifications[0].Record.URI)
		require.Equal(t, uris[1], first.body.Notifications[1].Record.URI)

		second := f.request(thread.caller, "limit=2&cursor="+url.QueryEscape(first.body.Cursor))
		require.Equal(t, http.StatusOK, second.status, "response: %s", second.raw)
		require.Len(t, second.body.Notifications, 1)
		require.Empty(t, second.body.Cursor)
		require.NotNil(t, second.body.Notifications[0].Record)
		require.Equal(t, uris[0], second.body.Notifications[0].Record.URI)
		require.NotContains(t, []string{first.body.Notifications[0].Record.URI, first.body.Notifications[1].Record.URI}, second.body.Notifications[0].Record.URI)
	})
}

func TestListNotifications_SeenAtAndIsRead(t *testing.T) {
	t.Run("stored seen time and fractional sort time", func(t *testing.T) {
		f := newListHandlerFixture(t)
		thread := f.seedThread("Root", "Body")
		base := time.Date(2026, time.September, 20, 9, 0, 0, 0, time.UTC)
		_, err := f.db.ExecContext(context.Background(), `INSERT INTO notification_state (did, seen_at) VALUES ($1, $2)`, thread.caller, base.Add(250001*time.Microsecond))
		require.NoError(t, err)
		for index, sortAt := range []time.Time{base, base.Add(500 * time.Millisecond)} {
			rkey := fmt.Sprintf("reply%d", index)
			uri := "at://" + thread.actor + "/social.coves.community.comment/" + rkey
			f.addComment(uri, "bafy"+rkey, rkey, thread.actor, thread.root, "bafyroot", thread.root, "bafyroot", "Reply", base)
			f.addReply(thread.caller, notifications.ReasonPostReply, uri, "bafy"+rkey, thread.actor, thread.root, thread.root, base, sortAt)
		}
		response := f.request(thread.caller, "")
		require.Equal(t, http.StatusOK, response.status, "response: %s", response.raw)
		require.Equal(t, "2026-09-20T09:00:00.250001Z", response.body.SeenAt)
		require.Len(t, response.body.Notifications, 2)
		require.Equal(t, "2026-09-20T09:00:00.5Z", response.body.Notifications[0].SortAt)
		require.False(t, response.body.Notifications[0].IsRead)
		require.Equal(t, "2026-09-20T09:00:00Z", response.body.Notifications[1].SortAt)
		require.True(t, response.body.Notifications[1].IsRead)
	})
	t.Run("no state and empty page", func(t *testing.T) {
		f := newListHandlerFixture(t)
		caller := "did:plc:listempty" + f.id
		f.addUser(caller, "listempty"+f.id+".test", "Empty Caller")
		response := f.request(caller, "")
		require.Equal(t, http.StatusOK, response.status, "response: %s", response.raw)
		require.Contains(t, string(response.raw), `"notifications":[]`, "empty page must serialize notifications as []")
		var body map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(response.raw, &body))
		require.Equal(t, "[]", string(body["notifications"]))
		require.NotContains(t, body, "seenAt")
	})
}
