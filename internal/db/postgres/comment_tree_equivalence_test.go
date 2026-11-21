//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	commentsAPI "Coves/internal/api/handlers/comments"
	"Coves/internal/api/middleware"
	"Coves/internal/core/accountstatus"
	"Coves/internal/core/comments"
	"Coves/internal/core/feedgen"
	"Coves/internal/core/moderation"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/tests/fixtures"
	"Coves/tests/fixtures/accountstatusfixture"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// getComments served by the comment generators must render exactly the tree
// today's queries render.
//
// One database serves two getComments stacks, both the real handler over a
// real comment service wired as production wires it:
//
//   - production: the registry's comment built-ins are the postgres comment
//     generators, hydrated by the comment repository's HydrateComments;
//   - oracle: a registry with the same rkeys whose generators run the
//     comment_oracle_test.go copies of today's SQL (one parent: the paged
//     ListByParentWithHotRank query, several parents: the ListByParentsBatch
//     LATERAL query), over a repository whose HydrateComments hands back the
//     rows those queries scanned. The reference never goes through the new
//     hydrate.
//
// Responses are compared as complete JSON (every comment view at every level,
// and the thread header), the top level paged to the end at limit 2 with each
// stack's own cursors. Cursor bytes are not compared: a hot cursor pins the
// rank clock of its own page one. Literal assertions on the production
// response catch a traversal bug the two stacks would share.

// commentTreeStack is one getComments stack over the shared database.
type commentTreeStack struct {
	name    string
	handler *commentsAPI.GetCommentsHandler
	feeds   *feedgen.Registry
}

// commentTreeHarness is the shared database, the thread, its cast and both
// stacks.
type commentTreeHarness struct {
	env        *commentEnv
	impl       *postgresCommentRepo
	viewer     string
	blocked    string
	restricted string
	production commentTreeStack
	oracle     commentTreeStack
	names      map[string]string // comment URI -> fixture name
}

// newCommentTreeHarness builds the database, the viewer cast and both stacks.
func newCommentTreeHarness(t *testing.T) *commentTreeHarness {
	t.Helper()
	env := commentEnvFor(t)
	instanceDID := fixtures.InstanceDID()
	accountRestriction := WithAccountRestrictionAuthority(instanceDID)

	cast := seedBlockFilterCast(t, env.db, "tree")
	insertUserBlock(t, env.db, cast.viewer, cast.blocked)
	restricted := "did:plc:treerestricted" + env.id
	createTestUser(t, env.db, "treerestricted-"+env.id+".test", restricted)
	accountstatusfixture.Set(t, env.db, restricted, accountstatus.Takendown)

	repo := NewCommentRepository(env.db, accountRestriction)
	impl := repo.(*postgresCommentRepo)

	productionFeeds := feedgen.NewRegistry(instanceDID,
		feedgen.CommentsHotBuiltin(NewCommentsHotGenerator(env.db)),
		feedgen.CommentsTopBuiltin(NewCommentsTopGenerator(env.db)),
		feedgen.CommentsNewBuiltin(NewCommentsNewGenerator(env.db)),
	)

	stash := &oracleCommentStash{rows: map[string]*comments.Comment{}}
	oracleBuiltin := func(rkey, sort string) feedgen.Builtin {
		return feedgen.Builtin{
			RKey:             rkey,
			DisplayName:      rkey,
			Description:      "oracle " + rkey,
			Kind:             feedgen.KindComments,
			Scopes:           []string{feedgen.ScopeThread},
			CommentGenerator: &oracleCommentGenerator{repo: impl, sort: sort, stash: stash},
		}
	}
	oracleFeeds := feedgen.NewRegistry(instanceDID,
		oracleBuiltin("comments-hot", "hot"),
		oracleBuiltin("comments-top", "top"),
		oracleBuiltin("comments-new", "new"),
	)

	build := func(name string, commentRepo comments.Repository, feeds *feedgen.Registry) commentTreeStack {
		service := comments.NewCommentService(
			commentRepo, NewUserRepository(env.db), NewPostRepository(env.db, accountRestriction),
			NewCommunityRepository(env.db, credentialciphertest.Fixed(), accountRestriction),
			nil, nil, nil,
			comments.WithCommunityBans(NewCommunityBanRepository(env.db)),
			comments.WithCommunityRemovals(NewCommunityRemovalReader(env.db, instanceDID)),
			comments.WithAccountBans(NewAccountBanReader(env.db, instanceDID)),
			comments.WithInstanceDID(instanceDID),
			comments.WithFeedRegistry(feeds),
		)
		return commentTreeStack{
			name: name,
			handler: commentsAPI.NewGetCommentsHandler(commentsAPI.NewServiceAdapter(service), nil,
				moderation.NewAllowlistAuthority(nil), instanceDID),
			feeds: feeds,
		}
	}

	return &commentTreeHarness{
		env:        env,
		impl:       impl,
		viewer:     cast.viewer,
		blocked:    cast.blocked,
		restricted: restricted,
		production: build("production", repo, productionFeeds),
		oracle:     build("oracle", &oracleCommentRepo{Repository: repo, stash: stash}, oracleFeeds),
		names:      map[string]string{},
	}
}

