//go:build integration

package notification_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"Coves/internal/api/handlers/notification"
	"Coves/internal/api/middleware"
	"Coves/internal/core/comments"
	"Coves/internal/core/notifications"
	"Coves/internal/core/posts"
	"Coves/internal/core/users"
	"Coves/internal/db/postgres"

	"github.com/stretchr/testify/require"
)

// Directly invokes the handler so a missing row fails at the response assertion,
// rather than the pre-check in request, while allowing real repositories to be wrapped.
func (f *listHandlerFixture) requestWithListDependencies(callerDID, query string, repo notifications.ReadRepository, profiles notifications.ProfileLookup, postViews notifications.PostViewLookup, commentLookup notifications.CommentLookup) listHandlerResponse {
	f.t.Helper()
	handler := notification.NewListHandler(notifications.NewListService(repo, profiles, postViews, commentLookup))
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

func (f *listHandlerFixture) placeholderRequest(callerDID, query string) listHandlerResponse {
	f.t.Helper()
	return f.requestWithListDependencies(callerDID, query, f.repo, postgres.NewUserRepository(f.db), postgres.NewPostRepository(f.db), postgres.NewCommentRepository(f.db))
}

func (f *listHandlerFixture) softDeleteComment(uri string) {
	f.t.Helper()
	_, err := f.db.ExecContext(context.Background(), `UPDATE comments SET deleted_at = $1 WHERE uri = $2`, time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC), uri)
	require.NoError(f.t, err)
}

func (f *listHandlerFixture) withdrawPost(uri, kind string) {
	f.t.Helper()
	if kind == "authorDelete" {
		_, err := f.db.ExecContext(context.Background(), `UPDATE posts SET deleted_at = $1 WHERE uri = $2`, time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC), uri)
		require.NoError(f.t, err)
		_, err = f.db.ExecContext(context.Background(), `INSERT INTO notification_public_post_withdrawals (post_uri, kind, community_rev) VALUES ($1, 'authorDelete', NULL)`, uri)
		require.NoError(f.t, err)
		return
	}
	_, err := f.db.ExecContext(context.Background(), `UPDATE community_post_admissions SET status = 'removed', decision_code = 'communityRule', decision_at = $1, acceptance_uri = NULL, acceptance_rkey = NULL, accepted_cid = NULL WHERE post_uri = $2`, time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC), uri)
	require.NoError(f.t, err)
	_, err = f.db.ExecContext(context.Background(), `INSERT INTO notification_public_post_withdrawals (post_uri, kind, community_rev) VALUES ($1, 'communityWithdrawal', '3lqqqqqqqqqq3')`, uri)
	require.NoError(f.t, err)
}

func placeholderObject(t *testing.T, row map[string]json.RawMessage, key string) map[string]json.RawMessage {
	t.Helper()
	var object map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(row[key], &object))
	require.NotNil(t, object, "%s must be present", key)
	return object
}

func TestListNotifications_DeletedRecordPlaceholder(t *testing.T) {
	f := newListHandlerFixture(t)
	thread := f.seedThread("Root title", "Root body")
	at := time.Date(2026, 9, 20, 9, 1, 0, 123456000, time.UTC)
	replyURI := "at://" + thread.actor + "/social.coves.community.comment/reply"
	f.addComment(replyURI, "bafycurrentreply", "reply", thread.actor, thread.root, "bafyroot", thread.root, "bafyroot", "Private deleted reply", at)
	f.addReply(thread.caller, notifications.ReasonPostReply, replyURI, "bafystoredoldreply", thread.actor, thread.root, thread.root, at, at)
	f.softDeleteComment(replyURI)

	response := f.placeholderRequest(thread.caller, "")
	require.Equal(t, http.StatusOK, response.status, "response: %s", response.raw)
	rows := listRawRows(t, response)
	require.Len(t, rows, 1, "deleted reply must remain listed")
	require.JSONEq(t, `{"uri":"`+replyURI+`","cid":"bafycurrentreply","createdAt":"2026-09-20T09:01:00.123456Z","status":"deleted"}`, string(rows[0]["record"]))
	require.NotContains(t, placeholderObject(t, rows[0], "record"), "excerpt")
	require.Equal(t, &notifications.ProfileView{DID: thread.actor, Handle: "listactor" + f.id + ".test", DisplayName: stringPointer("Indexed Author")}, response.body.Notifications[0].Author)
	require.JSONEq(t, `"Root title"`, string(placeholderObject(t, rows[0], "rootPost")["title"]))
	require.Contains(t, placeholderObject(t, rows[0], "rootPost"), "community")
	require.JSONEq(t, `"Root title"`, string(placeholderObject(t, rows[0], "subject")["preview"]))
	for _, key := range []string{"rootPost", "subject"} {
		require.NotContains(t, placeholderObject(t, rows[0], key), "status")
	}
}

