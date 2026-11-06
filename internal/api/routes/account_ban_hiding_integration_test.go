//go:build integration

package routes_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"Coves/internal/core/accountstatus"
	"Coves/internal/core/moderation"
	"Coves/tests/fixtures"
	"Coves/tests/fixtures/accountbanfixture"
	"Coves/tests/fixtures/accountstatusfixture"
	"Coves/tests/testkit"

	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	accountBanHidingSearchTerm   = "quetzalmarmot"
	accountBanHidingHandle       = "bannedauthor.test"
	accountBanHidingDisplayName  = "Banned Author"
	accountBanHidingDescription  = "an account the instance bans"
	accountBanHidingCommentText  = "the banned account's comment on another thread"
	accountBanHidingReplyText    = "a reply that outlives the banned account's comment"
	accountBanHidingNestedText   = "the banned account's nested reply to another account's comment"
	accountBanHidingHarassReason = "social.coves.moderation.defs#reasonHarassment"
)

// accountBanHidingScene is the route fixture plus banned-to-be account A
// (f.authorDID) with a post and a comment on another account B's thread that
// B replied to, and a nested reply beneath B's own top-level comment on that
// thread. V (f.nonAdminDID) is an unbanned signed-in viewer subscribed to
// the community.
type accountBanHidingScene struct {
	f              contentLabelRouteFixture
	moderator      *accountbanfixture.Moderator
	post           string
	thread         string
	comment        string
	commentRkey    string
	reply          string
	nestedParent   string
	nestedReply    string
	instanceDID    string
	viewerTokenFor map[string]string
}

func newAccountBanHidingScene(t *testing.T) accountBanHidingScene {
	t.Helper()
	f := newContentLabelRouteFixture(t)
	_, err := f.db.ExecContext(t.Context(), `
		UPDATE users SET handle = $2, display_name = $3, description = $4,
			avatar_cid = 'bafkreiaccountbanhidingavatar', banner_cid = 'bafkreiaccountbanhidingbanner'
		WHERE did = $1
	`, f.authorDID, accountBanHidingHandle, accountBanHidingDisplayName, accountBanHidingDescription)
	require.NoError(t, err)
	fixtures.User(t, f.db, testkit.UniqueIDWithPrefix(t, "hidingviewer")+".test", f.nonAdminDID)
	_, err = f.db.ExecContext(t.Context(),
		`INSERT INTO community_subscriptions (user_did, community_did) VALUES ($1, $2)`, f.nonAdminDID, f.communityDID)
	require.NoError(t, err)
	replierName := testkit.UniqueIDWithPrefix(t, "hidingreplier")
	replierDID := fixtures.DID(replierName)
	fixtures.User(t, f.db, replierName+".test", replierDID)

	post := f.insertPost("account ban hiding post " + accountBanHidingSearchTerm)
	thread := f.insertPostBy(replierDID, "a thread another account started")
	const commentCID = "bafyreiaccountbanhidingcomment"
	comment := moderationAcceptanceInsertComment(t, f.db, f.authorDID, thread, f.postCID, thread, f.postCID,
		commentCID, accountBanHidingCommentText)
	reply := moderationAcceptanceInsertComment(t, f.db, replierDID, thread, f.postCID, comment, commentCID,
		"bafyreiaccountbanhidingreply", accountBanHidingReplyText)
	const nestedParentCID = "bafyreiaccountbanhidingnestedparent"
	nestedParent := moderationAcceptanceInsertComment(t, f.db, replierDID, thread, f.postCID, thread, f.postCID,
		nestedParentCID, "another account's top-level comment")
	nestedReply := moderationAcceptanceInsertComment(t, f.db, f.authorDID, thread, f.postCID, nestedParent, nestedParentCID,
		"bafyreiaccountbanhidingnestedreply", accountBanHidingNestedText)
	_, err = f.db.ExecContext(t.Context(), `UPDATE comments SET reply_count = 1 WHERE uri = ANY($1)`,
		pq.Array([]string{comment, nestedParent}))
	require.NoError(t, err)
	return accountBanHidingScene{
		f: f, moderator: accountbanfixture.New(f.db), post: post, thread: thread,
		comment: comment, commentRkey: strings.TrimPrefix(comment, "at://"+f.authorDID+"/"+moderation.CommentCollection+"/"),
		reply: reply, nestedParent: nestedParent, nestedReply: nestedReply, instanceDID: f.instanceDID,
		viewerTokenFor: map[string]string{
			"other viewer": f.nonAdminToken, "signed-out": "", "banned account": f.authorToken, "fresh viewer": f.adminToken,
		},
	}
}