// oracleCommentStash holds the rows the oracle queries scanned, by URI, for
// oracleCommentRepo.HydrateComments to hand back.
type oracleCommentStash struct {
	mu   sync.Mutex
	rows map[string]*comments.Comment
}

func (s *oracleCommentStash) put(rows []*comments.Comment) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	uris := make([]string, 0, len(rows))
	for _, row := range rows {
		s.rows[row.URI] = row
		uris = append(uris, row.URI)
	}
	return uris
}

// oracleCommentGenerator is a comment generator over today's queries.
type oracleCommentGenerator struct {
	repo  *postgresCommentRepo
	sort  string
	stash *oracleCommentStash
}

func (g *oracleCommentGenerator) CommentSkeleton(ctx context.Context, req feedgen.CommentSkeletonRequest) (feedgen.CommentSkeletonPage, error) {
	if len(req.Parents) == 1 {
		var cursor *string
		if req.Cursor != "" {
			cursor = &req.Cursor
		}
		rows, next, err := oracleListByParentWithHotRank(ctx, g.repo, req.Parents[0], g.sort, req.Timeframe,
			req.LimitPerParent, cursor, req.ViewerDID)
		if err != nil {
			return feedgen.CommentSkeletonPage{}, err
		}
		replies := feedgen.CommentReplies{Parent: req.Parents[0], URIs: g.stash.put(rows)}
		if next != nil {
			replies.Cursor = *next
		}
		return feedgen.CommentSkeletonPage{Replies: []feedgen.CommentReplies{replies}}, nil
	}

	byParent, err := oracleListByParentsBatch(ctx, g.repo, req.Parents, g.sort, req.LimitPerParent, req.ViewerDID)
	if err != nil {
		return feedgen.CommentSkeletonPage{}, err
	}
	page := feedgen.CommentSkeletonPage{Replies: make([]feedgen.CommentReplies, 0, len(req.Parents))}
	for _, parent := range req.Parents {
		page.Replies = append(page.Replies, feedgen.CommentReplies{Parent: parent, URIs: g.stash.put(byParent[parent])})
	}
	return page, nil
}

// oracleCommentRepo is the real comment repository, except that hydration
// returns the rows the oracle queries scanned, in the requested order.
type oracleCommentRepo struct {
	comments.Repository
	stash *oracleCommentStash
}

func (r *oracleCommentRepo) HydrateComments(_ context.Context, uris []string, _ string) ([]*comments.Comment, error) {
	r.stash.mu.Lock()
	defer r.stash.mu.Unlock()
	out := make([]*comments.Comment, 0, len(uris))
	for _, uri := range uris {
		if row, ok := r.stash.rows[uri]; ok {
			out = append(out, row)
		}
	}
	return out, nil
}

