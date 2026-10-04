//go:build integration

package routes_test

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"Coves/internal/api/middleware"
	"Coves/internal/api/routes"
	"Coves/internal/core/communities"
	"Coves/internal/core/communityFeeds"
	"Coves/internal/core/moderation"
	"Coves/internal/core/posts"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/internal/validation"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/bluesky-social/indigo/atproto/atdata"
	"github.com/bluesky-social/indigo/atproto/lexicon"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
)

type contentLabelRouteFixture struct {
	db            *sql.DB
	server        *httptest.Server
	client        *http.Client
	instanceDID   string
	authorDID     string
	communityDID  string
	adminDID      string
	adminToken    string
	nonAdminToken string
	postCID       string
	pdsRequests   *atomic.Int64
	insertPost    func(title string) string
}

func newContentLabelRouteFixture(t *testing.T) contentLabelRouteFixture {
	t.Helper()
	db := testkit.DB(t)
	var pdsRequests atomic.Int64
	pds := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		pdsRequests.Add(1)
		http.NotFound(w, nil)
	}))
	t.Cleanup(pds.Close)
	t.Cleanup(func() { require.Zero(t, pdsRequests.Load(), "moderation must never contact the PDS") })

	postRepo := postgres.NewPostRepository(db)
	instanceDID := fixtures.InstanceDID()
	moderationService := moderation.NewService(
		moderation.NewRepositorySubjectReader(postRepo, postgres.NewCommentRepository(db)),
		postgres.NewModerationRepository(db),
		moderation.Config{InstanceDID: instanceDID, IdempotencyRetention: 24 * time.Hour, MaxLiveIdempotencyKeys: 1000, CursorSecret: "content-label-acceptance-secret"},
	)
	communityRepo := postgres.NewCommunityRepository(db, credentialciphertest.Fixed())
	communityService := communities.NewCommunityServiceWithPDSFactory(
		communityRepo, pds.URL, instanceDID, "", nil, nil, nil,
		communities.PrivateHostOptions(true)...,
	)
	postService := posts.NewPostService(postRepo, communityService, nil, nil, nil, nil, pds.URL,
		posts.WithAdmissionPolicy(posts.NewAllowAllAdmissionPolicyForTests()),
		posts.WithSyncAcceptance(postgres.NewAdmissionRepository(db), nil),
	)
	feedService := communityFeeds.NewCommunityFeedService(
		postgres.NewCommunityFeedRepository(db, "content-label-acceptance-cursor-secret"), communityService,
	)

	authorName := testkit.UniqueIDWithPrefix(t, "labelauthor")
	const authorDID = "did:plc:bbbbbbbbbbbbbbbbbbbbbbbb"
	fixtures.User(t, db, authorName+".test", authorDID)
	communityName := testkit.UniqueIDWithPrefix(t, "labelcommunity")
	const communityDID = "did:plc:cccccccccccccccccccccccc"
	const ownerDID = "did:plc:dddddddddddddddddddddddd"
	fixtures.User(t, db, "owner"+communityName+".test", ownerDID)
	_, err := db.ExecContext(t.Context(), `
		INSERT INTO communities (did, name, owner_did, created_by_did, hosted_by_did, handle, pds_url, created_at)
		VALUES ($1, $2, $3, $3, $4, $5, $6, NOW())
	`, communityDID, communityName, ownerDID, instanceDID, communityName+".coves.social", pds.URL)
	require.NoError(t, err)
	const postCID = "bafyreihgdyzzpkkzq2izfnhcmm77ycuacvkuziwbnqxfxtqsz7tmxwhnshi"
	insertAcceptedPost := func(title string) string {
		t.Helper()
		rkey := testkit.TID()
		uri := "at://" + authorDID + "/" + moderation.PostV2Collection + "/" + rkey
		_, err := db.ExecContext(t.Context(), `
			INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, content, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())
		`, uri, postCID, rkey, authorDID, communityDID, title, "original record body")
		require.NoError(t, err)
		_, err = db.ExecContext(t.Context(), `
			INSERT INTO community_post_admissions
				(community_did, post_uri, status, accepted_cid, evaluated_cid, last_community_rev, last_community_op_rank, created_at, updated_at)
			VALUES ($1, $2, 'accepted', $3, $3, '3lqqqqqqqqqq2', 1, NOW(), NOW())
		`, communityDID, uri, postCID)
		require.NoError(t, err)
		return uri
	}
	adminDID := fixtures.DID(testkit.UniqueIDWithPrefix(t, "labeladmin"))
	const adminToken = "content-label-admin-session"
	unsealer := fixtures.NewSessionUnsealer()
	oauthStore := fixtures.NewOAuthStore()
	unsealer.AddSession(adminToken, adminDID, "content-label-admin")
	oauthStore.AddSession(adminDID, "content-label-admin", "test-access-token")
	adminAuth := middleware.NewInstanceAdminMiddleware(unsealer, oauthStore, nil,
		moderation.NewAllowlistAuthority([]string{adminDID}))
	nonAdminDID := fixtures.DID(testkit.UniqueIDWithPrefix(t, "labelother"))
	const nonAdminToken = "content-label-nonadmin-session"
	unsealer.AddSession(nonAdminToken, nonAdminDID, "content-label-nonadmin")
	oauthStore.AddSession(nonAdminDID, "content-label-nonadmin", "non-admin-access-token")
	optionalAuth := middleware.NewOAuthAuthMiddleware(unsealer, oauthStore)
	mux := chi.NewRouter()
	routes.RegisterModerationRoutes(mux, moderationService, adminAuth)
	routes.RegisterPostRoutes(mux, postService, nil, nil, optionalAuth, optionalAuth)
	routes.RegisterCommunityFeedRoutes(mux, feedService, nil, nil, optionalAuth)
	routes.RegisterActorRoutes(mux, postService, nil, nil, nil, nil, optionalAuth)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return contentLabelRouteFixture{
		db: db, server: server, client: server.Client(), instanceDID: instanceDID,
		authorDID: authorDID, communityDID: communityDID, adminDID: adminDID,
		adminToken: adminToken, nonAdminToken: nonAdminToken, postCID: postCID,
		pdsRequests: &pdsRequests, insertPost: insertAcceptedPost,
	}
}

