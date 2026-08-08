//go:build e2e

package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"net/url"
	"strings"
	"testing"
	"time"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The author-owned post pipeline: the three collections that replace the
// community-repo post record (docs/PRD_AUTHOR_OWNED_POSTS.md §3, §5).
//
// # THE SHAPE IS INVERTED FROM post_contract_test.go, AND THAT IS THE POINT
//
// The deprecated social.coves.community.post lives in the COMMUNITY's repo and
// names its author in a field. Everything here is the other way round:
//
//   - social.coves.community.postv2 lives in the AUTHOR's repo and has NO author
//     field. The repo the commit arrived in IS the author, so the old contract's
//     central proof — a repo forging a post for another community — has no
//     analogue; what replaces it is that the community field is a CLAIM, and a
//     post making it is not visible in that community until the community says
//     otherwise.
//   - social.coves.community.acceptance and .removal live in the COMMUNITY's
//     repo and are the community saying otherwise. They are written here by the
//     test, holding the community's own session, because that is exactly what
//     the production engine does with the community's credentials — and no
//     community in this tier HAS credentials in the AppView (post_admission_
//     contract_test.go's standing ceiling), so the engine cannot be driven from
//     out here. Writing the records directly is not a shortcut around the
//     engine; it is the only way to feed the consumers the events a credentialed
//     production engine would emit.
//
// # THE OBSERVATION SURFACE IS getStatus, NOT post.get
//
// PRD rev 2.7 pulled social.coves.community.post.getStatus forward into this
// task precisely so these contracts have something to watch. post.get is
// status-agnostic until task 7 rebuilds the read paths behind the centralized
// visibility predicate (§6.2), so asserting "a pending post is invisible" here
// would be asserting a behaviour that does not exist yet and is not this task's
// to build. What IS true today is asserted; what task 7 owes is named where it
// would otherwise look like a gap.
//
// # THE POSITIVE FETCH ARC IS STRUCTURALLY T1-ONLY
//
// §5.4's direct fetch converges an acceptance whose subject the AppView has
// never indexed. That precondition cannot be staged in this tier, and the reason
// is the tier working as designed rather than a gap in it: every PDS write here
// reaches the AppView through Jetstream, so a postv2 record written to set up
// the arc IS delivered, and by the time the acceptance is written the subject is
// indexed — which is the ORDINARY path, not the fetch. Suppressing that delivery
// would mean either not writing the record (nothing to fetch) or reconfiguring
// the stack's consumers (rule 2 of this package: never instantiate a consumer).
//
// So what this file proves about acceptance-before-post is the NEGATIVE, which
// is stageable and is asserted below: an acceptance naming a post that exists
// nowhere admits nothing and keeps admitting nothing. The positive — fetch,
// recompute the CID from the repo's own blocks, index, accept — is proven at T1
// against a real repo on the test PDS, in
// internal/atproto/jetstream/direct_fetch_verification_test.go (the component)
// and acceptance_consumer_test.go (the consumer wiring). That split is
// deliberate and permanent; do not read the absence here as missing coverage.
//
// # THE rkey IS COMPUTED HERE RATHER THAN IMPORTED
//
// An acceptance's record key is the unpadded lowercase base32 encoding of the
// SHA-256 digest of the subject AT-URI (§3.2), and posts.SubjectRkey is the
// production implementation. This file re-derives it from stdlib instead of
// importing that package, for two reasons that point the same way: no test in
// this tier imports internal/core/... at all, and — more usefully — a test that
// called the production helper could not detect a bug in it, because both sides
// of the comparison would move together. Re-deriving makes these contracts an
// independent check of the derivation, so a change to either fails here.

const (
	postV2Collection      = "social.coves.community.postv2"
	acceptanceCollection  = "social.coves.community.acceptance"
	removalCollection     = "social.coves.community.removal"
	postGetStatusEndpoint = "social.coves.community.post.getStatus"
)

// subjectRkeyEncoding mirrors posts.SubjectRkey's encoding: RFC 4648 base32 with
// the padding dropped, because '=' is outside the atProto record-key charset.
var subjectRkeyEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// subjectRkey is the record key a community's acceptance and removal for one
// post share. See this file's header for why it is not posts.SubjectRkey.
func subjectRkey(postURI string) string {
	digest := sha256.Sum256([]byte(postURI))
	return strings.ToLower(subjectRkeyEncoding.EncodeToString(digest[:]))
}

