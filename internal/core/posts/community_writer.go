package posts

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"

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
	// PostV2Collection is the AUTHOR-repo collection a post record lives in
	// under author-owned posts (§3.1) — the successor to the deprecated
	// community-repo social.coves.community.post.
	//
	// It lives beside the two community-repo collections because the three are
	// one vocabulary: an acceptance's subject is a record in this collection,
	// and the ingestion consumer re-exports this constant rather than declaring
	// its own so that the reader and the writer cannot come to disagree about
	// what a post record is called.
	PostV2Collection = "social.coves.community.postv2"

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

	// Rev is a repo revision the caller may stamp as the §5.2 watermark.
	//
	// For a write that committed, it is the revision the commit landed in. For
	// a SKIPPED write it is the repo's HEAD revision, read BEFORE the pre-read
	// that found the standing record — the catch-up watermark. That is safe to
	// stamp because the standing record proves what the repo says about this
	// subject as of the pre-read: a standing acceptance pinning the target CID
	// means no subject-scoped community event lies between the acceptance's
	// commit and that head (a removal would have deleted the record; a repin
	// would pin a different CID), so a row stranded by an earlier failed stamp
	// is caught up rather than left pending forever. Reading the head BEFORE
	// the records keeps the rev conservative — a removal committing between
	// the two reads has a rev strictly greater than the stamp, so its firehose
	// event still applies.
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

	// DeleteAcceptance withdraws this community's acceptance WITHOUT writing a
	// removal, which is the one shape the other four cannot express.
	//
	// It exists for the author's own deletion (§5.3): the author tombstones
	// their post, and the community's acceptance now points at a record that no
	// longer exists. Leaving it standing means the community's repo — the
	// curated index its whole portability argument rests on — permanently cites
	// content nobody can fetch, and any peer replaying that CAR would show a
	// post the author withdrew.
	//
	// IT IS NOT A REMOVAL, and conflating the two would be a factual error the
	// firehose carries forever. A removal record is a MODERATION act, signed by
	// the community, carrying a reason code, portable and auditable. An author
	// deleting their own post is not the community judging anything, and
	// publishing a removal for it would put a moderation event in the public
	// record that never happened.
	//
	// Deleting a record that is not there is a no-op reported as a skip, not an
	// error: the sweep is idempotent by necessity — every tombstone event may be
	// redelivered, and the acceptance may already have been withdrawn by an
	// earlier pass.
	DeleteAcceptance(ctx context.Context, cmd CommunityAcceptanceDeleteCommand) (CommunityWriteResult, error)
}

// CommunityAcceptanceDeleteCommand withdraws an acceptance.
//
// It carries no CID, deliberately. Every other command pins one because it is
// making a claim about a specific version; this one is undoing a claim, and the
// subject it is undoing it for is identified by URI — the same URI the
// deterministic rkey is derived from. A CID here would suggest the delete is
// conditional on a version, which it is not: the post is gone, whatever version
// the acceptance happened to pin.
type CommunityAcceptanceDeleteCommand struct {
	CommunityDID string
	PostURI      string
}

// communityRecordWriter is the production writer over real repos.
type communityRecordWriter struct {
	repos CommunityRepoFactory
	now   Clock
	sleep func(ctx context.Context, d time.Duration) error
}

// WriterOption configures a CommunityRecordWriter.
type WriterOption func(*communityRecordWriter)

// WithSwapRetrySleeper replaces the pause between swap retries.
//
// Injected for the same reason the clock is: docs/TEST_ARCHITECTURE.md §3.3
// forbids a test from actually sleeping, so a test hands in a recorder and
// asserts on the durations instead of waiting through them.
func WithSwapRetrySleeper(sleep func(ctx context.Context, d time.Duration) error) WriterOption {
	return func(w *communityRecordWriter) { w.sleep = sleep }
}