func TestModerationContentLabelAcceptance(t *testing.T) {
	f := newContentLabelRouteFixture(t)
	db, server, client, instanceDID, adminDID, adminToken := f.db, f.server, f.client, f.instanceDID, f.adminDID, f.adminToken
	communityDID, postCID, pdsRequests := f.communityDID, f.postCID, f.pdsRequests
	postURI := f.insertPost("original labelled post")
	controlURI := f.insertPost("unlabelled control post")
	postURL := server.URL + "/xrpc/social.coves.community.post.get?" + url.Values{"uris": {postURI}}.Encode()
	feedURL := server.URL + "/xrpc/social.coves.communityFeed.getCommunity?" + url.Values{"community": {communityDID}, "sort": {"new"}}.Encode()

	catalog := lexicon.NewBaseCatalog()
	require.NoError(t, catalog.LoadDirectory("../../atproto/lexicon"))
	validate := func(response subjectStateHTTPResponse, lexiconID string) {
		t.Helper()
		data, err := atdata.UnmarshalJSON(response.body)
		require.NoError(t, err)
		require.NoError(t, validation.ValidateData(catalog, data, lexiconID, 0))
	}
	getPost := func() (map[string]any, json.RawMessage) {
		t.Helper()
		response := moderationAcceptanceRequest(t, client, http.MethodGet, postURL, "", nil)
		body := moderationAcceptanceBody(t, response)
		items := moderationAcceptanceArray(t, body["posts"])
		require.Len(t, items, 1)
		item := moderationAcceptanceObject(t, items[0])
		require.Equal(t, postURI, item["uri"])
		require.NotEqual(t, "social.coves.community.post.defs#moderatedPost", item["$type"])
		encoded, err := json.Marshal(item)
		require.NoError(t, err)
		data, err := atdata.UnmarshalJSON(encoded)
		require.NoError(t, err)
		require.NoError(t, validation.ValidateData(catalog, data, "social.coves.community.post.defs#postView", 0))
		var raw struct {
			Posts []struct {
				Record json.RawMessage `json:"record"`
			} `json:"posts"`
		}
		require.NoError(t, json.Unmarshal(response.body, &raw))
		require.Len(t, raw.Posts, 1)
		require.NotEmpty(t, raw.Posts[0].Record)
		return item, raw.Posts[0].Record
	}
	labelView := map[string]any{
		"state": "clear",
		"contentLabels": []any{map[string]any{
			"value": "nsfw", "sources": []any{map[string]any{
				"authorityDid": instanceDID, "scope": map[string]any{"kind": "instance"},
			}},
		}},
	}

	initialPost, initialRecord := getPost()
	require.NotContains(t, initialPost, "moderation")
	initialFeed := moderationPostAcceptanceFeedURIs(t, moderationAcceptanceBody(t,
		moderationAcceptanceRequest(t, client, http.MethodGet, feedURL, "", nil)))
	require.ElementsMatch(t, []string{postURI, controlURI}, initialFeed)

	labelResponse := moderationAcceptanceRequest(t, client, http.MethodPost,
		server.URL+"/xrpc/social.coves.moderation.labelContent", adminToken, map[string]any{
			"subject":    map[string]string{"uri": postURI, "cid": postCID},
			"labelValue": "nsfw", "expectedVersion": "v0", "idempotencyKey": testkit.UniqueIDWithPrefix(t, "label"),
		})
	labelResult := moderationAcceptanceBody(t, labelResponse)
	validate(labelResponse, "social.coves.moderation.defs#mutationResult")
	require.Equal(t, "applied", labelResult["outcome"])
	labelAction := modlogAction(t, labelResult["action"], true)
	require.Equal(t, "label", labelAction["action"])
	require.Equal(t, "nsfw", labelAction["labelValue"])
	labelID := modlogID(t, labelAction)

	labelledPost, labelledRecord := getPost()
	require.Equal(t, []byte(initialRecord), []byte(labelledRecord), "label must not rewrite the served record")
	require.Equal(t, labelView, labelledPost["moderation"])
	stateResponse := requestSubjectState(t, client, server.URL, postURI, adminToken)
	stateBody := moderationAcceptanceBody(t, stateResponse)
	validate(stateResponse, "social.coves.moderation.getSubjectState#output")
	state := moderationAcceptanceObject(t, stateBody["state"])
	require.Equal(t, []any{map[string]any{
		"value": "nsfw", "action": map[string]any{"serviceDid": instanceDID, "actionId": labelID},
	}}, state["localLabels"])
	require.Equal(t, "nsfw", moderationAcceptanceObject(t,
		moderationAcceptanceArray(t, moderationAcceptanceObject(t, state["moderation"])["contentLabels"])[0])["value"])
	labelledFeed := moderationAcceptanceBody(t, moderationAcceptanceRequest(t, client, http.MethodGet, feedURL, "", nil))
	require.Equal(t, initialFeed, moderationPostAcceptanceFeedURIs(t, labelledFeed))
	feedItems := moderationAcceptanceArray(t, labelledFeed["feed"])
	var feedPost map[string]any
	for _, entry := range feedItems {
		post := moderationAcceptanceObject(t, moderationAcceptanceObject(t, entry)["post"])
		if post["uri"] == postURI {
			feedPost = post
		}
	}
	require.NotNil(t, feedPost)
	require.Equal(t, labelView, feedPost["moderation"])
	version, ok := state["version"].(string)
	require.True(t, ok)

	retractResponse := moderationAcceptanceRequest(t, client, http.MethodPost,
		server.URL+"/xrpc/social.coves.moderation.retractContentLabel", adminToken, map[string]any{
			"actionId": labelID, "expectedVersion": version,
			"idempotencyKey":  testkit.UniqueIDWithPrefix(t, "retract"),
			"reviewedSubject": map[string]string{"uri": postURI, "cid": postCID},
		})
	retractResult := moderationAcceptanceBody(t, retractResponse)
	validate(retractResponse, "social.coves.moderation.defs#mutationResult")
	require.Equal(t, "applied", retractResult["outcome"])
	retractAction := modlogAction(t, retractResult["action"], true)
	require.Equal(t, "retract-label", retractAction["action"])
	require.Equal(t, "nsfw", retractAction["labelValue"])
	require.Equal(t, labelID, moderationAcceptanceObject(t, retractAction["reverses"])["actionId"])
	retractedPost, retractedRecord := getPost()
	require.Equal(t, []byte(initialRecord), []byte(retractedRecord))
	require.NotContains(t, retractedPost, "moderation")
	logResponse := moderationAcceptanceRequest(t, client, http.MethodGet,
		server.URL+modlogPublicPath+"?"+url.Values{"subject": {postURI}}.Encode(), "", nil)
	_, publicActions := modlogPage(t, logResponse, false)
	require.Empty(t, publicActions, "the public log never serves label or retract-label actions")
	adminLogResponse := moderationAcceptanceRequest(t, client, http.MethodGet,
		server.URL+modlogAdminPath+"?"+url.Values{"subject": {postURI}}.Encode(), adminToken, nil)
	_, adminActions := modlogPage(t, adminLogResponse, true)
	require.Len(t, adminActions, 2)
	actionsByKind := make(map[string]map[string]any)
	for _, item := range adminActions {
		action := modlogAction(t, item, true)
		kind, ok := action["action"].(string)
		require.True(t, ok)
		actionsByKind[kind] = action
	}
	require.Len(t, actionsByKind, 2)
	require.Contains(t, actionsByKind, "label")
	require.Contains(t, actionsByKind, "retract-label")
	require.Equal(t, "nsfw", actionsByKind["label"]["labelValue"])
	require.Equal(t, postURI, moderationAcceptanceObject(t, actionsByKind["label"]["subject"])["uri"])
	require.Equal(t, labelID, modlogID(t, actionsByKind["label"]))
	require.Equal(t, "nsfw", actionsByKind["retract-label"]["labelValue"])
	require.Equal(t, labelID, moderationAcceptanceObject(t, actionsByKind["retract-label"]["reverses"])["actionId"])

	// A decision from another authority must retain its own source identity;
	// an inactive decision on the same subject must not contribute a source.
	for _, decision := range []struct {
		authority string
		active    bool
	}{
		{"did:plc:eeeeeeeeeeeeeeeeeeeeeeee", true},
		{"did:plc:ffffffffffffffffffffffff", false},
	} {
		actionID := testkit.TID()
		_, err := db.ExecContext(t.Context(), `
			INSERT INTO moderation_actions
				(id, actor_did, authority_did, scope_kind, subject_uri, subject_collection,
				 subject_community_did, observed_cid, action, label_value, origin, created_at)
			VALUES ($1, $2, $3, 'instance', $4, $5, $6, $7, 'label', 'nsfw', 'inherited', NOW())
		`, actionID, adminDID, decision.authority, postURI, moderation.PostV2Collection, communityDID, postCID)
		require.NoError(t, err)
		_, err = db.ExecContext(t.Context(), `
			INSERT INTO moderation_decisions
				(authority_did, scope_kind, subject_uri, kind, value, active_action_id, active)
			VALUES ($1, 'instance', $2, 'label', 'nsfw', $3, $4)
		`, decision.authority, postURI, actionID, decision.active)
		require.NoError(t, err)
	}
	foreignView := map[string]any{
		"state": "clear",
		"contentLabels": []any{map[string]any{
			"value": "nsfw", "sources": []any{map[string]any{
				"authorityDid": "did:plc:eeeeeeeeeeeeeeeeeeeeeeee", "scope": map[string]any{"kind": "instance"},
			}},
		}},
	}
	foreignPost, _ := getPost()
	require.Equal(t, foreignView, foreignPost["moderation"])
	foreignFeed := moderationAcceptanceBody(t, moderationAcceptanceRequest(t, client, http.MethodGet, feedURL, "", nil))
	require.Equal(t, initialFeed, moderationPostAcceptanceFeedURIs(t, foreignFeed))
	for _, entry := range moderationAcceptanceArray(t, foreignFeed["feed"]) {
		post := moderationAcceptanceObject(t, moderationAcceptanceObject(t, entry)["post"])
		if post["uri"] == postURI {
			require.Equal(t, foreignView, post["moderation"])
		}
	}
	require.Zero(t, pdsRequests.Load(), "apply and retract must make no PDS requests")
}

