//go:build integration

package routes_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	actorAPI "Coves/internal/api/handlers/actor"
	commentsAPI "Coves/internal/api/handlers/comments"
	"Coves/internal/api/middleware"
	"Coves/internal/api/routes"
	"Coves/internal/core/blueskypost"
	"Coves/internal/core/comments"
	"Coves/internal/core/communities"
	"Coves/internal/core/communityFeeds"
	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Coves quotes must never invoke Bluesky resolution, but the serving handlers
// must still execute TransformPostEmbeds with a configured resolver.
type rejectingCovesQuoteResolver struct{}

func (rejectingCovesQuoteResolver) ResolvePost(context.Context, string) (*blueskypost.BlueskyPostResult, error) {
	panic("Coves quotes must not trigger Bluesky resolution")
}

func (rejectingCovesQuoteResolver) ParseBlueskyURL(context.Context, string) (string, error) {
	panic("Coves quote reads must not parse Bluesky URLs")
}

func (rejectingCovesQuoteResolver) IsBlueskyURL(string) bool {
	panic("Coves quote reads must not classify Bluesky URLs")
}

func TestModerationCovesQuoteProjectionAcrossReadPaths(t *testing.T) {
	for _, storedType := range []string{"social.coves.embed.post", "social.coves.embed.post#view"} {
		t.Run(storedType, func(t *testing.T) {
			db := testkit.DB(t)
			postRepo := postgres.NewPostRepository(db)
			commentRepo := postgres.NewCommentRepository(db)
			userRepo := postgres.NewUserRepository(db)
			communityRepo := postgres.NewCommunityRepository(db, credentialciphertest.Fixed())
			instanceDID := fixtures.InstanceDID()
			moderationService := moderation.NewService(
				moderation.NewRepositorySubjectReader(postRepo, commentRepo), postgres.NewModerationRepository(db),
				moderation.Config{InstanceDID: instanceDID, IdempotencyRetention: 24 * time.Hour, MaxLiveIdempotencyKeys: 1000},
			)
			pdsURL := testkit.Endpoints().PDS.BaseURL
			communityService := communities.NewCommunityServiceWithPDSFactory(
				communityRepo, pdsURL, instanceDID, "", nil, nil, nil,
				communities.PrivateHostOptions(true)...,
			)
			postService := posts.NewPostService(postRepo, communityService, nil, nil, nil, nil, pdsURL,
				posts.WithAdmissionPolicy(posts.NewAllowAllAdmissionPolicyForTests()),
				posts.WithSyncAcceptance(postgres.NewAdmissionRepository(db), nil),
			)
			commentService := comments.NewCommentService(commentRepo, userRepo, postRepo, communityRepo, nil, nil, nil)
			feedService := communityFeeds.NewCommunityFeedService(
				postgres.NewCommunityFeedRepository(db, "moderation-quote-cursor-secret"), communityService,
			)

			authorName := testkit.UniqueIDWithPrefix(t, "quoteauthor")
			const authorDID = "did:plc:bbbbbbbbbbbbbbbbbbbbbbbb"
			fixtures.User(t, db, authorName+".test", authorDID)
			communityName := testkit.UniqueIDWithPrefix(t, "quotecommunity")
			const communityDID = "did:plc:cccccccccccccccccccccccc"
			const ownerDID = "did:plc:dddddddddddddddddddddddd"
			fixtures.User(t, db, "owner"+communityName+".test", ownerDID)
			_, err := db.ExecContext(t.Context(), `
				INSERT INTO communities (did, name, owner_did, created_by_did, hosted_by_did, handle, pds_url, created_at)
				VALUES ($1, $2, $3, $3, $4, $5, $6, NOW())
			`, communityDID, communityName, ownerDID, instanceDID, communityName+".coves.social", pdsURL)
			require.NoError(t, err)
			const postCID = "bafyreihgdyzzpkkzq2izfnhcmm77ycuacvkuziwbnqxfxtqsz7tmxwhnshi"
			insertPost := func(title string, embed any) string {
				t.Helper()
				rkey := testkit.TID()
				uri := "at://" + authorDID + "/" + moderation.PostV2Collection + "/" + rkey
				encoded, err := json.Marshal(embed)
				require.NoError(t, err)
				_, err = db.ExecContext(t.Context(), `
					INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, content, embed, created_at)
					VALUES ($1, $2, $3, $4, $5, $6, 'quote fixture body', $7, NOW())
				`, uri, postCID, rkey, authorDID, communityDID, title, encoded)
				require.NoError(t, err)
				_, err = db.ExecContext(t.Context(), `
					INSERT INTO community_post_admissions
					    (community_did, post_uri, status, accepted_cid, evaluated_cid, last_community_rev, last_community_op_rank, created_at, updated_at)
					VALUES ($1, $2, 'accepted', $3, $3, '3lqqqqqqqqqq2', 1, NOW(), NOW())
				`, communityDID, uri, postCID)
				require.NoError(t, err)
				return uri
			}
			quotedURI := insertPost("quoted post private title", nil)
			cleanQuotedURI := insertPost("clean quoted post", nil)
			leakImage := map[string]any{"$type": "social.coves.embed.images#view", "images": []any{map[string]any{"fullsize": "leaked-image", "alt": "quoted image"}}}
			quoterEmbed := map[string]any{
				"$type": storedType, "post": map[string]any{"uri": quotedURI, "cid": postCID},
				"resolved": map[string]any{"title": "quoted post private title", "text": "quoted private body", "embed": leakImage},
				"title":    "leak", "text": "leak", "images": []any{leakImage},
			}
			quoterURI := insertPost("quoter searchable marker", quoterEmbed)
			storedQuoterEmbed, err := json.Marshal(quoterEmbed)
			require.NoError(t, err)
			cleanQuoterURI := insertPost("clean quoter", map[string]any{
				"$type": "social.coves.embed.post", "post": map[string]any{"uri": cleanQuotedURI, "cid": postCID},
			})

			adminDID := fixtures.DID(testkit.UniqueIDWithPrefix(t, "quoteadmin"))
			const adminToken = "quote-projection-admin-session"
			unsealer := fixtures.NewSessionUnsealer()
			oauthStore := fixtures.NewOAuthStore()
			unsealer.AddSession(adminToken, adminDID, "quote-admin")
			oauthStore.AddSession(adminDID, "quote-admin", "test-access-token")
			optionalAuth := middleware.NewOAuthAuthMiddleware(unsealer, oauthStore)
			adminAuth := middleware.NewInstanceAdminMiddleware(unsealer, oauthStore, nil, moderation.NewAllowlistAuthority([]string{adminDID}))
			quoteResolver := rejectingCovesQuoteResolver{}
			mux := chi.NewRouter()
			routes.RegisterModerationRoutes(mux, moderationService, adminAuth)
			routes.RegisterPostRoutes(mux, postService, nil, quoteResolver, optionalAuth, optionalAuth)
			routes.RegisterCommunityFeedRoutes(mux, feedService, nil, quoteResolver, optionalAuth)
			mux.With(optionalAuth.OptionalAuth).Get("/xrpc/social.coves.actor.getPosts",
				actorAPI.NewGetPostsHandler(postService, nil, nil, quoteResolver).HandleGetPosts)
			mux.With(optionalAuth.OptionalAuth).Get("/xrpc/social.coves.community.comment.getComments",
				commentsAPI.NewGetCommentsHandler(commentsAPI.NewServiceAdapter(commentService), nil).HandleGetComments)
			server := httptest.NewServer(mux)
			t.Cleanup(server.Close)
			client := server.Client()
			request := func(path string) map[string]any {
				t.Helper()
				return moderationAcceptanceBody(t, moderationAcceptanceRequest(t, client, http.MethodGet, server.URL+path, "", nil))
			}
			postPath := func(uri string) string {
				return "/xrpc/social.coves.community.post.get?" + url.Values{"uris": {uri}}.Encode()
			}
			postItem := func(uri string) map[string]any {
				t.Helper()
				items := moderationAcceptanceArray(t, request(postPath(uri))["posts"])
				require.Len(t, items, 1)
				return moderationAcceptanceObject(t, items[0])
			}
			// A clean Coves quote is currently served as social.coves.embed.post,
			// without server-side resolution. R4 pins that same strongRef-only type
			// even for stored #view embeds claiming pre-resolved data.
			quoteType := "social.coves.embed.post"
			assertQuote := func(t *testing.T, post map[string]any, quoted string) {
				t.Helper()
				require.Equal(t, quoterURI, post["uri"])
				require.NotContains(t, post, "$type", "a normal postView carries no union discriminator")
				_, hasRecord := post["record"]
				require.True(t, hasRecord, "the quoter itself must be served as a normal post view")
				embed := moderationAcceptanceObject(t, post["embed"])
				assert.Equal(t, map[string]any{
					"$type": quoteType, "post": map[string]any{"uri": quoted, "cid": postCID},
				}, embed, "the served embed must contain only the quoted strongRef, with no stored preview or media")
				// Decision (orchestrator, from the DoD "P's embed holds Q's
				// strongRef only"): record is the quoter's own authored record,
				// served verbatim per the lexicon, so record.embed keeps every
				// byte the quoter wrote, including the preview fields it forged.
				// None of it is AppView-held content of the quoted post: the
				// server adds resolved only at serve time, and only for Bluesky
				// URIs. The projection applies to the top-level embed alone.
				record := moderationAcceptanceObject(t, post["record"])
				recordEmbed, err := json.Marshal(record["embed"])
				require.NoError(t, err)
				assert.JSONEq(t, string(storedQuoterEmbed), string(recordEmbed), "record.embed must be the stored embed verbatim")
			}
			t.Run("clean-control", func(t *testing.T) {
				clean := postItem(cleanQuoterURI)
				require.Equal(t, cleanQuoterURI, clean["uri"])
				assert.Equal(t, map[string]any{
					"$type": quoteType, "post": map[string]any{"uri": cleanQuotedURI, "cid": postCID},
				}, moderationAcceptanceObject(t, clean["embed"]), "unremoved clean Coves quotes have the same strongRef-only projection")
			})

			state := moderationAcceptanceObject(t, moderationAcceptanceBody(t,
				requestSubjectState(t, client, server.URL, quotedURI, adminToken))["state"])
			version, ok := state["version"].(string)
			require.True(t, ok)
			removed := moderationAcceptanceBody(t, moderationAcceptanceRequest(t, client, http.MethodPost,
				server.URL+"/xrpc/social.coves.moderation.removeContent", adminToken, map[string]any{
					"subject": map[string]string{"uri": quotedURI, "cid": postCID}, "expectedVersion": version,
					"idempotencyKey": "remove-quoted-post", "reason": "social.coves.moderation.defs#reasonSpam",
				}))
			require.Equal(t, "applied", removed["outcome"])
			require.Equal(t, "social.coves.community.post.defs#moderatedPost", postItem(quotedURI)["$type"])

			readPaths := []struct {
				name       string
				path       string
				selectPost func(*testing.T, map[string]any) map[string]any
			}{
				{"post.get", postPath(quoterURI), func(t *testing.T, body map[string]any) map[string]any {
					items := moderationAcceptanceArray(t, body["posts"])
					require.Len(t, items, 1)
					return moderationAcceptanceObject(t, items[0])
				}},
				{"community feed", "/xrpc/social.coves.communityFeed.getCommunity?" + url.Values{"community": {communityDID}, "sort": {"new"}}.Encode(), func(t *testing.T, body map[string]any) map[string]any {
					return moderationQuoteFeedPost(t, body, quoterURI)
				}},
				{"actor.getPosts", "/xrpc/social.coves.actor.getPosts?" + url.Values{"actor": {authorDID}}.Encode(), func(t *testing.T, body map[string]any) map[string]any {
					return moderationQuoteFeedPost(t, body, quoterURI)
				}},
				{"searchPosts", "/xrpc/social.coves.feed.searchPosts?" + url.Values{"q": {"quoter searchable marker"}}.Encode(), func(t *testing.T, body map[string]any) map[string]any {
					return moderationQuoteFeedPost(t, body, quoterURI)
				}},
				{"getComments header", "/xrpc/social.coves.community.comment.getComments?" + url.Values{"post": {quoterURI}, "sort": {"new"}}.Encode(), func(t *testing.T, body map[string]any) map[string]any {
					return moderationAcceptanceObject(t, body["post"])
				}},
			}
			for _, readPath := range readPaths {
				t.Run(readPath.name, func(t *testing.T) {
					assertQuote(t, readPath.selectPost(t, request(readPath.path)), quotedURI)
				})
			}
		})
	}
}

func moderationQuoteFeedPost(t *testing.T, body map[string]any, uri string) map[string]any {
	t.Helper()
	for _, item := range moderationAcceptanceArray(t, body["feed"]) {
		post := moderationAcceptanceObject(t, moderationAcceptanceObject(t, item)["post"])
		if post["uri"] == uri {
			return post
		}
	}
	require.FailNow(t, "quoter missing from read path", "uri: %s; body: %#v", uri, body)
	return nil
}