func (s accountBanHidingScene) get(t *testing.T, target, viewer string) map[string]any {
	t.Helper()
	token, ok := s.viewerTokenFor[viewer]
	require.Truef(t, ok, "unknown viewer %q", viewer)
	return moderationAcceptanceBody(t, moderationAcceptanceRequest(t, s.f.client, http.MethodGet, s.f.server.URL+target, token, nil))
}

// accountBanHidingRow is one read path checked for each listed viewer. hidden
// is true while the account is banned and the viewer is not the account.
type accountBanHidingRow struct {
	name    string
	target  string
	viewers []string
	// stages, when set, limits the row to these stages.
	stages []string
	check  func(t *testing.T, body map[string]any, hidden bool)
}

func accountBanHidingFeedPost(uri string) func(*testing.T, map[string]any, bool) {
	return func(t *testing.T, body map[string]any, hidden bool) {
		t.Helper()
		listed := moderationPostAcceptanceFeedURIs(t, body)
		if hidden {
			assert.NotContains(t, listed, uri, "a banned account's post must be absent for other viewers")
			return
		}
		assert.Contains(t, listed, uri)
	}
}

// requireAccountBanHidingComment checks the banned account's comment node:
// while hidden a removed placeholder naming the instance authority with the
// reply kept, otherwise the comment with its content.
func (s accountBanHidingScene) requireComment(t *testing.T, node map[string]any, hidden bool) {
	t.Helper()
	comment := moderationAcceptanceObject(t, node["comment"])
	require.Equal(t, s.comment, comment["uri"])
	s.requireCommentView(t, comment, hidden, accountBanHidingCommentText)
	replies := moderationAcceptanceArray(t, node["replies"])
	require.Len(t, replies, 1, "the reply stays beneath the banned account's comment")
	reply := moderationAcceptanceObject(t, moderationAcceptanceObject(t, replies[0])["comment"])
	assert.Equal(t, s.reply, reply["uri"])
	assert.Equal(t, accountBanHidingReplyText, moderationAcceptanceObject(t, reply["record"])["content"])
}

// requireNestedReply checks the banned account's nested reply beneath
// another account's top-level comment in a thread: a placeholder while
// hidden, otherwise the reply with its content.
func (s accountBanHidingScene) requireNestedReply(t *testing.T, body map[string]any, hidden bool) {
	t.Helper()
	replies := moderationAcceptanceArray(t, viewerModerationThreadNode(t, body, s.nestedParent)["replies"])
	require.Len(t, replies, 1, "the banned account's nested reply stays in the thread")
	reply := moderationAcceptanceObject(t, moderationAcceptanceObject(t, replies[0])["comment"])
	require.Equal(t, s.nestedReply, reply["uri"])
	s.requireCommentView(t, reply, hidden, accountBanHidingNestedText)
}

// requireCommentView checks one of the banned account's comment views: while
// hidden a removed placeholder naming the instance authority, otherwise the
// comment with content.
func (s accountBanHidingScene) requireCommentView(t *testing.T, comment map[string]any, hidden bool, content string) {
	t.Helper()
	if hidden {
		assert.Equal(t, true, comment["isDeleted"], "a banned account's comment must be a placeholder")
		assert.Equal(t, "moderator", comment["deletionReason"])
		assert.Equal(t, map[string]any{
			"state": "removed",
			"sources": []any{map[string]any{
				"authorityDid": s.instanceDID, "scope": map[string]any{"kind": "instance"},
			}},
		}, comment["moderation"])
		assert.Nil(t, comment["record"], "a placeholder serves no record")
		encoded, err := json.Marshal(comment)
		require.NoError(t, err)
		assert.NotContains(t, string(encoded), content)
		return
	}
	assert.NotEqual(t, true, comment["isDeleted"])
	record := moderationAcceptanceObject(t, comment["record"])
	assert.Equal(t, content, record["content"])
}