// NewCommunityRecordWriter returns the writer that publishes acceptances and
// removals into the repos the factory opens.
//
// The clock is injected for the same reason admitPost's is: createdAt is the
// one field a test cannot otherwise pin, and docs/TEST_ARCHITECTURE.md §3.3
// forbids sleeping to move time.
func NewCommunityRecordWriter(repos CommunityRepoFactory, now Clock, opts ...WriterOption) CommunityRecordWriter {
	w := &communityRecordWriter{repos: repos, now: now, sleep: sleepWithContext}
	for _, opt := range opts {
		opt(w)
	}
	return w
}

// sleepWithContext is the production pause: a timer that a cancelled context
// cuts short, so a shutting-down worker is not held hostage by a backoff.
func sleepWithContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// ErrRemovalStands reports that an acceptance write met a standing removal
// record for its subject.
//
// §5.5 makes removal terminal: the ONLY sanctioned exit is a moderator restore,
// which deletes the removal in the same commit that writes the fresh acceptance
// (RestoreAcceptance). An acceptance created OVER a standing removal would leave
// both records live at once, and every consumer ordering by the §5.2 tuple
// would see the acceptance outrank the older removal — a moderated post
// laundered back into feeds by a retry. The writer therefore refuses, and the
// engine classifies the refusal as a deferral: the row is owed a decision, but
// not this one.
var ErrRemovalStands = errors.New("a removal record stands for this subject")

// swapRetryLimit is how many times a writer re-reads and re-shapes after losing
// an optimistic guard before it gives up and lets the caller try again later.
//
// It is bounded because a lost swap means somebody ELSE is writing this same
// record, and the only writers that can be are the three acceptance writers of
// §3.2 — all of which converge on the same rkey. Two retries is enough to
// absorb a real race; an unbounded loop against a community whose repo is
// genuinely churning would spin a queue worker against a PDS instead of
// deferring the subject and moving on.
const swapRetryLimit = 2

// swapRetryBaseDelay is the first retry's backoff ceiling. Each further retry
// doubles it.
const swapRetryBaseDelay = 25 * time.Millisecond

// backoff pauses before retry number `attempt` (zero-based), for a jittered
// duration in (0, base<<attempt].
//
// The jitter is the point, not a refinement: the writers that lose a swap to
// each other are the three acceptance writers of §3.2 converging on the SAME
// rkey, so retries that waited a fixed interval would collide again on
// schedule. Full jitter decorrelates them cheaply. This is only a courtesy
// between racing writers, though — actually serializing the work is the
// per-community queue, which is task 5's job, not this backoff's.
func (w *communityRecordWriter) backoff(ctx context.Context, attempt int) error {
	ceiling := swapRetryBaseDelay << attempt
	if err := w.sleep(ctx, time.Duration(1+rand.Int64N(int64(ceiling)))); err != nil {
		return fmt.Errorf("waiting to retry a lost swap: %w", err)
	}
	return nil
}

// standingRecord is what a pre-read found in the community's repo, reduced to
// the four things a writer shapes its commit from.
//
// It is read out of the decoded record rather than parsed into a typed struct
// because the writer only ever compares these fields; a full decode would give
// it the chance to fail on a record some other build wrote.
type standingRecord struct {
	// CID is the RECORD's own CID — the swapRecord guard, not the subject.
	CID string

	// SubjectCID is the content CID the record's strongRef pins. For an
	// acceptance this is the whole point: an acceptance pinning the CID we want
	// is one we must not rewrite.
	SubjectCID string

	// CreatedAt is carried forward onto every update. An acceptance's createdAt
	// means "when this community accepted this post", so restamping it on a
	// re-acceptance or a repin would rewrite history every time a bridge
	// refreshed its vote counts — and would give two writers racing to the same
	// outcome two different record CIDs.
	CreatedAt string

	// Code and Reason are the removal's decision, and empty on an acceptance.
	Code   string
	Reason string
}

// acceptanceMode says what an ABSENT acceptance record means to the caller.
type acceptanceMode int

const (
	// acceptanceMayCreate: absence is the ordinary case — this is the first
	// acceptance of the subject — and the writer creates one.
	acceptanceMayCreate acceptanceMode = iota

	// acceptanceMustExist: a repin moves a STANDING acceptance onto new content
	// and re-decides nothing, so it has no authority to create one. An absent
	// record means the AppView's row and the community's repo disagree, and
	// silently minting an acceptance nobody decided is the one thing a path that
	// skips admission must never do.
	acceptanceMustExist
)

