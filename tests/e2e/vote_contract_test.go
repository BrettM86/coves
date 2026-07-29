//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// The vote domain's pipeline contracts: the ingestion proof for
// social.coves.feed.vote, and the client-facing auth boundary.
//
// # VOTES LIVE IN THE VOTER'S REPO, AND THAT IS THE WHOLE SHAPE OF THE DOMAIN
//
// Unlike a post (which lives in the community's repo) or a comment (the
// author's), a vote's repo owner IS its subject-independent identity: the
// consumer takes the voter DID from the commit's repo and never reads one out of
// the record (vote_consumer.go createVote, "Vote comes from user's
// repository"). There is consequently no repo-ownership spoof to test here the
// way TestPostIngestion tests one — a repo cannot forge a vote as somebody else,
// because the repo is the identity.
//
// What that costs is the ordering guarantee the post contract leans on. A vote
// and its subject are necessarily in DIFFERENT repos, Jetstream parallelises
// across repos, and so "the vote arrives after the post" is topology luck rather
// than a protocol promise. The contract below does not pretend otherwise: it
// waits for the post to be INDEXED before writing a vote, and the out-of-order
// case is not an edge this contract avoids but the defect it pins (see
// TestVoteOutOfOrderIsLostAndSubtracts).
//
// # THERE IS NO RECONCILIATION PATH FOR VOTES (checked)
//
// Per the package doc's reconciliation hazard, the search for code that could
// satisfy a vote observation without the firehose:
//
//   - votes.voteService.CreateVote/DeleteVote write NOTHING to Postgres. They
//     resolve the subject, forward the record to the voter's PDS, and return —
//     the AppView learns about its own users' votes only from the firehose, the
//     same way it learns about a stranger's.
//   - The only writer of upvote_count/downvote_count/score reachable in a
//     running server is vote_consumer.go's transaction (plus the bridged_*
//     columns, which the aggregator path owns and which this contract does not
//     touch).
//   - votes.cache is a read-through cache of the PDS for VIEWER state, not a
//     count source, and nothing in this contract reads viewer state.
//
// So a single write → observe is honest here, with none of the arming the
// actor.profile contract needs.
//
// # WHAT "THE SAME VOTE AGAIN" MEANS, AND WHY IT IS TESTED THREE WAYS
//
// §3.4 rule 2's named invariant for this domain is that re-tapping must not
// double a count. "Re-tap" turns out to name three different code paths, keyed
// on three different things, and only asserting all three says the invariant
// holds:
//
//  1. THE SAME RECORD REWRITTEN AT THE SAME RKEY. This is an `update` commit,
//     and the vote consumer handles exactly `create` and `delete` — an update is
//     dropped on the floor by HandleEvent's switch. The count cannot double
//     because nothing runs at all. (That is also a defect for the flipped-
//     direction case; see the subtest that pins it.)
//  2. A DUPLICATE DELIVERY of one commit. Keyed on the record URI, twice over:
//     the rev gate (tryAdvanceRecordRev, equal rev loses) and
//     `INSERT … ON CONFLICT (uri) DO NOTHING RETURNING id`, whose no-rows result
//     skips the count update. Not reachable from this tier — manufacturing a
//     duplicate delivery is the reliability suite's job (§3.4c, task 16) — and
//     it is covered at T1 by
//     internal/atproto/jetstream/duplicate_delivery_test.go.
//  3. A SECOND VOTE RECORD UNDER A NEW RKEY. This is what a client actually
//     produces: votes.voteService deletes the old record and creates a new one
//     with a fresh TID, so the re-tap a user performs never reuses an rkey.
//     Keyed on (voter_did, subject_uri, deleted_at IS NULL) — the consumer's
//     "stale vote" cleanup soft-deletes the old row, decrements its direction,
//     then inserts and increments the new one. Net zero for a same-direction
//     re-tap, and a swing for a changed one.
//
// (1) and (3) are reachable from here and both are asserted below, each with a
// Holds, because "the count did not double" is a claim about a duplicate that
// has already been through the consumer — an eventually-check would pass while
// the second increment was still in flight.

// voteURI renders the AT-URI a vote record has once committed. The VOTER's DID
// is the authority — see the file's opening note.
func voteURI(voterDID, rkey string) string {
	return "at://" + voterDID + "/" + voteCollection + "/" + rkey
}