func TestListNotifications_WithdrawnPostAndCommentPlaceholders(t *testing.T) {
	t.Run("author deleted subject and root", func(t *testing.T) {
		f := newListHandlerFixture(t)
		thread := f.seedThread("Private title", "Private content")
		at := time.Date(2026, 9, 20, 9, 1, 0, 0, time.UTC)
		replyURI := "at://" + thread.actor + "/social.coves.community.comment/reply"
		f.addComment(replyURI, "bafyreply", "reply", thread.actor, thread.root, "bafyroot", thread.root, "bafyroot", "Visible reply", at)
		f.addReply(thread.caller, notifications.ReasonPostReply, replyURI, "bafyoldreply", thread.actor, thread.root, thread.root, at, at)
		f.withdrawPost(thread.root, "authorDelete")

		response := f.placeholderRequest(thread.caller, "")
		require.Equal(t, http.StatusOK, response.status, "response: %s", response.raw)
		rows := listRawRows(t, response)
		require.Len(t, rows, 1, "reply to author-deleted post must remain listed")
		want := `{"uri":"` + thread.root + `","cid":"bafyroot","status":"deleted"}`
		require.JSONEq(t, want, string(rows[0]["rootPost"]))
		require.JSONEq(t, want, string(rows[0]["subject"]))
		for _, field := range []string{"title", "community"} {
			require.NotContains(t, placeholderObject(t, rows[0], "rootPost"), field)
		}
		require.NotContains(t, placeholderObject(t, rows[0], "subject"), "preview")
		require.JSONEq(t, `"Visible reply"`, string(placeholderObject(t, rows[0], "record")["excerpt"]))
		require.NotContains(t, placeholderObject(t, rows[0], "record"), "status")
	})
	t.Run("deleted parent with moderator removed root", func(t *testing.T) {
		f := newListHandlerFixture(t)
		thread := f.seedThread("Removed title", "Removed content")
		at := time.Date(2026, 9, 20, 9, 1, 0, 0, time.UTC)
		parentURI := "at://" + thread.caller + "/social.coves.community.comment/parent"
		replyURI := "at://" + thread.actor + "/social.coves.community.comment/reply"
		f.addComment(parentURI, "bafyparent", "parent", thread.caller, thread.root, "bafyroot", thread.root, "bafyroot", "Private parent", at)
		f.addComment(replyURI, "bafyreply", "reply", thread.actor, thread.root, "bafyroot", parentURI, "bafyparent", "Visible reply", at.Add(time.Minute))
		f.addReply(thread.caller, notifications.ReasonCommentReply, replyURI, "bafyoldreply", thread.actor, parentURI, thread.root, at.Add(time.Minute), at.Add(time.Minute))
		f.softDeleteComment(parentURI)
		f.withdrawPost(thread.root, "communityWithdrawal")

		response := f.placeholderRequest(thread.caller, "")
		require.Equal(t, http.StatusOK, response.status, "response: %s", response.raw)
		rows := listRawRows(t, response)
		require.Len(t, rows, 1, "reply to deleted parent on removed root must remain listed")
		require.JSONEq(t, `{"uri":"`+parentURI+`","cid":"bafyparent","status":"deleted"}`, string(rows[0]["subject"]))
		require.JSONEq(t, `{"uri":"`+thread.root+`","cid":"bafyroot","status":"removedByModerator"}`, string(rows[0]["rootPost"]))
		require.NotContains(t, placeholderObject(t, rows[0], "subject"), "preview")
		for _, field := range []string{"title", "community"} {
			require.NotContains(t, placeholderObject(t, rows[0], "rootPost"), field)
		}
		require.JSONEq(t, `"Visible reply"`, string(placeholderObject(t, rows[0], "record")["excerpt"]))
		require.NotContains(t, placeholderObject(t, rows[0], "record"), "status")
	})
}

type countedNotificationReads struct {
	notifications.ReadRepository
	lists int
}

func (r *countedNotificationReads) List(ctx context.Context, did, cursor string, limit int) (notifications.ListPage, error) {
	r.lists++
	return r.ReadRepository.List(ctx, did, cursor, limit)
}

type countedProfiles struct {
	notifications.ProfileLookup
	calls int
}

func (p *countedProfiles) GetByDIDs(ctx context.Context, dids []string) (map[string]*users.User, error) {
	p.calls++
	return p.ProfileLookup.GetByDIDs(ctx, dids)
}

type countedPosts struct {
	notifications.PostViewLookup
	calls int
}

func (p *countedPosts) GetViewsByURIs(ctx context.Context, uris []string, viewer string) (map[string]*posts.PostView, error) {
	p.calls++
	return p.PostViewLookup.GetViewsByURIs(ctx, uris, viewer)
}

type countedComments struct {
	notifications.CommentLookup
	calls int
}

