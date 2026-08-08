package posts

import (
	"context"
	"time"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"

	"Coves/internal/atproto/pds"
)

// The author-repo half of the write path (docs/PRD_AUTHOR_OWNED_POSTS.md §3.1,
// §4.2): the record shape a post takes in its AUTHOR's repository, the
// deterministic key it is written at, and the seam the author's own credentials
// arrive through.
//
// STUB — the declarations exist so the contract can be written against them.
// Behaviour is task 6's GREEN cycle.

// PostStatus is what a CreatePost response reports about the community's
// decision, so a client knows whether the post is already visible in the
// community or is waiting on one (§4.2 steps 4 and 5).
const (
	// PostStatusAccepted means this AppView hosts the community, ran admission
	// synchronously, and a community acceptance now stands for the post.
	PostStatusAccepted = "accepted"

	// PostStatusPending means the post exists in the author's repo but no
	// community acceptance covers it yet — either the community is hosted
	// elsewhere and has to decide for itself (§4.2 step 5), or the synchronous
	// acceptance failed and the firehose engine will retry it (§5.6).
	PostStatusPending = "pending"
)

// StrongRef is com.atproto.repo.strongRef: a record URI pinned to the exact
// version of the record it names.
type StrongRef struct {
	URI string `json:"uri"`
	CID string `json:"cid"`
}

// BridgedStats is social.coves.community.postv2#bridgedStats: origin-platform
// vote aggregates asserted by a bridge for federated content.
type BridgedStats struct {
	Upvotes   int    `json:"upvotes"`
	Downvotes int    `json:"downvotes"`
	AsOf      string `json:"asOf"`
}

// PostV2Record is the social.coves.community.postv2 record: a post as it lives
// in its AUTHOR's repository.
//
// THERE IS NO AUTHOR FIELD, AND THAT IS THE WHOLE POINT. Under the deprecated
// social.coves.community.post the record lived in the COMMUNITY's repo, so the
// only thing that could say who wrote it was a field the community's own
// credentials had signed — an assertion by the community about a third party.
// Here authorship is the repository the record is in, which a verifying relay
// or a DID-resolved direct fetch can attribute. Re-adding the field would
// reintroduce a self-asserted, unverifiable claim beside a verifiable one, and
// consumers would have two answers to one question.
//
// Community is a DID rather than the at-identifier the client typed, and the
// lexicon calls it immutable: retargeting a post means writing a NEW post
// record, so an update that changes it is discarded entire by consumers.
type PostV2Record struct {
	Title          *string                `json:"title,omitempty"`
	Content        *string                `json:"content,omitempty"`
	Embed          map[string]interface{} `json:"embed,omitempty"`
	Labels         *SelfLabels            `json:"labels,omitempty"`
	CrosspostOf    *StrongRef             `json:"crosspostOf,omitempty"`
	BridgedStats   *BridgedStats          `json:"bridgedStats,omitempty"`
	Type           string                 `json:"$type"`
	Community      string                 `json:"community"`
	CreatedAt      string                 `json:"createdAt"`
	Facets         []interface{}          `json:"facets,omitempty"`
	Langs          []string               `json:"langs,omitempty"`
	Tags           []string               `json:"tags,omitempty"`
	CrosspostChain []StrongRef            `json:"crosspostChain,omitempty"`
}

// SubmissionRkey is the record key a submission's postv2 record is written at:
// a valid TID derived from the submission itself rather than from the clock.
//
// WHY DETERMINISTIC. §4.2 records a lost-response asymmetry on the write path:
// when a PDS write's outcome is ambiguous the record may or may not exist, and a
// client that retries produces a SECOND post. A server-chosen TID cannot fix
// that — every attempt gets a fresh one. A key derived from what is being
// submitted makes the retry aim at the record the first attempt may already have
// written, so a create-only write (swapRecord "must not exist") either creates
// it once or reports the standing one, and the duplicate becomes impossible
// rather than merely unlikely.
//
// THE MATERIAL IS (community, fingerprint, bucket), AND EACH PART EARNS ITS
// PLACE:
//
//   - The RESOLVED community DID. The fingerprint deliberately excludes the
//     community (see submissionFingerprint: the client types a handle one time
//     and a DID the next, and the ledger's unique key already scopes it), but
//     the rkey must NOT: the same content submitted to two communities is two
//     posts, and two posts sharing one rkey in one author repo is one post that
//     silently overwrote the other.
//   - The fingerprint, which is what makes a retry of the same content collide
//     with itself.
//   - The dedupe bucket, which is what makes the collision EXPIRE. Without it an
//     author could never repost identical content, because the rkey would name a
//     record that already exists forever.
//
// THE ANSWER IS A REAL TID, not merely a 13-character string. The postv2
// lexicon declares `"key": "tid"`, so a PDS may validate it, feed ordering reads
// the timestamp out of it, and a key that merely looked like one would sort
// posts to a plausible-but-wrong moment. The timestamp is placed INSIDE the
// submission's own dedupe bucket — bucket start plus an offset drawn from the
// digest — so the derived time is within one dedupe window of when the post was
// actually submitted, and the clock ID carries further digest bits so two
// submissions landing on the same microsecond still differ.
func SubmissionRkey(communityDID, fingerprint string, bucket int64, dedupeWindow time.Duration) string {
	// RED STUB (task 6): the contract is pinned in submission_rkey_test.go.
	// It answers the empty string rather than panicking so the reds read as
	// failed assertions naming the expected key, not as a stack trace.
	_, _, _, _ = communityDID, fingerprint, bucket, dedupeWindow
	return ""
}

