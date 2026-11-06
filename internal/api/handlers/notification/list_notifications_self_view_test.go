//go:build integration

package notification_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"Coves/internal/api/handlers/notification"
	"Coves/internal/api/middleware"
	"Coves/internal/core/accountstatus"
	"Coves/internal/core/comments"
	"Coves/internal/core/notifications"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/fixtures/accountbanfixture"
	"Coves/tests/fixtures/accountstatusfixture"
	"Coves/tests/fixtures/communityremovalfixture"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// hideReasonComments returns the real comment lookup's comments, with uri
// re-read as hidden for reason: the comment was re-created hidden between the
// notification read selecting it live and hydration loading it.
type hideReasonComments struct {
	notifications.CommentLookup
	uri, reason string
}

func (c hideReasonComments) GetByURIsBatch(ctx context.Context, uris []string) (map[string]*comments.Comment, error) {
	loaded, err := c.CommentLookup.GetByURIsBatch(ctx, uris)
	if err != nil {
		return nil, err
	}
	if comment := loaded[c.uri]; comment != nil {
		hidden := *comment
		reason := c.reason
		hidden.HideReason = &reason
		loaded[c.uri] = &hidden
	}
	return loaded, nil
}

// productionWiredRequests serves listNotifications and getUnreadCount for
// caller over repositories configured with the instance account-ban authority,
// as cmd/server buildRepositories wires them. wrapComments, when set, wraps
// the real comment repository.
func (f *listHandlerFixture) productionWiredRequests(caller string, wrapComments func(notifications.CommentLookup) notifications.CommentLookup) (listHandlerResponse, int) {
	f.t.Helper()
	authority := fixtures.InstanceDID()
	restriction := postgres.WithAccountRestrictionAuthority(authority)
	repo := postgres.NewNotificationRepository(f.db, postgres.WithNotificationAccountRestrictionAuthority(authority)).(notifications.ReadRepository)
	var commentLookup notifications.CommentLookup = postgres.NewCommentRepository(f.db, restriction)
	if wrapComments != nil {
		commentLookup = wrapComments(commentLookup)
	}
	listed := f.requestWithListDependencies(caller, "limit=100", repo, postgres.NewUserRepository(f.db), postgres.NewPostRepository(f.db, restriction), commentLookup)

	handler := notification.NewHandler(notifications.NewService(repo))
	req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.notification.getUnreadCount", nil)
	req = req.WithContext(middleware.SetTestUserDID(req.Context(), caller))
	rec := httptest.NewRecorder()
	handler.HandleGetUnreadCount(rec, req)
	require.Equal(f.t, http.StatusOK, rec.Code, "getUnreadCount response: %s", rec.Body.String())
	var unread notification.UnreadCountResponse
	require.NoError(f.t, json.Unmarshal(rec.Body.Bytes(), &unread))
	return listed, unread.Count
}

// removeAtInstance records an active instance removal decision directly on
// subjectURI, in the shape removeContent writes.
func (f *listHandlerFixture) removeAtInstance(subjectURI, collection string) {
	f.t.Helper()
	actionID := "self-view-removal-" + testkit.TID()
	_, err := f.db.ExecContext(context.Background(), `INSERT INTO moderation_actions
		(id, actor_did, authority_did, scope_kind, scope_community_did, subject_uri, subject_collection, action, origin, created_at)
		VALUES ($1, 'did:plc:selfviewmoderator', $2, 'instance', NULL, $3, $4, 'remove', 'local', NOW())`,
		actionID, fixtures.InstanceDID(), subjectURI, collection)
	require.NoError(f.t, err)
	_, err = f.db.ExecContext(context.Background(), `INSERT INTO moderation_decisions
		(authority_did, scope_kind, scope_community_did, subject_uri, kind, value, active_action_id, active)
		VALUES ($1, 'instance', NULL, $2, 'removal', NULL, $3, TRUE)`,
		fixtures.InstanceDID(), subjectURI, actionID)
	require.NoError(f.t, err)
}

func (f *listHandlerFixture) setHideReason(commentURI, reason string) {
	f.t.Helper()
	_, err := f.db.ExecContext(context.Background(), `UPDATE comments SET hide_reason = $1 WHERE uri = $2`, reason, commentURI)
	require.NoError(f.t, err)
}

// selfViewScene is recipient R (thread.caller) with R's own post (thread.root)
// in thread.community, actor A (thread.actor), R's comment parent on that post,
// and R's seen time set before every notification so all listed rows are
// unread.
type selfViewScene struct {
	f      *listHandlerFixture
	thread listThread
	parent string
}

// listedNotification is one expected listed row: its reason, its subject JSON
// and, when set, its record excerpt.
type listedNotification struct {
	reason        string
	subject       string
	recordExcerpt string
}