const (
	contentLabelRouteLabelPath   = "/xrpc/social.coves.moderation.labelContent"
	contentLabelRouteRetractPath = "/xrpc/social.coves.moderation.retractContentLabel"
)

func (f contentLabelRouteFixture) post(t *testing.T, path, token string, body map[string]any) subjectStateHTTPResponse {
	t.Helper()
	return moderationAcceptanceRequest(t, f.client, http.MethodPost, f.server.URL+path, token, body)
}

func (f contentLabelRouteFixture) label(t *testing.T, postURI, version, reason string) (string, string) {
	t.Helper()
	request := map[string]any{
		"subject": map[string]string{"uri": postURI, "cid": f.postCID}, "labelValue": "nsfw",
		"expectedVersion": version, "idempotencyKey": testkit.UniqueIDWithPrefix(t, "label"),
	}
	if reason != "" {
		request["reason"] = reason
	}
	result := moderationAcceptanceBody(t, f.post(t, contentLabelRouteLabelPath, f.adminToken, request))
	require.Equal(t, "applied", result["outcome"])
	labelVersion, ok := moderationAcceptanceObject(t, result["state"])["version"].(string)
	require.True(t, ok)
	return modlogID(t, modlogAction(t, result["action"], true)), labelVersion
}

// contentLabelRouteSnapshot is everything a rejected label mutation must leave
// unchanged: the admin subject state and both logs' entries for the subject.
type contentLabelRouteSnapshot struct {
	version      any
	localLabels  any
	moderation   any
	publicLogIDs []string
	adminLogIDs  []string
}