func (w *communityRecordWriter) WriteAcceptance(ctx context.Context, cmd CommunityWriteCommand) (CommunityWriteResult, error) {
	return w.pinAcceptance(ctx, cmd, acceptanceMayCreate)
}

func (w *communityRecordWriter) RepinAcceptance(ctx context.Context, cmd CommunityWriteCommand) (CommunityWriteResult, error) {
	return w.pinAcceptance(ctx, cmd, acceptanceMustExist)
}

// DeleteAcceptance withdraws a standing acceptance and writes nothing in its
// place.
//
// STATE-SHAPED, like every other writer here and for the same reason task 4
// recorded: the PDS has no tolerant delete, and answers a delete of a missing
// record with a 500. So absence is read first and reported as a skip. That is
// not an edge case — it is the COMMON one, because the sweep fires on every
// tombstone event and most posts a community sees were never accepted by it,
// and because the connector rewinds its cursor after every reconnect so each
// tombstone arrives at least twice.
//
// It is a batch of one rather than a putRecord-shaped call because applyWrites
// is where a delete can be guarded by swapCommit: the pre-read that found the
// acceptance is only true until somebody else writes, and the guard is what
// turns a concurrent restore into a detected conflict instead of a silently
// deleted fresh acceptance.
func (w *communityRecordWriter) DeleteAcceptance(ctx context.Context, cmd CommunityAcceptanceDeleteCommand) (CommunityWriteResult, error) {
	if err := validateAcceptanceDeleteCommand(cmd); err != nil {
		return CommunityWriteResult{}, err
	}

	repo, err := w.openRepo(ctx, cmd.CommunityDID)
	if err != nil {
		return CommunityWriteResult{}, err
	}

	rkey := SubjectRkey(cmd.PostURI)
	uri := recordURI(repo.DID(), AcceptanceCollection, rkey)

	for attempt := 0; ; attempt++ {
		// The head is read before the record, for the same reason commitPair
		// reads it first: a swapCommit read afterwards could be newer than the
		// state the batch was shaped from, guarding the commit against a
		// revision that already contains the change the shape assumed absent.
		head, err := repo.GetLatestCommit(ctx)
		if err != nil {
			return CommunityWriteResult{}, fmt.Errorf("reading the head of %s: %w", cmd.CommunityDID, err)
		}

		standing, err := readStandingRecord(ctx, repo, AcceptanceCollection, rkey)
		if err != nil {
			return CommunityWriteResult{}, err
		}
		if standing == nil {
			// Nothing to withdraw. The head is still reported as the Rev, on the
			// same catch-up reasoning the other writers' skips use: a row
			// stranded by an earlier failed stamp can be caught up from it.
			return CommunityWriteResult{URI: uri, RKey: rkey, Rev: head.Rev, Skipped: true}, nil
		}

		result, err := repo.ApplyWrites(ctx, []pds.Write{{
			Op:         pds.WriteOpDelete,
			Collection: AcceptanceCollection,
			RKey:       rkey,
		}}, head.CID)
		if err == nil {
			// No CID: a delete leaves no record to name. The Rev is the §5.2
			// watermark the firehose copy of this same deletion is compared
			// against, so it is the one field that must be here.
			return CommunityWriteResult{URI: uri, RKey: rkey, Rev: result.CommitRev}, nil
		}

		// A lost swapCommit and a 500 are the same fact from two directions: the
		// state this batch was shaped from is not the state the PDS is in. Both
		// are answered by reading again, never by resending the same shape —
		// here that matters most for the 500, which is what a delete of a record
		// somebody else removed between the pre-read and the commit looks like.
		staleShape := errors.Is(err, pds.ErrSwapConflict) || errors.Is(err, pds.ErrServerError)
		if !staleShape || attempt >= swapRetryLimit {
			return CommunityWriteResult{}, fmt.Errorf("withdrawing the acceptance of %s in %s: %w",
				cmd.PostURI, cmd.CommunityDID, err)
		}
		if err := w.backoff(ctx, attempt); err != nil {
			return CommunityWriteResult{}, fmt.Errorf("withdrawing the acceptance of %s in %s: %w",
				cmd.PostURI, cmd.CommunityDID, err)
		}
	}
}

