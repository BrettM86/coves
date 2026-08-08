//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The comment domain's pipeline contracts: the ingestion proof for
// social.coves.community.comment, and the client-facing surface a third-party
// client actually reaches.
//
// # COMMENTS LIVE IN THE AUTHOR'S REPO, WHICH IS THE OPPOSITE OF POSTS
//
// A post is written to the COMMUNITY's repo with the community's credentials
// (post_contract_test.go opens by explaining why). A comment is not: it is
// written to the repo of the human who wrote it, and the consumer takes the
// commenter's identity from the repo the commit arrived in —
// `CommenterDID: repoDID`, comment_consumer.go. The record itself names no
// author at all.
//
// That inversion is why this file's ingestion write uses the AUTHOR's session
// where the post contract used the community's, and it is also why the spoof
// the post contract spends most of its length on has no analogue here. A post
// record carries a `community` field that the consumer checks against the repo
// (forge it and you are publishing as someone else); a comment record carries
// no such field, so there is nothing to forge. The comment domain's equivalent
// security invariant is elsewhere, in the threading references, and it is what
// this contract's negative proves — see the hijack section of TestCommentIngestion.
//
// # THE CONSUMER HAS NO MUST-EXIST GATES AT ALL (measured, not inferred)
//
// The post consumer refuses a post whose community it has not indexed, and one
// whose author it has not indexed. The comment consumer refuses NEITHER, and
// says so in a comment block of its own (validateCommentEvent: "We do NOT check
// if the user exists in AppView because ... comment events may arrive before
// user events"). Both halves were confirmed against the running stack while
// writing this file, because a gate nobody documents is exactly the thing that
// gets added later:
//
//   - a comment whose root post the AppView has never indexed is STILL indexed,
//     and is served by social.coves.actor.getComments. It is invisible in the
//     thread view only because getComments needs the post row to answer at all.
//   - a comment from a repo that never went through social.coves.actor.signup
//     is STILL indexed and served in the thread, with the author's handle
//     falling back to their DID.
//
// So no classification of transient-vs-permanent is owed here in the way task
// 12 owed one for posts: every rejection the comment consumer makes is a
// PERMANENT one about the event payload (malformed DID, empty content,
// oversize content, missing or malformed reply refs, threading reassignment),
// and the ordering hazard that makes must-exist gates transient does not arise.
// Those payload rejections are pinned at T1 in
// internal/core/comments/comment_consumer_test.go, cheaply and exhaustively;
// this contract proves the one that is only observable end to end.
//
// The counts DO reconcile across arrival order — a reply that beats its parent
// into the index has the parent's reply_count fixed up when the parent lands
// (indexCommentAndUpdateCounts) — but that is an in-database reconciliation, not
// a PDS read, so it cannot false-pass a pipeline assertion. Which brings us to:
//
// # RECONCILIATION PATHS: NONE FOR THIS COLLECTION (checked, see contracts_test.go)
//
// The package doc's hazard is code that reads the PDS on its own and can satisfy
// a wait with every consumer dead. For comments the search comes up empty, and
// is recorded so the next reader need not repeat it:
//
//   - users.maybeBackfillProfile, the known instance, is actor.profile-only, and
//     cmd/backfill-profiles is an operator CLI over that same user path.
//   - internal/core/comments constructs a PDS client on three paths only —
//     CreateComment, UpdateComment, DeleteComment — all of them client writes,
//     and none of them writes a comment row to Postgres. UpdateComment's and
//     DeleteComment's PDS reads fetch the existing record for the CID swap and
//     the ownership check; both discard it.
//   - the read paths (GetComments, GetActorComments) are SELECT-only and never
//     construct a PDS client.
//   - the only INSERT INTO comments reachable in a running server is
//     comment_consumer.go's. postgres.commentRepo.Create has no non-test callers
//     (the two other writers are dev data-generator scripts under scripts/).
//
// A single create → visible observation is therefore already honest, with no
// arming write of the kind the actor.profile contract needs.
//
// # THE THREAD ENDPOINT IS RATE LIMITED FIVE TIMES TIGHTER THAN ANYTHING ELSE
//
// Worth knowing before writing or extending anything here, because the symptom
// is a 429 that reads like a broken endpoint. social.coves.community.comment.
// getComments does not sit under the global 100/minute limiter every other
// route in this tier meets: it has its own, at 20/minute per client IP
// (commentQueryRateLimit, cmd/server/routes.go), because a nested tree query
// fans out across the whole comment tree. It is the only route in the product
// with a dedicated cap.
//
// An ingestion contract has to watch that one endpoint through create, reply,
// update and delete plus two Holds windows, which costs more than twenty reads
// however it is written — and there is no cheaper vantage point, since no other
// endpoint shows reply placement, and actor.getComments omits deleted comments
// so it cannot see the placeholder at all. TestCommentIngestion therefore takes
// a fresh read quota at each of its two phase boundaries; pipeline.
// FreshReadQuota carries the argument for why that is legitimate. Task 14's
// vote contract will meet the same cap the moment it reads a comment thread.
//
// # A DELETED COMMENT IS NOT GONE, AND THAT IS THE POINT
//
// Every other contract in this package ends with "deleted → absent, and stays
// absent". Comments end differently, and the difference is a product decision
// rather than an implementation detail: deleteComment SOFT-deletes and blanks
// the content, and the serving layer renders the row as a placeholder —
// isDeleted true, record null, author handle emptied, vote stats zeroed,
// replyCount kept — precisely so that a deleted comment's REPLIES keep their
// place in the thread instead of vanishing with their parent.
//
// So the destructive assertion here is not absence, it is that the placeholder
// appears, that the surviving reply is still served in full beneath it, and
// that Holds keeps watching — a resurrection-by-replay would show up as the
// content coming back, which is the same failure Holds guards against
// everywhere else, just with a different shape of "wrong".
const commentCollection = "social.coves.community.comment"