// seed writes one named comment; parent is a fixture name ("" = the post).
func (h *commentTreeHarness) seed(name, author, parent string, age time.Duration) string {
	h.env.t.Helper()
	spec := commentSpec{rkey: name, author: author, createdAt: time.Now().UTC().Add(-age)}
	if parent != "" {
		spec.parent = h.uri(parent)
	}
	uri := h.env.seed(spec)
	h.names[uri] = name
	return uri
}

// uri is the URI of the named fixture comment.
func (h *commentTreeHarness) uri(name string) string {
	h.env.t.Helper()
	for uri, n := range h.names {
		if n == name {
			return uri
		}
	}
	h.env.t.Fatalf("no fixture comment named %q", name)
	return ""
}

// votes writes native and bridged vote aggregates as the vote consumer and the
// bridge would leave them.
func (h *commentTreeHarness) votes(name string, nativeUp, nativeDown, bridgedUp int) {
	h.env.t.Helper()
	_, err := h.env.db.ExecContext(h.env.ctx, `
		UPDATE comments SET upvote_count = $2, downvote_count = $3, bridged_upvote_count = $4,
			score = $2::int - $3::int + $4::int
		WHERE uri = $1
	`, h.uri(name), nativeUp, nativeDown, bridgedUp)
	require.NoError(h.env.t, err)
}

// countReplies sets every thread comment's reply_count to its stored direct
// replies, as the comment consumer maintains it.
func (h *commentTreeHarness) countReplies() {
	h.env.t.Helper()
	_, err := h.env.db.ExecContext(h.env.ctx, `
		UPDATE comments c SET reply_count = (SELECT count(*) FROM comments r WHERE r.parent_uri = c.uri)
		WHERE c.root_uri = $1
	`, h.env.root)
	require.NoError(h.env.t, err)
}

// removeComment records an active instance removal of the named comment in
// the shape the moderation service writes.
func (h *commentTreeHarness) removeComment(name string) {
	h.env.t.Helper()
	actionID := "tree-removal-" + testkit.UniqueID(h.env.t)
	_, err := h.env.db.ExecContext(h.env.ctx, `
		INSERT INTO moderation_actions
		    (id, actor_did, authority_did, scope_kind, subject_uri, subject_collection,
		     action, reason, origin, created_at)
		VALUES ($1, 'did:plc:treemoderator', $2, 'instance', $3,
		        'social.coves.community.comment', 'remove',
		        'social.coves.moderation.defs#reasonSpam', 'local', NOW())
	`, actionID, fixtures.InstanceDID(), h.uri(name))
	require.NoError(h.env.t, err)
	_, err = h.env.db.ExecContext(h.env.ctx, `
		INSERT INTO moderation_decisions
		    (authority_did, scope_kind, scope_community_did, subject_uri, kind, value, active_action_id, active)
		VALUES ($1, 'instance', NULL, $2, 'removal', NULL, $3, TRUE)
	`, fixtures.InstanceDID(), h.uri(name), actionID)
	require.NoError(h.env.t, err)
}

