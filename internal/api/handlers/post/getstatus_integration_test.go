//go:build integration

package post_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"Coves/internal/api/handlers/post"
	"Coves/internal/core/posts"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// What social.coves.community.post.getStatus answers, over the real admissions
// table (docs/PRD_AUTHOR_OWNED_POSTS.md §3.4, pulled into task 5 by rev 2.7).
//
// This is at T1 rather than in a handler unit test with a fake service, and the
// reason is the whole point of the endpoint. getStatus exists so that a client
// can learn a decision that lives NOWHERE ELSE: a rejection writes no community
// record (§3.3), so unlike acceptance and removal there is no firehose event, no
// repo record, and no other endpoint carrying it. The only source of truth is a
// row in community_post_admissions, and a test that faked the service would
// prove the JSON shape while leaving open the question that matters — whether
// the five statuses the table can actually hold each come out as something a
// client can act on. So every case below seeds its state through the real
// repository's own mutations, in the same way the consumer will.
//
// The pipeline tier then uses this endpoint as its observation surface for the
// consumer contracts (§9, rev 2.7): no T2 community holds credentials, so the
// accepted-state arcs prove here and at the repository, and T2 asserts consumer
// semantics by asking getStatus what the AppView concluded.

// statusStack is the getStatus endpoint over the real admissions store, plus the
// repository the tests seed through.
type statusStack struct {
	handler    *post.GetStatusHandler
	admissions posts.AdmissionRepository
}

func newStatusStack(db *sql.DB) statusStack {
	admissions := postgres.NewAdmissionRepository(db)
	return statusStack{
		handler:    post.NewGetStatusHandler(posts.NewStatusService(admissions)),
		admissions: admissions,
	}
}

// statusSubject is one (community, post) pair with the post row seeded in the
// AUTHOR's repo shape — at://<author>/social.coves.community.postv2/<rkey> —
// because that is where a post lives now (§3.1) and a URI in the old
// community-repo shape would exercise a normalization path this endpoint must
// not have.
type statusSubject struct {
	CommunityDID string
	AuthorDID    string
	PostURI      string
}

func newStatusSubject(t *testing.T, db *sql.DB) statusSubject {
	t.Helper()

	ctx := context.Background()
	name := testkit.UniqueIDWithPrefix(t, "stat")

	communityDID, err := fixtures.Community(ctx, db, name, "owner"+name)
	require.NoErrorf(t, err, "seeding community %s", name)

	authorDID := fixtures.DID(testkit.UniqueID(t))
	rkey := testkit.TID()
	postURI := "at://" + authorDID + "/social.coves.community.postv2/" + rkey

	_, err = db.ExecContext(ctx, `
		INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW())
	`, postURI, "bafyreistatusseed", rkey, authorDID, communityDID, "a post whose status someone is asking about")
	require.NoError(t, err, "seeding the post row the admission is about")

	return statusSubject{CommunityDID: communityDID, AuthorDID: authorDID, PostURI: postURI}
}