// commentView is the slice of a commentView union member that the contracts
// observe. As elsewhere in this package, modelling only the asserted fields
// keeps a new lexicon field from breaking every contract that reads a comment.
type commentView struct {
	URI            string         `json:"uri"`
	CID            string         `json:"cid"`
	Author         identityRef    `json:"author"`
	Record         map[string]any `json:"record"`
	Post           *strongRef     `json:"post"`
	Parent         *strongRef     `json:"parent"`
	Stats          commentStats   `json:"stats"`
	CreatedAt      string         `json:"createdAt"`
	IndexedAt      string         `json:"indexedAt"`
	IsDeleted      bool           `json:"isDeleted"`
	DeletionReason *string        `json:"deletionReason"`
	DeletedAt      *string        `json:"deletedAt"`
}

// strongRef is the uri+cid pair the lexicon uses for threading references.
type strongRef struct {
	URI string `json:"uri"`
	CID string `json:"cid"`
}

type commentStats struct {
	Upvotes    int `json:"upvotes"`
	Downvotes  int `json:"downvotes"`
	Score      int `json:"score"`
	ReplyCount int `json:"replyCount"`
}

// threadNode is one comment in the tree, with its replies nested underneath.
type threadNode struct {
	Comment commentView  `json:"comment"`
	Replies []threadNode `json:"replies"`
	HasMore bool         `json:"hasMore"`
}

// threadResponse is what social.coves.community.comment.getComments answers:
// the comment tree plus the post it hangs off, which the endpoint hydrates so a
// client can render a permalink page from one request.
type threadResponse struct {
	Comments []threadNode `json:"comments"`
	Post     postView     `json:"post"`
	Cursor   string       `json:"cursor"`
}

// Thread reads a post's comment tree.
//
// A post nobody has indexed is a 404 (RootNotFound) rather than an empty
// thread, which is what makes testkit.PendingIfNotFound usable in a probe here
// — unlike social.coves.community.post.get, whose not-found is a union member
// inside a 200 (post_contract_test.go's postView.NotFound).
func (p *pipeline) Thread(ctx context.Context, postURI string, extra url.Values) (threadResponse, error) {
	params := url.Values{"post": {postURI}}
	for k, vs := range extra {
		for _, v := range vs {
			params.Add(k, v)
		}
	}
	var out threadResponse
	err := p.AppView.Query(ctx, "social.coves.community.comment.getComments", params, &out)
	return out, err
}

// find returns the node for uri anywhere in the tree, and whether it was there.
// Recursive because a contract asserting reply placement must be able to say
// "under THIS parent", not merely "somewhere in the response".
func (t threadResponse) find(uri string) (threadNode, bool) {
	return findNode(t.Comments, uri)
}

func findNode(nodes []threadNode, uri string) (threadNode, bool) {
	for _, n := range nodes {
		if n.Comment.URI == uri {
			return n, true
		}
		if found, ok := findNode(n.Replies, uri); ok {
			return found, true
		}
	}
	return threadNode{}, false
}

// uris renders a thread's URIs for a failure message, so "the comment was not
// there" says what WAS there.
func (t threadResponse) uris() string {
	var out []string
	var walk func(nodes []threadNode, depth int)
	walk = func(nodes []threadNode, depth int) {
		for _, n := range nodes {
			out = append(out, strings.Repeat("  ", depth)+n.Comment.URI)
			walk(n.Replies, depth+1)
		}
	}
	walk(t.Comments, 0)
	if len(out) == 0 {
		return "(the thread was empty)"
	}
	return "\n" + strings.Join(out, "\n")
}

// commentURI renders the AT-URI a comment record has once it is committed: the
// AUTHOR's DID is the authority, which is this domain's defining shape.
func commentURI(authorDID, rkey string) string {
	return "at://" + authorDID + "/" + commentCollection + "/" + rkey
}