// seedCommentTree writes the B1/B2 thread. Ages are relative to now and hours
// or days apart, away from every timeframe boundary.
//
//	top level (name, author, age, score)         replies
//	hotel    author      -3h (future)  1
//	bravo    author       1h   native 6, bridged 4 = 10    bravoone (restricted)
//	alpha    author       3h   native 25-2, bridged 7 = 30  seven direct replies (one blocked,
//	                                                         one ten days old) -> hasMore;
//	                                                         alphaone > alphaoneone > alphaoneoneone
//	foxtrot  blocked      4h   50                           (absent)
//	delta    author       6h   3, moderator-removed         deltaone
//	echo     restricted  20h   8                            echoone
//	charlie  author       2d   12, deleted by its author    charlieone, charlietwo
//	golf     author      10d   100
func (h *commentTreeHarness) seedCommentTree() {
	t := h.env.t
	author := h.env.author
	hour, day := time.Hour, 24*time.Hour

	h.seed("hotel", author, "", -3*hour)
	h.seed("bravo", author, "", 1*hour)
	h.seed("alpha", author, "", 3*hour)
	h.seed("foxtrot", h.blocked, "", 4*hour)
	h.seed("delta", author, "", 6*hour)
	h.seed("echo", h.restricted, "", 20*hour)
	h.seed("charlie", author, "", 2*day)
	h.seed("golf", author, "", 10*day)

	h.seed("bravoone", h.restricted, "bravo", 30*time.Minute)
	h.seed("alphaone", author, "alpha", 2*hour)
	h.seed("alphatwo", author, "alpha", 150*time.Minute)
	h.seed("alphathree", author, "alpha", 160*time.Minute)
	h.seed("alphafour", author, "alpha", 170*time.Minute)
	h.seed("alphafive", author, "alpha", 175*time.Minute)
	h.seed("alphaold", author, "alpha", 10*day)
	h.seed("alphablocked", h.blocked, "alpha", 90*time.Minute)
	h.seed("alphaoneone", author, "alphaone", 1*hour)
	h.seed("alphaoneoneone", author, "alphaoneone", 30*time.Minute)
	h.seed("deltaone", author, "delta", 5*hour)
	h.seed("echoone", author, "echo", 19*hour)
	h.seed("charlieone", author, "charlie", 1*day)
	h.seed("charlietwo", author, "charlie", 36*hour)

	h.votes("hotel", 1, 0, 0)
	h.votes("bravo", 6, 0, 4)
	h.votes("alpha", 25, 2, 7)
	h.votes("foxtrot", 50, 0, 0)
	h.votes("delta", 3, 0, 0)
	h.votes("echo", 8, 0, 0)
	h.votes("charlie", 12, 0, 0)
	h.votes("golf", 100, 0, 0)
	h.votes("alphaone", 4, 0, 0)
	h.votes("alphatwo", 9, 1, 2)
	h.votes("alphathree", 2, 0, 0)
	h.votes("alphafour", 0, 1, 0)
	h.votes("alphaold", 40, 0, 0)
	h.votes("alphablocked", 70, 0, 0)
	h.votes("alphaoneone", 3, 0, 0)
	h.votes("charlieone", 5, 0, 0)

	// Populated record fields beyond content and langs.
	_, err := h.env.db.ExecContext(h.env.ctx, `
		UPDATE comments SET
			content_facets = '[{"index":{"byteStart":0,"byteEnd":4},"features":[{"$type":"social.coves.richtext.facet#mention","did":"did:plc:treementioned"}]}]',
			content_labels = '{"values":[{"val":"spoiler"}]}'
		WHERE uri = $1
	`, h.uri("alphatwo"))
	require.NoError(t, err)

	require.NoError(t, h.env.repo.SoftDeleteWithReason(h.env.ctx, h.uri("charlie"), comments.DeletionReasonAuthor, author))
	h.removeComment("delta")
	h.countReplies()
}

// commentTreePage is one getComments response: its comments array, header and
// whether it carried a cursor.
type commentTreePage struct {
	Comments  []any
	Post      any
	HasCursor bool
}

// get serves one getComments request as the signed-in viewer.
func (h *commentTreeHarness) get(t *testing.T, stack commentTreeStack, params url.Values) (int, []byte) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.community.comment.getComments?"+params.Encode(), nil)
	request = request.WithContext(middleware.SetTestUserDID(request.Context(), h.viewer))
	recorder := httptest.NewRecorder()
	stack.handler.HandleGetComments(recorder, request)
	return recorder.Code, recorder.Body.Bytes()
}