// requireSuspendedProfile checks A's profile as another viewer reads it while
// A is restricted: identity and the suspended status, zero activity, nothing
// else.
func (s accountBanHidingScene) requireSuspendedProfile(t *testing.T, profile map[string]any) {
	t.Helper()
	assert.Equal(t, "suspended", profile["accountStatus"])
	assert.Equal(t, s.f.authorDID, profile["did"])
	assert.Equal(t, accountBanHidingHandle, profile["handle"])
	for _, key := range []string{"displayName", "description", "avatar", "banner", "banNotice"} {
		assert.NotContainsf(t, profile, key, "a suspended profile must not carry %s", key)
	}
	stats := moderationAcceptanceObject(t, profile["stats"])
	assert.Equal(t, float64(0), stats["postCount"])
	assert.Equal(t, float64(0), stats["commentCount"])
}

// TestAccountBanHidesContentFromOtherViewers reads every content path before
// the ban, while banned and after unban. While banned, A's post is absent for
// a signed-in viewer and a signed-out viewer, post.get is notFound, and A's
// comment is a placeholder with B's reply kept; A itself still sees both.
func TestAccountBanHidesContentFromOtherViewers(t *testing.T) {
	s := newAccountBanHidingScene(t)
	others := []string{"other viewer", "signed-out"}
	communityFeed := func(sort string) string {
		return "/xrpc/social.coves.communityFeed.getCommunity?" + url.Values{"community": {s.f.communityDID}, "sort": {sort}}.Encode()
	}
	rows := []accountBanHidingRow{
		{
			name: "timeline", target: "/xrpc/social.coves.feed.getTimeline?sort=new",
			viewers: []string{"other viewer"}, check: accountBanHidingFeedPost(s.post),
		},
		{
			name: "search", target: "/xrpc/social.coves.feed.searchPosts?" + url.Values{"q": {accountBanHidingSearchTerm}}.Encode(),
			viewers: others, check: accountBanHidingFeedPost(s.post),
		},
		{name: "discover new", target: "/xrpc/social.coves.feed.getDiscover?sort=new", viewers: others, check: accountBanHidingFeedPost(s.post)},
		{name: "discover top", target: "/xrpc/social.coves.feed.getDiscover?sort=top", viewers: others, check: accountBanHidingFeedPost(s.post)},
		{
			// The snapshot each viewer builds before the ban is reused while banned.
			name: "discover hot reused snapshot", target: "/xrpc/social.coves.feed.getDiscover?sort=hot",
			viewers: others, stages: []string{"before ban", "while banned"}, check: accountBanHidingFeedPost(s.post),
		},
		{
			name: "discover hot fresh snapshot", target: "/xrpc/social.coves.feed.getDiscover?sort=hot",
			viewers: []string{"fresh viewer"}, stages: []string{"after unban"}, check: accountBanHidingFeedPost(s.post),
		},
		{
			name: "post.get", target: "/xrpc/social.coves.community.post.get?" + url.Values{"uris": {s.post}}.Encode(),
			viewers: others,
			check: func(t *testing.T, body map[string]any, hidden bool) {
				t.Helper()
				items := moderationAcceptanceArray(t, body["posts"])
				require.Len(t, items, 1)
				item := moderationAcceptanceObject(t, items[0])
				if hidden {
					assert.Equal(t, map[string]any{"uri": s.post, "notFound": true}, item,
						"a banned account's post view must be unavailable")
					return
				}
				assert.Equal(t, s.post, item["uri"])
				assert.NotContains(t, item, "notFound")
			},
		},
	}
	for _, sort := range []string{"hot", "new", "top"} {
		rows = append(rows, accountBanHidingRow{
			name: "community feed " + sort, target: communityFeed(sort),
			viewers: []string{"other viewer", "signed-out", "banned account"}, check: accountBanHidingFeedPost(s.post),
		}, accountBanHidingRow{
			name: "thread " + sort, target: "/xrpc/social.coves.community.comment.getComments?" + url.Values{"post": {s.thread}, "sort": {sort}}.Encode(),
			viewers: []string{"other viewer", "signed-out", "banned account"},
			check: func(t *testing.T, body map[string]any, hidden bool) {
				t.Helper()
				s.requireComment(t, viewerModerationThreadNode(t, body, s.comment), hidden)
				s.requireNestedReply(t, body, hidden)
			},
		}, accountBanHidingRow{
			name: "subtree anchored at the comment " + sort,
			target: "/xrpc/social.coves.community.comment.getComments?" + url.Values{
				"post": {s.thread}, "parentRkey": {s.commentRkey}, "sort": {sort},
			}.Encode(),
			viewers: others,
			check: func(t *testing.T, body map[string]any, hidden bool) {
				t.Helper()
				nodes := moderationAcceptanceArray(t, body["comments"])
				require.Len(t, nodes, 1, "the subtree keeps its anchor")
				s.requireComment(t, moderationAcceptanceObject(t, nodes[0]), hidden)
			},
		})
	}

	run := func(stage string, banned bool) {
		for _, row := range rows {
			if len(row.stages) > 0 && !slices.Contains(row.stages, stage) {
				continue
			}
			for _, viewer := range row.viewers {
				t.Run(stage+"/"+row.name+"/"+viewer, func(t *testing.T) {
					row.check(t, s.get(t, row.target, viewer), banned && viewer != "banned account")
				})
			}
		}
	}
	run("before ban", false)
	s.moderator.Ban(t, s.f.authorDID, nil)
	run("while banned", true)
	s.moderator.Unban(t, s.f.authorDID)
	run("after unban", false)
}

