//go:build integration

package routes_test

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"Coves/internal/api/middleware"
	"Coves/internal/api/routes"
	"Coves/internal/atproto/identity"
	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/internal/validation"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/bluesky-social/indigo/atproto/atdata"
	"github.com/bluesky-social/indigo/atproto/lexicon"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	modlogPublicPath  = "/xrpc/social.coves.moderation.listActions"
	modlogAdminPath   = "/xrpc/social.coves.moderation.listAdminActions"
	modlogRestorePath = "/xrpc/social.coves.moderation.restoreContent"
	modlogSpam        = "social.coves.moderation.defs#reasonSpam"
	modlogDiscretion  = "social.coves.moderation.defs#reasonModeratorDiscretion"
	modlogIllegal     = "social.coves.moderation.defs#reasonIllegalContent"
	modlogDoxing      = "social.coves.moderation.defs#reasonDoxing"
)

type modlogHandleResolver map[string]string

func (resolver modlogHandleResolver) ResolveHandle(_ context.Context, handle string) (string, string, error) {
	if did, ok := resolver[handle]; ok {
		return did, "", nil
	}
	return "", "", &identity.ErrNotFound{Identifier: handle}
}

type modlogFixture struct {
	db                                                                        *sql.DB
	service                                                                   moderation.Service
	server                                                                    *httptest.Server
	client                                                                    *http.Client
	adminA                                                                    string
	adminB                                                                    string
	outsider                                                                  string
	tokenA                                                                    string
	tokenB                                                                    string
	outsiderToken                                                             string
	communityDID, communityName, communityHandle, authorDID, postURI, postCID string
	resolver                                                                  modlogHandleResolver
	unsealer                                                                  *fixtures.SessionUnsealer
	oauthStore                                                                *fixtures.OAuthStore
}

func newModlogFixture(t *testing.T) *modlogFixture {
	t.Helper()
	db := testkit.DB(t)
	communityName := testkit.UniqueIDWithPrefix(t, "logcommunity")
	communityDID, err := fixtures.Community(t.Context(), db, communityName, "owner"+communityName)
	require.NoError(t, err)
	communityHandle := "c-" + communityName + ".coves.social"
	_, err = db.ExecContext(t.Context(), `UPDATE communities SET handle = $1 WHERE did = $2`, communityHandle, communityDID)
	require.NoError(t, err)
	authorName := testkit.UniqueIDWithPrefix(t, "logauthor")
	authorDID := fixtures.DID(authorName)
	fixtures.User(t, db, authorName+".test", authorDID)
	postURI := fixtures.Post(t, db, communityDID, authorDID, "modlog root", 0, time.Now())
	post, err := postgres.NewPostRepository(db).GetRawIndexedRow(t.Context(), postURI)
	require.NoError(t, err)
	f := &modlogFixture{
		db: db, adminA: fixtures.DID(testkit.UniqueIDWithPrefix(t, "logadmina")),
		adminB:   fixtures.DID(testkit.UniqueIDWithPrefix(t, "logadminb")),
		outsider: fixtures.DID(testkit.UniqueIDWithPrefix(t, "logoutsider")),
		tokenA:   "log-admin-a", tokenB: "log-admin-b", outsiderToken: "log-outsider",
		communityDID: communityDID, communityName: communityName,
		communityHandle: communityHandle, authorDID: authorDID,
		postURI: postURI, postCID: post.CID, unsealer: fixtures.NewSessionUnsealer(),
		oauthStore: fixtures.NewOAuthStore(),
	}
	f.resolver = modlogHandleResolver{
		"admin-a.test":   f.adminA,
		"authority.test": fixtures.InstanceDID(),
	}
	f.unsealer.AddSession(f.tokenA, f.adminA, "log-session-a")
	f.unsealer.AddSession(f.tokenB, f.adminB, "log-session-b")
	f.unsealer.AddSession(f.outsiderToken, f.outsider, "log-session-outsider")
	f.oauthStore.AddSession(f.adminA, "log-session-a", "log-access-a")
	f.oauthStore.AddSession(f.adminB, "log-session-b", "log-access-b")
	f.oauthStore.AddSession(f.outsider, "log-session-outsider", "log-access-outsider")
	f.service = moderation.NewService(
		moderation.NewRepositorySubjectReader(postgres.NewPostRepository(db), postgres.NewCommentRepository(db)),
		postgres.NewModerationRepository(db),
		moderation.Config{
			InstanceDID: fixtures.InstanceDID(), IdempotencyRetention: 24 * time.Hour,
			MaxLiveIdempotencyKeys: 1000, CursorSecret: "modlog-integration-cursor-secret",
			CommunityResolver: communities.NewCommunityService(
				postgres.NewCommunityRepository(db, credentialciphertest.Fixed()), testkit.Endpoints().PDS.BaseURL,
				"", "coves.social", nil, nil, nil),
			HandleResolver: f.resolver,
		},
	)
	f.rebuild(t, []string{f.adminA, f.adminB})
	return f
}