// pinAcceptance makes an acceptance of cmd.PostCID stand at the subject's rkey.
//
// THE PRE-READ DECIDES EVERYTHING. Whether there is work to do at all, what the
// put is guarded against, and what createdAt it carries are all read out of the
// repo rather than assumed, because the same record has three independent
// writers (§3.2) and every one of them retries.
//
// EVERY PUT IS GUARDED, including the first. A create is sent with an empty
// swapRecord, which pds.PutRecordWithCommit spells on the wire as "there must be
// no record here yet" — so a writer that lost the race between its pre-read and
// its put is told, rather than silently clobbering the winner's record.
func (w *communityRecordWriter) pinAcceptance(ctx context.Context, cmd CommunityWriteCommand, mode acceptanceMode) (CommunityWriteResult, error) {
	if err := validateWriteCommand(cmd); err != nil {
		return CommunityWriteResult{}, err
	}

	repo, err := w.openRepo(ctx, cmd.CommunityDID)
	if err != nil {
		return CommunityWriteResult{}, err
	}

	rkey := SubjectRkey(cmd.PostURI)
	uri := recordURI(repo.DID(), AcceptanceCollection, rkey)

	for attempt := 0; ; attempt++ {
		// THE HEAD IS READ FIRST, before either record, for the same reason
		// commitPair reads it first: it is the catch-up watermark a skip
		// reports, and a head read AFTER the records could include a removal
		// that committed between the two — stamping that head would outrank the
		// removal's own firehose event and freeze the row `accepted` over a
		// removed post. Read first, the rev is at worst conservative: any later
		// subject event carries a strictly greater rev and still applies.
		head, err := repo.GetLatestCommit(ctx)
		if err != nil {
			return CommunityWriteResult{}, fmt.Errorf("reading the head of %s: %w", cmd.CommunityDID, err)
		}

		standing, err := readStandingRecord(ctx, repo, AcceptanceCollection, rkey)
		if err != nil {
			return CommunityWriteResult{}, err
		}

		// THE REMOVAL GUARD (§5.5). Removal is terminal, and its ONLY
		// sanctioned exit is a moderator restore — which deletes the removal in
		// the same commit that writes the fresh acceptance (RestoreAcceptance).
		// An acceptance put over a standing removal would leave both records
		// live, and its younger watermark would outrank the removal at every
		// consumer: a moderated post laundered back into feeds by a retry. So
		// every pass through this loop — the pre-read AND each convergence
		// re-read after a lost swap — checks the removal rkey and refuses.
		//
		// It is read AFTER the acceptance to keep the window between this check
		// and the put as narrow as it can be; the removal commit deletes any
		// standing acceptance, so a removal landing after this read still
		// surfaces as ErrSwapConflict on the put, and the next iteration's
		// re-read is where it is caught.
		removal, err := readStandingRecord(ctx, repo, RemovalCollection, rkey)
		if err != nil {
			return CommunityWriteResult{}, err
		}
		if removal != nil {
			return CommunityWriteResult{}, fmt.Errorf(
				"writing the acceptance of %s in %s: %w: §5.5 sanctions no exit from removal except a restore",
				cmd.PostURI, cmd.CommunityDID, ErrRemovalStands)
		}

		if standing == nil && mode == acceptanceMustExist {
			return CommunityWriteResult{}, fmt.Errorf(
				"repinning the acceptance of %s in %s: %w: no acceptance record stands at %s, so there is nothing to repin",
				cmd.PostURI, cmd.CommunityDID, pds.ErrNotFound, uri)
		}

		// ALREADY DONE. Re-putting an identical record would mint a fresh record
		// CID, emit a commit that decided nothing, and invalidate every
		// reference to the acceptance it just rewrote — on every retry, forever.
		// The skip reports the head as its Rev — see CommunityWriteResult.Rev —
		// so a row stranded by an earlier failed stamp can be caught up.
		if standing != nil && standing.SubjectCID == cmd.PostCID {
			return CommunityWriteResult{URI: uri, RKey: rkey, CID: standing.CID, Rev: head.Rev, Skipped: true}, nil
		}

		swapRecord, createdAt := "", w.stamp()
		if standing != nil {
			swapRecord = standing.CID
			if standing.CreatedAt != "" {
				createdAt = standing.CreatedAt
			}
		}

		commit, err := repo.PutRecordWithCommit(ctx, AcceptanceCollection, rkey,
			acceptanceRecord(cmd.PostURI, cmd.PostCID, createdAt), swapRecord)
		if err == nil {
			return CommunityWriteResult{URI: commit.URI, RKey: rkey, CID: commit.CID, Rev: commit.CommitRev}, nil
		}

		if !errors.Is(err, pds.ErrSwapConflict) || attempt >= swapRetryLimit {
			return CommunityWriteResult{}, fmt.Errorf("writing the acceptance of %s in %s: %w",
				cmd.PostURI, cmd.CommunityDID, err)
		}
		// Lost the race. Back off, then loop: re-read what the winner actually
		// wrote, and either discover the work is done or aim at the new record.
		if err := w.backoff(ctx, attempt); err != nil {
			return CommunityWriteResult{}, fmt.Errorf("writing the acceptance of %s in %s: %w",
				cmd.PostURI, cmd.CommunityDID, err)
		}
	}
}