// commentRecord builds a social.coves.community.comment record in the shape
// internal/core/comments writes it (comment_service.go CreateComment), so the
// consumer parses exactly what production hands it.
//
// root is the post the thread hangs off; parent is what this comment replies to
// — the post itself for a top-level comment, another comment for a reply. Both
// refs are required by the lexicon and both are validated by the consumer.
func commentRecord(root, parent strongRef, content string) map[string]any {
	return map[string]any{
		"$type": commentCollection,
		"reply": map[string]any{
			"root":   map[string]any{"uri": root.URI, "cid": root.CID},
			"parent": map[string]any{"uri": parent.URI, "cid": parent.CID},
		},
		"content":   content,
		"langs":     []string{"en"},
		"createdAt": time.Now().UTC().Format(time.RFC3339),
	}
}

// indexedPost writes a post into a community's repo and waits for the AppView
// to have indexed it, returning the strong reference comments hang off.
//
// The wait is not optional politeness: getComments answers 404 RootNotFound
// until the post row exists, so a contract that commented before the post
// landed would report a comment-pipeline failure caused by the post pipeline.
// Left here rather than in post_contract_test.go because it is what the domains
// hanging off a post need, in the same way indexedCommunity is what the domains
// hanging off a community need.
func indexedPost(t *testing.T, p *pipeline, community provisionedCommunity, authorDID, title string) strongRef {
	t.Helper()

	rkey := testkit.TID()
	record := community.PutRecord(t, postCollection, rkey,
		postRecord(community.DID, authorDID, title, "a post to hang comments on"))
	uri := postURI(community.DID, rkey)

	// This wait was the first to hit the 250ms-era limiter cliff (measured
	// 23.97s healthy latency; five consecutive gate deaths) and carried its
	// own 600ms override until task 5 made that the tier default — see
	// contractPollInterval's HISTORY note.
	p.Await(t, "the post these comments hang off to be indexed", func() (bool, error) {
		view, err := p.Post(context.Background(), uri)
		if err != nil {
			return false, err
		}
		return !view.NotFound, nil
	})
	return strongRef{URI: uri, CID: record.CID}
}