func (f *modlogFixture) rebuild(t *testing.T, allowed []string) {
	t.Helper()
	auth := middleware.NewInstanceAdminMiddleware(f.unsealer, f.oauthStore, nil, moderation.NewAllowlistAuthority(allowed))
	router := chi.NewRouter()
	routes.RegisterModerationRoutes(router, f.service, auth)
	f.server = httptest.NewServer(router)
	t.Cleanup(f.server.Close)
	f.client = f.server.Client()
}

func (f *modlogFixture) comment(t *testing.T) (string, string) {
	t.Helper()
	cid := "bafyreilog" + testkit.UniqueIDWithPrefix(t, "cid")
	return moderationAcceptanceInsertComment(t, f.db, f.authorDID, f.postURI, f.postCID, f.postURI, f.postCID, cid, "modlog comment"), cid
}

func (f *modlogFixture) list(t *testing.T, admin bool, token string, params url.Values) subjectStateHTTPResponse {
	t.Helper()
	path := modlogPublicPath
	if admin {
		path = modlogAdminPath
	}
	target := f.server.URL + path
	if params != nil {
		target += "?" + params.Encode()
	}
	return moderationAcceptanceRequest(t, f.client, http.MethodGet, target, token, nil)
}

func modlogPage(t *testing.T, response subjectStateHTTPResponse, admin bool) (map[string]any, []any) {
	t.Helper()
	body := moderationAcceptanceBody(t, response)
	data, err := atdata.UnmarshalJSON(response.body)
	require.NoError(t, err)
	catalog := lexicon.NewBaseCatalog()
	require.NoError(t, catalog.LoadDirectory("../../atproto/lexicon"))
	nsid := "social.coves.moderation.listActions#output"
	if admin {
		nsid = "social.coves.moderation.listAdminActions#output"
	}
	require.NoError(t, validation.ValidateData(catalog, data, nsid, 0))
	return body, moderationAcceptanceArray(t, body["actions"])
}

func modlogAction(t *testing.T, value any, admin bool) map[string]any {
	t.Helper()
	item := moderationAcceptanceObject(t, value)
	if admin {
		return moderationAcceptanceObject(t, item["action"])
	}
	return item
}

func modlogID(t *testing.T, action map[string]any) string {
	t.Helper()
	id, ok := moderationAcceptanceObject(t, action["ref"])["actionId"].(string)
	require.True(t, ok)
	return id
}

func (f *modlogFixture) remove(t *testing.T, uri, cid, token, reason, note string) (string, string) {
	t.Helper()
	response := moderationAcceptanceRequest(t, f.client, http.MethodPost, f.server.URL+moderationRouteRemovePath, token, map[string]any{
		"subject": map[string]string{"uri": uri, "cid": cid}, "expectedVersion": "v0",
		"idempotencyKey": testkit.UniqueIDWithPrefix(t, "remove"), "reason": reason, "privateNote": note,
	})
	body := moderationAcceptanceBody(t, response)
	require.Equal(t, "applied", body["outcome"])
	action := modlogAction(t, body["action"], true)
	version, ok := moderationAcceptanceObject(t, body["state"])["version"].(string)
	require.True(t, ok)
	return modlogID(t, action), version
}

func (f *modlogFixture) restore(t *testing.T, uri, cid, token, removalID, version, reason, note string) string {
	t.Helper()
	response := moderationAcceptanceRequest(t, f.client, http.MethodPost, f.server.URL+modlogRestorePath, token, map[string]any{
		"actionId": removalID, "reviewedSubject": map[string]string{"uri": uri, "cid": cid},
		"expectedVersion": version, "idempotencyKey": testkit.UniqueIDWithPrefix(t, "restore"),
		"reason": reason, "privateNote": note,
	})
	body := moderationAcceptanceBody(t, response)
	require.Equal(t, "applied", body["outcome"])
	return modlogID(t, modlogAction(t, body["action"], true))
}