// TestAccountBanSuspendedProfileForOtherViewers reads A's profile and
// activity as a signed-in viewer and a signed-out viewer while A is banned.
func TestAccountBanSuspendedProfileForOtherViewers(t *testing.T) {
	s := newAccountBanHidingScene(t)
	actor := url.Values{"actor": {s.f.authorDID}}.Encode()
	before := s.get(t, "/xrpc/social.coves.actor.getProfile?"+actor, "signed-out")
	require.Equal(t, accountBanHidingDisplayName, before["displayName"], "fixture: the profile is full before the ban")
	require.Equal(t, map[string]any{"postCount": float64(1), "commentCount": float64(2)}, map[string]any{
		"postCount":    moderationAcceptanceObject(t, before["stats"])["postCount"],
		"commentCount": moderationAcceptanceObject(t, before["stats"])["commentCount"],
	}, "fixture: the account has one post and two comments")

	s.moderator.Ban(t, s.f.authorDID, nil)
	for _, viewer := range []string{"other viewer", "signed-out"} {
		t.Run("getProfile/"+viewer, func(t *testing.T) {
			s.requireSuspendedProfile(t, s.get(t, "/xrpc/social.coves.actor.getProfile?"+actor, viewer))
		})
		t.Run("getPosts/"+viewer, func(t *testing.T) {
			assert.Empty(t, s.get(t, "/xrpc/social.coves.actor.getPosts?"+actor, viewer)["feed"])
		})
		t.Run("getComments/"+viewer, func(t *testing.T) {
			assert.Empty(t, s.get(t, "/xrpc/social.coves.actor.getComments?"+actor, viewer)["comments"])
		})
	}
}