// walk pages the top level at limit 2 with the stack's own cursors to the end.
func (h *commentTreeHarness) walk(t *testing.T, stack commentTreeStack, params url.Values) []commentTreePage {
	t.Helper()
	var pages []commentTreePage
	cursor := ""
	for range 20 {
		query := url.Values{"post": {h.env.root}, "limit": {"2"}}
		for key, values := range params {
			query[key] = values
		}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		status, body := h.get(t, stack, query)
		require.Equalf(t, http.StatusOK, status, "%s stack, %v: %s", stack.name, query, body)
		var decoded struct {
			Comments []any  `json:"comments"`
			Post     any    `json:"post"`
			Cursor   string `json:"cursor"`
		}
		require.NoError(t, json.Unmarshal(body, &decoded))
		pages = append(pages, commentTreePage{Comments: decoded.Comments, Post: decoded.Post, HasCursor: decoded.Cursor != ""})
		if decoded.Cursor == "" {
			return pages
		}
		cursor = decoded.Cursor
	}
	t.Fatalf("%s stack, %v: still paging after 20 pages", stack.name, params)
	return nil
}

// requireSamePages asserts two walks rendered identical pages: first as a
// compact outline of fixture names (readable on failure), then as full JSON.
func (h *commentTreeHarness) requireSamePages(t *testing.T, want, got []commentTreePage, wantLabel, gotLabel string) {
	t.Helper()
	outlines := func(pages []commentTreePage) []string {
		out := make([]string, 0, len(pages))
		for _, page := range pages {
			out = append(out, h.outline(decodeCommentTreeNodes(t, page.Comments)))
		}
		return out
	}
	require.Equalf(t, outlines(want), outlines(got), "page outlines (name, *deleted, +hasMore, (replies)), %s vs %s", gotLabel, wantLabel)
	for i := range want {
		assert.Equalf(t, want[i].HasCursor, got[i].HasCursor, "page %d: cursor presence, %s vs %s", i+1, gotLabel, wantLabel)
		wantJSON, err := json.MarshalIndent(want[i].Comments, "", " ")
		require.NoError(t, err)
		gotJSON, err := json.MarshalIndent(got[i].Comments, "", " ")
		require.NoError(t, err)
		assert.JSONEqf(t, string(wantJSON), string(gotJSON), "page %d: the comment tree, %s vs %s", i+1, gotLabel, wantLabel)
		wantPost, err := json.Marshal(want[i].Post)
		require.NoError(t, err)
		gotPost, err := json.Marshal(got[i].Post)
		require.NoError(t, err)
		assert.JSONEqf(t, string(wantPost), string(gotPost), "page %d: the thread header, %s vs %s", i+1, gotLabel, wantLabel)
	}
}

// commentTreeNode is the part of a thread view node the literal assertions read.
type commentTreeNode struct {
	URI       string
	IsDeleted bool
	HasMore   bool
	Replies   []commentTreeNode
}

func decodeCommentTreeNodes(t *testing.T, raw []any) []commentTreeNode {
	t.Helper()
	nodes := make([]commentTreeNode, 0, len(raw))
	for _, entry := range raw {
		object, ok := entry.(map[string]any)
		require.Truef(t, ok, "thread node %#v", entry)
		comment, ok := object["comment"].(map[string]any)
		require.Truef(t, ok, "thread node comment %#v", object["comment"])
		node := commentTreeNode{}
		node.URI, _ = comment["uri"].(string)
		node.IsDeleted, _ = comment["isDeleted"].(bool)
		node.HasMore, _ = object["hasMore"].(bool)
		if replies, ok := object["replies"].([]any); ok {
			node.Replies = decodeCommentTreeNodes(t, replies)
		}
		nodes = append(nodes, node)
	}
	return nodes
}

// topLevel flattens a walk's top-level nodes in page order.
func topLevel(t *testing.T, pages []commentTreePage) []commentTreeNode {
	t.Helper()
	var nodes []commentTreeNode
	for _, page := range pages {
		nodes = append(nodes, decodeCommentTreeNodes(t, page.Comments)...)
	}
	return nodes
}