func TestModerationModlogListsRemovalHistory(t *testing.T) {
	f := newModlogFixture(t)
	xURI, xCID := f.comment(t)
	h1URI, h1CID := f.comment(t)
	h2URI, h2CID := f.comment(t)
	xRemove, xVersion := f.remove(t, xURI, xCID, f.tokenA, modlogSpam, "X private removal note")
	xRestore := f.restore(t, xURI, xCID, f.tokenB, xRemove, xVersion, modlogDiscretion, "X private restore note")
	h1Remove, h1Version := f.remove(t, h1URI, h1CID, f.tokenA, modlogIllegal, "H1 private removal note")
	h2Remove, _ := f.remove(t, h2URI, h2CID, f.tokenB, modlogDoxing, "H2 private removal note")
	h1Restore := f.restore(t, h1URI, h1CID, f.tokenA, h1Remove, h1Version, modlogDiscretion, "H1 private restore note")
	expected := map[string]struct {
		uri, cid, actor, reason, action, reverses, note string
		hidden                                          bool
	}{
		xRemove:   {xURI, xCID, f.adminA, modlogSpam, "remove", "", "X private removal note", false},
		xRestore:  {xURI, xCID, f.adminB, modlogDiscretion, "restore", xRemove, "X private restore note", false},
		h1Remove:  {h1URI, h1CID, f.adminA, modlogIllegal, "remove", "", "H1 private removal note", true},
		h2Remove:  {h2URI, h2CID, f.adminB, modlogDoxing, "remove", "", "H2 private removal note", true},
		h1Restore: {h1URI, h1CID, f.adminA, modlogDiscretion, "restore", h1Remove, "H1 private restore note", true},
	}
	publicResponse := f.list(t, false, "", nil)
	publicBody, publicItems := modlogPage(t, publicResponse, false)
	require.Subset(t, []string{"actions", "cursor"}, keysOf(publicBody))
	require.Len(t, publicItems, 5)
	var previousTime time.Time
	var previousID string
	for index, item := range publicItems {
		action := modlogAction(t, item, false)
		id := modlogID(t, action)
		want, found := expected[id]
		require.Truef(t, found, "unexpected action %s", id)
		stamp, err := time.Parse(time.RFC3339Nano, action["createdAt"].(string))
		require.NoError(t, err)
		if index > 0 {
			require.True(t, stamp.Before(previousTime) || stamp.Equal(previousTime) && id < previousID,
				"actions must be newest first, breaking timestamp ties by ID")
		}
		previousTime, previousID = stamp, id
		assert.Equal(t, want.action, action["action"])
		assert.Equal(t, want.reason, action["reason"])
		assert.Equal(t, want.actor, moderationAcceptanceObject(t, action["actor"])["did"])
		assert.Equal(t, fixtures.InstanceDID(), moderationAcceptanceObject(t, action["ref"])["serviceDid"])
		if want.hidden {
			assert.NotContains(t, action, "subject")
		} else {
			assert.Equal(t, want.uri, moderationAcceptanceObject(t, action["subject"])["uri"])
		}
		if want.reverses != "" {
			assert.Equal(t, want.reverses, moderationAcceptanceObject(t, action["reverses"])["actionId"])
		}
		assert.NotContains(t, action, "privateNote")
		assert.NotContains(t, action, "actorDid")
	}
	assert.Equal(t, publicResponse.body, f.list(t, false, f.tokenA, nil).body, "public projection must not depend on admin identity")
	for _, private := range []string{h1URI, h2URI, "H1 private removal note", "H2 private removal note", "H1 private restore note"} {
		assert.NotContains(t, string(publicResponse.body), private)
	}
	_, adminItems := modlogPage(t, f.list(t, true, f.tokenB, nil), true)
	require.Len(t, adminItems, 5)
	for _, item := range adminItems {
		entry := moderationAcceptanceObject(t, item)
		action := modlogAction(t, item, true)
		want := expected[modlogID(t, action)]
		assert.Equal(t, want.actor, entry["actorDid"])
		assert.Equal(t, want.note, entry["privateNote"])
		if want.hidden {
			assert.NotContains(t, action, "subject")
			assert.Equal(t, map[string]any{"uri": want.uri, "cid": want.cid}, entry["privateSubject"])
		} else {
			assert.NotContains(t, entry, "privateSubject")
		}
	}
	requireXRPCError(t, f.list(t, true, "", nil), http.StatusUnauthorized, "AuthRequired")
	adminCursor := modlogCursor(t, f, true, f.tokenB, url.Values{"limit": {"1"}})
	f.rebuild(t, []string{f.adminB})
	requireXRPCError(t, f.list(t, true, f.tokenA, nil), http.StatusForbidden, "Forbidden")
	requireXRPCError(t, f.list(t, true, f.tokenA, url.Values{"cursor": {adminCursor}}), http.StatusForbidden, "Forbidden")
	publicAfter := f.list(t, false, "", nil)
	assert.Equal(t, publicResponse.body, publicAfter.body)
	_, afterItems := modlogPage(t, f.list(t, true, f.tokenB, nil), true)
	require.Len(t, afterItems, 5)
	for _, item := range afterItems {
		entry := moderationAcceptanceObject(t, item)
		want := expected[modlogID(t, modlogAction(t, item, true))]
		assert.Equal(t, want.actor, entry["actorDid"])
	}
}

