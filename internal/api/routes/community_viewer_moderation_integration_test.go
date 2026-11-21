//go:build integration

package routes_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"Coves/internal/api/handlers/actor"
	"Coves/internal/api/handlers/post"
	"Coves/internal/api/routes"
	"Coves/internal/core/communities"
	"Coves/internal/core/feedgen"
	"Coves/internal/core/moderation"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/fixtures/accountbanfixture"
	"Coves/tests/testkit"

	commentsAPI "Coves/internal/api/handlers/comments"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
)

// The community moderation actions a viewer may take, served over the real
// role authorizer and role index: the hosting query, the indexed roster and
// the in-force instance account ban decide them.
func TestCommunityViewerModerationReadsOverTheRealAuthorizer(t *testing.T) {
	f := newContentLabelRouteFixture(t)
	ctx := t.Context()
	db := f.db

	// The fixture community becomes hosted the way the authorizer checks it:
	// this instance stores its PDS credentials.
	communityRepo := postgres.NewCommunityRepository(db, credentialciphertest.Fixed())
	require.NoError(t, communityRepo.UpdateCredentials(ctx, f.communityDID, "community-access-token", "community-refresh-token"))
	hosted := postgres.NewHostedCommunityQuery(db)
	isHosted, err := hosted.IsHostedCommunity(ctx, f.communityDID)
	require.NoError(t, err)
	require.True(t, isHosted, "fixture: the community must be hosted")

	// A community this instance indexes but does not host.
	const unhostedDID = "did:plc:uuuuuuuuuuuuuuuuuuuuuuuu"
	unhostedName := testkit.UniqueIDWithPrefix(t, "unhosted")
	_, err = db.ExecContext(ctx, `
		INSERT INTO communities (did, name, owner_did, created_by_did, hosted_by_did, handle, created_at)
		VALUES ($1, $2, $1, $1, $3, $4, NOW())
	`, unhostedDID, unhostedName, f.instanceDID, unhostedName+".coves.social")
	require.NoError(t, err)
	isHosted, err = hosted.IsHostedCommunity(ctx, unhostedDID)
	require.NoError(t, err)
	require.False(t, isHosted, "fixture: the second community must not be hosted")

	const ownerDID = "did:plc:dddddddddddddddddddddddd" // the fixture community's owner
	user := func(prefix string) string {
		name := testkit.UniqueIDWithPrefix(t, prefix)
		did := fixtures.DID(name)
		fixtures.User(t, db, name+".test", did)
		return did
	}
	moderatorDID := user("cvmod")
	bannedModeratorDID := user("cvbannedmod")
	moderatorAuthorDID := user("cvmodauthor")

	roles := postgres.NewCommunityRoleRepository(db)
	grant := func(communityDID, subjectDID, role string) {
		t.Helper()
		require.NoError(t, roles.UpsertCommunityRole(ctx, communities.CommunityRole{
			CommunityDID: communityDID, SubjectDID: subjectDID, Role: role,
			RecordKey: communities.RoleRecordKey(subjectDID), RecordCID: "bafyreicommunityviewerrole",
			RecordCreatedAt: time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC),
		}))
	}
	grant(f.communityDID, ownerDID, communities.RoleOwner)
	for _, did := range []string{moderatorDID, bannedModeratorDID, moderatorAuthorDID} {
		grant(f.communityDID, did, communities.RoleModerator)
	}
	grant(unhostedDID, unhostedDID, communities.RoleOwner)
	grant(unhostedDID, moderatorDID, communities.RoleModerator)
	grant(unhostedDID, bannedModeratorDID, communities.RoleModerator)

	accountbanfixture.New(db).Ban(t, bannedModeratorDID, nil)

	// The reader exactly as cmd/server wiring.go builds it.
	authorizer := communities.NewRoleAuthorizer(communities.RoleAuthorizerConfig{
		Hosted:        hosted,
		Removals:      postgres.NewCommunityRemovalReader(db, f.instanceDID),
		Roles:         roles,
		AccountBans:   postgres.NewAccountBanReader(db, f.instanceDID),
		AccountStatus: postgres.NewAccountStatusRepository(db),
		Admins:        f.authority,
		Now:           time.Now,
	})
	viewer := moderation.NewCommunityViewer(authorizer, roles, f.authority)
	mux := chi.NewRouter()
	getPost := post.NewGetHandler(f.postService, nil, nil).WithViewerModeration(f.authority, f.instanceDID).
		WithCommunityViewerModeration(viewer)
	mux.With(f.optionalAuth.OptionalAuth).Get("/xrpc/social.coves.community.post.get", getPost.HandleGet)
	// communityFeed.getCommunity is a getFeed alias; sort new is the
	// production new generator over the real post hydrator.
	restriction := postgres.WithAccountRestrictionAuthority(f.instanceDID)
	feedRegistry := feedgen.NewRegistry(f.instanceDID, feedgen.NewBuiltin(postgres.NewNewGenerator(db, restriction)))
	feedService := feedgen.NewService(feedRegistry, postgres.NewPostHydrator(db, restriction),
		postgres.NewCursorSigner("community-viewer-moderation-cursor-secret"))
	routes.RegisterFeedRoutes(mux, feedService, feedRegistry, f.communityService, nil, nil, f.optionalAuth, f.authority, f.instanceDID,
		routes.WithCommunityViewerModeration(viewer))
	actorComments := actor.NewGetCommentsHandler(f.commentService, f.userService, nil).WithViewerModeration(f.authority, f.instanceDID).
		WithCommunityViewerModeration(viewer)
	mux.With(f.optionalAuth.OptionalAuth).Get("/xrpc/social.coves.actor.getComments", actorComments.HandleGetComments)
	thread := commentsAPI.NewGetCommentsHandler(commentsAPI.NewServiceAdapter(f.commentService), nil, f.authority, f.instanceDID).
		WithCommunityViewerModeration(viewer)
	mux.Handle("/xrpc/social.coves.community.comment.getComments",
		commentsAPI.OptionalAuthMiddleware(f.optionalAuth, thread.HandleGetComments))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	// Posts in the hosted community.
	livePost := f.insertPost("community viewer live post")
	moderatorPost := f.insertPostBy(moderatorAuthorDID, "community viewer post by a moderator")
	instanceRemovedPost := f.insertPost("community viewer instance-removed post")
	doublyRemovedPost := f.insertPost("community viewer post removed by instance and community")
	for _, uri := range []string{instanceRemovedPost, doublyRemovedPost} {
		removed := moderationAcceptanceBody(t, f.post(t, moderationRouteRemovePath, f.adminToken, map[string]any{
			"subject": map[string]string{"uri": uri, "cid": f.postCID}, "expectedVersion": "v0",
			"idempotencyKey": testkit.UniqueIDWithPrefix(t, "remove"), "reason": "social.coves.moderation.defs#reasonSpam",
		}))
		require.Equal(t, "applied", removed["outcome"])
	}
	_, err = db.ExecContext(ctx, `
		UPDATE community_post_admissions SET status = 'removed', decision_code = 'communityRule', decision_at = NOW(), updated_at = NOW()
		WHERE community_did = $1 AND post_uri = $2
	`, f.communityDID, doublyRemovedPost)
	require.NoError(t, err)
	// A post in the community this instance does not host.
	unhostedPost := f.insertPostIn(unhostedDID, f.authorDID, "community viewer post in an unhosted community")

	// Comments on a thread in the hosted community.
	const topCID = "bafyreicommunityviewertopcomment"
	const removedCID = "bafyreicommunityviewerremovedcomment"
	topComment := moderationAcceptanceInsertComment(t, db, f.authorDID, livePost, f.postCID, livePost, f.postCID,
		topCID, "live top-level comment")
	reply := moderationAcceptanceInsertComment(t, db, f.authorDID, livePost, f.postCID, topComment, topCID,
		"bafyreicommunityviewerreply", "live nested reply")
	removedComment := moderationAcceptanceInsertComment(t, db, f.authorDID, livePost, f.postCID, livePost, f.postCID,
		removedCID, "top-level comment the community removes")
	_, err = db.ExecContext(ctx, `UPDATE comments SET reply_count = 1 WHERE uri = $1`, topComment)
	require.NoError(t, err)
	// A community removal of the comment, as the community comment removal
	// consumer stores it.
	_, err = db.ExecContext(ctx, `
		INSERT INTO moderation_actions
		    (id, actor_did, authority_did, scope_kind, scope_community_did,
		     subject_uri, subject_collection, subject_community_did, subject_root_uri, observed_cid,
		     action, origin, created_at)
		VALUES ('community-viewer-comment-removal', $1, $1, 'community', $1, $2, $3, $1, $4, $5,
		        'apply-removal', 'inherited', NOW())
	`, f.communityDID, removedComment, moderation.CommentCollection, livePost, removedCID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO moderation_decisions
		    (authority_did, scope_kind, scope_community_did, subject_uri, kind, value, active_action_id, active)
		VALUES ($1, 'community', $1, $2, 'removal', NULL, 'community-viewer-comment-removal', TRUE)
	`, f.communityDID, removedComment)
	require.NoError(t, err)

	feed := func(communityDID string) string {
		return "/xrpc/social.coves.communityFeed.getCommunity?" + url.Values{"community": {communityDID}, "sort": {"new"}}.Encode()
	}
	getPosts := func(uri string) string {
		return "/xrpc/social.coves.community.post.get?" + url.Values{"uris": {uri}}.Encode()
	}
	threadTarget := "/xrpc/social.coves.community.comment.getComments?" + url.Values{"post": {livePost}, "sort": {"new"}}.Encode()
	// No sort: the service defaults to hot, the hot-rank top-level query.
	threadHotTarget := "/xrpc/social.coves.community.comment.getComments?" + url.Values{"post": {livePost}}.Encode()
	// A parentRkey request resolves the anchor comment by (root, rkey) and
	// serves it as the sole top-level comment.
	topCommentRkey := topComment[strings.LastIndex(topComment, "/")+1:]
	subtreeTarget := "/xrpc/social.coves.community.comment.getComments?" + url.Values{
		"post": {livePost}, "parentRkey": {topCommentRkey}, "sort": {"new"},
	}.Encode()
	actorCommentsTarget := "/xrpc/social.coves.actor.getComments?" + url.Values{"actor": {f.authorDID}}.Encode()

	rows := []struct {
		name   string
		target string
		item   viewerModerationRowItem
		// want is the moderator's literal list; nil means the key is absent.
		want []any
	}{
		{name: "feed post by an author with no role", target: feed(f.communityDID), item: viewerModerationFeedPost(livePost), want: []any{"remove", "ban-author"}},
		{name: "feed post by a moderator of the community", target: feed(f.communityDID), item: viewerModerationFeedPost(moderatorPost), want: []any{"remove"}},
		{name: "thread top-level comment", target: threadTarget, item: viewerModerationThreadComment(topComment), want: []any{"remove", "ban-author"}},
		{name: "thread top-level comment sorted hot", target: threadHotTarget, item: viewerModerationThreadComment(topComment), want: []any{"remove", "ban-author"}},
		{name: "thread subtree anchor comment", target: subtreeTarget, item: viewerModerationThreadComment(topComment), want: []any{"remove", "ban-author"}},
		{name: "thread nested reply", target: threadTarget, item: viewerModerationThreadReply(topComment, reply), want: []any{"remove", "ban-author"}},
		{
			name: "thread community-removed comment", target: threadTarget, want: []any{"restore", "ban-author"},
			item: func(t *testing.T, body map[string]any) map[string]any {
				t.Helper()
				placeholder := viewerModerationThreadComment(removedComment)(t, body)
				require.Equal(t, true, placeholder["isDeleted"])
				return placeholder
			},
		},
		{name: "actor comments", target: actorCommentsTarget, item: viewerModerationActorComment(topComment), want: []any{"remove", "ban-author"}},
		{
			name: "post.get instance-removed post accepted in its community", target: getPosts(instanceRemovedPost),
			item: viewerModerationGetPost(instanceRemovedPost, true), want: []any{"remove", "ban-author"},
		},
		{
			name: "post.get post removed by instance and community", target: getPosts(doublyRemovedPost),
			item: viewerModerationGetPost(doublyRemovedPost, true), want: []any{"restore", "ban-author"},
		},
		{name: "feed post in an unhosted community", target: feed(unhostedDID), item: viewerModerationFeedPost(unhostedPost)},
		{name: "post.get post in an unhosted community", target: getPosts(unhostedPost), item: viewerModerationGetPost(unhostedPost, false)},
	}
	viewers := []struct {
		name      string
		token     string
		moderator bool
	}{
		{name: "moderator", token: f.signIn(moderatorDID), moderator: true},
		{name: "banned moderator", token: f.signIn(bannedModeratorDID)},
	}
	for _, row := range rows {
		for _, v := range viewers {
			t.Run(row.name+"/"+v.name, func(t *testing.T) {
				body := moderationAcceptanceBody(t, moderationAcceptanceRequest(t, server.Client(), http.MethodGet,
					server.URL+row.target, v.token, nil))
				item := row.item(t, body)
				var actions any
				present := false
				if rawViewer, ok := item["viewer"]; ok {
					actions, present = moderationAcceptanceObject(t, rawViewer)["communityModeration"]
				}
				if v.moderator && row.want != nil {
					require.Truef(t, present, "the moderator must get viewer.communityModeration; item viewer was %#v", item["viewer"])
					require.Equal(t, row.want, actions)
					return
				}
				require.Falsef(t, present, "viewer.communityModeration must be omitted for the %s; got %#v", v.name, actions)
			})
		}
	}
}