// awaitStats waits for a post's stats to satisfy accept, and returns them.
//
// Every observation in this contract is a stats read on
// social.coves.community.post.get, so the shape is worth naming once: the
// endpoint answers 200 with a notFoundPost member for an unindexed post, which
// is not what any wait here is waiting for — the post is indexed before the
// first vote is written — so a notFound is a hard error rather than "not yet".
func awaitStats(t *testing.T, p *pipeline, uri, description string, accept func(postStats) bool) postStats {
	t.Helper()
	var observed postStats
	p.Await(t, description, func() (bool, error) {
		view, err := p.Post(context.Background(), uri)
		if err != nil {
			return false, err
		}
		if view.NotFound {
			return false, errPostVanished(uri)
		}
		observed = view.Stats
		return accept(view.Stats), nil
	})
	return observed
}

// holdStats asserts a post's stats stay exactly want for contractHoldWindow.
//
// The vote domain's destructive-and-duplicate assertions are all of this shape:
// a count that must not move. Stated as an exact equality on the whole struct
// rather than on one field, because the failures worth catching here are
// cross-field — a decrement that lands on the wrong direction leaves the field
// under test correct and the score wrong.
func holdStats(t *testing.T, p *pipeline, uri, description string, want postStats) {
	t.Helper()
	p.Holds(t, description, func() (bool, error) {
		view, err := p.Post(context.Background(), uri)
		if err != nil {
			return false, err
		}
		if view.NotFound {
			return false, errPostVanished(uri)
		}
		return view.Stats == want, nil
	})
}

// errPostVanished is the terminal error both helpers above raise when the post
// they are measuring stops being served. A vote cannot delete its subject, so
// this means something outside the contract removed the post — and retrying
// would turn that into an opaque timeout about vote counts.
func errPostVanished(uri string) error {
	return &voteSubjectGoneError{uri: uri}
}

type voteSubjectGoneError struct{ uri string }

func (e *voteSubjectGoneError) Error() string {
	return "the post being voted on (" + e.uri + ") stopped being served by " +
		"social.coves.community.post.get part-way through the contract: a vote cannot " +
		"delete its subject, so the post was removed by something outside this test"
}