// TestCommentIngestion is the pipeline proof for comments.
//
// coves:ingestion-contract social.coves.community.comment
//
// Every record below is written straight into the AUTHOR's own repo with the
// author's session, and every observation is made through
// social.coves.community.comment.getComments:
//
//	create → the comment appears as a top-level node on the post
//	reply  → the second comment is placed UNDER the first, not beside it
//	hijack → an update that reassigns the threading refs is refused, and stays
//	         refused (Holds), with the comment's content left untouched
//	update → a content-only edit reaches the same URI in place
//	delete → the comment becomes a "[deleted]" placeholder whose reply survives,
//	         and STAYS one (Holds, §3.4a's destructive step in this domain's shape)
//
// # WHY THE NEGATIVE IS A THREAD HIJACK, AND HOW IT IS BOUNDED
//
// Threading references are immutable after creation, and the consumer enforces
// it as a security rule rather than a consistency one ("prevents thread
// hijacking", comment_consumer.go). The attack it stops is worth stating plainly
// because it explains why this is the comment domain's headline negative: a
// comment record lives in its author's OWN repo, so its author may rewrite it at
// will, with no AppView involved. Were the reply refs mutable, anyone could take
// a comment that had accumulated agreement in one thread and re-point it at
// another post — moving other people's replies with it, since the replies name
// their parent by URI and the URI does not change.
//
// "The reassignment never takes effect" cannot be proven by waiting, for the
// reason the post contract's spoof section sets out: waiting only ever shows it
// has not happened YET. It is bounded here the same way, and by the same
// mechanism — a LATER WRITE INTO THE SAME REPO. The hijack is attempted first,
// then a second comment is written to the author's repo; when that second
// comment is visible, the hijack's commit has necessarily already been through
// the same consumer, because the PDS sequencer orders one repo's commits,
// Jetstream serializes per repo, and the connector feeds one feed's events to
// the consumer sequentially. Only then is the non-effect meaningful, and Holds
// keeps watching in case a redrive changes its mind.
//
// Same repo is the load-bearing word — it is a property of the comment domain
// that the bounding write is naturally in the same repo, since every one of an
// author's comments is. Please do not "simplify" the bounding write into the
// community's repo or another author's; cross-repo ordering is topology luck
// today and Phase 5's relay would break it silently.
//
// The hijack's target is a real, indexed post rather than a fabricated URI, so
// the refusal can only be attributed to the immutability rule: had it pointed
// at a post nobody indexed, the event would still have been refused, but a
// reader could not tell that apart from a missing-parent rejection — and as the
// package doc records, this consumer does not have one of those.
//
// # WHAT THE REFUSAL ACTUALLY PROTECTS (measured by mutation, worth stating)
//
// Disabling the immutability check and rebuilding the AppView fails the
// assertion below, so it is load-bearing. But it is worth being exact about
// WHY, because the obvious reading is wrong in an interesting way: the update
// path's SQL does not write root_uri or parent_uri at all, so even with the
// check gone, an update cannot relocate a row. What gets through instead is the
// CONTENT — the comment stays where it is and silently acquires text written
// for another thread, which is the assertion that actually goes red.
//
// So the rule is doing two jobs, and only the second is visible from here:
// refusing the disguised edit today, and keeping the update path safe if it
// ever learns to write those columns. Threading CAN legitimately change, but
// only through delete-then-recreate, which the consumer's resurrection path
// handles deliberately ("User may have deleted old comment and created a new
// one on a different parent/root") and which re-parents the counts as it goes.
func TestCommentIngestion(t *testing.T) {
	p := newPipeline(t)

	author := p.IndexedAccount(t, "ci")
	community := indexedCommunity(t, p, "ci", author.DID)
	post := indexedPost(t, p, community, author.DID, "the thread under test")
	elsewhere := indexedPost(t, p, community, author.DID, "the thread a hijack would move a comment to")

	created := "created " + testkit.UniqueID(t)
	updated := "updated " + testkit.UniqueID(t)

	// observe waits for the thread to satisfy accept, and returns the node the
	// contract is about. Threaded through every step so a failure names the
	// comment it was waiting on rather than "the thread".
	observe := func(uri, description string, accept func(threadNode) bool) threadNode {
		t.Helper()
		var observed threadNode
		p.Await(t, description, func() (bool, error) {
			thread, err := p.Thread(context.Background(), post.URI, nil)
			if done, err := testkit.PendingIfNotFound(err); !done || err != nil {
				return done, err
			}
			node, ok := thread.find(uri)
			if !ok {
				return false, nil
			}
			observed = node
			return accept(node), nil
		}, withReadCadence())
		return observed
	}

	// ---- create --------------------------------------------------------------
	// Straight into the author's repo. The AppView has never seen this write, so
	// its appearance below is firehose delivery or nothing.
	topRKey := testkit.TID()
	topURI := commentURI(author.DID, topRKey)
	topRecord := author.PutRecord(t, commentCollection, topRKey,
		commentRecord(post, post, created))
	top := strongRef{URI: topURI, CID: topRecord.CID}

	node := observe(topURI, "the directly-written comment to reach getComments via the consumers",
		func(n threadNode) bool { return n.Comment.Record["content"] == created })

	require.Equal(t, topURI, node.Comment.URI,
		"the AppView served the comment under a different URI than the record's")
	require.Equal(t, topRecord.CID, node.Comment.CID,
		"the indexed CID must be the commit's, not a re-derived one")
	require.Equal(t, author.DID, node.Comment.Author.DID,
		"the commenter is the repo the commit arrived in — the record names no author")
	require.Equal(t, author.Handle, node.Comment.Author.Handle,
		"the author reference is joined to the users table, so an unindexed author would show a DID here")
	require.Equal(t, commentCollection, node.Comment.Record["$type"])
	require.NotNil(t, node.Comment.Post)
	require.Equal(t, post.URI, node.Comment.Post.URI, "the comment must be filed under the post it names as root")
	require.Equal(t, post.CID, node.Comment.Post.CID)
	require.Nil(t, node.Comment.Parent,
		"a TOP-LEVEL comment replies to the post itself, so the view must omit the parent-comment "+
			"reference — a client uses its presence to decide whether to indent")
	require.Equal(t, commentStats{}, node.Comment.Stats,
		"a comment arrives with no votes and no replies; non-zero stats would mean the consumer invented them")
	require.False(t, node.Comment.IsDeleted)
	require.Empty(t, node.Replies)

	// The post's own comment count is part of this pipeline, not the post's: it
	// is the COMMENT consumer that increments posts.comment_count.
	postView, err := p.Post(context.Background(), post.URI)
	require.NoError(t, err)
	require.Equal(t, 1, postView.Stats.CommentCount,
		"indexing a comment must increment its post's commentCount — the comment consumer owns that counter")

	// ---- reply ----------------------------------------------------------------
	// Root stays the post, parent becomes the comment above: that pair is the
	// whole of thread placement, and getting it backwards is the classic way a
	// reply lands beside its parent instead of under it.
	replyRKey := testkit.TID()
	replyURI := commentURI(author.DID, replyRKey)
	author.PutRecord(t, commentCollection, replyRKey,
		commentRecord(post, top, "a reply that belongs under the first comment"))

	node = observe(topURI, "the reply to be placed under its parent comment",
		func(n threadNode) bool { return len(n.Replies) == 1 })

	require.Len(t, node.Replies, 1)
	reply := node.Replies[0].Comment
	require.Equal(t, replyURI, reply.URI,
		"the reply must be nested under the comment it names as parent, not returned as a sibling")
	require.NotNil(t, reply.Parent)
	require.Equal(t, topURI, reply.Parent.URI, "the reply's parent reference must name the comment it answers")
	require.Equal(t, top.CID, reply.Parent.CID)
	require.NotNil(t, reply.Post)
	require.Equal(t, post.URI, reply.Post.URI,
		"a reply's post reference is the thread ROOT, which is what lets a client link a nested reply "+
			"back to its post without walking the tree")
	require.Equal(t, 1, node.Comment.Stats.ReplyCount,
		"the parent's replyCount must count the reply — it is what the client renders as \"1 reply\" "+
			"before expanding the subtree")

	postView, err = p.Post(context.Background(), post.URI)
	require.NoError(t, err)
	require.Equal(t, 2, postView.Stats.CommentCount,
		"a nested reply counts toward its post's total, not only toward its parent's replyCount")

	// ---- the hijack, refused --------------------------------------------------
	// A phase boundary, and the first of this contract's two fresh read quotas.
	// getComments allows 20 requests a minute per client (commentQueryRateLimit),
	// which is the tightest cap in the product and less than this arc's honest
	// cost — pipeline.FreshReadQuota has the full reasoning. Taken HERE because
	// the hijack phase ends in a Holds, and a Holds that ran out of quota
	// half-way would report a rate limit where the invariant it watches is
	// perfectly healthy.
	p.FreshReadQuota(t, "hijack")

	// Same rkey as the top-level comment, so this is an update commit; the reply
	// refs point at a DIFFERENT post. The consumer must refuse the whole event —
	// including the content change riding along with it, which is how a real
	// hijack would disguise itself as an edit.
	author.PutRecord(t, commentCollection, topRKey,
		commentRecord(
			strongRef{URI: elsewhere.URI, CID: elsewhere.CID},
			strongRef{URI: elsewhere.URI, CID: elsewhere.CID},
			"hijacked into another thread"))

	// The bounding write: a second top-level comment into the SAME repo. Once it
	// is visible, the hijack commit ahead of it has been through the consumer.
	boundRKey := testkit.TID()
	boundURI := commentURI(author.DID, boundRKey)
	author.PutRecord(t, commentCollection, boundRKey,
		commentRecord(post, post, "the write that bounds the hijack attempt"))

	observe(boundURI, "a later comment from the same repo, which bounds the hijack attempt", func(threadNode) bool {
		return true
	})

	unmoved := func() (bool, error) {
		thread, err := p.Thread(context.Background(), post.URI, nil)
		if err != nil {
			return false, err
		}
		n, ok := thread.find(topURI)
		if !ok {
			return false, fmt.Errorf(
				"the hijacked comment %s left the thread of the post it was written under: %s",
				topURI, thread.uris())
		}
		return n.Comment.Record["content"] == created && len(n.Replies) == 1, nil
	}

	held, err := unmoved()
	require.NoError(t, err)
	require.Truef(t, held,
		"an update reassigning comment %s's reply refs to post %s was APPLIED rather than refused. The "+
			"consumer must reject such an event whole (comment_consumer.go, \"threading references are "+
			"immutable\"): what lands otherwise is the content edit the reassignment was riding on, with "+
			"the refs quietly ignored — the comment keeps its place but silently takes on text written "+
			"for a different thread. Verified by mutation: disabling that check alone is enough to fail "+
			"this assertion",
		topURI, elsewhere.URI)
	p.Holds(t, "the hijacked comment to stay in its original thread with its original content", unmoved)

	// And the targeted thread never received it: a rejection that filed the
	// comment under the new post would be the worse half of the same bug.
	target, err := p.Thread(context.Background(), elsewhere.URI, nil)
	require.NoError(t, err)
	_, moved := target.find(topURI)
	require.Falsef(t, moved,
		"comment %s was re-filed under post %s, the thread the hijack named: %s",
		topURI, elsewhere.URI, target.uris())

	// ---- update ---------------------------------------------------------------
	// The second and last phase boundary: the update-and-delete phase ends in the
	// other Holds, and starts with the hijack phase's quota already spent.
	p.FreshReadQuota(t, "update-and-delete")

	// Threading refs unchanged this time, so the consumer's update path applies
	// it. Worth doing AFTER the hijack: it proves the refusal above was specific
	// to the reassignment rather than the consumer having stopped taking updates.
	author.PutRecord(t, commentCollection, topRKey, commentRecord(post, post, updated))

	node = observe(topURI, "the content-only edit to reach getComments",
		func(n threadNode) bool { return n.Comment.Record["content"] == updated })

	require.Equal(t, topURI, node.Comment.URI, "an update must edit the comment in place, not create a second one")
	require.NotEqual(t, topRecord.CID, node.Comment.CID, "the update must index the new commit's CID")
	require.Len(t, node.Replies, 1, "an edit must not disturb the replies hanging off the comment")
	require.Nil(t, node.Comment.Parent, "an edit must not turn a top-level comment into a reply")

	// ---- delete ---------------------------------------------------------------
	// DeleteExistingRecord rather than DeleteRecord: deleting a key that is not
	// there answers 200 and emits no commit, so a wrong rkey would turn into a
	// timeout blaming the firehose (testkit/pds.go).
	//
	// The comment being deleted is the one with a reply under it, deliberately:
	// the placeholder exists to keep that reply reachable, so deleting a leaf
	// would prove the easy half and skip the reason the behaviour exists.
	author.DeleteExistingRecord(t, commentCollection, topRKey)

	placeholder := func() (bool, error) {
		thread, err := p.Thread(context.Background(), post.URI, nil)
		if err != nil {
			return false, err
		}
		n, ok := thread.find(topURI)
		if !ok {
			return false, fmt.Errorf(
				"deleted comment %s was REMOVED from the thread rather than replaced by a placeholder, "+
					"which orphans the reply underneath it: %s", topURI, thread.uris())
		}
		return n.Comment.IsDeleted, nil
	}
	p.Await(t, "the deleted comment to become a placeholder in the thread", placeholder, withReadCadence())

	thread, err := p.Thread(context.Background(), post.URI, nil)
	require.NoError(t, err)
	node, ok := thread.find(topURI)
	require.True(t, ok)

	require.True(t, node.Comment.IsDeleted)
	require.Nil(t, node.Comment.Record,
		"a deleted comment must serve NO record — the placeholder exists to keep the thread's shape, "+
			"and serving the content back would defeat the deletion entirely")
	require.Equal(t, author.DID, node.Comment.Author.DID,
		"the placeholder keeps the commenter's DID, which is what lets a client render \"[deleted]\" "+
			"against the right slot")
	require.Empty(t, node.Comment.Author.Handle,
		"the placeholder must not carry the handle: it is the identifying half a reader would recognise")
	require.NotNil(t, node.Comment.DeletionReason)
	require.Equal(t, "author", *node.Comment.DeletionReason,
		"a delete commit in the author's own repo is an author deletion, not a moderator removal — "+
			"the two are rendered differently and only the consumer can tell them apart")
	require.NotNil(t, node.Comment.DeletedAt)
	require.Equal(t, commentStats{ReplyCount: 1}, node.Comment.Stats,
		"a deleted comment's votes are zeroed but its replyCount survives, because the replies do")

	require.Len(t, node.Replies, 1, "deleting a comment must not take its replies with it")
	survivor := node.Replies[0].Comment
	require.Equal(t, replyURI, survivor.URI)
	require.False(t, survivor.IsDeleted)
	require.Equal(t, "a reply that belongs under the first comment", survivor.Record["content"],
		"the surviving reply is served in FULL — the placeholder hides its parent's content, not its own")

	postView, err = p.Post(context.Background(), post.URI)
	require.NoError(t, err)
	require.Equal(t, 3, postView.Stats.CommentCount,
		"deleting a comment must NOT decrement the post's commentCount: the placeholder still occupies "+
			"a slot in the thread, and a count that disagreed with the rendered tree is the bug this "+
			"deliberate non-decrement avoids (comment_consumer.go deleteComment)")

	// Holds, because a replayed create carrying the pre-delete content is exactly
	// how a soft delete comes undone, and it looks correct at the moment it is
	// checked.
	p.Holds(t, "the deleted comment to stay a placeholder", func() (bool, error) {
		thread, err := p.Thread(context.Background(), post.URI, nil)
		if err != nil {
			return false, err
		}
		n, ok := thread.find(topURI)
		if !ok {
			return false, fmt.Errorf("the placeholder for %s disappeared from the thread: %s", topURI, thread.uris())
		}
		if !n.Comment.IsDeleted || n.Comment.Record != nil {
			return false, fmt.Errorf(
				"comment %s came back after being deleted (isDeleted=%v, record=%v) — a replayed create "+
					"resurrected it", topURI, n.Comment.IsDeleted, n.Comment.Record)
		}
		return true, nil
	})
}