func TestModerationModlogHiddenSubjectsAreNotAnOracle(t *testing.T) {
	f := newModlogFixture(t)
	for _, token := range []string{f.tokenA, f.tokenB} {
		uri, cid := f.comment(t)
		f.remove(t, uri, cid, token, modlogSpam, "visible")
	}
	const since, until = "2020-01-01T00:00:00Z", "2100-01-01T00:00:00Z"
	// Each walk filters on community or collection, which excludes hidden rows.
	// The hidden actions below match every other predicate in every walk.
	walks := []url.Values{
		{"community": {f.communityDID}}, {"community": {f.communityHandle}},
		{"collection": {moderation.CommentCollection}},
		{"community": {f.communityDID}, "collection": {moderation.CommentCollection}},
		{"community": {f.communityDID}, "actor": {f.adminA}},
		{"community": {f.communityDID}, "action": {"remove"}},
		{"community": {f.communityDID}, "origin": {"local"}},
		{"community": {f.communityDID}, "authority": {fixtures.InstanceDID()}},
		{"community": {f.communityDID}, "since": {since}, "until": {until}},
		{"collection": {moderation.CommentCollection}, "actor": {"admin-a.test"}, "action": {"remove"},
			"origin": {"local"}, "authority": {"authority.test"}, "since": {since}, "until": {until}},
		{"community": {f.communityName + "@coves.social"}, "collection": {moderation.CommentCollection},
			"actor": {f.adminA}, "action": {"remove"}, "origin": {"local"}, "authority": {fixtures.InstanceDID()},
			"since": {since}, "until": {until}},
	}
	walk := func(params url.Values) [][]byte {
		query := url.Values{"limit": {"1"}}
		for key, values := range params {
			query[key] = values
		}
		return modlogWalk(t, f, query)
	}
	before := make([][][]byte, len(walks))
	for index, params := range walks {
		before[index] = walk(params)
	}
	hiddenURI, hiddenCID := f.comment(t)
	activeURI, activeCID := f.comment(t)
	neverURI, _ := f.comment(t)
	indexedURI, _ := f.comment(t)
	_, err := f.db.ExecContext(t.Context(), `DELETE FROM comments WHERE uri = $1`, neverURI)
	require.NoError(t, err)
	removalID, version := f.remove(t, hiddenURI, hiddenCID, f.tokenA, modlogIllegal, "hidden removal")
	f.restore(t, hiddenURI, hiddenCID, f.tokenA, removalID, version, modlogDiscretion, "hidden restore")
	// Left active: the subject is tombstoned on content surfaces as removed.
	f.remove(t, activeURI, activeCID, f.tokenA, modlogDoxing, "active hidden removal")
	for index, params := range walks {
		after := walk(params)
		require.Equal(t, before[index], after, "hidden actions must not affect public page bodies or cursors under %s", params.Encode())
		for _, body := range after {
			require.NotContains(t, string(body), hiddenURI)
			require.NotContains(t, string(body), activeURI)
		}
	}
	for _, body := range walk(url.Values{}) {
		require.NotContains(t, string(body), hiddenURI)
		require.NotContains(t, string(body), activeURI)
	}
	// H (restored) and A (active) are hidden; N was never indexed and I is
	// indexed but never actioned. All four must answer identically.
	subjects := []string{hiddenURI, activeURI, neverURI, indexedURI}
	suffixes := []url.Values{
		{}, {"community": {f.communityDID}}, {"community": {f.communityHandle}},
		{"community": {f.communityName + "@coves.social"}}, {"collection": {moderation.CommentCollection}},
		{"actor": {f.adminA}}, {"actor": {"admin-a.test"}}, {"action": {"remove"}}, {"origin": {"local"}},
		{"authority": {fixtures.InstanceDID()}}, {"authority": {"authority.test"}},
		{"since": {since}, "until": {until}},
		{"community": {f.communityDID}, "actor": {f.adminA}, "action": {"remove"}, "since": {since}, "until": {until}},
		{"community": {f.communityHandle}, "collection": {moderation.CommentCollection}, "actor": {"admin-a.test"},
			"action": {"remove"}, "origin": {"local"}, "authority": {"authority.test"}, "since": {since}, "until": {until}},
	}
	for _, suffix := range suffixes {
		t.Run(suffix.Encode(), func(t *testing.T) {
			var responses []subjectStateHTTPResponse
			for _, subject := range subjects {
				query := url.Values{"limit": {"1"}, "subject": {subject}}
				for key, values := range suffix {
					query[key] = values
				}
				responses = append(responses, f.list(t, false, "", query))
			}
			require.Equal(t, http.StatusOK, responses[0].status)
			require.JSONEq(t, `{"actions":[]}`, string(responses[0].body))
			for _, response := range responses[1:] {
				require.Equal(t, responses[0].status, response.status)
				require.Equal(t, responses[0].body, response.body)
			}
		})
	}
	for subject, count := range map[string]int{hiddenURI: 2, activeURI: 1} {
		_, adminItems := modlogPage(t, f.list(t, true, f.tokenA, url.Values{"subject": {subject}}), true)
		require.Len(t, adminItems, count)
		for _, item := range adminItems {
			assert.Equal(t, subject, moderationAcceptanceObject(t, moderationAcceptanceObject(t, item)["privateSubject"])["uri"])
		}
	}
	for _, cursor := range []string{"!!invalid!!", modlogCursor(t, f, false, f.tokenB, url.Values{"limit": {"1"}, "action": {"remove"}})} {
		var responses []subjectStateHTTPResponse
		for _, subject := range subjects {
			responses = append(responses, f.list(t, false, "", url.Values{"subject": {subject}, "action": {"remove"}, "cursor": {cursor}}))
		}
		requireXRPCError(t, responses[0], http.StatusBadRequest, "InvalidCursor")
		for _, response := range responses[1:] {
			require.Equal(t, responses[0].status, response.status)
			require.Equal(t, responses[0].body, response.body)
		}
	}
}