func (w *communityRecordWriter) WriteRemoval(ctx context.Context, cmd CommunityRemovalCommand) (CommunityWriteResult, error) {
	if err := validateRemovalCommand(cmd); err != nil {
		return CommunityWriteResult{}, err
	}

	return w.commitPair(ctx, pairCommit{
		communityDID:    cmd.CommunityDID,
		postURI:         cmd.PostURI,
		standCollection: RemovalCollection,
		clearCollection: AcceptanceCollection,
		record: func(createdAt string) map[string]any {
			return removalRecord(cmd, createdAt)
		},
		// A removal is URI-SCOPED: the pinned CID is audit metadata recording
		// the version present when the post was removed, and the removal applies
		// to the post across later edits (§5.5). So a standing removal carrying
		// the same decision is already the answer, and rewriting it to pin a
		// newer CID would churn the record's CID on every re-fire while changing
		// nothing anyone reads. The DECISION is what has to match.
		unchanged: func(standing *standingRecord) bool {
			return standing.Code == string(cmd.Code) && standing.Reason == cmd.Reason
		},
	})
}

func (w *communityRecordWriter) RestoreAcceptance(ctx context.Context, cmd CommunityWriteCommand) (CommunityWriteResult, error) {
	if err := validateWriteCommand(cmd); err != nil {
		return CommunityWriteResult{}, err
	}

	return w.commitPair(ctx, pairCommit{
		communityDID:    cmd.CommunityDID,
		postURI:         cmd.PostURI,
		standCollection: AcceptanceCollection,
		clearCollection: RemovalCollection,
		record: func(createdAt string) map[string]any {
			return acceptanceRecord(cmd.PostURI, cmd.PostCID, createdAt)
		},
		unchanged: func(standing *standingRecord) bool {
			return standing.SubjectCID == cmd.PostCID
		},
	})
}

// pairCommit is the shape both moderation commits have: one record is made to
// stand and its opposite is cleared, TOGETHER, so the firehose never carries a
// half-completed moderation action (§3.3).
//
// A removal and a restore are the same commit with the two collections swapped,
// which is exactly what §5.5 means by "there is no distinct restore operation on
// the wire" — consumers see ordinary events winning the §5.2 tuple CAS.
type pairCommit struct {
	communityDID string
	postURI      string

	// standCollection holds the record this commit makes stand.
	standCollection string

	// clearCollection holds the record this commit deletes, IF one is there.
	// The delete is emitted only on presence: the PDS answers a delete of a
	// missing record with a 500 and refuses the whole batch with it.
	clearCollection string

	// record builds the body to write, stamped with the given createdAt.
	record func(createdAt string) map[string]any

	// unchanged reports whether the standing record already says what this
	// commit would say, so an identical re-fire writes nothing.
	unchanged func(standing *standingRecord) bool
}