// postStatusView is the slice of getStatus's response the contracts observe.
type postStatusView struct {
	Status        string `json:"status"`
	DecisionCode  string `json:"decisionCode"`
	DecisionAt    string `json:"decisionAt"`
	AcceptanceURI string `json:"acceptanceUri"`
}

// PostStatus asks the community host what it decided about a post.
//
// A subject the community has never seen is a not-found StatusError, which
// testkit.PendingIfNotFound turns into "no decision yet" inside a probe — the
// same shape every other read in this tier uses.
func (p *pipeline) PostStatus(ctx context.Context, postURI, communityDID string) (postStatusView, error) {
	var view postStatusView
	err := p.AppView.Query(ctx, postGetStatusEndpoint, url.Values{
		"post":      {postURI},
		"community": {communityDID},
	}, &view)
	return view, err
}

// authorPostURI renders the AT-URI a postv2 record has once committed: the
// AUTHOR's DID is the authority, which is the whole shape of this domain.
func authorPostURI(authorDID, rkey string) string {
	return "at://" + authorDID + "/" + postV2Collection + "/" + rkey
}

// postV2Record builds a social.coves.community.postv2 record. There is no
// author field, by construction — the lexicon has none (§3.1), and a consumer
// that still read one would be reading a field only a forger would send.
func postV2Record(communityDID, title, content string) map[string]any {
	return map[string]any{
		"$type":     postV2Collection,
		"community": communityDID,
		"title":     title,
		"content":   content,
		"createdAt": time.Now().UTC().Format(time.RFC3339),
	}
}

// acceptanceRecord builds the community's attestation, pinning the exact
// version it accepted.
func acceptanceRecord(postURI, postCID string) map[string]any {
	return map[string]any{
		"$type":     acceptanceCollection,
		"subject":   map[string]any{"uri": postURI, "cid": postCID},
		"createdAt": time.Now().UTC().Format(time.RFC3339),
	}
}

// removalRecord builds the community's record that a post has been removed.
func removalRecord(postURI, postCID, code string) map[string]any {
	return map[string]any{
		"$type":     removalCollection,
		"subject":   map[string]any{"uri": postURI, "cid": postCID},
		"code":      code,
		"createdAt": time.Now().UTC().Format(time.RFC3339),
	}
}