func modlogCursor(t *testing.T, f *modlogFixture, admin bool, token string, params url.Values) string {
	t.Helper()
	page, _ := modlogPage(t, f.list(t, admin, token, params), admin)
	cursor, ok := page["cursor"].(string)
	require.True(t, ok, "expected another page")
	require.NotEmpty(t, cursor)
	return cursor
}

func modlogWalk(t *testing.T, f *modlogFixture, params url.Values) [][]byte {
	t.Helper()
	var pages [][]byte
	query := url.Values{}
	for key, values := range params {
		query[key] = values
	}
	for {
		response := f.list(t, false, "", query)
		body, _ := modlogPage(t, response, false)
		pages = append(pages, response.body)
		cursor, ok := body["cursor"].(string)
		if !ok {
			return pages
		}
		require.LessOrEqual(t, len(pages), 10, "pagination must terminate")
		query.Set("cursor", cursor)
	}
}

func TestModerationModlogPagination(t *testing.T) {
	f := newModlogFixture(t)
	for range 5 {
		uri, cid := f.comment(t)
		f.remove(t, uri, cid, f.tokenA, modlogSpam, "paging")
	}
	full, all := modlogPage(t, f.list(t, false, "", nil), false)
	require.Len(t, all, 5)
	assert.NotContains(t, full, "cursor")
	query := url.Values{"limit": {"2"}}
	var found []string
	for _, count := range []int{2, 2, 1} {
		response := f.list(t, false, "", query)
		page, items := modlogPage(t, response, false)
		require.Len(t, items, count)
		assert.Equal(t, response.body, f.list(t, false, "", query).body)
		for _, item := range items {
			found = append(found, modlogID(t, modlogAction(t, item, false)))
		}
		cursor, ok := page["cursor"].(string)
		if count == 1 {
			require.False(t, ok)
		} else {
			require.True(t, ok)
			require.NotEmpty(t, cursor)
			query.Set("cursor", cursor)
		}
	}
	var expected []string
	for _, item := range all {
		expected = append(expected, modlogID(t, modlogAction(t, item, false)))
	}
	require.Equal(t, expected, found)
	changedLimit := url.Values{"limit": {"1"}, "cursor": {modlogCursor(t, f, false, "", url.Values{"limit": {"2"}})}}
	_, continued := modlogPage(t, f.list(t, false, "", changedLimit), false)
	require.Len(t, continued, 1)
	assert.Equal(t, expected[2], modlogID(t, modlogAction(t, continued[0], false)))
	removeCursor := modlogCursor(t, f, false, "", url.Values{"limit": {"1"}, "action": {"remove"}})
	requireXRPCError(t, f.list(t, false, "", url.Values{"cursor": {removeCursor}, "action": {"restore"}}), http.StatusBadRequest, "InvalidCursor")
	requireXRPCError(t, f.list(t, true, f.tokenA, url.Values{"cursor": {removeCursor}, "action": {"remove"}}), http.StatusBadRequest, "InvalidCursor")
	adminCursor := modlogCursor(t, f, true, f.tokenA, url.Values{"limit": {"1"}})
	requireXRPCError(t, f.list(t, false, "", url.Values{"cursor": {adminCursor}}), http.StatusBadRequest, "InvalidCursor")
	for _, query := range []url.Values{{"limit": {"0"}}, {"limit": {"101"}}, {"cursor": {strings.Repeat("a", 2049)}}, {"community": {strings.Repeat("x", 321)}}} {
		requireXRPCError(t, f.list(t, false, "", query), http.StatusBadRequest, "InvalidRequest")
	}
	requireXRPCError(t, f.list(t, true, f.outsiderToken, url.Values{"cursor": {adminCursor}}), http.StatusForbidden, "Forbidden")
}