func (h *commentTreeHarness) nodeNames(nodes []commentTreeNode) []string {
	names := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if name, ok := h.names[node.URI]; ok {
			names = append(names, name)
		} else {
			names = append(names, node.URI)
		}
	}
	return names
}

// outline renders nodes as "name*+(child child)": * deleted, + hasMore.
func (h *commentTreeHarness) outline(nodes []commentTreeNode) string {
	parts := make([]string, 0, len(nodes))
	for i, node := range nodes {
		part := h.nodeNames(nodes[i : i+1])[0]
		if node.IsDeleted {
			part += "*"
		}
		if node.HasMore {
			part += "+"
		}
		if len(node.Replies) > 0 {
			part += "(" + h.outline(node.Replies) + ")"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, " ")
}

func findCommentTreeNode(nodes []commentTreeNode, uri string) (commentTreeNode, bool) {
	for _, node := range nodes {
		if node.URI == uri {
			return node, true
		}
		if found, ok := findCommentTreeNode(node.Replies, uri); ok {
			return found, true
		}
	}
	return commentTreeNode{}, false
}

// TestGetComments_TreeMatchesOracle: for each sort, the production stack's
// getComments tree equals the oracle stack's at every page.
func TestGetComments_TreeMatchesOracle(t *testing.T) {
	t.Parallel()
	h := newCommentTreeHarness(t)
	h.seedCommentTree()

	rows := []struct {
		name   string
		params url.Values
	}{
		{"hot", url.Values{"sort": {"hot"}}},
		{"top", url.Values{"sort": {"top"}}},
		{"new", url.Values{"sort": {"new"}}},
		{"top week", url.Values{"sort": {"top"}, "timeframe": {"week"}}},
	}
	walks := map[string][]commentTreePage{}
	for _, row := range rows {
		oracle := h.walk(t, h.oracle, row.params)
		production := h.walk(t, h.production, row.params)
		h.requireSamePages(t, oracle, production, "oracle "+row.name, "production "+row.name)
		walks[row.name] = production
	}

	// Literals on the production response, so a traversal bug both stacks
	// share is still caught.
	newOrder := topLevel(t, walks["new"])
	assert.Equal(t,
		[]string{"hotel", "bravo", "alpha", "delta", "echo", "charlie", "golf"},
		h.nodeNames(newOrder),
		"sort=new: newest first, the future-dated comment leading, the blocked commenter's comment absent")
	if len(newOrder) == 7 {
		assert.True(t, newOrder[5].IsDeleted, "charlie is served as a deleted placeholder at its own position")
		assert.ElementsMatch(t, []string{"charlieone", "charlietwo"}, h.nodeNames(newOrder[5].Replies),
			"the deleted placeholder keeps its replies")
		assert.True(t, newOrder[2].HasMore, "alpha has seven replies and only five load per parent")
		assert.Len(t, newOrder[2].Replies, 5, "alpha's reply batch is five")
	}
	for name, walk := range walks {
		_, found := findCommentTreeNode(topLevel(t, walk), h.uri("foxtrot"))
		assert.Falsef(t, found, "%s: the blocked commenter's top-level comment is absent", name)
		_, found = findCommentTreeNode(topLevel(t, walk), h.uri("alphablocked"))
		assert.Falsef(t, found, "%s: the blocked commenter's reply is absent", name)
	}
	if alpha, ok := findCommentTreeNode(topLevel(t, walks["new"]), h.uri("alpha")); ok {
		if one, ok := findCommentTreeNode(alpha.Replies, h.uri("alphaone")); ok {
			assert.Equal(t, []string{"alphaoneone"}, h.nodeNames(one.Replies), "second reply level")
			if len(one.Replies) == 1 {
				assert.Equal(t, []string{"alphaoneoneone"}, h.nodeNames(one.Replies[0].Replies), "third reply level")
			}
		} else {
			t.Errorf("sort=new: alphaone (alpha's newest reply) is missing")
		}
	}

	// The week row is not vacuous: the ten-day-old top-level comment is
	// excluded while a recent top-level comment keeps its ten-day-old reply
	// (reply batches are all-time).
	week := topLevel(t, walks["top week"])
	assert.NotContains(t, h.nodeNames(week), "golf", "top/week excludes the ten-day-old top-level comment")
	alpha, ok := findCommentTreeNode(week, h.uri("alpha"))
	require.True(t, ok, "top/week keeps alpha")
	assert.Contains(t, h.nodeNames(alpha.Replies), "alphaold",
		"top/week keeps alpha's ten-day-old reply: reply levels are not timeframe-filtered")
}

// TestGetComments_FeedParamMatchesSortAlias: on the production stack,
// feed=<comments-hot|top|new> renders the same pages as sort=hot|top|new.
func TestGetComments_FeedParamMatchesSortAlias(t *testing.T) {
	t.Parallel()
	h := newCommentTreeHarness(t)
	h.seedCommentTree()

	for _, sort := range []string{"hot", "top", "new"} {
		bySort := h.walk(t, h.production, url.Values{"sort": {sort}})
		byFeed := h.walk(t, h.production, url.Values{"feed": {h.production.feeds.URI("comments-" + sort)}})
		h.requireSamePages(t, bySort, byFeed, "sort="+sort, "feed=comments-"+sort)
	}
}

// TestGetComments_SubtreeTopTimeframeReachesNestedLevel: a parentRkey subtree
// read with feed=comments-top applies the timeframe to the parent's direct
// replies.
func TestGetComments_SubtreeTopTimeframeReachesNestedLevel(t *testing.T) {
	t.Parallel()
	h := newCommentTreeHarness(t)
	author := h.env.author
	day := 24 * time.Hour

	h.seed("papa", author, "", time.Hour)
	h.seed("papatwoday", author, "papa", 2*day)
	h.seed("papatenday", author, "papa", 10*day)
	h.seed("papahour", author, "papa", 30*time.Minute)
	h.seed("papafourday", author, "papa", 4*day)
	h.votes("papa", 2, 0, 0)
	h.votes("papatwoday", 5, 0, 0)
	h.votes("papatenday", 90, 0, 3)
	h.votes("papahour", 1, 0, 0)
	h.votes("papafourday", 3, 0, 0)
	h.countReplies()

	// The subtree's direct replies across every page.
	subtreeReplies := func(pages []commentTreePage) []string {
		var names []string
		for i, page := range pages {
			nodes := decodeCommentTreeNodes(t, page.Comments)
			require.Lenf(t, nodes, 1, "page %d: the subtree response has the parent as its one entry", i+1)
			require.Equal(t, h.uri("papa"), nodes[0].URI)
			names = append(names, h.nodeNames(nodes[0].Replies)...)
		}
		return names
	}
	const rkey = "papa"
	topFeed := h.production.feeds.URI("comments-top")

	week := h.walk(t, h.production, url.Values{"parentRkey": {rkey}, "feed": {topFeed}, "timeframe": {"week"}})
	assert.Equal(t, []string{"papatwoday", "papafourday", "papahour"}, subtreeReplies(week),
		"feed=comments-top&timeframe=week: the ten-day reply is absent, the rest by score")
	oracleWeek := h.walk(t, h.oracle, url.Values{"parentRkey": {rkey}, "sort": {"top"}, "timeframe": {"week"}})
	h.requireSamePages(t, oracleWeek, week, "oracle sort=top week", "production feed=comments-top week")

	month := h.walk(t, h.production, url.Values{"parentRkey": {rkey}, "feed": {topFeed}, "timeframe": {"month"}})
	assert.Equal(t, []string{"papatenday", "papatwoday", "papafourday", "papahour"}, subtreeReplies(month),
		"feed=comments-top&timeframe=month: the highest-scored ten-day reply is first")
}