// TestVoteIngestion is the pipeline proof for votes.
//
// coves:ingestion-contract social.coves.feed.vote
//
// Every record is written straight into the voter's own repo with the voter's
// session, and every observation is a stats read on
// social.coves.community.post.get — the vote itself has no serving endpoint of
// its own that an unauthenticated caller can reach, so the denormalised counts
// the consumer maintains ARE the observable:
//
//	create           → upvotes 1, score 1
//	same-rkey re-put → the count does not move, and STAYS put (Holds)
//	new-rkey re-tap  → still exactly one vote's worth, and STAYS (Holds)
//	direction change → the up is withdrawn as the down lands (0/1/-1)
//	delete           → back to zero, and STAYS zero (Holds, §3.4a)
func TestVoteIngestion(t *testing.T) {
	p := newPipeline(t)

	author := p.IndexedAccount(t, "vi")
	voter := p.IndexedAccount(t, "vv")
	community := indexedCommunity(t, p, "vi", author.DID)
	post := indexedPost(t, p, community, author.DID, "vote target "+testkit.UniqueID(t))

	// The voter is deliberately NOT the author: a self-vote and a stranger's
	// vote take the same path (the consumer has no self-vote rule), but using
	// two identities keeps the fixture honest about which repo the record is in.
	require.NotEqual(t, author.DID, voter.DID)

	// ---- create -------------------------------------------------------------
	first := testkit.TID()
	record := voter.PutRecord(t, voteCollection, first, voteRecord(post, "up"))
	require.Equal(t, voteURI(voter.DID, first), record.URI,
		"the PDS committed the vote under a different URI than the voter's repo and rkey imply, "+
			"which would make every assertion below measure a record this test did not write")

	stats := awaitStats(t, p, post.URI, "the directly-written upvote to reach the post's stats via the consumers",
		func(s postStats) bool { return s.Upvotes == 1 })
	require.Equal(t, postStats{Upvotes: 1, Score: 1}, stats,
		"an upvote raises upvotes and score by one and touches nothing else")

	// ---- PINNED DEFECT: a same-rkey update is silently dropped ---------------
	//
	// Rewriting the record at the SAME rkey is an `update` commit, and
	// HandleEvent switches on create and delete only — so an update is
	// discarded before any vote logic runs, whatever it says.
	//
	// For a BYTE-IDENTICAL rewrite that is the right outcome reached for the
	// wrong reason, and it is not worth a step of its own: a first draft spent a
	// full hold window on it, and review pointed out the assertion could not
	// fail. Identical bytes produce an identical CID, so the PDS may emit no
	// commit at all; if it does, the rev gate and `ON CONFLICT (uri) DO NOTHING`
	// would each independently stop the count moving even if the switch did
	// route updates. Three redundant guards and a coin-flip on whether an event
	// is even emitted — a green result there said nothing.
	//
	// The FLIP is the same code path with a visible consequence, so it is the
	// one worth holding. A third-party atProto client changing its vote in place
	// — the obvious way to express "I changed my mind", and what
	// com.atproto.repo.putRecord is FOR — leaves the record on the PDS saying
	// "down" and the AppView saying "up", permanently: nothing ever revisits it.
	// Coves' own client never hits this (votes.voteService always deletes and
	// re-creates under a fresh rkey, path 3 in the file's opening note), so the
	// bug is invisible from inside the product and appears the moment a
	// federated peer or a third-party client votes.
	//
	// PINNED, not asserted-as-intended: the suite documents the shipped
	// behaviour rather than failing over a bug this task is not fixing. When it
	// is fixed, this block inverts to expect {Downvotes: 1, Score: -1}, and the
	// Holds is what fails first — loudly, and naming the issue — which is the
	// intent.
	voter.PutRecord(t, voteCollection, first, voteRecord(post, "down"))
	holdStats(t, p, post.URI,
		"a same-rkey direction flip to STILL be ignored (pinning known defect "+
			"2026-07-29-vote-putrecord-update-silently-ignored: the vote consumer handles create "+
			"and delete only, so an update commit never reaches it. IF THIS FAILED, the defect is "+
			"FIXED — invert this step to expect {Downvotes:1, Score:-1} and close the issue)",
		postStats{Upvotes: 1, Score: 1})

	// ---- a second vote record, new rkey, same direction ----------------------
	// The shape a real re-tap produces. The consumer's stale-vote cleanup keys
	// on (voter_did, subject_uri) rather than on the URI: it soft-deletes the
	// first vote, decrements for it, then inserts the second and increments.
	// Net zero, which is only observable as "the count did not become 2".
	second := testkit.TID()
	voter.PutRecord(t, voteCollection, second, voteRecord(post, "up"))
	holdStats(t, p, post.URI,
		"the count to stay at one upvote after the same voter votes again under a new rkey "+
			"(the consumer supersedes the old vote instead of adding a second)",
		postStats{Upvotes: 1, Score: 1})

	// ---- direction change ----------------------------------------------------
	// Same mechanism, opposite outcome: the superseded upvote is withdrawn and
	// the downvote lands, so the swing is two points of score in one event.
	third := testkit.TID()
	voter.PutRecord(t, voteCollection, third, voteRecord(post, "down"))
	stats = awaitStats(t, p, post.URI, "the voter's change of mind to swing the post's score",
		func(s postStats) bool { return s.Downvotes == 1 })
	require.Equal(t, postStats{Downvotes: 1, Score: -1}, stats,
		"changing direction must withdraw the upvote in the same transaction that records the "+
			"downvote — a stale upvote left behind shows up here as upvotes 1, score 0")

	// ---- delete --------------------------------------------------------------
	// DeleteExistingRecord, not DeleteRecord: deleting an absent rkey answers
	// 200 and emits no commit, so a wrong rkey would become a timeout blaming
	// the firehose (testkit/pds.go).
	voter.DeleteExistingRecord(t, voteCollection, third)
	stats = awaitStats(t, p, post.URI, "the withdrawn vote to leave the post's stats",
		func(s postStats) bool { return s.Downvotes == 0 })
	require.Equal(t, postStats{}, stats)
	holdStats(t, p, post.URI, "the withdrawn vote to stay withdrawn", postStats{})

	// ---- deleting an already-superseded vote is a no-op ----------------------
	// `second` was soft-deleted by the stale-vote cleanup when `third` arrived,
	// and its decrement was applied then. Its own delete commit must not
	// decrement a second time. The floor (GREATEST(0, …)) would hide a double
	// decrement from zero, so this is asserted where it can be seen: with a live
	// vote from ANOTHER voter present, so a spurious decrement has something to
	// take away.
	other := p.IndexedAccount(t, "vo")
	otherRKey := testkit.TID()
	other.PutRecord(t, voteCollection, otherRKey, voteRecord(post, "up"))
	awaitStats(t, p, post.URI, "a second voter's upvote, so a spurious decrement is visible",
		func(s postStats) bool { return s.Upvotes == 1 })

	voter.DeleteExistingRecord(t, voteCollection, second)
	holdStats(t, p, post.URI,
		"deleting an already-superseded vote to leave the other voter's upvote alone "+
			"(a second decrement for a vote whose count was already withdrawn)",
		postStats{Upvotes: 1, Score: 1})
}