func TestModerationModlogFilters(t *testing.T) {
	f := newModlogFixture(t)
	uri, cid := f.comment(t)
	otherURI, otherCID := f.comment(t)
	firstID, version := f.remove(t, uri, cid, f.tokenA, modlogSpam, "first")
	f.restore(t, uri, cid, f.tokenB, firstID, version, modlogDiscretion, "second")
	f.remove(t, otherURI, otherCID, f.tokenB, modlogSpam, "third")
	for _, query := range []url.Values{
		{"actor": {f.adminA}}, {"actor": {"admin-a.test"}},
		{"authority": {fixtures.InstanceDID()}}, {"authority": {"authority.test"}},
		{"subject": {uri}}, {"collection": {moderation.CommentCollection}},
		{"action": {"restore"}}, {"origin": {"local"}},
		{"community": {f.communityDID}}, {"community": {f.communityHandle}},
		{"community": {f.communityName + "@coves.social"}},
	} {
		t.Run(query.Encode(), func(t *testing.T) {
			_, items := modlogPage(t, f.list(t, false, "", query), false)
			count := 3
			if query.Has("subject") {
				count = 2
			}
			if query.Has("actor") || query.Has("action") {
				count = 1
			}
			require.Len(t, items, count)
			for _, item := range items {
				action := modlogAction(t, item, false)
				switch {
				case query.Has("actor"):
					assert.Equal(t, f.adminA, moderationAcceptanceObject(t, action["actor"])["did"])
				case query.Has("subject"):
					assert.Equal(t, uri, moderationAcceptanceObject(t, action["subject"])["uri"])
				case query.Has("action"):
					assert.Equal(t, "restore", action["action"])
				case query.Has("origin"):
					assert.Equal(t, "local", action["origin"])
				case query.Has("authority"):
					assert.Equal(t, fixtures.InstanceDID(), action["authorityDid"])
				}
			}
		})
	}
	for _, query := range []url.Values{
		{"actor": {"missing.test"}}, {"authority": {"missing.test"}},
		{"community": {"nonexistent.coves.social"}},
		{"origin": {"inherited"}}, {"collection": {moderation.PostV2Collection}},
	} {
		response := f.list(t, false, "", query)
		_, items := modlogPage(t, response, false)
		require.Empty(t, items, "filter %s", query.Encode())
		require.JSONEq(t, `{"actions":[]}`, string(response.body))
	}
	first := f.list(t, false, "", url.Values{"actor": {"missing.test"}})
	assert.Equal(t, first.body, f.list(t, false, "", url.Values{"community": {"nonexistent.coves.social"}}).body)
	assert.Equal(t, first.body, f.list(t, false, "", url.Values{"subject": {"at://did:plc:never/social.coves.community.comment/3kabc"}}).body)
	_, exact := modlogPage(t, f.list(t, true, f.tokenA, url.Values{"actionId": {firstID}}), true)
	require.Len(t, exact, 1)
	assert.Equal(t, firstID, modlogID(t, modlogAction(t, exact[0], true)))
	_, items := modlogPage(t, f.list(t, false, "", url.Values{"subject": {uri}, "action": {"remove"}}), false)
	require.Len(t, items, 1)
	stamp, err := time.Parse(time.RFC3339Nano, modlogAction(t, items[0], false)["createdAt"].(string))
	require.NoError(t, err)
	for _, bound := range []struct {
		key      string
		when     time.Time
		included bool
	}{
		{"since", stamp, true}, {"since", stamp.Add(100 * time.Nanosecond), false},
		{"until", stamp, false}, {"until", stamp.Add(100 * time.Nanosecond), true},
	} {
		query := url.Values{"subject": {uri}, "action": {"remove"}, bound.key: {bound.when.Format(time.RFC3339Nano)}}
		_, matches := modlogPage(t, f.list(t, false, "", query), false)
		if bound.included {
			require.Len(t, matches, 1, "%s", query.Encode())
		} else {
			require.Empty(t, matches, "%s", query.Encode())
		}
	}
}

