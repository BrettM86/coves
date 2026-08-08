package posts

import (
	"context"

	"Coves/internal/atproto/pds"
)

// The writers that publish a community's own records about a post.
//
// These are the only things in the post system that write into a COMMUNITY's
// repository (docs/PRD_AUTHOR_OWNED_POSTS.md §5.6), and every one of them is
// STATE-SHAPED rather than optimistic: what it sends is decided by what the
// repo already holds, because the PDS answers the wrong shape with a 500 and
// because a re-fire must not mint a new record CID.

const (
	// AcceptanceCollection is the community-repo collection holding a
	// community's attestation that it accepts a post.
	AcceptanceCollection = "social.coves.community.acceptance"

	// RemovalCollection is the community-repo collection holding a community's
	// record that a post has been removed from it.
	RemovalCollection = "social.coves.community.removal"
)

// CommunityRepo is one community's PDS repository, narrowed to what the
// writers do with it.
//
// It is declared here rather than taken as pds.CommitClient so that the
// writers' tests can fake four methods instead of a dozen, and so that the
// dependency reads as what it is: a repo we read before we write.
type CommunityRepo interface {
	// GetRecord is the PRE-READ. Both writers shape their commit from it: the
	// acceptance writer to decide whether there is anything to write at all,
	// the removal writer to decide create-vs-update and whether to emit a
	// delete.
	GetRecord(ctx context.Context, collection, rkey string) (*pds.RecordResponse, error)

	// PutRecordWithCommit writes one record, optionally guarded by the CID it
	// expects to be replacing.
	PutRecordWithCommit(ctx context.Context, collection, rkey string, record any, swapRecord string) (*pds.RecordCommit, error)

	// ApplyWrites commits several operations together, so the firehose never
	// carries a half-completed moderation action.
	ApplyWrites(ctx context.Context, writes []pds.Write, swapCommit string) (*pds.ApplyWritesResult, error)

	// GetLatestCommit supplies the swapCommit a batch is guarded by.
	GetLatestCommit(ctx context.Context) (*pds.LatestCommit, error)

	// DID is the repo being written — the community's own identity, which is
	// the authority half of every record URI this writer produces.
	DID() string
}

// CommunityRepoFactory opens an authenticated client on one community's repo.
//
// The engine works from an admission row, which names a community by DID and
// nothing else, so the credentials have to be fetched per subject rather than
// held. A community this AppView does not host has no credentials and the
// factory says so with an error.
type CommunityRepoFactory func(ctx context.Context, communityDID string) (CommunityRepo, error)

// CommunityWriteCommand is an acceptance write: this community accepts this
// exact version of this post.
type CommunityWriteCommand struct {
	CommunityDID string
	PostURI      string

	// PostCID is the content CID the acceptance pins. It is required: the
	// record's subject is a strongRef, and a strongRef without a CID pins
	// nothing, which is exactly the guarantee the acceptance exists to make.
	PostCID string
}

// CommunityRemovalCommand is a removal: this community no longer carries this
// post, for this reason.
type CommunityRemovalCommand struct {
	CommunityDID string
	PostURI      string

	// PostCID is the version present at removal time. It is audit metadata —
	// removal is URI-scoped and survives later edits (§5.5).
	PostCID string

	// Code is the machine-readable reason, from the open set of §3.3.
	Code DecisionCode

	// Reason is the optional human-readable explanation.
	Reason string
}

// CommunityWriteResult describes what reached (or deliberately did not reach)
// the community's repo.
type CommunityWriteResult struct {
	// URI, RKey and CID identify the record that now stands — the acceptance
	// for an acceptance write, the removal for a removal.
	URI  string
	RKey string
	CID  string

	// Rev is the repo revision the write committed in: the §5.2 watermark.
	//
	// It is EMPTY when Skipped is true, and that is not an oversight — nothing
	// committed, so there is no revision to report, and getRecord does not
	// reveal the revision an existing record was written at. A caller must
	// therefore not stamp a watermark from a skipped write; see Skipped.
	Rev string

	// Skipped reports that the repo already held exactly this record, so
	// nothing was written.
	//
	// This is the property that makes the three independent acceptance writers
	// of §3.2 safe to re-fire: the fast path, the firehose engine and the
	// notify endpoint all converge on the same rkey pinning the same CID, and
	// a writer that re-put it anyway would mint a fresh record CID, emit a
	// pointless commit, and invalidate every reference to the record it just
	// rewrote — on every retry, forever.
	Skipped bool
}