// getStatus drives the handler with NO Authorization header, which is half the
// contract: the caller this endpoint is built for is an author on another
// server with no account here (§7). That the ROUTE carries no auth middleware
// is declared in internal/api/routes/registration_test.go; what is proven here
// is that the handler itself serves a complete answer to an anonymous caller
// rather than degrading to an empty or partial one.
func getStatus(t *testing.T, h *post.GetStatusHandler, postURI, communityDID string) *httptest.ResponseRecorder {
	t.Helper()

	target := "/xrpc/social.coves.community.post.getStatus?post=" +
		url.QueryEscape(postURI) + "&community=" + url.QueryEscape(communityDID)
	rec := httptest.NewRecorder()
	h.HandleGetStatus(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

// decodeStatus reads a 200 body as a generic map, so that a MISSING optional
// field and a field present-but-null are distinguishable. That distinction is
// load-bearing: the lexicon marks decisionCode, decisionAt and acceptanceUri
// optional, and a client that meets `"decisionCode": null` on a pending post has
// been handed a decision that does not exist.
func decodeStatus(t *testing.T, rec *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()

	require.Equalf(t, http.StatusOK, rec.Code, "status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var body map[string]interface{}
	require.NoErrorf(t, json.Unmarshal(rec.Body.Bytes(), &body),
		"decoding the getStatus response (body: %q)", rec.Body.String())
	return body
}

func TestGetStatus_Pending(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	stack := newStatusStack(db)
	subject := newStatusSubject(t, db)

	_, err := stack.admissions.UpsertPending(ctx, posts.UpsertPendingCommand{
		CommunityDID: subject.CommunityDID,
		PostURI:      subject.PostURI,
		EvaluatedCID: "bafyreipendingcontent",
	})
	require.NoError(t, err)

	body := decodeStatus(t, getStatus(t, stack.handler, subject.PostURI, subject.CommunityDID))

	assert.Equal(t, "pending", body["status"])

	// A pending post has been decided about by nobody, and the response must
	// say exactly that by carrying no decision fields at all. A client polling
	// this endpoint for the accepted transition (§7's UX) reads their presence
	// as "the wait is over".
	assert.NotContains(t, body, "decisionCode", "a pending post carries no decision")
	assert.NotContains(t, body, "decisionAt", "a pending post carries no decision time")
	assert.NotContains(t, body, "acceptanceUri", "a pending post has no acceptance record to point at")
}

func TestGetStatus_AcceptedNamesTheAcceptanceRecord(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	stack := newStatusStack(db)
	subject := newStatusSubject(t, db)

	const acceptedCID = "bafyreiacceptedcontent"
	rkey := testkit.TID()
	acceptanceURI := "at://" + subject.CommunityDID + "/social.coves.community.acceptance/" + rkey

	_, err := stack.admissions.UpsertPending(ctx, posts.UpsertPendingCommand{
		CommunityDID: subject.CommunityDID,
		PostURI:      subject.PostURI,
		EvaluatedCID: acceptedCID,
	})
	require.NoError(t, err)

	result, err := stack.admissions.ApplyAcceptance(ctx, posts.ApplyAcceptanceCommand{
		CommunityDID:   subject.CommunityDID,
		PostURI:        subject.PostURI,
		AcceptanceURI:  acceptanceURI,
		AcceptanceRkey: rkey,
		PinnedCID:      acceptedCID,
		Watermark:      posts.CommunityWatermark{Rev: testkit.TID()},
	})
	require.NoError(t, err)
	require.Equal(t, posts.AdmissionApplied, result.Outcome, "fixture: the acceptance must have applied")

	body := decodeStatus(t, getStatus(t, stack.handler, subject.PostURI, subject.CommunityDID))

	assert.Equal(t, "accepted", body["status"])

	// The acceptance URI is what turns this answer from a claim into something
	// verifiable: the caller can go read the community's signed attestation
	// instead of trusting this AppView's summary of it. Omitting it would make
	// getStatus the authority on a fact it is only reporting.
	assert.Equal(t, acceptanceURI, body["acceptanceUri"],
		"an accepted post must name its acceptance record so the caller can read the signed attestation")
	assert.NotContains(t, body, "decisionCode", "acceptance is not a refusal and carries no code")
}

func TestGetStatus_RejectedCarriesTheReasonAndWhen(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	stack := newStatusStack(db)
	subject := newStatusSubject(t, db)

	const judgedCID = "bafyreirejectedcontent"
	_, err := stack.admissions.UpsertPending(ctx, posts.UpsertPendingCommand{
		CommunityDID: subject.CommunityDID,
		PostURI:      subject.PostURI,
		EvaluatedCID: judgedCID,
	})
	require.NoError(t, err)

	before := time.Now().Add(-time.Minute)
	result, err := stack.admissions.RecordRejection(ctx, posts.RecordRejectionCommand{
		CommunityDID: subject.CommunityDID,
		PostURI:      subject.PostURI,
		DecisionCode: string(posts.DecisionRateLimitExceeded),
		JudgedCID:    judgedCID,
		Redrivable:   false,
	})
	require.NoError(t, err)
	require.Equal(t, posts.AdmissionApplied, result.Outcome, "fixture: the rejection must have applied")

	body := decodeStatus(t, getStatus(t, stack.handler, subject.PostURI, subject.CommunityDID))

	assert.Equal(t, "rejected", body["status"])

	// THIS is the case the endpoint exists for. A rejection writes no community
	// record (§3.3), so there is no acceptance to read, no removal to read, and
	// nothing on the firehose — an author whose post vanished has no other way
	// to learn it was refused, or why. A `rejected` with no code is a status
	// that tells them only that asking was pointless.
	assert.Equal(t, string(posts.DecisionRateLimitExceeded), body["decisionCode"],
		"a rejection is invisible everywhere else, so the code is the only explanation the author will ever get")

	decisionAt, ok := body["decisionAt"].(string)
	require.Truef(t, ok, "decisionAt must be present and a string on a rejected post; got %#v", body["decisionAt"])
	parsed, err := time.Parse(time.RFC3339, decisionAt)
	require.NoErrorf(t, err, "decisionAt %q must be an RFC 3339 datetime, per the lexicon's datetime format", decisionAt)
	assert.Truef(t, parsed.After(before), "decisionAt (%s) must record when the decision was made", parsed)

	assert.NotContains(t, body, "acceptanceUri", "a rejected post was never accepted, so there is no record to name")
}

func TestGetStatus_RemovedCarriesTheModerationCode(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	stack := newStatusStack(db)
	subject := newStatusSubject(t, db)

	_, err := stack.admissions.UpsertPending(ctx, posts.UpsertPendingCommand{
		CommunityDID: subject.CommunityDID,
		PostURI:      subject.PostURI,
		EvaluatedCID: "bafyreiremovedcontent",
	})
	require.NoError(t, err)

	result, err := stack.admissions.ApplyRemoval(ctx, posts.ApplyRemovalCommand{
		CommunityDID: subject.CommunityDID,
		PostURI:      subject.PostURI,
		DecisionCode: string(posts.DecisionRuleViolation),
		Watermark:    posts.CommunityWatermark{Rev: testkit.TID()},
	})
	require.NoError(t, err)
	require.Equal(t, posts.AdmissionApplied, result.Outcome, "fixture: the removal must have applied")

	body := decodeStatus(t, getStatus(t, stack.handler, subject.PostURI, subject.CommunityDID))

	assert.Equal(t, "removed", body["status"])
	assert.Equal(t, string(posts.DecisionRuleViolation), body["decisionCode"],
		"a removal's code is what #removedPost renders to the author; a removal without one is an unexplained moderation act")
	assert.NotContains(t, body, "acceptanceUri",
		"a removal deletes the acceptance in the same commit (§3.3), so naming one would point at a record that no longer exists")
}

func TestGetStatus_PendingReacceptance(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	stack := newStatusStack(db)
	subject := newStatusSubject(t, db)

	const originalCID = "bafyreioriginalcontent"
	rkey := testkit.TID()
	acceptanceURI := "at://" + subject.CommunityDID + "/social.coves.community.acceptance/" + rkey

	_, err := stack.admissions.UpsertPending(ctx, posts.UpsertPendingCommand{
		CommunityDID: subject.CommunityDID,
		PostURI:      subject.PostURI,
		EvaluatedCID: originalCID,
	})
	require.NoError(t, err)
	_, err = stack.admissions.ApplyAcceptance(ctx, posts.ApplyAcceptanceCommand{
		CommunityDID:   subject.CommunityDID,
		PostURI:        subject.PostURI,
		AcceptanceURI:  acceptanceURI,
		AcceptanceRkey: rkey,
		PinnedCID:      originalCID,
		Watermark:      posts.CommunityWatermark{Rev: testkit.TID()},
	})
	require.NoError(t, err)

	// The author edits. The standing acceptance now pins content that is no
	// longer current, and §5.5 forbids rendering the new CID under it.
	_, err = stack.admissions.UpsertPending(ctx, posts.UpsertPendingCommand{
		CommunityDID: subject.CommunityDID,
		PostURI:      subject.PostURI,
		EvaluatedCID: "bafyreieditedcontent",
	})
	require.NoError(t, err)

	body := decodeStatus(t, getStatus(t, stack.handler, subject.PostURI, subject.CommunityDID))

	// The status is reported verbatim rather than collapsed into "pending". An
	// author who edited an accepted post and is shown plain `pending` cannot
	// tell that from a post that was never accepted at all, and the two have
	// completely different next steps.
	assert.Equal(t, "pending_reacceptance", body["status"],
		"pending_reacceptance must not be flattened into pending: the author needs to know their edit un-published an accepted post")
}

func TestGetStatus_UnknownSubjectIsNotFound(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	stack := newStatusStack(db)
	subject := newStatusSubject(t, db)

	// The post and the community both exist; what does not exist is a decision.
	// That is the ordinary state of a post the community has never been offered,
	// and it must be answered as not-found rather than invented as `pending` —
	// pending is a promise that someone is going to decide.
	rec := getStatus(t, stack.handler, subject.PostURI, subject.CommunityDID)

	require.Equalf(t, http.StatusNotFound, rec.Code,
		"a subject with no admission row must be 404, not a fabricated status (body: %s)", rec.Body.String())

	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "NotFound", body["error"],
		"the XRPC error name is what a client switches on; the shared mapper spells post-not-found as NotFound (errors.go)")
}

func TestGetStatus_RequiresBothHalvesOfTheSubject(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	stack := newStatusStack(db)
	subject := newStatusSubject(t, db)

	// A post carries independent decisions from several communities (§2), so
	// "the status of this post" is not a question with one answer. A request
	// missing either half has to be refused rather than answered about whichever
	// row happens to be found first.
	cases := []struct {
		name   string
		target string
	}{
		{"no post", "/xrpc/social.coves.community.post.getStatus?community=" + url.QueryEscape(subject.CommunityDID)},
		{"no community", "/xrpc/social.coves.community.post.getStatus?post=" + url.QueryEscape(subject.PostURI)},
		{"neither", "/xrpc/social.coves.community.post.getStatus"},
		{"empty post", "/xrpc/social.coves.community.post.getStatus?post=&community=" + url.QueryEscape(subject.CommunityDID)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			stack.handler.HandleGetStatus(rec, httptest.NewRequest(http.MethodGet, tc.target, nil))
			assert.Equalf(t, http.StatusBadRequest, rec.Code,
				"an incomplete subject must be 400 (body: %s)", rec.Body.String())
		})
	}
}

func TestGetStatus_ScopesTheAnswerToTheNamedCommunity(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	stack := newStatusStack(db)

	// One post, two communities, opposite decisions. This is the fork case the
	// per-(community, post) key exists for (§2, §6.1), and it is the sharpest
	// available proof that the community parameter is actually part of the
	// lookup rather than decoration on a post-scoped query.
	accepting := newStatusSubject(t, db)

	otherName := testkit.UniqueIDWithPrefix(t, "statfork")
	forkDID, err := fixtures.Community(ctx, db, otherName, "owner"+otherName)
	require.NoError(t, err)

	const cid = "bafyreitwocommunities"
	rkey := testkit.TID()
	acceptanceURI := "at://" + accepting.CommunityDID + "/social.coves.community.acceptance/" + rkey

	for _, communityDID := range []string{accepting.CommunityDID, forkDID} {
		_, err = stack.admissions.UpsertPending(ctx, posts.UpsertPendingCommand{
			CommunityDID: communityDID,
			PostURI:      accepting.PostURI,
			EvaluatedCID: cid,
		})
		require.NoError(t, err)
	}

	_, err = stack.admissions.ApplyAcceptance(ctx, posts.ApplyAcceptanceCommand{
		CommunityDID:   accepting.CommunityDID,
		PostURI:        accepting.PostURI,
		AcceptanceURI:  acceptanceURI,
		AcceptanceRkey: rkey,
		PinnedCID:      cid,
		Watermark:      posts.CommunityWatermark{Rev: testkit.TID()},
	})
	require.NoError(t, err)

	_, err = stack.admissions.ApplyRemoval(ctx, posts.ApplyRemovalCommand{
		CommunityDID: forkDID,
		PostURI:      accepting.PostURI,
		DecisionCode: string(posts.DecisionOffTopic),
		Watermark:    posts.CommunityWatermark{Rev: testkit.TID()},
	})
	require.NoError(t, err)

	accepted := decodeStatus(t, getStatus(t, stack.handler, accepting.PostURI, accepting.CommunityDID))
	assert.Equal(t, "accepted", accepted["status"])

	removed := decodeStatus(t, getStatus(t, stack.handler, accepting.PostURI, forkDID))
	assert.Equal(t, "removed", removed["status"],
		"the same post is accepted in one community and removed in another; an answer that ignored the community parameter would report one of them everywhere")
	assert.Equal(t, string(posts.DecisionOffTopic), removed["decisionCode"])
}

func TestGetStatus_AcceptsALegalLongDID(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	stack := newStatusStack(db)

	// A DID may legally run to 2048 bytes (the same fact that killed the
	// readable-rkey transform in PRD rev 2.2 and forced the SHA-256 digest). An
	// author-owned post URI is authority-scoped, so the author's DID is INSIDE
	// the URI this endpoint takes — which means a length cap sized for the old
	// community-repo URIs silently makes long-DID authors unqueryable, and only
	// them. Nothing else in the system would notice: their posts index fine and
	// every other endpoint serves them.
	name := testkit.UniqueIDWithPrefix(t, "longdid")
	communityDID, err := fixtures.Community(ctx, db, name, "owner"+name)
	require.NoError(t, err)

	longDID := "did:web:" + strings.Repeat("a", 2048-len("did:web:"))
	require.Len(t, longDID, 2048, "fixture: the DID must be exactly at the legal ceiling")

	rkey := testkit.TID()
	postURI := "at://" + longDID + "/social.coves.community.postv2/" + rkey
	_, err = db.ExecContext(ctx, `
		INSERT INTO posts (uri, cid, rkey, author_did, community_did, title, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW())
	`, postURI, "bafyreilongdid", rkey, longDID, communityDID, "a post by an author with a very long DID")
	require.NoError(t, err)

	_, err = stack.admissions.UpsertPending(ctx, posts.UpsertPendingCommand{
		CommunityDID: communityDID,
		PostURI:      postURI,
		EvaluatedCID: "bafyreilongdid",
	})
	require.NoError(t, err)

	body := decodeStatus(t, getStatus(t, stack.handler, postURI, communityDID))
	assert.Equal(t, "pending", body["status"],
		"a legal 2048-byte DID must be queryable; a cap below the spec's ceiling excludes real authors from the only "+
			"endpoint that can tell them why their post is not visible")
}

func TestGetStatus_RefusesAMalformedURI(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	stack := newStatusStack(db)
	subject := newStatusSubject(t, db)

	// Raising the cap must not become "accept anything long". The URI is parsed
	// — a handle-authority URI resolves to whoever holds the handle next, and a
	// non-at:// string is a client bug that has to come back as one rather than
	// as a silent not-found.
	for _, malformed := range []string{
		"not-an-at-uri",
		"at://",
		"https://example.com/post",
		"at://" + strings.Repeat("b", 4096) + "/social.coves.community.postv2/x",
	} {
		rec := httptest.NewRecorder()
		target := "/xrpc/social.coves.community.post.getStatus?post=" +
			url.QueryEscape(malformed) + "&community=" + url.QueryEscape(subject.CommunityDID)
		stack.handler.HandleGetStatus(rec, httptest.NewRequest(http.MethodGet, target, nil))

		assert.Equalf(t, http.StatusBadRequest, rec.Code,
			"the URI %.40q must be refused as malformed, not answered; a 404 here tells a client with a bug that its post "+
				"does not exist (body: %s)", malformed, rec.Body.String())
	}
}

func TestGetStatus_IsNotCacheable(t *testing.T) {
	t.Parallel()

	db := testkit.DB(t)
	ctx := context.Background()
	stack := newStatusStack(db)
	subject := newStatusSubject(t, db)

	_, err := stack.admissions.UpsertPending(ctx, posts.UpsertPendingCommand{
		CommunityDID: subject.CommunityDID,
		PostURI:      subject.PostURI,
		EvaluatedCID: "bafyreicacheable",
	})
	require.NoError(t, err)

	rec := getStatus(t, stack.handler, subject.PostURI, subject.CommunityDID)
	require.Equal(t, http.StatusOK, rec.Code)

	// This endpoint exists to be POLLED for a transition (§7), so a cached
	// answer is not a stale nicety — it is the endpoint failing at its only job:
	// the client keeps being handed `pending` after the post was accepted and
	// stops polling. It is also unauthenticated and reports a moderation
	// decision, so an intermediary holding a copy is a disclosure surface that
	// outlives the request.
	assert.Equalf(t, "no-store", rec.Header().Get("Cache-Control"),
		"getStatus must answer Cache-Control: no-store; got %q", rec.Header().Get("Cache-Control"))
}