// commitPair reads the subject's two records, shapes one commit from what it
// found, and applies it.
//
// THE SHAPE IS NOT OPTIONAL. applyWrites has no upsert and no tolerant delete:
// a create of an existing record and a delete of a missing one are both a 500
// that takes the whole batch down with them. So presence chooses delete-or-not
// and create-or-update, and a pre-read that went stale under a concurrent writer
// is met the same way a lost swap is — by reading again and re-shaping.
func (w *communityRecordWriter) commitPair(ctx context.Context, spec pairCommit) (CommunityWriteResult, error) {
	repo, err := w.openRepo(ctx, spec.communityDID)
	if err != nil {
		return CommunityWriteResult{}, err
	}

	rkey := SubjectRkey(spec.postURI)
	uri := recordURI(repo.DID(), spec.standCollection, rkey)

	for attempt := 0; ; attempt++ {
		// THE HEAD IS READ FIRST, before the records the batch is shaped from.
		// A swapCommit read afterwards could be NEWER than the state that shaped
		// the batch, which would guard the commit against a revision that
		// already contains the change the shape assumed was absent. Read first,
		// and any interleaved write makes the guard stale — a detected conflict
		// rather than a silent clobber.
		head, err := repo.GetLatestCommit(ctx)
		if err != nil {
			return CommunityWriteResult{}, fmt.Errorf("reading the head of %s: %w", spec.communityDID, err)
		}

		standing, err := readStandingRecord(ctx, repo, spec.standCollection, rkey)
		if err != nil {
			return CommunityWriteResult{}, err
		}
		toClear, err := readStandingRecord(ctx, repo, spec.clearCollection, rkey)
		if err != nil {
			return CommunityWriteResult{}, err
		}

		// Nothing to clear and the standing record already says it: a re-fire of
		// a commit that has already landed writes nothing, for the same reason
		// an identical acceptance is not re-put. The skip reports the head as
		// its Rev — the catch-up watermark of CommunityWriteResult.Rev — so a
		// row stranded by an earlier failed stamp is caught up on the re-fire.
		if toClear == nil && standing != nil && spec.unchanged(standing) {
			return CommunityWriteResult{URI: uri, RKey: rkey, CID: standing.CID, Rev: head.Rev, Skipped: true}, nil
		}

		writes := make([]pds.Write, 0, 2)
		if toClear != nil {
			writes = append(writes, pds.Write{
				Op:         pds.WriteOpDelete,
				Collection: spec.clearCollection,
				RKey:       rkey,
			})
		}

		op, createdAt := pds.WriteOpCreate, w.stamp()
		if standing != nil {
			op = pds.WriteOpUpdate
			if standing.CreatedAt != "" {
				createdAt = standing.CreatedAt
			}
		}
		writes = append(writes, pds.Write{
			Op:         op,
			Collection: spec.standCollection,
			RKey:       rkey,
			Record:     spec.record(createdAt),
		})

		result, err := repo.ApplyWrites(ctx, writes, head.CID)
		if err == nil {
			return CommunityWriteResult{
				URI:  uri,
				RKey: rkey,
				CID:  standCIDOf(result, len(writes)-1),
				Rev:  result.CommitRev,
			}, nil
		}

		// A lost swapCommit and a 500 are the same fact from two directions: the
		// state this batch was shaped from is not the state the PDS is in. Both
		// are answered by reading again, never by resending the same shape.
		staleShape := errors.Is(err, pds.ErrSwapConflict) || errors.Is(err, pds.ErrServerError)
		if !staleShape || attempt >= swapRetryLimit {
			return CommunityWriteResult{}, fmt.Errorf("committing %s for %s in %s: %w",
				spec.standCollection, spec.postURI, spec.communityDID, err)
		}
		if err := w.backoff(ctx, attempt); err != nil {
			return CommunityWriteResult{}, fmt.Errorf("committing %s for %s in %s: %w",
				spec.standCollection, spec.postURI, spec.communityDID, err)
		}
	}
}