// TestAccountBanNoticeForBannedAccount reads A's own profile, viewer
// authority and activity while banned with a known reason, and an unbanned
// account's own profile and viewer authority.
func TestAccountBanNoticeForBannedAccount(t *testing.T) {
	temporaryExpiry := time.Date(2099, time.January, 1, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name       string
		expiresAt  *time.Time
		wantNotice map[string]any
	}{
		{name: "permanent ban", wantNotice: map[string]any{"reason": accountBanHidingHarassReason}},
		{
			name: "temporary ban", expiresAt: &temporaryExpiry,
			wantNotice: map[string]any{"reason": accountBanHidingHarassReason, "expiresAt": "2099-01-01T00:00:00Z"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := newAccountBanHidingScene(t)
			s.moderator.BanWithReason(t, s.f.authorDID, accountBanHidingHarassReason, test.expiresAt)
			self := url.Values{"actor": {s.f.authorDID}}.Encode()

			t.Run("own getProfile", func(t *testing.T) {
				profile := s.get(t, "/xrpc/social.coves.actor.getProfile?"+self, "banned account")
				assert.Equal(t, accountBanHidingDisplayName, profile["displayName"], "the account sees its full profile")
				assert.NotContains(t, profile, "accountStatus")
				assert.Equal(t, test.wantNotice, profile["banNotice"])
			})
			t.Run("own getViewerAuthority", func(t *testing.T) {
				authority := s.get(t, "/xrpc/social.coves.actor.getViewerAuthority", "banned account")
				assert.Equal(t, test.wantNotice, authority["banNotice"])
			})
			t.Run("own getPosts", func(t *testing.T) {
				assert.Equal(t, []string{s.post},
					moderationPostAcceptanceFeedURIs(t, s.get(t, "/xrpc/social.coves.actor.getPosts?"+self, "banned account")))
			})
			t.Run("own getComments", func(t *testing.T) {
				listed := s.get(t, "/xrpc/social.coves.actor.getComments?"+self, "banned account")
				viewerModerationActorComment(s.comment)(t, listed)
			})
			t.Run("unbanned account own getProfile", func(t *testing.T) {
				profile := s.get(t, "/xrpc/social.coves.actor.getProfile?"+url.Values{"actor": {s.f.nonAdminDID}}.Encode(), "other viewer")
				assert.NotContains(t, profile, "banNotice")
			})
			t.Run("unbanned account own getViewerAuthority", func(t *testing.T) {
				assert.Equal(t, map[string]any{"instanceAdmin": false},
					s.get(t, "/xrpc/social.coves.actor.getViewerAuthority", "other viewer"))
			})
		})
	}
}

// TestAccountBanTemporaryBanLapse bans A until a future expiresAt, then lets
// the stored expiry pass with no admin action (accountbanfixture.Lapse). While
// the ban is in force V gets the suspended profile; once it has lapsed V sees
// A's pre-ban post and comment and A's full profile, and neither A's own
// profile nor its viewer authority carries a ban notice.
func TestAccountBanTemporaryBanLapse(t *testing.T) {
	s := newAccountBanHidingScene(t)
	expiresAt := time.Date(2099, time.January, 1, 0, 0, 0, 0, time.UTC)
	s.moderator.Ban(t, s.f.authorDID, &expiresAt)
	actor := url.Values{"actor": {s.f.authorDID}}.Encode()

	t.Run("before lapse/getProfile/other viewer", func(t *testing.T) {
		assert.Equal(t, "suspended", s.get(t, "/xrpc/social.coves.actor.getProfile?"+actor, "other viewer")["accountStatus"])
	})

	s.moderator.Lapse(t, s.f.authorDID)
	t.Run("after lapse/community feed/other viewer", func(t *testing.T) {
		feed := "/xrpc/social.coves.communityFeed.getCommunity?" + url.Values{"community": {s.f.communityDID}, "sort": {"new"}}.Encode()
		assert.Contains(t, moderationPostAcceptanceFeedURIs(t, s.get(t, feed, "other viewer")), s.post)
	})
	t.Run("after lapse/thread/other viewer", func(t *testing.T) {
		thread := "/xrpc/social.coves.community.comment.getComments?" + url.Values{"post": {s.thread}, "sort": {"new"}}.Encode()
		s.requireComment(t, viewerModerationThreadNode(t, s.get(t, thread, "other viewer"), s.comment), false)
	})
	t.Run("after lapse/getProfile/other viewer", func(t *testing.T) {
		profile := s.get(t, "/xrpc/social.coves.actor.getProfile?"+actor, "other viewer")
		assert.NotContains(t, profile, "accountStatus")
		assert.NotContains(t, profile, "banNotice")
		assert.Equal(t, accountBanHidingDisplayName, profile["displayName"])
		assert.Equal(t, accountBanHidingDescription, profile["description"])
		stats := moderationAcceptanceObject(t, profile["stats"])
		assert.Equal(t, float64(1), stats["postCount"])
		assert.Equal(t, float64(2), stats["commentCount"])
	})
	t.Run("after lapse/own getProfile", func(t *testing.T) {
		profile := s.get(t, "/xrpc/social.coves.actor.getProfile?"+actor, "banned account")
		assert.Equal(t, accountBanHidingDisplayName, profile["displayName"])
		assert.NotContains(t, profile, "banNotice")
	})
	t.Run("after lapse/own getViewerAuthority", func(t *testing.T) {
		assert.Equal(t, map[string]any{"instanceAdmin": false},
			s.get(t, "/xrpc/social.coves.actor.getViewerAuthority", "banned account"))
	})
}