// TestModerationModlogFiltersReturnOnlyTheirOwnRows gives every filter a row it
// must exclude: a second community, a second authority, a second collection
// and an inherited origin. Each form of each filter must return exactly its
// own rows, so dropping any predicate fails.
func TestModerationModlogFiltersReturnOnlyTheirOwnRows(t *testing.T) {
	f := newModlogFixture(t)
	ctx := t.Context()
	secondName := testkit.UniqueIDWithPrefix(t, "logsecond")
	secondDID, err := fixtures.Community(ctx, f.db, secondName, "owner"+secondName)
	require.NoError(t, err)
	secondHandle := "c-" + secondName + ".coves.social"
	_, err = f.db.ExecContext(ctx, `UPDATE communities SET handle = $1 WHERE did = $2`, secondHandle, secondDID)
	require.NoError(t, err)
	secondPostURI := fixtures.Post(t, f.db, secondDID, f.authorDID, "modlog second community root", 0, time.Now())
	secondPost, err := postgres.NewPostRepository(f.db).GetRawIndexedRow(ctx, secondPostURI)
	require.NoError(t, err)
	secondCommentCID := "bafyreilog" + testkit.UniqueIDWithPrefix(t, "cid")
	secondCommentURI := moderationAcceptanceInsertComment(t, f.db, f.authorDID, secondPostURI, secondPost.CID,
		secondPostURI, secondPost.CID, secondCommentCID, "modlog second community comment")

	firstURI, firstCID := f.comment(t)
	otherURI, otherCID := f.comment(t)
	removeFirst, version := f.remove(t, firstURI, firstCID, f.tokenA, modlogSpam, "first")
	restoreFirst := f.restore(t, firstURI, firstCID, f.tokenB, removeFirst, version, modlogDiscretion, "second")
	removeOther, _ := f.remove(t, otherURI, otherCID, f.tokenB, modlogSpam, "third")
	// The only legacy-post row, in the second community.
	removeSecondPost, _ := f.remove(t, secondPostURI, secondPost.CID, f.tokenA, modlogSpam, "second community post")

	// A second authority writing to the same log, as another instance would.
	secondAuthority := fixtures.DID(testkit.UniqueIDWithPrefix(t, "logauthority"))
	f.resolver["authority-b.test"] = secondAuthority
	secondService := moderation.NewService(
		moderation.NewRepositorySubjectReader(postgres.NewPostRepository(f.db), postgres.NewCommentRepository(f.db)),
		postgres.NewModerationRepository(f.db),
		moderation.Config{InstanceDID: secondAuthority, IdempotencyRetention: 24 * time.Hour, MaxLiveIdempotencyKeys: 1000},
	)
	secondResult, err := secondService.RemoveContent(ctx, f.adminA, moderation.RemoveContentRequest{
		Subject:         moderation.StrongRef{URI: secondCommentURI, CID: secondCommentCID},
		ExpectedVersion: "v0", IdempotencyKey: testkit.UniqueIDWithPrefix(t, "remove"), Reason: modlogSpam,
	})
	require.NoError(t, err)
	require.NotNil(t, secondResult.Action)
	removeSecondAuthority := secondResult.Action.ID

	// No service writes inherited actions yet; the 050 schema accepts the row.
	inheritedURI, inheritedCID := f.comment(t)
	inheritedAuthority := fixtures.DID(testkit.UniqueIDWithPrefix(t, "logupstream"))
	inheritedID := testkit.TID()
	_, err = f.db.ExecContext(ctx, `
		INSERT INTO moderation_actions
		    (id, actor_did, authority_did, scope_kind, subject_uri, subject_collection,
		     subject_community_did, observed_cid, action, reason, origin, created_at)
		VALUES ($1, $2, $2, 'instance', $3, $4, $5, $6, 'apply-removal', $7, 'inherited', NOW())
	`, inheritedID, inheritedAuthority, inheritedURI, moderation.CommentCollection, f.communityDID, inheritedCID, modlogSpam)
	require.NoError(t, err)

	all := []string{removeFirst, restoreFirst, removeOther, removeSecondPost, removeSecondAuthority, inheritedID}
	firstCommunity := []string{removeFirst, restoreFirst, removeOther, inheritedID}
	secondCommunity := []string{removeSecondPost, removeSecondAuthority}
	instance := []string{removeFirst, restoreFirst, removeOther, removeSecondPost}
	cases := []struct {
		query url.Values
		want  []string
	}{
		{url.Values{}, all},
		{url.Values{"community": {f.communityDID}}, firstCommunity},
		{url.Values{"community": {f.communityHandle}}, firstCommunity},
		{url.Values{"community": {f.communityName + "@coves.social"}}, firstCommunity},
		{url.Values{"community": {secondDID}}, secondCommunity},
		{url.Values{"community": {secondHandle}}, secondCommunity},
		{url.Values{"community": {secondName + "@coves.social"}}, secondCommunity},
		{url.Values{"authority": {fixtures.InstanceDID()}}, instance},
		{url.Values{"authority": {"authority.test"}}, instance},
		{url.Values{"authority": {secondAuthority}}, []string{removeSecondAuthority}},
		{url.Values{"authority": {"authority-b.test"}}, []string{removeSecondAuthority}},
		{url.Values{"authority": {inheritedAuthority}}, []string{inheritedID}},
		{url.Values{"origin": {"local"}}, []string{removeFirst, restoreFirst, removeOther, removeSecondPost, removeSecondAuthority}},
		{url.Values{"origin": {"inherited"}}, []string{inheritedID}},
		{url.Values{"collection": {moderation.CommentCollection}}, []string{removeFirst, restoreFirst, removeOther, removeSecondAuthority, inheritedID}},
		{url.Values{"collection": {moderation.LegacyPostCollection}}, []string{removeSecondPost}},
		{url.Values{"community": {secondHandle}, "authority": {"authority.test"}}, []string{removeSecondPost}},
		{url.Values{"community": {secondDID}, "collection": {moderation.CommentCollection}}, []string{removeSecondAuthority}},
		{url.Values{"community": {f.communityHandle}, "origin": {"inherited"}}, []string{inheritedID}},
		{url.Values{"collection": {moderation.CommentCollection}, "authority": {fixtures.InstanceDID()}, "origin": {"local"}},
			[]string{removeFirst, restoreFirst, removeOther}},
	}
	for _, testCase := range cases {
		t.Run(testCase.query.Encode(), func(t *testing.T) {
			_, items := modlogPage(t, f.list(t, false, "", testCase.query), false)
			var got []string
			for _, item := range items {
				got = append(got, modlogID(t, modlogAction(t, item, false)))
			}
			require.ElementsMatch(t, testCase.want, got)
		})
	}
}