// awaitStatus waits for getStatus to report want, and returns the view it saw.
func awaitStatus(t *testing.T, p *pipeline, postURI, communityDID, want, description string) postStatusView {
	t.Helper()

	var observed postStatusView
	p.Await(t, description, func() (bool, error) {
		view, err := p.PostStatus(context.Background(), postURI, communityDID)
		// NOT testkit.PendingIfNotFound: it is for probes where a successful read
		// IS the answer, so its nil case reports DONE — which here would end the
		// wait before the status was compared. This wait needs the opposite: a
		// successful read is where the question starts.
		if err != nil {
			if testkit.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		observed = view
		return view.Status == want, nil
	})
	return observed
}

// statusAbsent reports whether the community has no decision at all about a
// post — a 404 from getStatus, which is different from every status value.
func statusAbsent(p *pipeline, postURI, communityDID string) func() (bool, error) {
	return func() (bool, error) {
		_, err := p.PostStatus(context.Background(), postURI, communityDID)
		if err == nil {
			return false, nil
		}
		if testkit.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
}

// TestAuthorPostIngestion is the pipeline proof for the author-repo post record,
// and the outer frame the other two contracts hang off.
//
// coves:ingestion-contract social.coves.community.postv2
//
//	create  → the post is indexed, attributed to the REPO it came from, and the
//	          community it names holds it as `pending`
//	retarget→ an update changing `community` is discarded WHOLE: the new
//	          community never sees it and the old one's decision is untouched
//	delete  → the post stops being served, and STAYS gone (Holds, §3.4a)
//
// # WHY `pending` IS THE INTERESTING ASSERTION
//
// Under the old model an indexed post was a visible post. Here a post claiming a
// community is never shown in it until an acceptance exists (§2), so the state
// this contract has to prove is the one that did not exist before: indexed,
// attributed, and NOT yet admitted. A consumer that opened the row as anything
// other than pending would publish speech the community never agreed to carry —
// and getStatus is the only surface that can tell the difference, because
// post.get is status-agnostic until task 7.
func TestAuthorPostIngestion(t *testing.T) {
	p := newPipeline(t)

	author := p.IndexedAccount(t, "av")
	community := indexedCommunity(t, p, "av", author.DID)
	elsewhere := indexedCommunity(t, p, "ae", author.DID)

	title := "author-owned " + testkit.UniqueID(t)
	rkey := testkit.TID()
	uri := authorPostURI(author.DID, rkey)

	// Written into the AUTHOR's own repo, with the author's own session. That is
	// the entire point of the flip: the repo signature is the authorship anchor,
	// so nobody but this account can produce this record.
	record := author.PutRecord(t, postV2Collection, rkey,
		postV2Record(community.DID, title, "words the author is accountable for"))

	view := awaitStatus(t, p, uri, community.DID, "pending",
		"the author's post to reach the community's admission state via the consumers")

	assert.Empty(t, view.AcceptanceURI, "a pending post has no acceptance record to point at")
	assert.Empty(t, view.DecisionCode, "a pending post has been refused by nobody")

	// The post itself is served, attributed to the repo it arrived in. post.get
	// is status-agnostic today — task 7 owes the centralized visibility
	// predicate that makes a pending post invisible to non-authors (§6.2) — so
	// what is asserted here is what IS true: the record was indexed, and its
	// author is the DID that signed the commit rather than a field somebody
	// could have written.
	served, err := p.Post(context.Background(), uri)
	require.NoError(t, err)
	require.Falsef(t, served.NotFound, "the indexed post must be served by post.get: %+v", served)
	assert.Equalf(t, author.DID, served.Author.DID,
		"authorship must come from the repo the commit arrived in; the postv2 record carries no author field at all, so a different DID here means one was invented")
	assert.Equal(t, community.DID, served.Community.DID)
	assert.Equal(t, record.CID, served.CID, "the indexed CID must be the commit's")
	assert.Equal(t, title, served.Record["title"])
	assert.Nilf(t, served.Record["author"],
		"the record must not carry an author field: it is the field whose removal makes authorship unforgeable (§3.1)")

	// ---- retarget: the whole event is invalid ------------------------------
	// §3.1 is explicit — a consumer must DISCARD an update that changes
	// `community`, not merely retain the old value. Retargeting a post means
	// writing a new record, and applying the content half while ignoring the
	// community half would leave the original community's admission holding a
	// CID it never evaluated.
	retargeted := "retargeted " + testkit.UniqueID(t)
	author.PutRecord(t, postV2Collection, rkey,
		postV2Record(elsewhere.DID, retargeted, "aimed at a community that never received this post"))

	// Bounded by a later event in the SAME repo, the way TestPostIngestion
	// bounds its spoof: a second post by this author, committed after the
	// retarget, cannot overtake it — the PDS sequencer orders a repo's own
	// commits and Jetstream serializes per repo. When the bystander is visible,
	// the retarget has already been through the consumer.
	bystander := testkit.TID()
	bystanderURI := authorPostURI(author.DID, bystander)
	author.PutRecord(t, postV2Collection, bystander,
		postV2Record(community.DID, "bystander "+testkit.UniqueID(t), "committed after the retarget"))
	awaitStatus(t, p, bystanderURI, community.DID, "pending",
		"a later post in the same repo, which bounds the retarget's rejection")

	absentElsewhere := statusAbsent(p, uri, elsewhere.DID)
	absent, err := absentElsewhere()
	require.NoError(t, err)
	require.Truef(t, absent,
		"the retargeted update opened an admission in %s. A post's community is immutable across updates (§3.1); "+
			"honouring the change lets an author move a post into any community by editing it, with no acceptance and no moderator involved",
		elsewhere.DID)
	p.Holds(t, "the retargeted community to stay undecided", absentElsewhere)

	original, err := p.PostStatus(context.Background(), uri, community.DID)
	require.NoError(t, err)
	assert.Equal(t, "pending", original.Status,
		"the original community's decision must be untouched by an invalid update")

	unchanged, err := p.Post(context.Background(), uri)
	require.NoError(t, err)
	assert.Equalf(t, title, unchanged.Record["title"],
		"the CONTENT of a discarded event must be discarded with it: applying the new title while refusing the new community would leave the community holding a CID it never judged")
	assert.NotEqual(t, retargeted, unchanged.Record["title"])

	// ---- delete -------------------------------------------------------------
	author.DeleteExistingRecord(t, postV2Collection, rkey)

	gone := func() (bool, error) {
		v, err := p.Post(context.Background(), uri)
		if err != nil {
			return false, err
		}
		return v.NotFound, nil
	}
	p.Await(t, "the author's deleted post to stop being served", gone)
	p.Holds(t, "the deleted post to stay deleted", gone)
}

// TestCommunityAcceptanceIngestion is the pipeline proof for the community's
// attestation record.
//
// coves:ingestion-contract social.coves.community.acceptance
//
//	accept        → the pending post becomes `accepted` and names the acceptance
//	edit          → the standing acceptance no longer covers the content, and the
//	                post falls back to `pending_reacceptance`
//	unaccept      → deleting the acceptance returns the post to `pending`
//	unknown subject → an acceptance naming a post that does not exist admits
//	                  nothing, and keeps admitting nothing (Holds)
//
// # THE CID IS THE SUBSTANCE OF THE ATTESTATION
//
// An acceptance is a strongRef, and agreeing to at://x/postv2/y is not agreeing
// to whatever that URI holds tomorrow. The edit step is the whole reason the
// pinned CID exists: if the AppView rendered new content under an old
// acceptance, an author could get anything approved and then swap it, which is
// the single most valuable attack against a moderation system built on pointers.
func TestCommunityAcceptanceIngestion(t *testing.T) {
	p := newPipeline(t)

	author := p.IndexedAccount(t, "cv")
	community := indexedCommunity(t, p, "cv", author.DID)

	rkey := testkit.TID()
	uri := authorPostURI(author.DID, rkey)
	record := author.PutRecord(t, postV2Collection, rkey,
		postV2Record(community.DID, "accept me "+testkit.UniqueID(t), "content the community will attest to"))

	awaitStatus(t, p, uri, community.DID, "pending", "the post to be indexed and awaiting a decision")

	// The community attests, from its own repo, at the deterministic rkey. One
	// post has exactly one acceptance rkey per community, forever — which is
	// what makes the three independent production writers (§3.2) converge on
	// putRecord of the same record instead of minting duplicates.
	acceptRkey := subjectRkey(uri)
	community.PutRecord(t, acceptanceCollection, acceptRkey, acceptanceRecord(uri, record.CID))

	accepted := awaitStatus(t, p, uri, community.DID, "accepted",
		"the community's acceptance to admit the post")
	assert.Equalf(t, "at://"+community.DID+"/"+acceptanceCollection+"/"+acceptRkey, accepted.AcceptanceURI,
		"the reported acceptance URI must resolve to the record that actually stands, or a client following it to verify the attestation gets nothing")
	assert.Empty(t, accepted.DecisionCode, "an acceptance is not a refusal")

	// ---- the edit that un-accepts ------------------------------------------
	edited := author.PutRecord(t, postV2Collection, rkey,
		postV2Record(community.DID, "edited after acceptance "+testkit.UniqueID(t),
			"content the community has never seen"))
	require.NotEqualf(t, record.CID, edited.CID,
		"the edit produced an identical CID, so this step would prove nothing about re-acceptance")

	reacceptance := awaitStatus(t, p, uri, community.DID, "pending_reacceptance",
		"the edit to fall out from under the standing acceptance")
	assert.NotEmptyf(t, reacceptance.AcceptanceURI,
		"the acceptance record still STANDS in the community's repo — only its pinned CID no longer matches — so getStatus must keep naming it; "+
			"clearing it would tell the author their acceptance was withdrawn, which is a different and untrue thing")

	// ---- withdrawing the acceptance ----------------------------------------
	// Deleting the acceptance with NO removal in the same commit is the author-
	// deletion sweep's shape (§5.3), and it means "no longer accepted", not
	// "removed": the post falls back to undecided rather than to a moderation
	// state nobody entered.
	community.DeleteExistingRecord(t, acceptanceCollection, acceptRkey)

	withdrawn := awaitStatus(t, p, uri, community.DID, "pending",
		"the deleted acceptance to return the post to undecided")
	assert.Emptyf(t, withdrawn.DecisionCode,
		"an acceptance deleted on its own is not a removal; minting a decision code here would put a moderation act in the record that no moderator performed")

	// ---- an acceptance for a post that does not exist ----------------------
	// The §5.4 direct fetch resolves the subject author's PDS and reads the
	// record when the AppView has not indexed it. Here there is no record to
	// read, and the CID cannot be verified against anything — so nothing may be
	// indexed and nothing may be admitted. Bounded by a later commit in the SAME
	// repo, then held.
	phantomURI := authorPostURI(author.DID, testkit.TID())
	community.PutRecord(t, acceptanceCollection, subjectRkey(phantomURI),
		acceptanceRecord(phantomURI, "bafyreiaphantomcidthatnobodyminted"))

	laterRkey := testkit.TID()
	laterURI := authorPostURI(author.DID, laterRkey)
	author.PutRecord(t, postV2Collection, laterRkey,
		postV2Record(community.DID, "later "+testkit.UniqueID(t), "committed after the phantom acceptance"))
	community.PutRecord(t, acceptanceCollection, subjectRkey(laterURI), acceptanceRecord(laterURI, "bafyreiplaceholder"))
	p.Await(t, "a later acceptance in the same repo, which bounds the phantom's rejection", func() (bool, error) {
		_, err := p.PostStatus(context.Background(), laterURI, community.DID)
		return testkit.PendingIfNotFound(err)
	})

	phantomAbsent := func() (bool, error) {
		view, err := p.PostStatus(context.Background(), phantomURI, community.DID)
		if err != nil {
			if testkit.IsNotFound(err) {
				return true, nil
			}
			return false, err
		}
		return view.Status != "accepted", nil
	}
	nothingAdmitted, err := phantomAbsent()
	require.NoError(t, err)
	require.True(t, nothingAdmitted,
		"an acceptance naming a post that exists nowhere was admitted. The pinned CID is unverifiable against a record nobody wrote, "+
			"so admitting it means the AppView will render whatever eventually appears at that URI under an attestation made before it existed")
	p.Holds(t, "the phantom subject to stay unadmitted", phantomAbsent)

	served, err := p.Post(context.Background(), phantomURI)
	require.NoError(t, err)
	assert.Truef(t, served.NotFound,
		"a post that only an acceptance ever mentioned must not be indexed: the direct fetch had nothing to verify the pinned CID against")
}

// TestCommunityRemovalIngestion is the pipeline proof for the moderation record.
//
// coves:ingestion-contract social.coves.community.removal
//
//	remove       → an accepted post becomes `removed` and carries the code
//	pre-emptive  → a removal with no prior acceptance is valid and indexes
//	terminal     → an author edit after removal does NOT reopen the decision
//
// # THE REMOVAL COMMIT IS ATOMIC, AND THE TEST WRITES IT THAT WAY
//
// §3.3 requires the acceptance's deletion and the removal's write to reach the
// firehose in ONE com.atproto.repo.applyWrites commit, so the firehose never
// carries a half-completed moderation action. Sending them as two commits would
// let this contract pass against a consumer that cannot order them — which is
// the exact defect §5.2's composite watermark exists to prevent — so the test
// uses applyWrites directly rather than two testkit calls.
func TestCommunityRemovalIngestion(t *testing.T) {
	p := newPipeline(t)

	author := p.IndexedAccount(t, "rv")
	community := indexedCommunity(t, p, "rv", author.DID)

	rkey := testkit.TID()
	uri := authorPostURI(author.DID, rkey)
	record := author.PutRecord(t, postV2Collection, rkey,
		postV2Record(community.DID, "remove me "+testkit.UniqueID(t), "content a moderator will take down"))

	awaitStatus(t, p, uri, community.DID, "pending", "the post to be indexed")

	subject := subjectRkey(uri)
	community.PutRecord(t, acceptanceCollection, subject, acceptanceRecord(uri, record.CID))
	awaitStatus(t, p, uri, community.DID, "accepted", "the post to be accepted before it is removed")

	// One commit, both operations. rank(delete) < rank(put) inside a commit
	// (§5.2), so whichever half the consumer applies second either lands or is
	// skipped as not-greater — and the subject converges on `removed` either way.
	applyWrites(t, community, []map[string]any{
		{"$type": "com.atproto.repo.applyWrites#delete", "collection": acceptanceCollection, "rkey": subject},
		{
			"$type":      "com.atproto.repo.applyWrites#create",
			"collection": removalCollection,
			"rkey":       subject,
			"value":      removalRecord(uri, record.CID, "rule-violation"),
		},
	})

	removed := awaitStatus(t, p, uri, community.DID, "removed",
		"the atomic removal commit to take the post down")
	assert.Equalf(t, "rule-violation", removed.DecisionCode,
		"the removal's code is what a client renders to the author; a removal without one is an unexplained disappearance")
	assert.NotEmpty(t, removed.DecisionAt, "a removal must record when it happened")
	assert.Emptyf(t, removed.AcceptanceURI,
		"the acceptance was deleted in the same commit, so naming one would point a verifying client at a record that no longer exists")

	// ---- removal is terminal across author edits ---------------------------
	// §5.5: `removed` is exited only by a moderator restore. An author edit
	// updates audit metadata and nothing else — otherwise editing a removed post
	// would launder it straight back through auto-acceptance, which is the most
	// obvious way to defeat moderation on a system where the author holds the
	// record.
	author.PutRecord(t, postV2Collection, rkey,
		postV2Record(community.DID, "edited while removed "+testkit.UniqueID(t), "an attempt to start over"))

	stillRemoved := func() (bool, error) {
		view, err := p.PostStatus(context.Background(), uri, community.DID)
		if err != nil {
			return false, err
		}
		return view.Status == "removed", nil
	}
	p.Holds(t, "the removed post to stay removed across an author edit", stillRemoved)

	// ---- pre-emptive removal ------------------------------------------------
	// A removal with no prior acceptance is valid (§5.4): a community may decide
	// in advance about content it has already seen elsewhere, or about an author
	// it is banning. A consumer requiring an acceptance first would drop exactly
	// the decisions a community most wants to make early.
	preRkey := testkit.TID()
	preURI := authorPostURI(author.DID, preRkey)
	preRecord := author.PutRecord(t, postV2Collection, preRkey,
		postV2Record(community.DID, "pre-emptively removed "+testkit.UniqueID(t), "never accepted at all"))
	awaitStatus(t, p, preURI, community.DID, "pending", "the pre-emptive subject to be indexed")

	community.PutRecord(t, removalCollection, subjectRkey(preURI),
		removalRecord(preURI, preRecord.CID, "spam"))

	preRemoved := awaitStatus(t, p, preURI, community.DID, "removed",
		"a removal with no prior acceptance to take the post down")
	assert.Equal(t, "spam", preRemoved.DecisionCode)
}

// applyWrites commits several repo operations together, which is the only way
// to produce the atomic moderation commit §3.3 requires.
//
// testkit has no applyWrites helper — every other contract in this tier writes
// one record at a time — so this speaks the XRPC procedure directly through the
// account's own client. validate is false for the reason task 4 recorded: the
// PDS refuses validate:true for lexicons it has not been served, and these three
// are not published to it.
func applyWrites(t *testing.T, community provisionedCommunity, writes []map[string]any) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), contractBudget)
	defer cancel()

	err := community.XRPC().Procedure(ctx, "com.atproto.repo.applyWrites", map[string]any{
		"repo":     community.DID,
		"validate": false,
		"writes":   writes,
	}, nil)
	require.NoErrorf(t, err,
		"com.atproto.repo.applyWrites was refused. The removal commit must carry the acceptance's delete and the removal's create TOGETHER (§3.3); "+
			"splitting them into two commits would let a consumer that cannot order them pass this contract")
}