// TestAccountStatusRestrictionProfileAndThread restricts A by a stored
// #account status restriction, alone or alongside an in-force instance ban
// with a known reason. Another viewer and the signed-out public get A's
// suspended profile, A's post is absent from the community feed and A's
// comment is an instance-removed placeholder. A itself sees its full profile;
// a ban notice appears only when the admin ban is also in force, with the
// ban's reason.
func TestAccountStatusRestrictionProfileAndThread(t *testing.T) {
	for _, test := range []struct {
		name       string
		withBan    bool
		wantNotice map[string]any
	}{
		{name: "status restriction only"},
		{name: "status restriction and admin ban", withBan: true, wantNotice: map[string]any{"reason": accountBanHidingHarassReason}},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := newAccountBanHidingScene(t)
			accountstatusfixture.Set(t, s.f.db, s.f.authorDID, accountstatus.Takendown)
			if test.withBan {
				s.moderator.BanWithReason(t, s.f.authorDID, accountBanHidingHarassReason, nil)
			}
			actor := url.Values{"actor": {s.f.authorDID}}.Encode()
			feed := "/xrpc/social.coves.communityFeed.getCommunity?" + url.Values{"community": {s.f.communityDID}, "sort": {"new"}}.Encode()
			thread := "/xrpc/social.coves.community.comment.getComments?" + url.Values{"post": {s.thread}, "sort": {"new"}}.Encode()

			for _, viewer := range []string{"other viewer", "signed-out"} {
				t.Run("getProfile/"+viewer, func(t *testing.T) {
					s.requireSuspendedProfile(t, s.get(t, "/xrpc/social.coves.actor.getProfile?"+actor, viewer))
				})
				t.Run("community feed/"+viewer, func(t *testing.T) {
					accountBanHidingFeedPost(s.post)(t, s.get(t, feed, viewer), true)
				})
				t.Run("thread/"+viewer, func(t *testing.T) {
					s.requireComment(t, viewerModerationThreadNode(t, s.get(t, thread, viewer), s.comment), true)
				})
			}
			t.Run("own getProfile", func(t *testing.T) {
				profile := s.get(t, "/xrpc/social.coves.actor.getProfile?"+actor, "banned account")
				assert.Equal(t, accountBanHidingDisplayName, profile["displayName"], "the account sees its full profile")
				assert.NotContains(t, profile, "accountStatus")
				if test.wantNotice == nil {
					assert.NotContains(t, profile, "banNotice", "a status restriction alone carries no ban notice")
					return
				}
				assert.Equal(t, test.wantNotice, profile["banNotice"])
			})
		})
	}
}

// accountRestrictionPostGetItem reads post.get for the scene's post as viewer
// and returns its single item.
func (s accountBanHidingScene) accountRestrictionPostGetItem(t *testing.T, viewer string) map[string]any {
	t.Helper()
	items := moderationAcceptanceArray(t, s.get(t, "/xrpc/social.coves.community.post.get?"+url.Values{"uris": {s.post}}.Encode(), viewer)["posts"])
	require.Len(t, items, 1)
	return moderationAcceptanceObject(t, items[0])
}