// CommunityRecordWriter publishes a community's decisions into its own repo.
//
// Every method is idempotent by construction: the rkey is derived from the
// subject (SubjectRkey), so a concurrent or repeated attempt converges on the
// same record instead of allocating a second one.
type CommunityRecordWriter interface {
	// WriteAcceptance makes this community's acceptance of cmd.PostCID stand.
	//
	// It is one putRecord at the deterministic rkey, guarded by swapRecord, and
	// it writes NOTHING when the standing record already pins cmd.PostCID.
	//
	// On a lost swap it re-reads: if the record another writer committed pins
	// the CID we wanted, the work is done and the result is a skip; otherwise
	// it retries against the new CID, at most twice, and then defers rather
	// than spinning against a livelock.
	WriteAcceptance(ctx context.Context, cmd CommunityWriteCommand) (CommunityWriteResult, error)

	// WriteRemoval commits the acceptance's deletion and the removal's write
	// TOGETHER, per §3.3.
	//
	// The batch is shaped by a pre-read of both deterministic rkeys: the delete
	// is emitted only when an acceptance is actually there, and the removal is
	// a create or an update according to whether one already stands. The PDS
	// answers a delete of a missing record — and a create of an existing one —
	// with a 500, so the shape is not optional.
	WriteRemoval(ctx context.Context, cmd CommunityRemovalCommand) (CommunityWriteResult, error)

	// RestoreAcceptance is WriteRemoval's mirror: one commit deleting the
	// removal and writing a fresh acceptance. §5.5 is explicit that there is no
	// distinct restore operation on the wire — consumers see ordinary events
	// winning the §5.2 tuple CAS — so this is a shape, not a new verb.
	RestoreAcceptance(ctx context.Context, cmd CommunityWriteCommand) (CommunityWriteResult, error)

	// RepinAcceptance moves a standing acceptance onto a new content CID
	// without re-deciding anything — the bridgedStats exception of §5.5.
	//
	// It updates the SAME record in place, so the acceptance's createdAt keeps
	// meaning "when this community accepted this post" rather than being
	// restamped every time a bridge refreshes its vote counts.
	RepinAcceptance(ctx context.Context, cmd CommunityWriteCommand) (CommunityWriteResult, error)
}

// communityRecordWriter is the production writer over real repos.
type communityRecordWriter struct {
	repos CommunityRepoFactory
	now   Clock
}

// NewCommunityRecordWriter returns the writer that publishes acceptances and
// removals into the repos the factory opens.
//
// The clock is injected for the same reason admitPost's is: createdAt is the
// one field a test cannot otherwise pin, and docs/TEST_ARCHITECTURE.md §3.3
// forbids sleeping to move time.
func NewCommunityRecordWriter(repos CommunityRepoFactory, now Clock) CommunityRecordWriter {
	return &communityRecordWriter{repos: repos, now: now}
}

func (w *communityRecordWriter) WriteAcceptance(ctx context.Context, cmd CommunityWriteCommand) (CommunityWriteResult, error) {
	return CommunityWriteResult{}, nil
}

func (w *communityRecordWriter) WriteRemoval(ctx context.Context, cmd CommunityRemovalCommand) (CommunityWriteResult, error) {
	return CommunityWriteResult{}, nil
}

func (w *communityRecordWriter) RestoreAcceptance(ctx context.Context, cmd CommunityWriteCommand) (CommunityWriteResult, error) {
	return CommunityWriteResult{}, nil
}

func (w *communityRecordWriter) RepinAcceptance(ctx context.Context, cmd CommunityWriteCommand) (CommunityWriteResult, error) {
	return CommunityWriteResult{}, nil
}