func TestModerationModlogCommunityAssociationSurvivesReindex(t *testing.T) {
	f := newModlogFixture(t)
	uri, cid := f.comment(t)
	id, _ := f.remove(t, uri, cid, f.tokenA, modlogSpam, "durable")
	check := func(t *testing.T) {
		t.Helper()
		for _, community := range []string{f.communityDID, f.communityHandle} {
			_, items := modlogPage(t, f.list(t, false, "", url.Values{"community": {community}}), false)
			require.Len(t, items, 1)
			assert.Equal(t, id, modlogID(t, modlogAction(t, items[0], false)))
		}
	}
	check(t)
	_, err := f.db.ExecContext(t.Context(), `DELETE FROM comments WHERE uri = $1`, uri)
	require.NoError(t, err)
	_, err = f.db.ExecContext(t.Context(), `TRUNCATE TABLE comments, posts CASCADE`)
	require.NoError(t, err)
	// With nothing indexed to show the subject is public, it is restricted:
	// listed without its subject, and not selectable by community.
	for _, community := range []string{f.communityDID, f.communityHandle} {
		_, items := modlogPage(t, f.list(t, false, "", url.Values{"community": {community}}), false)
		assert.Empty(t, items)
	}
	_, items := modlogPage(t, f.list(t, false, "", nil), false)
	require.Len(t, items, 1)
	assert.NotContains(t, modlogAction(t, items[0], false), "subject")
	_, err = f.db.ExecContext(t.Context(), `INSERT INTO posts (uri,cid,rkey,author_did,community_did,title,created_at) VALUES ($1,$2,$3,$4,$5,$6,NOW())`, f.postURI, f.postCID, f.postURI[strings.LastIndex(f.postURI, "/")+1:], f.authorDID, f.communityDID, "reindexed post")
	require.NoError(t, err)
	_, err = f.db.ExecContext(t.Context(), `INSERT INTO comments (uri,cid,rkey,commenter_did,root_uri,root_cid,parent_uri,parent_cid,content,created_at) VALUES ($1,$2,$3,$4,$5,$6,$5,$6,$7,NOW())`, uri, cid, uri[strings.LastIndex(uri, "/")+1:], f.authorDID, f.postURI, f.postCID, "reindexed comment")
	require.NoError(t, err)
	check(t)
}