func (c *countedComments) GetByURIsBatch(ctx context.Context, uris []string) (map[string]*comments.Comment, error) {
	c.calls++
	return c.CommentLookup.GetByURIsBatch(ctx, uris)
}

func TestListNotifications_PlaceholdersBatchWithinOneLookupPerKind(t *testing.T) {
	for _, count := range []int{50, 2} {
		t.Run(fmt.Sprintf("page of %d", count), func(t *testing.T) {
			f := newListHandlerFixture(t)
			thread := f.seedThread("Live root", "Live body")
			base := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
			deletedPostURI := "at://" + thread.caller + "/social.coves.community.postv2/deleted"
			removedPostURI := "at://" + thread.caller + "/social.coves.community.postv2/removed"
			f.addPost(deletedPostURI, "bafydeletedpost", "deleted", thread.caller, thread.community, "Deleted title", "Private body", base)
			f.addPost(removedPostURI, "bafyremovedpost", "removed", thread.caller, thread.community, "Removed title", "Private body", base)
			parentURI := "at://" + thread.caller + "/social.coves.community.comment/parent"
			removedParentURI := "at://" + thread.caller + "/social.coves.community.comment/removedparent"
			f.addComment(parentURI, "bafyparent", "parent", thread.caller, thread.root, "bafyroot", thread.root, "bafyroot", "Deleted parent", base)
			f.addComment(removedParentURI, "bafyremovedparent", "removedparent", thread.caller, removedPostURI, "bafyremovedpost", removedPostURI, "bafyremovedpost", "Parent", base)
			// Distinct actors and distinct live posts make a per-DID or per-URI lookup loop issue more than one call.
			secondActor := "did:plc:listactorb" + f.id
			f.addUser(secondActor, "listactorb"+f.id+".test", "Second Author")
			actors := []string{thread.actor, secondActor}
			for i := 0; i < count; i++ {
				rkey := fmt.Sprintf("batchreply%d", i)
				actor := actors[i%len(actors)]
				uri := "at://" + actor + "/social.coves.community.comment/" + rkey
				rootURI, rootCID, subjectURI, subjectCID, reason := thread.root, "bafyroot", thread.root, "bafyroot", notifications.ReasonPostReply
				switch i % 5 {
				case 0, 1:
					postRkey := fmt.Sprintf("batchroot%d", i)
					rootURI, rootCID = "at://"+thread.caller+"/social.coves.community.postv2/"+postRkey, "bafy"+postRkey
					subjectURI, subjectCID = rootURI, rootCID
					f.addPost(rootURI, rootCID, postRkey, thread.caller, thread.community, "Live root", "Live body", base)
				case 2:
					rootURI, rootCID, subjectURI, subjectCID = deletedPostURI, "bafydeletedpost", deletedPostURI, "bafydeletedpost"
				case 3:
					subjectURI, subjectCID, reason = parentURI, "bafyparent", notifications.ReasonCommentReply
				case 4:
					rootURI, rootCID, subjectURI, subjectCID, reason = removedPostURI, "bafyremovedpost", removedParentURI, "bafyremovedparent", notifications.ReasonCommentReply
				}
				at := base.Add(time.Duration(i+1) * time.Second)
				f.addComment(uri, "bafy"+rkey, rkey, actor, rootURI, rootCID, subjectURI, subjectCID, "Reply", at)
				f.addReply(thread.caller, reason, uri, "bafystoredold"+rkey, actor, subjectURI, rootURI, at, at)
				if i%5 == 1 {
					f.softDeleteComment(uri)
				}
			}
			f.softDeleteComment(parentURI)
			f.withdrawPost(deletedPostURI, "authorDelete")
			f.withdrawPost(removedPostURI, "communityWithdrawal")
			repo := &countedNotificationReads{ReadRepository: f.repo}
			profiles := &countedProfiles{ProfileLookup: postgres.NewUserRepository(f.db)}
			postViews := &countedPosts{PostViewLookup: postgres.NewPostRepository(f.db)}
			commentLookup := &countedComments{CommentLookup: postgres.NewCommentRepository(f.db)}
			response := f.requestWithListDependencies(thread.caller, fmt.Sprintf("limit=%d", count), repo, profiles, postViews, commentLookup)
			require.Equal(t, http.StatusOK, response.status, "response: %s", response.raw)
			rows := listRawRows(t, response)
			require.Equal(t, count, len(rows), "live and placeholder rows all fill the page")
			var returned []string
			for _, row := range response.body.Notifications {
				require.NotNil(t, row.Record)
				returned = append(returned, row.Record.URI)
			}
			require.ElementsMatch(t, f.expected[thread.caller], returned)
			require.Equal(t, 1, repo.lists)
			require.Equal(t, 1, profiles.calls, "authors load in one batched lookup")
			require.Equal(t, 1, postViews.calls, "live posts load in one batched lookup")
			require.LessOrEqual(t, commentLookup.calls, 1)
		})
	}
}