func (f contentLabelRouteFixture) snapshot(t *testing.T, postURI string) contentLabelRouteSnapshot {
	t.Helper()
	state := moderationAcceptanceObject(t, moderationAcceptanceBody(t,
		requestSubjectState(t, f.client, f.server.URL, postURI, f.adminToken))["state"])
	logIDs := func(path string, admin bool) []string {
		response := moderationAcceptanceRequest(t, f.client, http.MethodGet,
			f.server.URL+path+"?"+url.Values{"subject": {postURI}}.Encode(), f.adminToken, nil)
		_, actions := modlogPage(t, response, admin)
		ids := make([]string, 0, len(actions))
		for _, item := range actions {
			ids = append(ids, modlogID(t, modlogAction(t, item, admin)))
		}
		return ids
	}
	return contentLabelRouteSnapshot{
		version: state["version"], localLabels: state["localLabels"], moderation: state["moderation"],
		publicLogIDs: logIDs(modlogPublicPath, false), adminLogIDs: logIDs(modlogAdminPath, true),
	}
}

func TestModerationContentLabelRejectsRemovalOnlyReasons(t *testing.T) {
	f := newContentLabelRouteFixture(t)
	postURI := f.insertPost("post an admin reviews for a removal-only reason")
	removalOnlyReasons := []string{
		"social.coves.moderation.defs#reasonDoxing",
		"social.coves.moderation.defs#reasonIllegalContent",
	}

	unlabelled := f.snapshot(t, postURI)
	require.Equal(t, "v0", unlabelled.version)
	require.Empty(t, unlabelled.adminLogIDs)
	for _, reason := range removalOnlyReasons {
		response := f.post(t, contentLabelRouteLabelPath, f.adminToken, map[string]any{
			"subject": map[string]string{"uri": postURI, "cid": f.postCID}, "labelValue": "nsfw",
			"expectedVersion": "v0", "idempotencyKey": testkit.UniqueIDWithPrefix(t, "label"), "reason": reason,
		})
		requireXRPCError(t, response, http.StatusBadRequest, "UnsupportedReason")
		require.Equal(t, unlabelled, f.snapshot(t, postURI), "a rejected label must not write an action or change state")
	}

	labelID, version := f.label(t, postURI, "v0", "social.coves.moderation.defs#reasonSpam")
	labelled := f.snapshot(t, postURI)
	require.Equal(t, version, labelled.version)
	require.Equal(t, []string{labelID}, labelled.adminLogIDs)
	for _, reason := range removalOnlyReasons {
		response := f.post(t, contentLabelRouteRetractPath, f.adminToken, map[string]any{
			"actionId": labelID, "expectedVersion": version,
			"idempotencyKey":  testkit.UniqueIDWithPrefix(t, "retract"),
			"reviewedSubject": map[string]string{"uri": postURI, "cid": f.postCID}, "reason": reason,
		})
		requireXRPCError(t, response, http.StatusBadRequest, "UnsupportedReason")
		require.Equal(t, labelled, f.snapshot(t, postURI), "a rejected retraction must not write an action or change state")
	}
}