// TestVoteOutOfOrderIsLostAndSubtracts PINS A DEFECT. It is not an aspiration
// and it is not a contract — it carries no ingestion marker — it is the
// executable record of what the shipped pipeline does when a vote reaches the
// AppView before the thing it votes on.
//
// # WHY THIS IS REACHABLE IN PRODUCTION
//
// A vote lives in the voter's repo and its subject lives in another (a post is
// in the community's repo, a comment in its author's). Jetstream serialises a
// single repo's commits and PARALLELISES ACROSS REPOS, so nothing orders a vote
// against its subject. The window is small in a healthy stack and arbitrarily
// large in an unhealthy one — a redriven post, a consumer catching up after a
// restart, a slow blob fetch upstream — and Phase 5's relay topology widens it
// further.
//
// # WHAT THE CONSUMER DOES, AND WHY IT LOSES
//
// vote_consumer.go's createVote has NO must-exist gate on the subject: unlike
// the post consumer (which rejects a post whose community it has not seen, with
// a TRANSIENT error so the redrive succeeds later) and unlike the subscription
// consumer (same pattern), a vote whose subject is unknown is accepted. The row
// is inserted; the `UPDATE posts SET upvote_count = …` that follows matches
// zero rows; and the zero-row case is a log line:
//
//	log.Printf("Warning: Vote subject not found or deleted: %s (vote indexed anyway)", …)
//
// Nothing reconciles afterwards. The post consumer, when the post finally
// arrives, INSERTs it with fresh zeroed counters and never looks at the votes
// table. So the vote is counted by nobody, forever.
//
// # AND IT IS WORSE THAN A LOST VOTE
//
// The row that was inserted is a live, undeleted vote. When it is eventually
// withdrawn — the user un-taps, or the record is tidied up — deleteVote loads
// that row, finds a direction and a subject, and DECREMENTS the post. It
// subtracts a vote it never added, taking a real vote from a real voter with it.
// The steady-state error is therefore not "one vote short", it is unbounded
// downward drift, floored at zero by GREATEST(0, …) so that it never even looks
// wrong in the data.
//
// Both halves are asserted below, because the second is the one that turns a
// missing-increment annoyance into a correctness bug, and it is the one a
// reader would not predict.
func TestVoteOutOfOrderIsLostAndSubtracts(t *testing.T) {
	p := newPipeline(t)

	author := p.IndexedAccount(t, "vd")
	early := p.IndexedAccount(t, "ve")
	late := p.IndexedAccount(t, "vl")
	community := indexedCommunity(t, p, "vd", author.DID)

	// The post's URI is knowable before the post exists — the community's DID
	// and an rkey this test chooses are all it is made of — which is exactly why
	// a vote can name a subject that has not been indexed.
	rkey := testkit.TID()
	uri := postURI(community.DID, rkey)

	// ---- the vote, before its subject ---------------------------------------
	// The CID is a well-formed placeholder rather than the post's real one,
	// which the test cannot know yet. The consumer stores subject_cid without
	// checking it against anything (there is no CID validation on the vote path
	// at all — worth knowing, and not this test's subject).
	orphanRKey := testkit.TID()
	early.PutRecord(t, voteCollection, orphanRKey, voteRecord(
		strongRef{URI: uri, CID: "bafyreiaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, "up"))

	// Bounded INTRA-REPO, the way §3.4's negatives have to be: a second record
	// in the SAME repo, whose arrival proves the first has already been through
	// the consumer. Cross-repo ordering is the very thing this test is about, so
	// it cannot also be the thing the test assumes.
	//
	// The bounding record is a vote on a DIFFERENT, already-indexed post, so its
	// effect is observable — an unobservable bound is not a bound.
	boundPost := indexedPost(t, p, community, author.DID, "ordering bound "+testkit.UniqueID(t))
	early.PutRecord(t, voteCollection, testkit.TID(), voteRecord(boundPost, "up"))
	awaitStats(t, p, boundPost.URI,
		"a later vote from the SAME repo to be indexed, proving the orphan vote's commit "+
			"has already passed through the vote consumer",
		func(s postStats) bool { return s.Upvotes == 1 })

	// ---- now the subject arrives --------------------------------------------
	community.PutRecord(t, postCollection, rkey,
		postRecord(community.DID, author.DID, "voted on before it existed", "the late post"))
	p.Await(t, "the post to be indexed after the vote that names it", func() (bool, error) {
		view, err := p.Post(context.Background(), uri)
		if err != nil {
			return false, err
		}
		return !view.NotFound, nil
	})

	// ---- half one: the vote is lost, and stays lost -------------------------
	// Holds rather than a single read, because "no reconciliation happens" is a
	// claim about the future: a repair pass that ran a second later would make
	// this the wrong assertion, and Holds is what would notice.
	holdStats(t, p, uri,
		"the out-of-order vote to be INVISIBLE on the post it names, and to stay invisible "+
			"(pinning known defect 2026-07-29-vote-before-subject-lost-then-subtracts: createVote "+
			"has no must-exist gate on its subject, its count UPDATE matches zero rows, and nothing "+
			"recomputes counts afterwards. IF THIS FAILED, the defect is FIXED — invert this step "+
			"to expect {Upvotes:1, Score:1} and close the issue)",
		postStats{})

	// ---- half two: the lost vote can still subtract -------------------------
	// A real, correctly-ordered vote from another actor takes the count to one.
	view, err := p.Post(context.Background(), uri)
	require.NoError(t, err)
	late.PutRecord(t, voteCollection, testkit.TID(), voteRecord(strongRef{URI: uri, CID: view.CID}, "up"))
	awaitStats(t, p, uri, "a correctly-ordered vote to be counted normally",
		func(s postStats) bool { return s.Upvotes == 1 })

	// Withdrawing the ORPHAN — which never incremented anything — takes the
	// other voter's upvote away with it.
	early.DeleteExistingRecord(t, voteCollection, orphanRKey)
	stats := awaitStats(t, p,
		uri, "withdrawing the never-counted vote to DECREMENT the post anyway",
		func(s postStats) bool { return s.Upvotes == 0 })
	require.Equal(t, postStats{}, stats,
		"the orphaned vote's delete subtracted a vote it never added, and the count it took "+
			"belonged to a different voter whose vote is still live in the votes table")

	// AND IT STAYS WRONG. The Await above would be satisfied by a fix that
	// repairs the count ASYNCHRONOUSLY — a reconciliation pass, a recount
	// triggered by the delete — because it only has to observe zero once, on its
	// way back up. This pin would then keep passing against code that no longer
	// has the defect, which is the specific way a pin rots: it stops being a
	// record of current behaviour and becomes a test of nothing.
	//
	// Holding the wrong-but-current value is what closes that. When the defect
	// is fixed by any means, sync or async, THIS is the assertion that fails.
	holdStats(t, p, uri,
		"the stolen upvote to STAY stolen (pinning known defect "+
			"2026-07-29-vote-before-subject-lost-then-subtracts, second half: deleteVote decrements "+
			"unconditionally from the stored row and cannot tell an applied increment from a missed "+
			"one. IF THIS FAILED, the defect is FIXED — the surviving voter's upvote should read "+
			"{Upvotes:1, Score:1}; invert this step and close the issue)",
		postStats{})
}

// TestVoteAPIContract covers the client-facing surface of the vote endpoints as
// a third-party client meets it. It carries NO ingestion marker — markers are
// for pipeline proofs (§3.4a) — and it is short, because votes have no public
// read endpoint of their own.
//
// A vote is only ever visible to an unauthenticated client as somebody else's
// count, which the ingestion contract above already reads through
// social.coves.community.post.get. Viewer state (did I vote, and which way) is
// the part a client asks about by identity, and it is behind OptionalAuth with
// no credential this tier can mint (§3.4b) — covered at T1 instead, in
// tests/integration/comment_vote_test.go for comments and
// internal/db/postgres/vote_repo_test.go for posts.
//
// So what is left, and what nothing else can see, is the auth boundary of the
// shipped router.
func TestVoteAPIContract(t *testing.T) {
	p := newPipeline(t)

	author := p.IndexedAccount(t, "va")
	community := indexedCommunity(t, p, "va", author.DID)
	post := indexedPost(t, p, community, author.DID, "api contract "+testkit.UniqueID(t))

	ctx, cancel := context.WithTimeout(context.Background(), contractBudget)
	defer cancel()

	t.Run("the write endpoints refuse an unauthenticated client", func(t *testing.T) {
		// One request each, no polling: this is the auth boundary of the shipped
		// router and the answer does not become true later.
		//
		// Both NSIDs RegisterVoteRoutes puts behind RequireAuth are listed.
		// Asserting it HERE rather than only in the handler tests is the point: a
		// handler test proves the handler refuses an unauthenticated call and
		// structurally cannot see a route registered without the middleware in
		// front of it. Only a request to the running router can — and for votes
		// that gap is the difference between a rate-limited, authenticated write
		// and an open ballot box.
		for _, endpoint := range []struct {
			nsid  string
			input map[string]any
		}{
			{"social.coves.feed.vote.create", map[string]any{
				"subject":   map[string]any{"uri": post.URI, "cid": post.CID},
				"direction": "up",
			}},
			{"social.coves.feed.vote.delete", map[string]any{"subject": post.URI}},
		} {
			err := p.AppView.Procedure(ctx, endpoint.nsid, endpoint.input, nil)
			require.Truef(t, testkit.IsStatus(err, http.StatusUnauthorized),
				"%s must answer 401 to a client with no session, answered: %v", endpoint.nsid, err)
		}
	})

	t.Run("a vote's effect is public even though the vote is not", func(t *testing.T) {
		// The complement of the auth boundary: an anonymous client cannot cast a
		// vote and cannot ask whether it voted, but it must still see the totals
		// — that is what a score on a feed is. Read with no credential at all.
		voter := p.IndexedAccount(t, "vp")
		voter.PutRecord(t, voteCollection, testkit.TID(), voteRecord(post, "down"))

		stats := awaitStats(t, p, post.URI, "an anonymous client to see the vote totals",
			func(s postStats) bool { return s.Downvotes == 1 })
		require.Equal(t, postStats{Downvotes: 1, Score: -1}, stats)

		// The same totals through the community feed, which is a different query
		// over different joins: a count correct on post.get and wrong here is a
		// hydration bug neither endpoint's own test would show.
		require.Equal(t, stats, feedStats(t, p, community.DID, post.URI),
			"the community feed disagreed with post.get about the same post's vote totals")
	})
}

// feedStats reads one post's stats out of the community feed, which is the
// other public surface a vote total reaches a client through.
func feedStats(t *testing.T, p *pipeline, communityDID, postURI string) postStats {
	t.Helper()
	var feed struct {
		Feed []struct {
			Post postView `json:"post"`
		} `json:"feed"`
	}
	ctx, cancel := context.WithTimeout(context.Background(), contractBudget)
	defer cancel()
	require.NoError(t, p.AppView.Query(ctx, "social.coves.communityFeed.getCommunity",
		url.Values{"community": {communityDID}, "sort": {"new"}, "limit": {"25"}}, &feed))

	for _, item := range feed.Feed {
		if item.Post.URI == postURI {
			return item.Post.Stats
		}
	}
	t.Fatalf("the post %s was not in community %s's %d newest posts, so its feed stats could "+
		"not be compared with post.get's", postURI, communityDID, len(feed.Feed))
	return postStats{}
}