// TestCommentAPIContract covers the client-facing surface of the comment
// endpoints as a third-party client meets it: what an unauthenticated caller
// gets from the write endpoints, and what any caller can read back about a
// thread that exists.
//
// It carries NO ingestion marker — markers are for pipeline proofs (§3.4a), and
// this asserts the client path.
//
// The authenticated half of all three write endpoints is proven at T1, for the
// reason §3.4b records and TestCommentAPIContract's siblings spell out: nothing
// but the browser OAuth callback mints a session RequireAuth accepts. For
// comments that half is internal/core/comments/comment_write_service_test.go
// (validation, ownership and the record the service writes, against a mock PDS),
// internal/core/comments/comment_write_test.go (the same against a REAL PDS,
// including the CID-swap conflict), and internal/api/handlers/comments (mapping and
// parameter validation). What this adds is the part none of them can see — that
// the shipped binary really routes these NSIDs, really guards them, and really
// serves an indexed thread back.
//
// Parameter-validation breadth deliberately does NOT live here (§3.4 rule 3): the
// depth/limit/sort/timeframe matrix is a handler concern and is covered against
// the real handler in internal/api/handlers/comments/get_comments_test.go. The
// two read assertions kept below are the ones that are only true of a running
// system — the identifier forms a client may use, and the not-found shapes every
// wait in this tier depends on being able to recognise.
func TestCommentAPIContract(t *testing.T) {
	p := newPipeline(t)

	author := p.IndexedAccount(t, "ca")
	community := indexedCommunity(t, p, "ca", author.DID)
	post := indexedPost(t, p, community, author.DID, "api contract thread")

	content := "api contract " + testkit.UniqueID(t)
	topRKey := testkit.TID()
	topURI := commentURI(author.DID, topRKey)
	topRecord := author.PutRecord(t, commentCollection, topRKey, commentRecord(post, post, content))

	replyRKey := testkit.TID()
	replyURI := commentURI(author.DID, replyRKey)
	author.PutRecord(t, commentCollection, replyRKey,
		commentRecord(post, strongRef{URI: topURI, CID: topRecord.CID}, "the subtree's only child"))

	p.Await(t, "both comments to be indexed before the client surface is exercised", func() (bool, error) {
		thread, err := p.Thread(context.Background(), post.URI, nil)
		if done, err := testkit.PendingIfNotFound(err); !done || err != nil {
			return done, err
		}
		_, hasReply := thread.find(replyURI)
		_, hasTop := thread.find(topURI)
		return hasTop && hasReply, nil
	}, withReadCadence())

	ctx, cancel := context.WithTimeout(context.Background(), contractBudget)
	defer cancel()

	t.Run("the write endpoints refuse an unauthenticated client", func(t *testing.T) {
		// One request each, no polling: this is the auth boundary of the shipped
		// router, and the answer does not become true later.
		//
		// All three comment NSIDs that RegisterCommentRoutes puts behind
		// RequireAuth are listed. Asserting it HERE rather than only in the
		// handler tests is the point: a handler test proves the handler refuses an
		// unauthenticated call, and structurally cannot see a route registered
		// without the middleware in front of it. Only a request to the running
		// router can.
		for _, endpoint := range []struct {
			nsid  string
			input map[string]any
		}{
			{"social.coves.community.comment.create", map[string]any{
				"reply": map[string]any{
					"root":   map[string]any{"uri": post.URI, "cid": post.CID},
					"parent": map[string]any{"uri": post.URI, "cid": post.CID},
				},
				"content": "nope"}},
			{"social.coves.community.comment.update", map[string]any{"uri": topURI, "content": "nope"}},
			{"social.coves.community.comment.delete", map[string]any{"uri": topURI}},
		} {
			err := p.AppView.Procedure(ctx, endpoint.nsid, endpoint.input, nil)
			require.Truef(t, testkit.IsStatus(err, http.StatusUnauthorized),
				"%s must answer 401 to a client with no session, answered: %v", endpoint.nsid, err)
		}
	})

	t.Run("a client reads the thread and its post in one request", func(t *testing.T) {
		// getComments hydrates the post alongside the comments, which is the whole
		// reason a permalink page is one request rather than two. A client that
		// found the comments but an empty post would render a thread with no
		// heading, so the join is worth pinning at this level.
		thread, err := p.Thread(ctx, post.URI, nil)
		require.NoError(t, err)

		require.Equal(t, post.URI, thread.Post.URI, "the thread must carry the post it belongs to")
		require.Equal(t, community.DID, thread.Post.Community.DID)
		require.Equal(t, author.DID, thread.Post.Author.DID)
		require.Equal(t, 2, thread.Post.Stats.CommentCount)

		node, ok := thread.find(topURI)
		require.Truef(t, ok, "the indexed comment was not in the thread: %s", thread.uris())
		require.Equal(t, content, node.Comment.Record["content"])
		require.Len(t, node.Replies, 1, "the default depth must be deep enough to include a direct reply")
	})

	t.Run("a subtree is addressable by the parent's rkey", func(t *testing.T) {
		// The permalink surface: parentRkey scopes the response to one comment and
		// its descendants, with the parent as the sole top-level entry. It is the
		// only way a client can link to a comment rather than to a thread.
		sub, err := p.Thread(ctx, post.URI, url.Values{"parentRkey": {topRKey}})
		require.NoError(t, err)

		require.Len(t, sub.Comments, 1, "a subtree request must answer with the parent alone at top level")
		require.Equal(t, topURI, sub.Comments[0].Comment.URI)
		require.Len(t, sub.Comments[0].Replies, 1, "the subtree must carry the parent's descendants")
		require.Equal(t, replyURI, sub.Comments[0].Replies[0].Comment.URI)
		require.Equal(t, post.URI, sub.Post.URI, "a subtree response still names the post it came from")
	})

	t.Run("the not-found shapes are distinguishable", func(t *testing.T) {
		// Every wait in this tier depends on telling "not indexed yet" from "the
		// route is gone" (testkit.IsNotFound insists on the XRPC shape), and this
		// endpoint has TWO not-founds a client must not confuse: the post is
		// missing, or the post is fine and the comment is not.
		unindexedPost := postURI(community.DID, testkit.TID())
		_, err := p.Thread(ctx, unindexedPost, nil)
		require.Truef(t, testkit.IsNotFound(err),
			"a thread request for a post nobody has indexed must be an XRPC not-found, got: %v", err)
		require.True(t, testkit.IsStatus(err, http.StatusNotFound))
		require.Contains(t, err.Error(), "RootNotFound",
			"the post-missing case must be RootNotFound, so a client can tell it from a missing comment")

		_, err = p.Thread(ctx, post.URI, url.Values{"parentRkey": {testkit.TID()}})
		require.True(t, testkit.IsStatus(err, http.StatusNotFound))
		require.Contains(t, err.Error(), "ParentNotFound",
			"a real post with an unknown parentRkey must be ParentNotFound — a client following a stale "+
				"permalink needs to know the THREAD still exists")
	})

	t.Run("a malformed post URI answers 500 (known defect)", func(t *testing.T) {
		// PINNED AS-IS, NOT ENDORSED. `post` is client-supplied and unvalidated
		// past "is it present": the handler checks only for emptiness, and the
		// service's own at:// check returns a bare errors.New that
		// comments.IsValidationError cannot recognise, so the mapper falls through
		// to 500 (internal/api/handlers/comments/errors.go's catch-alls match
		// sentinels, and this is not one).
		//
		// The correct answer is 400 InvalidRequest. Asserting that here would fail
		// this suite over a bug this task is not fixing, and asserting nothing
		// would let the fix land unnoticed — so the current behaviour is pinned
		// with a message that says what it should be. Filed as
		// 2026-07-29-getcomments-malformed-uri-returns-500.
		//
		// THIS IS THE PIN THAT FIRES ON FIX. Its sibling at T0
		// (TestGetComments_UnmappedValidationErrorIs500) drives a fake service
		// with a hand-built error, so it can only see the mapper half and stays
		// green whichever half is corrected. Only a real malformed URI through
		// the shipped binary — this — goes red.
		err := p.AppView.Query(ctx, "social.coves.community.comment.getComments",
			url.Values{"post": {"not-an-at-uri"}}, nil)
		require.Truef(t, testkit.IsStatus(err, http.StatusInternalServerError),
			"a malformed post URI answered %v; if this now says 400 InvalidRequest the defect is FIXED — "+
				"delete this subtest and fold the case into the not-found-shapes one above", err)
	})

	t.Run("the comment is served on its author's feed, by DID and by handle", func(t *testing.T) {
		// social.coves.actor.getComments is the other read surface a comment
		// reaches a client through, and it is a different query against different
		// joins — so an indexed comment can be visible in the thread and missing
		// here.
		//
		// Both identifier forms, because they take different paths through the
		// handler: a DID goes straight to the query, a handle is resolved first
		// (get_comments.go resolveActor). The handle case doubles as proof that
		// resolution is served from the AppView's own index — the stack is
		// egress-blocked, so a lookup that escaped to DNS could not have answered.
		for _, actor := range []string{author.DID, author.Handle} {
			var feed struct {
				Comments []commentView `json:"comments"`
			}
			require.NoErrorf(t, p.AppView.Query(ctx, "social.coves.actor.getComments",
				url.Values{"actor": {actor}, "limit": {"100"}}, &feed),
				"social.coves.actor.getComments rejected identifier %q", actor)

			var found bool
			seen := make([]string, 0, len(feed.Comments))
			for _, c := range feed.Comments {
				seen = append(seen, c.URI)
				if c.URI == topURI {
					found = true
					require.Equal(t, content, c.Record["content"])
					require.Equal(t, post.URI, c.Post.URI)
				}
			}
			require.Truef(t, found, "comment %s was not in the %d comments returned for actor %q: %s",
				topURI, len(feed.Comments), actor, strings.Join(seen, ", "))
		}
	})

	t.Run("an unknown actor's comment feed is empty rather than an error", func(t *testing.T) {
		// A DID is passed straight through with no existence check, so an unknown
		// one answers 200 with an empty feed — indistinguishable, by design, from a
		// real account that has not commented. Worth pinning because the natural
		// reading is wrong: a client cannot use this endpoint to ask whether an
		// actor exists.
		//
		// The DID is a literal rather than a generated one, at the full 24
		// characters of a real did:plc, for the reason
		// TestPostAPIContract spells out: the validator checks the base32 character
		// set, which UniqueID does not promise, and a shorter one would quietly
		// become a validation test the day the length check is added.
		var empty struct {
			Comments []commentView `json:"comments"`
		}
		require.NoError(t, p.AppView.Query(ctx, "social.coves.actor.getComments",
			url.Values{"actor": {"did:plc:aaaaaaaanevercommentedat"}}, &empty),
			"a well-formed DID nobody has indexed is an empty feed, not an error")
		assert.Empty(t, empty.Comments)
	})
}