func TestModerationContentLabelRequiresAdmin(t *testing.T) {
	f := newContentLabelRouteFixture(t)
	postURI := f.insertPost("post a non-admin tries to label")

	unlabelled := f.snapshot(t, postURI)
	response := f.post(t, contentLabelRouteLabelPath, f.nonAdminToken, map[string]any{
		"subject": map[string]string{"uri": postURI, "cid": f.postCID}, "labelValue": "nsfw",
		"expectedVersion": "v0", "idempotencyKey": testkit.UniqueIDWithPrefix(t, "label"),
	})
	requireXRPCError(t, response, http.StatusForbidden, "Forbidden")
	require.Equal(t, unlabelled, f.snapshot(t, postURI))

	labelID, version := f.label(t, postURI, "v0", "")
	labelled := f.snapshot(t, postURI)
	response = f.post(t, contentLabelRouteRetractPath, f.nonAdminToken, map[string]any{
		"actionId": labelID, "expectedVersion": version,
		"idempotencyKey":  testkit.UniqueIDWithPrefix(t, "retract"),
		"reviewedSubject": map[string]string{"uri": postURI, "cid": f.postCID},
	})
	requireXRPCError(t, response, http.StatusForbidden, "Forbidden")
	require.Equal(t, labelled, f.snapshot(t, postURI))
}