// AuthorRepo is one author's PDS repository, narrowed to what the write path
// does with it.
//
// It is declared here rather than taken as pds.CommitClient for the same reason
// CommunityRepo is: the tests fake four methods instead of a dozen, and the
// dependency reads as what it is — a repo we read before we write.
//
// PutRecordWithCommit rather than CreateRecord, because every write on this path
// is GUARDED. A create needs swapRecord "" (the record must not exist) so a
// retry that finds the record already there is told rather than minting a
// second; an update needs the standing CID so a concurrent edit is a detected
// conflict rather than a silent clobber.
type AuthorRepo interface {
	// GetRecord reads the standing record — the pre-read an update shapes its
	// swap guard from, and what a create falls back to when its guard fires.
	GetRecord(ctx context.Context, collection, rkey string) (*pds.RecordResponse, error)

	// PutRecordWithCommit writes one record under a swap guard. An empty
	// swapRecord means "there must be nothing here".
	PutRecordWithCommit(ctx context.Context, collection, rkey string, record any, swapRecord string) (*pds.RecordCommit, error)

	// DeleteRecord removes a record from the author's repo.
	DeleteRecord(ctx context.Context, collection, rkey string) error

	// DID is the repo being written — the author's own identity, which is the
	// authority half of every post URI this path produces.
	DID() string
}

// AuthorRepoFactory opens an authenticated client on ONE author's repo.
//
// The session is the author's own OAuth session, as the API boundary already
// holds it (middleware.GetOAuthSession) and as comments' PDSClientFactory
// already takes it. It may be nil for a non-interactive author — an aggregator
// posting under its stored tokens (migration 025) — and the production factory
// resolves those through the OAuth app's session store, answering
// ErrNoAuthorCredentials when there is nothing to resume.
//
// authorDID is passed alongside rather than read off the session so that the
// nil-session path has an identity to resolve at all, and so a factory can
// assert the two agree.
type AuthorRepoFactory func(ctx context.Context, authorDID string, session *oauth.ClientSessionData) (AuthorRepo, error)

// WithAuthorRepoFactory supplies the seam the author's own credentials arrive
// through. Integration tests inject password auth over a real PDS; production
// injects OAuth/DPoP.
func WithAuthorRepoFactory(factory AuthorRepoFactory) PostServiceOption {
	return func(s *postService) { s.authorRepos = factory }
}

// SubmissionAcceptor is the acceptance engine's SYNCHRONOUS entry point — §4.2
// step 4's local-community fast path.
//
// It is an interface on the post service rather than a *AcceptanceEngine so the
// write path can be tested against an acceptance that fails on purpose. That
// case is not exotic: the whole design of the fast path is that its failure is
// invisible to the author, and a fixture that cannot make it fail cannot prove
// the author's record survives.
type SubmissionAcceptor interface {
	// AcceptSubmission settles a post this AppView has just written, without
	// waiting for the firehose copy of it to come back.
	AcceptSubmission(ctx context.Context, communityDID, postURI, postCID string) (EngineOutcome, error)
}

// WithSyncAcceptance wires the local-community fast path: the admission row the
// post is seeded into, and the engine that settles it.
//
// Both or neither. A service holding the acceptor but not the repository would
// hand the engine a subject with no row to read; one holding the repository but
// not the acceptor would seed rows nothing ever settles until the firehose
// arrives.
func WithSyncAcceptance(admissions AdmissionRepository, acceptor SubmissionAcceptor) PostServiceOption {
	return func(s *postService) {
		s.admissions = admissions
		s.acceptor = acceptor
	}
}
