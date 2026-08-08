package posts

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"strconv"
	"time"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/blobs"
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
	// OriginalAuthor, FederatedFrom and Location are the bridge/federation
	// surfaces a client may send alongside a post. They are `unknown` in the
	// lexicon and `interface{}` here for the same reason: no bridge has fixed
	// their shape yet, and declaring one now would be inventing a contract the
	// first real bridge would contradict.
	//
	// They are carried on the record — rather than dropped as CreatePostRequest
	// surfaces nothing reads — because UpdatePost round-trips the standing
	// record through this struct: a field absent here is a field an edit erases
	// from a post the first time its author fixes a typo.
	OriginalAuthor interface{}            `json:"originalAuthor,omitempty"`
	FederatedFrom  interface{}            `json:"federatedFrom,omitempty"`
	Location       interface{}            `json:"location,omitempty"`
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
	digest := sha256.Sum256([]byte(
		communityDID + submissionRkeyDelimiter +
			fingerprint + submissionRkeyDelimiter +
			strconv.FormatInt(bucket, 10)))

	// THE TIMESTAMP IS THE BUCKET'S START PLUS A DIGEST-DRAWN OFFSET INSIDE IT,
	// so the derived time never leaves the window the submission actually
	// landed in. Taking the offset modulo the window is what bounds it: 64 bits
	// of digest used raw would name a moment in some arbitrary century, and the
	// rkey's timestamp is not decoration — repo listings and feeds order by it.
	windowMicros := int64(dedupeWindow / time.Microsecond)
	var micros int64
	if windowMicros > 0 {
		offset := binary.BigEndian.Uint64(digest[0:8]) % uint64(windowMicros)
		micros = bucket*windowMicros + int64(offset)
	}
	// A non-positive window collapses to the epoch rather than dividing by
	// zero, the same answer dedupeBucket gives the same misconfiguration.
	// config.Validate refuses to start a process with one, so this is a wiring
	// bug being kept off the write path, not a supported mode.

	// The clock ID carries FURTHER digest bits, so two submissions whose
	// offsets collide on one microsecond still differ. NewTID masks it to the
	// 10 bits a TID has room for; masking here too keeps this function's
	// arithmetic the whole story.
	clockID := uint(binary.BigEndian.Uint16(digest[8:10])) & 0x3FF

	// syntax.NewTID rather than a hand-rolled base32 encoding: the postv2
	// lexicon declares `key: tid`, and the one encoder guaranteed to agree with
	// every ParseTID in the network is the one ParseTID ships beside.
	return syntax.NewTID(micros, clockID).String()
}

// submissionRkeyDelimiter separates the three parts of the rkey material.
//
// A newline, because neither a DID nor a hex fingerprint may contain one. With
// no delimiter at all — or one drawn from a charset the inputs share — a
// community DID ending in the delimiter and a fingerprint beginning with it
// would hash to the same bytes as some other pair, and one author's rkey could
// be forged from another submission's.
const submissionRkeyDelimiter = "\n"

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

	// UploadBlob puts a post's media into the author's own storage.
	//
	// THE BLOB HAS TO TRAVEL WITH THE RECORD. A blob ref names a CID and not a
	// repository, so a reader resolves it against the repo it believes owns the
	// record — the author's. A thumbnail left in the community's storage
	// therefore produces a record that looks identical to a correct one and
	// resolves for nobody, is garbage-collectable by a repo that references it
	// nowhere, and is not the author's to release when they delete the post.
	//
	// It is on THIS interface rather than reached through blobs.Service because
	// an author authenticates with a DPoP-signed OAuth session, and that cannot
	// be expressed as the bearer token blobs.BlobOwner carries. The fetch and
	// the size/MIME guard still come from blobs.Service.FetchImageForURL — only
	// the upload leg moved.
	UploadBlob(ctx context.Context, data []byte, mimeType string) (*blobs.BlobRef, error)

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