// TestListNotifications_RecipientSelfViewMatchesUnreadCount lists, for
// recipient R, notifications about R's own content that the notification read
// keeps live for R (R account-banned or PDS-restricted, R's post in a removed
// community, R's comment hidden by an account ban), and the controls the read
// does not exempt (a community-ban-hidden comment, a direct instance removal).
// Each listed row is unread, and getUnreadCount counts exactly the listed
// rows. The last row guards that hydration applies the exemption only to
// content R authored.
func TestListNotifications_RecipientSelfViewMatchesUnreadCount(t *testing.T) {
	base := time.Date(2026, time.September, 20, 9, 0, 0, 0, time.UTC)
	first, second := base.Add(time.Minute), base.Add(2*time.Minute)
	for _, tc := range []struct {
		name string
		// seed writes the notifications and the restriction; want returns the
		// listed rows newest first.
		seed func(t *testing.T, s selfViewScene)
		want func(s selfViewScene) []listedNotification
		// wrapComments, when set, wraps the real comment lookup.
		wrapComments func(s selfViewScene) func(notifications.CommentLookup) notifications.CommentLookup
		// selectedThenHidden: the row is counted unread by the read but its
		// record is hidden at hydration, so it is omitted from the list.
		selectedThenHidden bool
	}{
		{
			name: "a. upvote on own post while account-banned",
			seed: func(t *testing.T, s selfViewScene) {
				s.f.addUpvoteGroup(s.thread.caller, s.thread.root, s.thread.root, first)
				s.f.addUpvoteVote(s.thread.actor, s.thread.root, "up", first)
				accountbanfixture.New(s.f.db).Ban(t, s.thread.caller, nil)
			},
			want: func(s selfViewScene) []listedNotification {
				return []listedNotification{{reason: "upvote", subject: `{"uri":"` + s.thread.root + `","cid":"bafyroot","preview":"Own post title"}`}}
			},
		},
		{
			name: "b. reply on own post while PDS-restricted",
			seed: func(t *testing.T, s selfViewScene) {
				reply := "at://" + s.thread.actor + "/social.coves.community.comment/reply"
				s.f.addComment(reply, "bafyreply", "reply", s.thread.actor, s.thread.root, "bafyroot", s.thread.root, "bafyroot", "A reply to you", first)
				s.f.addReply(s.thread.caller, notifications.ReasonPostReply, reply, "bafyreply", s.thread.actor, s.thread.root, s.thread.root, first, first)
				accountstatusfixture.Set(t, s.f.db, s.thread.caller, accountstatus.Takendown)
			},
			want: func(s selfViewScene) []listedNotification {
				return []listedNotification{{reason: "postReply", subject: `{"uri":"` + s.thread.root + `","cid":"bafyroot","preview":"Own post title"}`, recordExcerpt: "A reply to you"}}
			},
		},
		{
			name: "c. upvote and reply on own post in a removed community",
			seed: func(t *testing.T, s selfViewScene) {
				s.f.addUpvoteGroup(s.thread.caller, s.thread.root, s.thread.root, first)
				s.f.addUpvoteVote(s.thread.actor, s.thread.root, "up", first)
				reply := "at://" + s.thread.actor + "/social.coves.community.comment/reply"
				s.f.addComment(reply, "bafyreply", "reply", s.thread.actor, s.thread.root, "bafyroot", s.thread.root, "bafyroot", "A reply to you", second)
				s.f.addReply(s.thread.caller, notifications.ReasonPostReply, reply, "bafyreply", s.thread.actor, s.thread.root, s.thread.root, second, second)
				communityremovalfixture.New(s.f.db).Remove(t, s.thread.community)
			},
			want: func(s selfViewScene) []listedNotification {
				subject := `{"uri":"` + s.thread.root + `","cid":"bafyroot","preview":"Own post title"}`
				return []listedNotification{{reason: "postReply", subject: subject}, {reason: "upvote", subject: subject}}
			},
		},
		{
			name: "d. reply and upvote on own account-ban-hidden comment",
			seed: func(t *testing.T, s selfViewScene) {
				s.f.addUpvoteGroup(s.thread.caller, s.parent, s.thread.root, first)
				s.f.addUpvoteVote(s.thread.actor, s.parent, "up", first)
				reply := "at://" + s.thread.actor + "/social.coves.community.comment/child"
				s.f.addComment(reply, "bafychild", "child", s.thread.actor, s.thread.root, "bafyroot", s.parent, "bafyparent", "Answering you", second)
				s.f.addReply(s.thread.caller, notifications.ReasonCommentReply, reply, "bafychild", s.thread.actor, s.parent, s.thread.root, second, second)
				s.f.setHideReason(s.parent, comments.HideReasonAccountBan)
			},
			want: func(s selfViewScene) []listedNotification {
				subject := `{"uri":"` + s.parent + `","cid":"bafyparent","preview":"My own words"}`
				return []listedNotification{{reason: "commentReply", subject: subject, recordExcerpt: "Answering you"}, {reason: "upvote", subject: subject}}
			},
		},
		{
			name: "e. guard: reply to own community-ban-hidden comment",
			seed: func(t *testing.T, s selfViewScene) {
				reply := "at://" + s.thread.actor + "/social.coves.community.comment/child"
				s.f.addComment(reply, "bafychild", "child", s.thread.actor, s.thread.root, "bafyroot", s.parent, "bafyparent", "Answering you", first)
				s.f.addReply(s.thread.caller, notifications.ReasonCommentReply, reply, "bafychild", s.thread.actor, s.parent, s.thread.root, first, first)
				s.f.setHideReason(s.parent, comments.HideReasonCommunityBan)
			},
			want: func(s selfViewScene) []listedNotification {
				return []listedNotification{{reason: "commentReply", subject: `{"uri":"` + s.parent + `","cid":"bafyparent","status":"removedByModerator"}`, recordExcerpt: "Answering you"}}
			},
		},
		{
			name: "f. guard: upvote on own post under a direct instance removal while banned",
			seed: func(t *testing.T, s selfViewScene) {
				s.f.addUpvoteGroup(s.thread.caller, s.thread.root, s.thread.root, first)
				s.f.addUpvoteVote(s.thread.actor, s.thread.root, "up", first)
				s.f.removeAtInstance(s.thread.root, "social.coves.community.postv2")
				accountbanfixture.New(s.f.db).Ban(t, s.thread.caller, nil)
			},
			want: func(s selfViewScene) []listedNotification {
				return []listedNotification{{reason: "upvote", subject: `{"uri":"` + s.thread.root + `","cid":"bafyroot","status":"removedByServerAdmin"}`}}
			},
		},
		{
			name: "g. guard: another account's mention hidden by an account ban after selection",
			seed: func(t *testing.T, s selfViewScene) {
				mention := "at://" + s.thread.actor + "/social.coves.community.comment/mention"
				s.f.addComment(mention, "bafymention", "mention", s.thread.actor, s.thread.root, "bafyroot", s.thread.root, "bafyroot", "Hidden mention text", first)
				s.f.addMention(s.thread.caller, mention, "bafymention", s.thread.actor, s.thread.root, first, first)
			},
			wrapComments: func(s selfViewScene) func(notifications.CommentLookup) notifications.CommentLookup {
				return func(inner notifications.CommentLookup) notifications.CommentLookup {
					return hideReasonComments{CommentLookup: inner, uri: "at://" + s.thread.actor + "/social.coves.community.comment/mention", reason: comments.HideReasonAccountBan}
				}
			},
			want:               func(selfViewScene) []listedNotification { return nil },
			selectedThenHidden: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newListHandlerFixture(t)
			thread := f.seedThread("Own post title", "Own post body")
			parent := "at://" + thread.caller + "/social.coves.community.comment/parent"
			f.addComment(parent, "bafyparent", "parent", thread.caller, thread.root, "bafyroot", thread.root, "bafyroot", "My own words", base)
			_, err := f.db.ExecContext(context.Background(), `INSERT INTO notification_state (did, seen_at) VALUES ($1, $2)`, thread.caller, base.Add(-time.Hour))
			require.NoError(t, err)
			s := selfViewScene{f: f, thread: thread, parent: parent}
			tc.seed(t, s)
			var wrap func(notifications.CommentLookup) notifications.CommentLookup
			if tc.wrapComments != nil {
				wrap = tc.wrapComments(s)
			}

			response, unread := f.productionWiredRequests(thread.caller, wrap)
			require.Equal(t, http.StatusOK, response.status, "response: %s", response.raw)
			rows := listRawRows(t, response)
			want := tc.want(s)
			require.Len(t, rows, len(want), "listed notifications: %s", response.raw)
			for index, expected := range want {
				row := rows[index]
				require.JSONEq(t, `"`+expected.reason+`"`, string(row["reason"]), "row %d", index)
				require.JSONEq(t, expected.subject, string(row["subject"]), "row %d subject", index)
				require.JSONEq(t, `false`, string(row["isRead"]), "row %d is unread", index)
				if expected.recordExcerpt != "" {
					require.JSONEq(t, `"`+expected.recordExcerpt+`"`, string(placeholderObject(t, row, "record")["excerpt"]), "row %d record", index)
				}
			}
			if tc.selectedThenHidden {
				require.Equal(t, 1, unread, "the read selected the mention live")
				require.NotContains(t, string(response.raw), "Hidden mention text", "another account's hidden comment content is never rendered")
				return
			}
			require.Equal(t, len(rows), unread, "getUnreadCount counts exactly the listed notifications")
		})
	}
}