// TestAccountRestrictionPostGetTombstones reads post.get for author P's post
// after a direct instance removal, a community withdrawal (removedPost), or
// both, while P is restricted by an in-force instance account ban or by a
// stored PDS status restriction. Every viewer but P reads the post as
// notFoundPost, which names no author, community or moderation; P keeps the
// moderated tombstone for a direct removal and its own post view for a
// withdrawal alone. Unrestricted controls keep moderatedPost and removedPost.
func TestAccountRestrictionPostGetTombstones(t *testing.T) {
	const (
		directRemoval = "direct instance removal"
		withdrawal    = "community withdrawal"
	)
	type want int
	const (
		notFound want = iota
		moderated
		removed
		ownView
	)
	restrictions := map[string]func(s accountBanHidingScene, t *testing.T){
		"account ban": func(s accountBanHidingScene, t *testing.T) { s.moderator.Ban(t, s.f.authorDID, nil) },
		"status restriction": func(s accountBanHidingScene, t *testing.T) {
			accountstatusfixture.Set(t, s.f.db, s.f.authorDID, accountstatus.Takendown)
		},
		"unrestricted": func(accountBanHidingScene, *testing.T) {},
	}
	type row struct {
		name        string
		restriction string
		states      []string
		viewers     map[string]want
	}
	var rows []row
	for _, restriction := range []string{"account ban", "status restriction"} {
		rows = append(rows,
			row{
				name: "restricted author, direct removal", restriction: restriction, states: []string{directRemoval},
				viewers: map[string]want{"signed-out": notFound, "other viewer": notFound, "banned account": moderated},
			},
			row{
				name: "restricted author, community withdrawal", restriction: restriction, states: []string{withdrawal},
				viewers: map[string]want{"signed-out": notFound, "other viewer": notFound, "banned account": ownView},
			},
			row{
				name: "restricted author, withdrawal and direct removal", restriction: restriction, states: []string{withdrawal, directRemoval},
				viewers: map[string]want{"signed-out": notFound, "other viewer": notFound},
			},
		)
	}
	rows = append(rows,
		row{name: "unrestricted author, direct removal", restriction: "unrestricted", states: []string{directRemoval}, viewers: map[string]want{"signed-out": moderated}},
		row{name: "unrestricted author, community withdrawal", restriction: "unrestricted", states: []string{withdrawal}, viewers: map[string]want{"signed-out": removed}},
	)
	for _, tc := range rows {
		t.Run(tc.restriction+"/"+tc.name, func(t *testing.T) {
			s := newAccountBanHidingScene(t)
			for _, state := range tc.states {
				switch state {
				case directRemoval:
					result := moderationAcceptanceBody(t, s.f.post(t, moderationRouteRemovePath, s.f.adminToken, map[string]any{
						"subject": map[string]string{"uri": s.post, "cid": s.f.postCID}, "expectedVersion": "v0",
						"idempotencyKey": testkit.UniqueIDWithPrefix(t, "remove"), "reason": "social.coves.moderation.defs#reasonSpam",
					}))
					require.Equal(t, "applied", result["outcome"])
				case withdrawal:
					_, err := s.f.db.ExecContext(t.Context(), `UPDATE community_post_admissions
						SET status = 'removed', decision_code = 'communityRule', decision_at = NOW()
						WHERE post_uri = $1`, s.post)
					require.NoError(t, err)
				}
			}
			restrictions[tc.restriction](s, t)
			for viewer, expected := range tc.viewers {
				t.Run(viewer, func(t *testing.T) {
					item := s.accountRestrictionPostGetItem(t, viewer)
					switch expected {
					case notFound:
						assert.Equal(t, map[string]any{"uri": s.post, "notFound": true}, item,
							"a restricted author's post is notFoundPost with no author, community or moderation")
					case moderated:
						assert.Equal(t, "social.coves.community.post.defs#moderatedPost", item["$type"], "item: %v", item)
						assert.Equal(t, s.f.authorDID, item["authorDid"])
						assert.Contains(t, item, "moderation")
					case removed:
						assert.Equal(t, map[string]any{"uri": s.post, "removed": true, "code": "communityRule"}, item)
					case ownView:
						assert.Equal(t, s.post, item["uri"])
						assert.NotContains(t, item, "$type", "the author's own post view carries no tombstone type")
						for _, key := range []string{"notFound", "removed", "blocked"} {
							assert.NotContains(t, item, key)
						}
						assert.NotNil(t, item["record"], "the author's own post view serves its record")
					}
				})
			}
		})
	}
}