// openRepo opens the community's repo and proves it is the one that was asked
// for.
//
// The DID check is not paranoia about the factory: the repo's DID is the
// AUTHORITY half of every record URI this writer produces, so a factory that
// handed back the wrong session would have one community vouching for a post
// with another community's key, and the resulting acceptance would look
// perfectly valid to every consumer on the network.
func (w *communityRecordWriter) openRepo(ctx context.Context, communityDID string) (CommunityRepo, error) {
	repo, err := w.repos(ctx, communityDID)
	if err != nil {
		return nil, fmt.Errorf("opening the repo of community %s: %w", communityDID, err)
	}
	if repo == nil {
		return nil, fmt.Errorf("opening the repo of community %s: the factory returned no client", communityDID)
	}
	if repo.DID() != communityDID {
		return nil, fmt.Errorf("opening the repo of community %s: the factory returned a session on %s instead",
			communityDID, repo.DID())
	}
	return repo, nil
}

// stamp is the createdAt a newly written record carries.
func (w *communityRecordWriter) stamp() string {
	return w.now().UTC().Format(time.RFC3339)
}

// readStandingRecord returns what stands at a rkey, or nil when nothing does.
//
// An absent record is a VALUE here rather than an error, because absence is
// half of what the shape is chosen from: it is the difference between a create
// and an update, and between a batch that carries a delete and one that must
// not.
func readStandingRecord(ctx context.Context, repo CommunityRepo, collection, rkey string) (*standingRecord, error) {
	response, err := repo.GetRecord(ctx, collection, rkey)
	if err != nil {
		if errors.Is(err, pds.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s/%s from %s: %w", collection, rkey, repo.DID(), err)
	}
	if response == nil {
		return nil, nil
	}

	standing := &standingRecord{CID: response.CID}
	if subject, ok := response.Value["subject"].(map[string]any); ok {
		standing.SubjectCID, _ = subject["cid"].(string)
	}
	standing.CreatedAt, _ = response.Value["createdAt"].(string)
	standing.Code, _ = response.Value["code"].(string)
	standing.Reason, _ = response.Value["reason"].(string)
	return standing, nil
}

// standCIDOf picks the written record's CID out of a batch's results.
//
// Results are POSITIONAL — the lexicon returns one per submitted write, in
// order — so the record this commit made stand is the last one. The transport
// (pds.ApplyWrites) refuses a success whose results are short or whose
// create/update entries lack uri/cid, so the bounds check here is defensive
// only.
func standCIDOf(result *pds.ApplyWritesResult, index int) string {
	if result == nil || index < 0 || index >= len(result.Results) {
		return ""
	}
	return result.Results[index].CID
}

// recordURI is the AT-URI of a record in a repo. The authority is the repo's
// own DID, which for these two collections is the COMMUNITY — an acceptance in
// the author's repo would be an author vouching for themselves.
func recordURI(repoDID, collection, rkey string) string {
	return "at://" + repoDID + "/" + collection + "/" + rkey
}

// acceptanceRecord is a social.coves.community.acceptance body. The community
// is implicit in the repo it lands in, which is why it is not a field.
func acceptanceRecord(postURI, postCID, createdAt string) map[string]any {
	return map[string]any{
		"$type":     AcceptanceCollection,
		"subject":   map[string]any{"uri": postURI, "cid": postCID},
		"createdAt": createdAt,
	}
}

// removalRecord is a social.coves.community.removal body.
func removalRecord(cmd CommunityRemovalCommand, createdAt string) map[string]any {
	record := map[string]any{
		"$type":     RemovalCollection,
		"subject":   map[string]any{"uri": cmd.PostURI, "cid": cmd.PostCID},
		"code":      string(cmd.Code),
		"createdAt": createdAt,
	}
	// reason is optional in the lexicon, and an empty string is not the same
	// thing as an absent one: a client rendering #removedPost would show an
	// explanation that says nothing.
	if cmd.Reason != "" {
		record["reason"] = cmd.Reason
	}
	return record
}

// removalCodeMaxLength is the removal lexicon's maxLength for `code`
// (internal/atproto/lexicon/social/coves/community/removal.json), in BYTES —
// which is what a lexicon maxLength counts.
const removalCodeMaxLength = 64

// validateSubjectURI refuses a subject that is not a parseable at:// URI.
//
// EVERY RECORD THESE WRITERS SEND GOES OUT WITH validate:false — the PDS has
// never been taught Coves lexicons, so it checks nothing. What this process
// sends is exactly what the firehose carries under the community's signature,
// and a subject that is not an AT-URI would be a malformed strongRef every
// conformant consumer is entitled to refuse. The engine only ever hands the
// writers URIs read from its own admission rows, so a violation here is a
// programming error, refused before any network call.
func validateSubjectURI(kind, postURI string) error {
	if _, err := syntax.ParseATURI(postURI); err != nil {
		return fmt.Errorf("%s: %w", kind, NewValidationError("postURI",
			fmt.Sprintf("must be a parseable at:// URI (%v) — the record embeds it in a strongRef and is sent with validate:false, so nothing downstream re-checks it", err)))
	}
	return nil
}

// validateWriteCommand refuses an acceptance that would pin nothing.
//
// A strongRef without a CID is the one thing an acceptance may not be: the
// pinned CID IS the guarantee, and an acceptance naming only a URI would render
// whatever the author put there most recently.
func validateWriteCommand(cmd CommunityWriteCommand) error {
	switch {
	case cmd.CommunityDID == "":
		return fmt.Errorf("acceptance write: %w", NewValidationError("communityDID", "is required"))
	case cmd.PostURI == "":
		return fmt.Errorf("acceptance write: %w", NewValidationError("postURI", "is required"))
	case cmd.PostCID == "":
		return fmt.Errorf("acceptance write: %w", NewValidationError("postCID",
			"is required — an acceptance's subject is a strongRef, and one without a CID pins nothing"))
	}
	return validateSubjectURI("acceptance write", cmd.PostURI)
}

// validateAcceptanceDeleteCommand refuses a withdrawal that names no repo or no
// subject.
//
// The subject check is not symmetry with the other writers — it is the point.
// The rkey is a DIGEST of the subject URI, so a malformed or empty subject
// hashes to a perfectly well-formed key pointing at something else entirely,
// and a delete aimed at the wrong rkey in a community's own repo is a WRITE. A
// validation that only the create paths performed would leave the one operation
// that destroys data unchecked.
func validateAcceptanceDeleteCommand(cmd CommunityAcceptanceDeleteCommand) error {
	switch {
	case cmd.CommunityDID == "":
		return fmt.Errorf("acceptance withdrawal: %w", NewValidationError("communityDID", "is required"))
	case cmd.PostURI == "":
		return fmt.Errorf("acceptance withdrawal: %w", NewValidationError("postURI",
			"is required — the record key is derived from it, so an empty subject deletes a well-formed key belonging to nothing"))
	}
	return validateSubjectURI("acceptance withdrawal", cmd.PostURI)
}

// validateRemovalCommand refuses a removal with no reason code. `code` is
// required by the lexicon and is what a client renders in #removedPost and what
// the author is told.
func validateRemovalCommand(cmd CommunityRemovalCommand) error {
	switch {
	case cmd.CommunityDID == "":
		return fmt.Errorf("removal write: %w", NewValidationError("communityDID", "is required"))
	case cmd.PostURI == "":
		return fmt.Errorf("removal write: %w", NewValidationError("postURI", "is required"))
	case cmd.PostCID == "":
		return fmt.Errorf("removal write: %w", NewValidationError("postCID",
			"is required — it records the version present at removal time"))
	case cmd.Code == "":
		return fmt.Errorf("removal write: %w", NewValidationError("code", "is required"))
	case len(cmd.Code) > removalCodeMaxLength:
		// Sent with validate:false, so the PDS would happily commit a longer
		// one — and every conformant consumer could then refuse the record this
		// community signed.
		return fmt.Errorf("removal write: %w", NewValidationError("code",
			fmt.Sprintf("is %d bytes; the removal lexicon caps code at %d (maxLength)", len(cmd.Code), removalCodeMaxLength)))
	}
	return validateSubjectURI("removal write", cmd.PostURI)
}