func TestModerationContentLabelOnRemovedPostAppearsAfterRestore(t *testing.T) {
	f := newContentLabelRouteFixture(t)
	postURI := f.insertPost("post removed and then labelled")
	postURL := f.server.URL + "/xrpc/social.coves.community.post.get?" + url.Values{"uris": {postURI}}.Encode()
	getPost := func() map[string]any {
		t.Helper()
		items := moderationAcceptanceArray(t, moderationAcceptanceBody(t,
			moderationAcceptanceRequest(t, f.client, http.MethodGet, postURL, "", nil))["posts"])
		require.Len(t, items, 1)
		item := moderationAcceptanceObject(t, items[0])
		require.Equal(t, postURI, item["uri"])
		return item
	}

	removed := moderationAcceptanceBody(t, f.post(t, moderationRouteRemovePath, f.adminToken, map[string]any{
		"subject": map[string]string{"uri": postURI, "cid": f.postCID}, "expectedVersion": "v0",
		"idempotencyKey": testkit.UniqueIDWithPrefix(t, "remove"), "reason": "social.coves.moderation.defs#reasonSpam",
	}))
	require.Equal(t, "applied", removed["outcome"])
	removalID := modlogID(t, modlogAction(t, removed["action"], true))
	removedVersion, ok := moderationAcceptanceObject(t, removed["state"])["version"].(string)
	require.True(t, ok)

	labelID, labelledVersion := f.label(t, postURI, removedVersion, "")
	labelledRemoved := getPost()
	require.Equal(t, "social.coves.community.post.defs#moderatedPost", labelledRemoved["$type"])
	removedView := moderationAcceptanceObject(t, labelledRemoved["moderation"])
	require.Equal(t, "removed", removedView["state"])
	require.NotContains(t, removedView, "contentLabels", "a removed post must not reveal its labels")

	restored := moderationAcceptanceBody(t, f.post(t, modlogRestorePath, f.adminToken, map[string]any{
		"actionId": removalID, "reviewedSubject": map[string]string{"uri": postURI, "cid": f.postCID},
		"expectedVersion": labelledVersion, "idempotencyKey": testkit.UniqueIDWithPrefix(t, "restore"),
		"reason": "social.coves.moderation.defs#reasonModeratorDiscretion",
	}))
	require.Equal(t, "applied", restored["outcome"])
	restoredPost := getPost()
	require.NotEqual(t, "social.coves.community.post.defs#moderatedPost", restoredPost["$type"])
	require.Equal(t, map[string]any{
		"state": "clear",
		"contentLabels": []any{map[string]any{
			"value": "nsfw", "sources": []any{map[string]any{
				"authorityDid": f.instanceDID, "scope": map[string]any{"kind": "instance"},
			}},
		}},
	}, restoredPost["moderation"])
	state := moderationAcceptanceObject(t, moderationAcceptanceBody(t,
		requestSubjectState(t, f.client, f.server.URL, postURI, f.adminToken))["state"])
	require.Equal(t, []any{map[string]any{
		"value": "nsfw", "action": map[string]any{"serviceDid": f.instanceDID, "actionId": labelID},
	}}, state["localLabels"])
}
