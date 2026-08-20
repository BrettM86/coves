package posts

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"

	covesoauth "Coves/internal/atproto/oauth"
	"Coves/internal/core/blobs"
)

// The re-materialization tool: the cutover step that moves every legacy
// social.coves.community.post record (written into a COMMUNITY's repo under the
// community's credentials) to an author-owned social.coves.community.postv2 in
// the AUTHOR's repo, plus the community's acceptance that pins it
// (docs/PRD_AUTHOR_OWNED_POSTS.md §10.1 step 5 and §11 the rev-2.8 deploy
// runbook).
//
// # THIS CODE DELETES PRODUCTION USER DATA IRREVERSIBLY
//
// Read that sentence before changing anything below it. The tool's only defence
// against destroying a post is that it VERIFIES, with fresh reads, that a
// replacement stands — and it re-verifies on every path, including a resumed
// one, because a ledger row is a memory of a past truth and a delete needs a
// present one.
//
// It is exercised by:
//   - internal/core/posts/rematerialize_rkey_test.go  (T0: the rkey derivation,
//     including the golden values that pin it ACROSS processes)
//   - internal/core/posts/rematerialize_credentials_test.go (T0: the terminal
//     vs. retryable credential split)
//   - internal/core/posts/rematerialize_test.go       (T1: the state machine
//     against a real migration-037 ledger and faked repos)
//   - internal/core/posts/rematerialize_guard_test.go (T1: the pre-delete
//     verification guards and the crash/resume boundaries)
//   - internal/core/posts/rematerialize_outer_test.go (T1: the whole tool
//     against a REAL PDS and a real ledger)
//   - cmd/rematerialize-posts/*_test.go               (T0: the production
//     LegacySource, the scope wiring and the operator surface)
//
// # WHY THE LOGIC LIVES IN PACKAGE posts (not a subpackage)
//
// The tool's whole safety argument is that it REUSES the write path's two
// idempotent primitives rather than re-deriving them:
//
//   - createAuthorRecord (service.go) — the converge-by-read create: a re-run at
//     the same rkey meets its own first attempt (ErrSwapConflict / ErrNoCommit)
//     and reports the standing record instead of minting a second post. This is
//     what makes a crash-resumed run a no-op.
//   - communityRecordWriter.WriteAcceptance (community_writer.go) — the
//     deterministic-rkey acceptance write that SKIPS when the standing record
//     already pins the target CID, so a re-run emits no new commit and no new
//     CID.
//
// Both are package-private. A subpackage would have to either export them (widen
// the write path's surface for one caller) or reimplement them (the exact
// divergence the design forbids — a second rkey scheme mints duplicates). The
// cost is that package posts grows; the benefit is that the tool cannot drift
// from the write path it is draining into. cmd/rematerialize-posts/main.go stays
// a thin wrapper that only wires the seams below.

// RematerializeState is one legacy record's position in the ledger state machine
// (migration 037). The happy path is discovered → postv2_written → verified →
// migrated → done; the fallback state is terminal.
type RematerializeState string

const (
	// RematerializeDiscovered: the ledger row exists for this old URI and
	// nothing has been written yet.
	RematerializeDiscovered RematerializeState = "discovered"

	// RematerializePostV2Written: the postv2 record stands in the AUTHOR's repo
	// at the deterministic rkey. Its URI/CID/rkey are on the ledger row.
	RematerializePostV2Written RematerializeState = "postv2_written"

	// RematerializeVerified: the acceptance stands in the COMMUNITY's repo and
	// both records have been read back and confirmed to pin the same CID.
	//
	// IT IS NOT A LICENCE TO DELETE. It records that verification passed ONCE,
	// at a moment now in the past; the delete re-verifies from scratch.
	RematerializeVerified RematerializeState = "verified"

	// RematerializeMigrated is the CHECKPOINT BEFORE DELETE, distinct from done
	// on purpose: it means "verified safe to delete, and the OLD record is still
	// present". A crash after this checkpoint resumes by retrying ONLY the delete
	// (§11 step 4) — but that retry re-verifies first, for the same reason.
	RematerializeMigrated RematerializeState = "migrated"

	// RematerializeDone: the old community.post record has been deleted.
	RematerializeDone RematerializeState = "done"

	// RematerializeFallbackLeftLegacy is the terminal state for a record whose
	// author credentials could not be restored: the postv2 is NOT written, the
	// old record is NOT deleted, the row is logged. §11 step 3 is explicit that
	// the fallback NEVER forges authorship (no admin-signed postv2), because
	// forging reintroduces the exact §2 impersonation liability the whole flip
	// removes.
	//
	// IT IS TERMINAL BUT NOT IRREVERSIBLE. RematerializeLedger.ReopenFallback
	// moves such rows back to discovered, which is the supported recovery once
	// the operator has re-authorized the author — see its doc, and the header of
	// migration 037.
	//
	// There is exactly ONE fallback state on purpose. An earlier revision also
	// declared fallback_no_creds, and no code path ever wrote it: a vocabulary
	// entry nothing produces is a trap for whoever writes recovery SQL at 2am
	// against a state that cannot exist.
	RematerializeFallbackLeftLegacy RematerializeState = "fallback_left_legacy"
)

// IsFallback reports whether a state is a terminal fallback state, so the census
// can gate "complete" on any of them surviving without enumerating each string
// at every call site.
func IsFallback(state RematerializeState) bool {
	return state == RematerializeFallbackLeftLegacy
}

// LegacyPost is one deprecated social.coves.community.post record discovered for
// migration.
type LegacyPost struct {
	// URI is the OLD record's AT-URI — at://<communityDID>/social.coves.community.post/<rkey>.
	// It is the ledger's primary key and the material the deterministic postv2
	// rkey is derived from.
	URI string

	// CID is the old record's content CID AS OF THE READ THAT PRODUCED THIS
	// VALUE, and it is LOAD-BEARING, not audit trim.
	//
	// The postv2 is built from a body read at one instant; the delete happens
	// minutes to hours later. If an aggregator cron or a cached mobile session
	// lands an edit in between, deleting on the strength of the earlier read
	// destroys the newer content with no trace. So this CID is persisted on the
	// ledger row at the postv2 write, re-checked against a FRESH read
	// immediately before the delete, and passed to the PDS as the delete's swap
	// guard so the PDS itself refuses a stale delete.
	CID string

	// CommunityDID is the repo the old record lived in — the community whose
	// acceptance the tool writes.
	CommunityDID string

	// AuthorDID is the record's `author` field: the DID whose repo the postv2 is
	// written into. Under author-owned posts this field is dropped from the new
	// record (postV2From), but it is exactly who to re-author under.
	AuthorDID string

	// Record is the decoded legacy body.
	//
	// DEPRECATED, LOSSY: PostRecord omits published fields — langs, tags,
	// crosspostOf, crosspostChain, bridgedStats — so converting through it before
	// deleting the old record IRREVERSIBLY drops them (whole-branch review, P5).
	// The conversion runs off RawRecord instead; this field is carried only
	// because callers already populate it and the author DID is read from it.
	Record PostRecord

	// RawRecord is the legacy record EXACTLY as it stands in the community repo —
	// the lossless source the postv2 is built from. The conversion drops only the
	// `author` field and re-stamps `$type`; every other field (including langs,
	// tags, crosspostOf, crosspostChain, bridgedStats, facets, embed, labels) is
	// carried through byte-for-byte, and createdAt is preserved so the
	// re-materialized post keeps its original time.
	RawRecord map[string]any
}

// LegacySource enumerates, re-reads and deletes the deprecated community.post
// records.
//
// It is a seam so the T1 state machine runs against an in-memory source while
// the T2 contract and production run it against real community repos on the PDS
// (listRecords over social.coves.community.post, getRecord for the pre-delete
// re-read, delete via the community's own credentials).
type LegacySource interface {
	// ListLegacyPosts enumerates every legacy record in scope. The bodies it
	// returns are a SNAPSHOT: by the time a given record is processed they may be
	// stale, which is why ReadLegacyPost exists.
	ListLegacyPosts(ctx context.Context) ([]LegacyPost, error)

	// ReadLegacyPost re-reads ONE record as it stands right now. found is false
	// when the record is gone — which is a legitimate outcome on a resumed run
	// whose delete already landed, and a fatal surprise on a fresh one.
	ReadLegacyPost(ctx context.Context, uri string) (post LegacyPost, found bool, err error)

	// DeleteLegacyPost removes the old record, GUARDED by swapCID: the PDS must
	// refuse the delete if the record no longer carries that exact CID, so a
	// concurrent edit cannot be destroyed even if every check above it somehow
	// passed. An empty swapCID is not permitted and implementations must refuse
	// it — an unguarded delete is the failure mode this parameter exists to make
	// unrepresentable.
	//
	// The delete is idempotent by contract — a delete of an already-gone record
	// reports success — because it is the step a crash after the migrated
	// checkpoint retries.
	DeleteLegacyPost(ctx context.Context, legacy LegacyPost, swapCID string) error
}

// RematerializeLedgerRow is one row of the migration-037 ledger.
type RematerializeLedgerRow struct {
	OldURI    string
	State     RematerializeState
	AuthorDID string

	// CommunityDID is the repo the legacy record lives in. It is stored rather
	// than parsed back out of OldURI because the whole destructive half of the
	// tool is scoped by it: a staged run for one community must not resume, and
	// must not DELETE, a row belonging to another.
	CommunityDID string

	// SourceCID is the legacy record's CID as read at the moment the postv2 was
	// built from it. The delete is refused unless a fresh read still reports this
	// exact CID, and it is the swap guard the delete is sent under.
	SourceCID string

	// NewURI, NewCID, NewRkey identify the postv2 the tool wrote. Populated at
	// the postv2_written transition and never recomputed on resume — the resumed
	// run reads them back rather than deriving a fresh CID.
	NewURI  string
	NewCID  string
	NewRkey string

	// Reason is the human-readable note attached to a fallback row.
	Reason string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// RematerializeLedger is the migration-037 Postgres table, behind an interface
// so the state-machine tests drive a real ledger while the source and repos are
// faked.
//
// Every method that names a community takes it as an explicit scope rather than
// reading an ambient filter, because a scope that can be forgotten at one call
// site is a scope that lets a staged run delete another community's posts.
type RematerializeLedger interface {
	// Discover upserts the row for oldURI in state discovered, idempotently: a
	// re-run finds the existing row (whatever state it is in) rather than
	// resetting it.
	Discover(ctx context.Context, oldURI, communityDID, authorDID string) (RematerializeLedgerRow, error)

	// Get reads one row. found is false when the URI has never been discovered.
	Get(ctx context.Context, oldURI string) (row RematerializeLedgerRow, found bool, err error)

	// ListResumable returns every row in a non-terminal state (not done, not a
	// fallback), restricted to communityDID when it is non-empty.
	//
	// It is what makes crash-resume drive off the LEDGER rather than the source
	// listing (whole-branch review, P7): a record whose delete succeeded but whose
	// MarkDone crashed is GONE from the community repo, so a re-run's listRecords
	// can never rediscover it — only the ledger row proves it is owed a final
	// MarkDone.
	//
	// THE SCOPE IS NOT COSMETIC. Unscoped, a staged run for community A resumes —
	// and deletes — rows belonging to community B, which is the one thing a staged
	// rollout exists to prevent.
	ListResumable(ctx context.Context, communityDID string) ([]RematerializeLedgerRow, error)

	// RecordPostV2Written moves discovered → postv2_written, recording both the
	// postv2 coordinates and the SOURCE CID the postv2 was built from — the value
	// the pre-delete re-read is checked against on every later pass.
	RecordPostV2Written(ctx context.Context, oldURI, sourceCID, newURI, newCID, newRkey string) error

	// MarkVerified moves postv2_written → verified.
	MarkVerified(ctx context.Context, oldURI string) error

	// MarkMigrated moves verified → migrated — the checkpoint before delete.
	MarkMigrated(ctx context.Context, oldURI string) error

	// MarkDone moves migrated → done, after the old record is deleted.
	MarkDone(ctx context.Context, oldURI string) error

	// MarkFallback moves the row to a terminal fallback state with a reason.
	MarkFallback(ctx context.Context, oldURI string, state RematerializeState, reason string) error

	// ReopenFallback moves fallback rows back to discovered so a later run can
	// retry them, restricted to communityDID when it is non-empty, and returns how
	// many rows moved.
	//
	// THIS IS THE RECOVERY PATH, and it exists because the fallback state is
	// otherwise a one-way door: an author whose grant was missing at census time
	// can be re-authorized, and without this the operator's only remedy is
	// hand-written UPDATE statements against a production table at 2am. It only
	// ever moves a row from a fallback state to discovered — it cannot resurrect a
	// done row, and it writes nothing to any repo.
	ReopenFallback(ctx context.Context, communityDID string) (int, error)

	// CountByState is the census: how many rows sit in each state, restricted to
	// communityDID when it is non-empty, so a run can report on its own scope and
	// on the whole migration separately.
	CountByState(ctx context.Context, communityDID string) (map[RematerializeState]int, error)
}

// RematerializeReport is the census a run returns.
//
// IT CARRIES TWO DIFFERENT COMPLETION SIGNALS because they answer two different
// questions and conflating them trains the operator to ignore both. A staged
// `-community` run can finish everything it was asked to do (ScopeComplete)
// while the migration as a whole still has thousands of posts to go (Complete).
// Reporting only the second makes every staged run look like a failure, and an
// operator who has learned to ignore a red exit code is an operator with no gate
// on §11 step 6 at all.
type RematerializeReport struct {
	// CommunityScope is the community DID this run was restricted to, or "" for
	// every hosted community.
	CommunityScope string

	// Discovered, Done and Fallbacks describe THIS RUN'S SCOPE.
	Discovered int
	Done       int
	Fallbacks  int
	ByState    map[RematerializeState]int

	// RemainingLegacy is how many legacy records a FINAL RE-SCAN of the source
	// still saw that are not accounted for by a fallback row. It is what turns
	// "the ledger says we are done" into "the source agrees" — a ledger-only
	// completion check cannot see a record written after the run began, or one
	// the discovery pass never listed.
	RemainingLegacy int

	// ScopeComplete: every row in this run's scope reached done AND the final
	// re-scan found nothing left in scope.
	ScopeComplete bool

	// GlobalByState, GlobalDiscovered, GlobalDone and GlobalFallbacks are the
	// UNSCOPED census — the whole migration, regardless of what this run touched.
	GlobalByState    map[RematerializeState]int
	GlobalDiscovered int
	GlobalDone       int
	GlobalFallbacks  int

	// Complete is the gate on the separate, manual and IRREVERSIBLE legacy-removal
	// follow-up (§11 step 6). It requires the whole migration — not this run's
	// scope — to have reached done, with no fallback surviving and nothing left in
	// the source.
	Complete bool
}

// RematerializeProgress is one observable transition, handed to the caller's
// Progress hook so a batch run says what it is doing while it does it.
//
// It exists because a tool that deletes production data in silence for an hour
// gives the operator exactly one bit of information — the exit code — and no way
// to tell a hung run from a slow one, or to answer "how far did it get?" after a
// crash.
type RematerializeProgress struct {
	OldURI string
	From   RematerializeState
	To     RematerializeState
	Index  int
	Total  int
	Note   string
}

// Rematerializer drives the cutover.
//
// It holds a CommunityRecordWriter — the DIRECT acceptance writer — and NOT an
// AcceptanceEngine or an AdmissionDecider, by design: the engine re-decides
// admission, which for a since-banned author would REJECT a post that is live in
// production today, rewriting history (§11 step 4). The tool preserves the
// existing acceptance; it never re-adjudicates one.
type Rematerializer struct {
	Source      LegacySource
	Ledger      RematerializeLedger
	AuthorRepos AuthorRepoFactory
	Acceptances CommunityRecordWriter

	// CommunityRepos opens the COMMUNITY's repo for READING — specifically to
	// read the acceptance back after writing it.
	//
	// It is required, not optional. Without it the acceptance leg of "verify BOTH
	// records" cannot happen at all, and an acceptance that was never read back is
	// an acceptance that might not stand: the writer's own result cannot testify
	// to it, because the writer computes that result from the same inputs it was
	// handed.
	CommunityRepos CommunityRepoFactory

	// Blobs fetches and probes blob bytes. Defaulted to a bounded HTTP client;
	// injectable so the failure modes that matter (a truncated body, a probe that
	// errors rather than 404s) can be exercised without a PDS.
	Blobs RematerializeBlobClient

	// CommunityScope restricts the run — discovery, resume, delete and census — to
	// one community DID. Empty means every hosted community.
	CommunityScope string

	// Progress, when set, is called on every state transition and on notable
	// non-transitions. It must not block for long: the run is serial.
	Progress func(RematerializeProgress)

	// PerRecordTimeout bounds everything ONE record's processing does — several
	// PDS round trips and a handful of ledger writes. Zero means no per-record
	// bound, which is only appropriate in tests: in production a single half-open
	// socket otherwise stalls the whole migration silently.
	PerRecordTimeout time.Duration

	// AbortOnFallback stops the run after the credential census if that census
	// marked any NEW row as a fallback, before a single repo is mutated.
	//
	// It is the operator's answer to the failure this tool's worst day looks
	// like: one aggregator session expires, the census sentences most of the
	// corpus, and a run that "succeeded" has migrated almost nothing. With this
	// set, the run stops and names the authors instead.
	AbortOnFallback bool

	// credentials caches ONE resolution per distinct author DID for the lifetime
	// of this Rematerializer. Each resolution is a refresh-token rotation against
	// the PDS; an aggregator with 5,000 posts would otherwise rotate 5,000 times
	// in a single run, which is both minutes of avoidable load and thousands of
	// extra chances to hit the transient failure that used to be recorded as a
	// terminal verdict.
	credentialsMu sync.Mutex
	repoCache     map[string]AuthorRepo
	repoErrCache  map[string]error
}

// RematerializeBlobClient reads blob bytes out of a repo, and reports whether a
// repo serves one.
//
// It is an interface rather than two package functions because both of its
// failure modes are silent by default and both destroy data: a fetch that
// truncates at a size cap uploads DIFFERENT bytes under a DIFFERENT CID, and a
// probe that reports "absent" for a transport error turns a network blip into a
// refusal — or, if the polarity were ever flipped, a missing blob into a
// go-ahead to delete the only copy.
type RematerializeBlobClient interface {
	// Fetch returns the blob's bytes. It MUST fail rather than truncate.
	Fetch(ctx context.Context, host, did, cid string) ([]byte, error)

	// Present reports whether the repo serves the blob. A transport failure is an
	// ERROR, never a false: "I could not ask" and "it is not there" are different
	// facts and only one of them is about the data.
	Present(ctx context.Context, host, did, cid string) (bool, error)
}

// RematerializeRkey is the postv2 record key the tool writes a legacy record at.
//
// It has TWO hard constraints that pull against each other (§11 step 4, whole-
// branch review P9):
//
//  1. A PURE, STABLE FUNCTION OF THE OLD URI ALONE. A re-run must recompute the
//     IDENTICAL key so createAuthorRecord converges by read instead of minting a
//     second postv2 that dangles every strongRef built from the first. Nothing
//     submission-time (fingerprint, bucket, clock) may leak in — the migration
//     does not have it and a re-run could not reproduce it — so it is NOT
//     SubmissionRkey.
//  2. A VALID TID. The postv2 lexicon declares "key": "tid", so a validating PDS
//     rejects any rkey that is not a TID and feed ordering reads the timestamp
//     out of the key. SubjectRkey (a 52-char base32 digest) is NOT a TID, so it
//     cannot be reused here.
//
// Both are satisfied by deriving a DETERMINISTIC TID from the SHA-256 of the old
// URI: the timestamp and clock bits are drawn from the digest, masked to the
// widths a TID has room for, and encoded with syntax.NewTID — the one encoder
// guaranteed to agree with every ParseTID in the network. Same URI in, same TID
// out; different URIs, different TIDs (SHA-256 collision resistance).
//
// THE DERIVATION IS PINNED BY GOLDEN VALUES in rematerialize_rkey_test.go, and
// those literals are not a test to update: every post already migrated sits at
// the OLD key, so changing this function writes a SECOND postv2 for every one of
// them on the next run.
func RematerializeRkey(legacyPostURI string) string {
	digest := sha256.Sum256([]byte(legacyPostURI))

	// The TID timestamp is a 53-bit microsecond field; masking the digest to 53
	// bits keeps it non-negative and inside the range NewTID encodes, while
	// staying a pure function of the URI. The value is not a real time — the
	// migration has none — but it is stable, which is the only property the rkey
	// needs and feed ordering can tolerate.
	micros := int64(binary.BigEndian.Uint64(digest[0:8]) & ((1 << 53) - 1))

	// Ten further digest bits become the clock ID, so two URIs whose 53-bit
	// timestamps happened to collide still draw distinct keys.
	clockID := uint(binary.BigEndian.Uint16(digest[8:10])) & 0x3FF

	return syntax.NewTID(micros, clockID).String()
}

// Run discovers every legacy record in scope and drives each to a terminal
// state, returning the census.
//
// It runs in FOUR ordered passes:
//
//  1. THE CREDENTIAL CENSUS (P8). Every discovered author is resolved with NO
//     repo mutation, so an author whose credentials cannot be restored is marked
//     a fallback BEFORE a single record is written or deleted. Without this, a
//     mutate-as-you-go run would fully migrate and delete the early records
//     before ever discovering that a later author is stranded — the exact
//     ordering §11 step 3 forbids. A RETRYABLE credential failure fails the run
//     here rather than sentencing a row.
//  2. THE SOURCE PASS. Each listed record is driven to a terminal state.
//  3. THE LEDGER RECONCILE (P7). A row whose delete succeeded but whose MarkDone
//     crashed is GONE from the community repo, so the source's listRecords can
//     never rediscover it — only ListResumable can. Every non-terminal ledger
//     row past the postv2 write is finished from the ledger, IN SCOPE.
//  4. THE FINAL RE-SCAN. The source is enumerated again and anything still
//     standing that is not accounted for by a fallback row is counted. A
//     completion signal computed only from rows the run already knew about
//     cannot see a record written during the run, or one the first listing
//     missed — and "complete" is the gate on an irreversible step.
//
// A per-record error FAILS THE RUN rather than being logged and skipped: the
// safety properties are all ordering ones, and continuing past a record the tool
// could not verify would let the operator read a "done"-heavy census as
// permission to run the irreversible legacy-removal step while a record sits
// half-migrated. A no-creds fallback is NOT such an error — it is an expected
// terminal outcome the census counts.
func (r *Rematerializer) Run(ctx context.Context) (RematerializeReport, error) {
	legacies, err := r.Source.ListLegacyPosts(ctx)
	if err != nil {
		return RematerializeReport{}, fmt.Errorf("enumerating legacy posts: %w", err)
	}

	// Pass 1 — the census. Resolve EVERY not-yet-started author before ANY repo is
	// mutated. A row already past discovered had its credentials confirmed on an
	// earlier pass and must not be re-marked — the fallback transition is guarded on
	// the discovered state, so re-marking a resumed or already-fallen-back row would
	// fail; skipping it keeps the whole run idempotent.
	var stranded []string
	for i, legacy := range legacies {
		if err := ctx.Err(); err != nil {
			return RematerializeReport{}, err
		}
		row, err := r.Ledger.Discover(ctx, legacy.URI, legacy.CommunityDID, legacy.AuthorDID)
		if err != nil {
			return RematerializeReport{}, err
		}
		if row.State != RematerializeDiscovered {
			continue
		}
		if _, err := r.authorRepo(ctx, legacy.AuthorDID); err != nil {
			if errors.Is(err, ErrNoAuthorCredentials) {
				reason := fmt.Sprintf("author %s has no restorable repo credentials: %v", legacy.AuthorDID, err)
				if markErr := r.Ledger.MarkFallback(ctx, legacy.URI, RematerializeFallbackLeftLegacy, reason); markErr != nil {
					return RematerializeReport{}, markErr
				}
				stranded = append(stranded, legacy.AuthorDID)
				r.report(RematerializeProgress{
					OldURI: legacy.URI, From: RematerializeDiscovered, To: RematerializeFallbackLeftLegacy,
					Index: i + 1, Total: len(legacies), Note: reason,
				})
				continue
			}
			// RETRYABLE. Failing the run is the WHOLE POINT: writing this as a
			// fallback would sentence the row on the strength of a network blip, and
			// nothing inside a later run would move it back.
			return RematerializeReport{}, fmt.Errorf("preflighting the credentials of %s: %w", legacy.AuthorDID, err)
		}
	}

	if r.AbortOnFallback && len(stranded) > 0 {
		return RematerializeReport{}, fmt.Errorf(
			"the credential census left %d post(s) as legacy across %d author(s) (%s); stopping before any repo was mutated. "+
				"Re-authorize the author(s) and re-run with -reopen-fallbacks, or re-run with -accept-fallbacks to proceed and leave those posts as legacy",
			len(stranded), len(distinct(stranded)), joinFirst(distinct(stranded), 5))
	}

	// Pass 2 — the source pass. A record whose census marked it a fallback is left
	// untouched by RematerializeOne (it returns early on a terminal row).
	for i, legacy := range legacies {
		if err := ctx.Err(); err != nil {
			return RematerializeReport{}, err
		}
		state, err := r.rematerializeOneBounded(ctx, legacy)
		if err != nil {
			return RematerializeReport{}, fmt.Errorf("re-materializing %s: %w", legacy.URI, err)
		}
		r.report(RematerializeProgress{OldURI: legacy.URI, To: state, Index: i + 1, Total: len(legacies)})
	}

	// Pass 3 — the ledger reconcile, IN SCOPE. Finish any row the source could not
	// present. Scoped, because an unscoped resume would drive — and delete — rows
	// belonging to a community this staged run was told to leave alone.
	resumable, err := r.Ledger.ListResumable(ctx, r.CommunityScope)
	if err != nil {
		return RematerializeReport{}, fmt.Errorf("listing resumable rows: %w", err)
	}
	for i, ledgerRow := range resumable {
		if err := ctx.Err(); err != nil {
			return RematerializeReport{}, err
		}
		// A row still at discovered needs the ORIGINAL record's bytes to build its
		// postv2. RematerializeOne re-reads them from the source itself, so a row
		// whose record still stands is finished here; one whose record is gone is
		// genuinely incomplete and keeps Complete false.
		if ledgerRow.State == RematerializeDiscovered {
			continue
		}
		legacy := legacyFromLedgerRow(ledgerRow)
		state, err := r.rematerializeOneBounded(ctx, legacy)
		if err != nil {
			return RematerializeReport{}, fmt.Errorf("reconciling %s: %w", ledgerRow.OldURI, err)
		}
		r.report(RematerializeProgress{OldURI: ledgerRow.OldURI, From: ledgerRow.State, To: state, Index: i + 1, Total: len(resumable), Note: "reconciled from the ledger"})
	}

	return r.census(ctx)
}

// rematerializeOneBounded runs one record under PerRecordTimeout, so a single
// stalled PDS call cannot hang the whole migration with no output. The bound is
// per RECORD rather than per call because the safety properties are ordering
// ones: a timeout mid-record leaves the ledger at its last checkpoint, which a
// re-run resumes from.
func (r *Rematerializer) rematerializeOneBounded(ctx context.Context, legacy LegacyPost) (RematerializeState, error) {
	if r.PerRecordTimeout <= 0 {
		return r.RematerializeOne(ctx, legacy)
	}
	recordCtx, cancel := context.WithTimeout(ctx, r.PerRecordTimeout)
	defer cancel()
	return r.RematerializeOne(recordCtx, legacy)
}

// census builds the report: the scoped tally, the global tally, and a FINAL
// RE-SCAN of the source that the completion signals are gated on.
func (r *Rematerializer) census(ctx context.Context) (RematerializeReport, error) {
	scoped, err := r.Ledger.CountByState(ctx, r.CommunityScope)
	if err != nil {
		return RematerializeReport{}, fmt.Errorf("taking the census: %w", err)
	}
	global := scoped
	if r.CommunityScope != "" {
		global, err = r.Ledger.CountByState(ctx, "")
		if err != nil {
			return RematerializeReport{}, fmt.Errorf("taking the global census: %w", err)
		}
	}

	report := RematerializeReport{
		CommunityScope: r.CommunityScope,
		ByState:        scoped,
		GlobalByState:  global,
	}
	for state, n := range scoped {
		report.Discovered += n
		if state == RematerializeDone {
			report.Done += n
		}
		if IsFallback(state) {
			report.Fallbacks += n
		}
	}
	for state, n := range global {
		report.GlobalDiscovered += n
		if state == RematerializeDone {
			report.GlobalDone += n
		}
		if IsFallback(state) {
			report.GlobalFallbacks += n
		}
	}

	// THE FINAL RE-SCAN. Completion computed only from ledger rows the run already
	// discovered is circular: it can never see a record that was written after the
	// discovery pass, or one the listing missed. Asking the source again is the only
	// evidence that the collection is actually drained.
	remaining, err := r.Source.ListLegacyPosts(ctx)
	if err != nil {
		return report, fmt.Errorf("re-scanning the source to confirm the drain: %w", err)
	}
	for _, legacy := range remaining {
		row, found, err := r.Ledger.Get(ctx, legacy.URI)
		if err != nil {
			return report, fmt.Errorf("checking the ledger for the re-scanned %s: %w", legacy.URI, err)
		}
		// A record deliberately left as legacy is accounted for, not remaining. A
		// record with no row at all, or one not yet done, is remaining.
		if found && IsFallback(row.State) {
			continue
		}
		report.RemainingLegacy++
	}

	report.ScopeComplete = report.Done == report.Discovered && report.RemainingLegacy == 0
	// COMPLETE MEANS THE WHOLE MIGRATION IS DRAINED — every row everywhere at done,
	// no fallback surviving, and the source re-scan agreeing. A row stranded in any
	// non-terminal state, a surviving fallback, or a legacy record still standing all
	// leave it false, and the operator's irreversible legacy-removal step (§11 step 6)
	// must not run while any of them is true.
	report.Complete = report.GlobalDone == report.GlobalDiscovered &&
		report.GlobalFallbacks == 0 &&
		report.RemainingLegacy == 0
	return report, nil
}

// RematerializeOne drives a single legacy record from wherever its ledger row
// stands to a terminal state, and returns the state it reached.
//
// The steps are guarded on the ledger state each moves FROM, so a resumed run
// re-enters at exactly the step its predecessor stopped before and re-does none
// of the completed ones — WITH ONE DELIBERATE EXCEPTION. The verification that
// licenses the delete is re-run from fresh reads on EVERY path, resumed or not,
// because the ledger records that verification passed at a moment now in the
// past and the delete needs it to be true now.
func (r *Rematerializer) RematerializeOne(ctx context.Context, legacy LegacyPost) (RematerializeState, error) {
	if r.CommunityScope != "" && legacy.CommunityDID != r.CommunityScope {
		// A SCOPED RUN NEVER TOUCHES ANOTHER COMMUNITY'S POSTS. This is the last
		// gate before a delete, and it is checked here rather than only at discovery
		// because the reconcile pass reaches records the discovery pass never listed.
		return "", fmt.Errorf(
			"refusing to re-materialize %s: it belongs to %s but this run is scoped to %s",
			legacy.URI, legacy.CommunityDID, r.CommunityScope)
	}

	row, err := r.Ledger.Discover(ctx, legacy.URI, legacy.CommunityDID, legacy.AuthorDID)
	if err != nil {
		return "", err
	}

	// A row already in a terminal fallback state is left exactly as it stands: the
	// credential census reached its verdict on a prior pass, and re-opening it
	// would be the one thing §11 step 3 forbids — a second chance to forge.
	// (ReopenFallback is the supported, explicit way back.)
	if IsFallback(row.State) {
		return row.State, nil
	}
	if row.State == RematerializeDone {
		return row.State, nil
	}

	// Step 1 — postv2_written. Copy the embed blobs into the author's repo, build
	// the postv2 LOSSLESSLY from the raw legacy record AS IT STANDS NOW, and write
	// it at the deterministic rkey. createAuthorRecord is create-only and converges
	// by read, so a resume that re-enters here finds its own first attempt rather
	// than minting a second post.
	if row.State == RematerializeDiscovered {
		newRow, err := r.writePostV2(ctx, legacy, row)
		if err != nil {
			return row.State, err
		}
		row = newRow
		r.report(RematerializeProgress{OldURI: legacy.URI, From: RematerializeDiscovered, To: row.State})
		if IsFallback(row.State) {
			return row.State, nil
		}
	}

	// Step 2 — write the community's acceptance DIRECT (never through the engine —
	// see the type's doc) pinning the NEW postv2 CID.
	//
	// NOTE WHAT DOES *NOT* HAPPEN HERE: the row is not checkpointed to `verified`.
	// The writer's own result cannot testify that the record stands — it is
	// computed from the inputs it was handed — so a checkpoint written here would
	// mean "verified" on the strength of nothing. The state advances only after
	// the read-back below.
	if row.State == RematerializePostV2Written {
		if _, err := r.Acceptances.WriteAcceptance(ctx, CommunityWriteCommand{
			CommunityDID: legacy.CommunityDID,
			PostURI:      row.NewURI,
			PostCID:      row.NewCID,
		}); err != nil {
			return row.State, fmt.Errorf("writing the acceptance for %s: %w", row.NewURI, err)
		}
	}

	// THE PRE-DELETE VERIFICATION. Unconditional, on every path into the delete,
	// from FRESH READS ONLY.
	//
	// This is the load-bearing ordering of the whole tool, and it is deliberately
	// NOT gated on the ledger state: a row at verified or migrated says only that
	// the check passed once, before a crash whose duration nothing here knows. In
	// that gap the postv2 could have been edited or deleted, the acceptance
	// withdrawn, or the legacy record itself edited by a writer the maintenance
	// window failed to stop. A ledger memory is not evidence; a read is.
	verified, err := r.verifyBeforeDelete(ctx, legacy, row)
	if err != nil {
		return row.State, err
	}

	// Step 3 — verified. Recorded ONLY now, after the reads that make it true.
	if row.State == RematerializePostV2Written {
		if err := r.Ledger.MarkVerified(ctx, legacy.URI); err != nil {
			return row.State, err
		}
		row.State = RematerializeVerified
		r.report(RematerializeProgress{OldURI: legacy.URI, From: RematerializePostV2Written, To: RematerializeVerified})
	}

	// Step 4 — migrated. The checkpoint BEFORE the delete: postv2, blobs and
	// acceptance verified, old record still present. Persisting it as its own state
	// is what lets a crash on the delete retry ONLY the delete.
	if row.State == RematerializeVerified {
		if err := r.Ledger.MarkMigrated(ctx, legacy.URI); err != nil {
			return row.State, err
		}
		row.State = RematerializeMigrated
		r.report(RematerializeProgress{OldURI: legacy.URI, From: RematerializeVerified, To: RematerializeMigrated})
	}

	// Step 5 — done. Delete the old community.post, GUARDED by the source CID the
	// postv2 was built from, so the PDS refuses the delete if anything landed on
	// the record since. A delete of an already-gone record is success (the source's
	// contract), so a resumed delete is idempotent.
	if row.State == RematerializeMigrated {
		if verified.legacyPresent {
			if err := r.Source.DeleteLegacyPost(ctx, legacy, row.SourceCID); err != nil {
				return row.State, fmt.Errorf("deleting the old record %s: %w", legacy.URI, err)
			}
		}
		if err := r.Ledger.MarkDone(ctx, legacy.URI); err != nil {
			return row.State, err
		}
		row.State = RematerializeDone
		r.report(RematerializeProgress{OldURI: legacy.URI, From: RematerializeMigrated, To: RematerializeDone})
	}

	return row.State, nil
}

// writePostV2 is step 1: resolve the author, re-read the legacy record, copy its
// blobs, build the lossless conversion and write it at the deterministic rkey.
//
// IT RE-READS THE RECORD rather than trusting the listing it was handed. The
// listing snapshots every body at t0 and the pass that consumes it runs for
// minutes to hours; building the postv2 from a stale body and then deleting the
// original destroys whatever landed in between.
func (r *Rematerializer) writePostV2(ctx context.Context, legacy LegacyPost, row RematerializeLedgerRow) (RematerializeLedgerRow, error) {
	repo, err := r.authorRepo(ctx, legacy.AuthorDID)
	if err != nil {
		// NO CREDENTIALS IS A TERMINAL FALLBACK, NEVER A FORGERY. An author whose
		// repo cannot be restored is left as legacy — the postv2 is not written and
		// the old record survives — because re-authoring under any other identity
		// reintroduces the §2 impersonation the whole flip removes.
		//
		// A RETRYABLE failure is not that, and is not written to the ledger at all.
		if errors.Is(err, ErrNoAuthorCredentials) {
			reason := fmt.Sprintf("author %s has no restorable repo credentials: %v", legacy.AuthorDID, err)
			if markErr := r.Ledger.MarkFallback(ctx, legacy.URI, RematerializeFallbackLeftLegacy, reason); markErr != nil {
				return row, markErr
			}
			row.State = RematerializeFallbackLeftLegacy
			return row, nil
		}
		return row, fmt.Errorf("opening the author repo of %s: %w", legacy.AuthorDID, err)
	}

	fresh, found, err := r.Source.ReadLegacyPost(ctx, legacy.URI)
	if err != nil {
		return row, fmt.Errorf("re-reading the legacy record %s before converting it: %w", legacy.URI, err)
	}
	if !found {
		// The row is at discovered — this tool has written nothing for it — and the
		// record is gone. Something outside the tool deleted it. There is nothing to
		// migrate and nothing safe to assume, so say so rather than inventing a body.
		return row, fmt.Errorf(
			"the legacy record %s is gone from its community repo but the ledger has never written a postv2 for it; "+
				"it was deleted by something other than this tool, so there is nothing to re-materialize", legacy.URI)
	}
	if fresh.CID == "" {
		return row, fmt.Errorf("re-reading the legacy record %s: the community repo reported no CID, so no delete could ever be guarded on it", legacy.URI)
	}
	fresh.CommunityDID = legacy.CommunityDID
	if fresh.AuthorDID == "" {
		fresh.AuthorDID = legacy.AuthorDID
	}

	// P5 — the conversion is built from the LOSSLESS raw record, dropping only
	// the author field and re-stamping $type. Building it through PostRecord
	// would silently strip langs/tags/crosspostOf/crosspostChain/bridgedStats,
	// which the old record can never be recovered from once it is deleted.
	intended, err := postV2Body(fresh)
	if err != nil {
		return row, err
	}

	// P4 — the embed's blob BYTES must live in the AUTHOR's repo before the old
	// record (and the community's blob store) can go, or the postv2's media
	// resolves against a repo that never held it. The bytes are UPLOADED here,
	// before the record that references them is written; the PDS only serves an
	// uploaded blob once a record pins it, so presence is VERIFIED after the
	// write, in verifyBeforeDelete.
	if err := r.uploadEmbedBlobs(ctx, repo, fresh); err != nil {
		return row, err
	}

	rkey := RematerializeRkey(legacy.URI)
	newURI, newCID, converged, err := createAuthorRecord(ctx, repo, rkey, intended)
	if err != nil {
		return row, fmt.Errorf("writing the postv2 for %s: %w", legacy.URI, err)
	}

	// P6 — a converged write means a record ALREADY stood at the deterministic
	// rkey. createAuthorRecord accepts it by CID, but a CID match alone would
	// adopt a DIFFERENT record that merely shares the key. Confirm the standing
	// record IS this legacy post's conversion before trusting it; otherwise the
	// legacy original would be deleted in favour of a foreign record.
	if converged {
		standing, err := repo.GetRecord(ctx, PostV2Collection, rkey)
		if err != nil {
			return row, fmt.Errorf("reading the converged postv2 for %s: %w", legacy.URI, err)
		}
		if !sameRecordBody(standing.Value, intended) {
			return row, fmt.Errorf(
				"a different record already stands at %s in the author's repo: its body is not this legacy post's conversion, so re-materializing would adopt a foreign record and delete the real one",
				rkey)
		}
	}

	if err := r.Ledger.RecordPostV2Written(ctx, legacy.URI, fresh.CID, newURI, newCID, rkey); err != nil {
		return row, err
	}
	row.State = RematerializePostV2Written
	row.SourceCID = fresh.CID
	row.NewURI, row.NewCID, row.NewRkey = newURI, newCID, rkey
	return row, nil
}

// verificationResult reports what the pre-delete verification actually observed,
// so the delete step knows whether there is still a record to delete.
type verificationResult struct {
	legacyPresent bool
}

// verifyBeforeDelete is the guarantee. It runs immediately before every delete,
// on every path, and it reads everything it asserts.
//
// FOUR FACTS, ALL FROM FRESH READS:
//
//  1. The postv2 stands in the AUTHOR's repo and still carries the CID the
//     acceptance pinned.
//  2. The ACCEPTANCE stands in the COMMUNITY's repo and its subject strongRef
//     names our postv2 URI and that same CID. This is read back from the repo,
//     never inferred from the writer's own result — the writer computes its
//     result from the inputs it was handed, so comparing it to those inputs is a
//     tautology that cannot fail and proves nothing about what the repo holds.
//  3. Every embed blob is served by the AUTHOR's repo, so the post's media does
//     not break the instant the community's copy becomes collectable.
//  4. The LEGACY record either is gone already (an idempotent resumed delete) or
//     still carries the exact CID the postv2 was built from. Anything else means
//     a writer landed an edit the maintenance window did not stop, and the newer
//     content must not be destroyed.
func (r *Rematerializer) verifyBeforeDelete(ctx context.Context, legacy LegacyPost, row RematerializeLedgerRow) (verificationResult, error) {
	if row.NewURI == "" || row.NewCID == "" || row.NewRkey == "" {
		return verificationResult{}, fmt.Errorf(
			"refusing to verify %s for deletion: the ledger row names no postv2 (uri %q cid %q rkey %q), so there is nothing proven to replace it",
			legacy.URI, row.NewURI, row.NewCID, row.NewRkey)
	}
	if row.SourceCID == "" {
		return verificationResult{}, fmt.Errorf(
			"refusing to verify %s for deletion: the ledger row records no source CID, so the delete could not be guarded against a concurrent edit",
			legacy.URI)
	}

	// (1) The postv2, read fresh out of the author's repo.
	repo, err := r.authorRepo(ctx, legacy.AuthorDID)
	if err != nil {
		return verificationResult{}, fmt.Errorf("opening the author repo of %s to verify: %w", legacy.AuthorDID, err)
	}
	standing, err := repo.GetRecord(ctx, PostV2Collection, row.NewRkey)
	if err != nil {
		return verificationResult{}, fmt.Errorf("verifying the postv2 for %s: %w", row.NewURI, err)
	}
	if standing == nil {
		return verificationResult{}, fmt.Errorf("verifying the postv2 for %s: the author's repo returned no record", row.NewURI)
	}
	if standing.CID != row.NewCID {
		// VERIFY BEFORE DELETE fails closed: the acceptance pins a CID the postv2 no
		// longer carries, so deleting the old record would destroy the only copy of a
		// post whose new attestation points at content that no longer stands. No
		// checkpoint, no delete.
		return verificationResult{}, fmt.Errorf(
			"verifying the postv2 for %s: the standing record pins %s but the acceptance pinned %s (a concurrent edit landed after the write)",
			row.NewURI, standing.CID, row.NewCID)
	}

	// (2) The acceptance, read fresh out of the COMMUNITY's repo.
	if r.CommunityRepos == nil {
		return verificationResult{}, fmt.Errorf(
			"refusing to delete %s: no community-repo factory is wired, so the acceptance cannot be read back and 'verify BOTH records' is unsatisfiable",
			legacy.URI)
	}
	communityRepo, err := r.CommunityRepos(ctx, legacy.CommunityDID)
	if err != nil {
		return verificationResult{}, fmt.Errorf("opening the community repo of %s to verify the acceptance: %w", legacy.CommunityDID, err)
	}
	acceptanceRkey := SubjectRkey(row.NewURI)
	acceptance, err := communityRepo.GetRecord(ctx, AcceptanceCollection, acceptanceRkey)
	if err != nil {
		return verificationResult{}, fmt.Errorf(
			"verifying the acceptance of %s at %s/%s: %w (the community's acceptance is what keeps the post IN the community; without it the postv2 stands orphaned)",
			row.NewURI, AcceptanceCollection, acceptanceRkey, err)
	}
	if acceptance == nil {
		return verificationResult{}, fmt.Errorf("verifying the acceptance of %s: the community's repo returned no record", row.NewURI)
	}
	subjectURI, subjectCID := acceptanceSubject(acceptance.Value)
	if subjectURI != row.NewURI || subjectCID != row.NewCID {
		return verificationResult{}, fmt.Errorf(
			"the acceptance standing at %s/%s does not pin our postv2: its subject is uri %q cid %q, we need uri %q cid %q. "+
				"Deleting the legacy record now would drop the post out of its community",
			AcceptanceCollection, acceptanceRkey, subjectURI, subjectCID, row.NewURI, row.NewCID)
	}

	// (4) The legacy record, read fresh — done before the blob check so the blob
	// refs can come from the body that actually stands.
	fresh, legacyPresent, err := r.Source.ReadLegacyPost(ctx, legacy.URI)
	if err != nil {
		return verificationResult{}, fmt.Errorf("re-reading the legacy record %s before deleting it: %w", legacy.URI, err)
	}
	if legacyPresent && fresh.CID != row.SourceCID {
		return verificationResult{}, fmt.Errorf(
			"refusing to delete %s: it now carries CID %s but the postv2 was built from %s. "+
				"An edit landed after the conversion, so deleting would destroy content that was never re-materialized. "+
				"Re-run after the writer is stopped; the ledger row stays where it is",
			legacy.URI, fresh.CID, row.SourceCID)
	}

	// (3) The blobs, in the AUTHOR's repo. The refs come from the standing legacy
	// body when there is one, and from the postv2 itself when the legacy record has
	// already gone — a resumed run must still prove the media is there.
	blobSource := standing.Value
	if legacyPresent {
		blobSource = fresh.RawRecord
	}
	if err := r.verifyEmbedBlobsPresent(ctx, repo, legacy.URI, blobSource); err != nil {
		return verificationResult{}, err
	}

	return verificationResult{legacyPresent: legacyPresent}, nil
}

// authorRepo resolves ONE author's repo, caching both the repo and a terminal
// failure for the lifetime of the run.
//
// Caching is not an optimisation here so much as a correctness property: every
// resolution rotates the stored refresh token, so resolving per POST rather than
// per AUTHOR means an aggregator with 5,000 posts performs 5,000 rotations in
// one run — each one a chance to break the session the AppView itself depends
// on. A RETRYABLE failure is deliberately NOT cached: it is the caller's job to
// fail the run on it, and a later run must be free to succeed.
func (r *Rematerializer) authorRepo(ctx context.Context, authorDID string) (AuthorRepo, error) {
	r.credentialsMu.Lock()
	defer r.credentialsMu.Unlock()

	if repo, ok := r.repoCache[authorDID]; ok {
		return repo, nil
	}
	if err, ok := r.repoErrCache[authorDID]; ok {
		return nil, err
	}

	repo, err := r.AuthorRepos(ctx, authorDID, nil)
	if err != nil {
		if errors.Is(err, ErrNoAuthorCredentials) {
			if r.repoErrCache == nil {
				r.repoErrCache = map[string]error{}
			}
			r.repoErrCache[authorDID] = err
		}
		return nil, err
	}
	if r.repoCache == nil {
		r.repoCache = map[string]AuthorRepo{}
	}
	r.repoCache[authorDID] = repo
	return repo, nil
}

// report hands one transition to the caller's Progress hook, if it wired one.
func (r *Rematerializer) report(p RematerializeProgress) {
	if r.Progress != nil {
		r.Progress(p)
	}
}

// blobClient returns the injected blob client, or the bounded HTTP default.
//
// THE DEFAULT IS GUARDED, WITH NO WAY TO OPEN IT FROM HERE. The host it dials
// comes from the author repo's HostURL — a DID document's serviceEndpoint — so
// the fallback has to be the safe one; a caller that genuinely needs the dev
// hatch (cmd/rematerialize-posts against a local PDS) sets Blobs explicitly from
// DefaultRematerializeBlobClient(cfg.IsDevEnv), which is a decision visible at
// the wiring rather than a boolean threaded through the state machine.
func (r *Rematerializer) blobClient() RematerializeBlobClient {
	if r.Blobs != nil {
		return r.Blobs
	}
	return DefaultRematerializeBlobClient(false)
}

// maxRematerializeBlobBytes caps a single blob copy. It is generous — larger than
// any post media the lexicons admit — because the bytes come from our own
// community repo, not an untrusted origin; the cap exists to bound a corrupt or
// runaway response, not to enforce a content policy.
const maxRematerializeBlobBytes = 100 << 20 // 100 MiB

// rematerializeBlobFetchTimeout bounds a single blob transfer.
//
// http.DefaultClient has NO timeout, so a half-open socket to the PDS hangs the
// whole run forever — at 3am, with no output, indistinguishable from a slow one.
const rematerializeBlobFetchTimeout = 2 * time.Minute

// postV2Body builds the author-owned postv2 record from the legacy record's
// LOSSLESS raw map: every published field is carried through byte-for-byte, only
// the `author` field is dropped (authorship is the repo now, §3.1) and the $type
// is re-stamped to the postv2 collection.
func postV2Body(legacy LegacyPost) (map[string]any, error) {
	if len(legacy.RawRecord) == 0 {
		return nil, fmt.Errorf("re-materializing %s: the legacy record carries no RawRecord to convert losslessly", legacy.URI)
	}
	body := cloneRecord(legacy.RawRecord)
	delete(body, "author")
	body["$type"] = PostV2Collection
	return body, nil
}

// acceptanceSubject reads the subject strongRef out of a decoded acceptance
// record. A record whose subject is missing or malformed yields two empty
// strings, which the caller treats as "this is not our acceptance" — the
// fail-closed reading.
func acceptanceSubject(record map[string]any) (uri, cid string) {
	subject, ok := record["subject"].(map[string]any)
	if !ok {
		return "", ""
	}
	uri, _ = subject["uri"].(string)
	cid, _ = subject["cid"].(string)
	return uri, cid
}

// uploadEmbedBlobs copies every embed blob's BYTES from the COMMUNITY's blob
// store into the author's repo. It runs BEFORE the postv2 record is written,
// because a record's embed may only reference blobs the repo has already
// received.
//
// THE BYTES COME FROM THE COMMUNITY'S OWN PDS. A blob lives in the repo that
// holds it, so the community's blobs are fetched from the community's host —
// which is the same machine as the author's only for as long as every account on
// this instance shares one PDS. Fetching them from the author's host works today
// and silently 404s the moment that stops being true, which is exactly the kind
// of assumption a one-shot destructive tool must not carry.
func (r *Rematerializer) uploadEmbedBlobs(ctx context.Context, repo AuthorRepo, legacy LegacyPost) error {
	refs := extractBlobRefs(cloneRecord(legacy.RawRecord))
	if len(refs) == 0 {
		return nil
	}

	communityHost, err := r.communityHostURL(ctx, legacy.CommunityDID)
	if err != nil {
		return fmt.Errorf("copying blobs for %s: %w", legacy.URI, err)
	}

	client := r.blobClient()
	for _, ref := range refs {
		data, err := client.Fetch(ctx, communityHost, legacy.CommunityDID, ref.cid)
		if err != nil {
			return fmt.Errorf("fetching embed blob %s from %s: %w", ref.cid, legacy.CommunityDID, err)
		}
		mimeType := ref.mimeType
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		uploaded, err := repo.UploadBlob(ctx, data, mimeType)
		if err != nil {
			return fmt.Errorf("uploading embed blob %s into the author repo of %s: %w", ref.cid, repo.DID(), err)
		}
		// A blob CID is content-addressed, so re-uploading identical bytes yields
		// the identical CID and the postv2 may carry the community's ref unchanged.
		// That is a PROPERTY OF THE PDS, not of this code, so it is checked rather
		// than assumed: a host that re-encoded on upload would mint a different CID,
		// and the postv2 would reference bytes that are not there.
		if got := uploadedBlobCID(uploaded); got != "" && got != ref.cid {
			return fmt.Errorf(
				"the author's PDS stored embed blob %s under a DIFFERENT CID (%s); the postv2 would reference bytes the repo does not serve. "+
					"Blob CIDs are content-addressed, so this means the host re-encoded the upload and the record cannot be re-materialized unchanged",
				ref.cid, got)
		}
	}
	return nil
}

// uploadedBlobCID reads the CID out of an upload result, tolerating a ref shape
// the fakes leave empty.
func uploadedBlobCID(ref *blobs.BlobRef) string {
	if ref == nil {
		return ""
	}
	return ref.Ref["$link"]
}

// verifyEmbedBlobsPresent confirms every embed blob referenced by body is served
// by the author's repo — the P4 guarantee that the postv2's media resolves
// against the author, not the community repo the old record is about to be
// deleted from.
//
// It runs as part of the pre-delete verification on EVERY path. Running it only
// in the first-pass branch meant a resumed run that re-entered at postv2_written
// deleted the legacy record — and with it the last reference keeping the
// community's blobs alive — having checked nothing about the media at all.
func (r *Rematerializer) verifyEmbedBlobsPresent(ctx context.Context, repo AuthorRepo, oldURI string, body map[string]any) error {
	refs := extractBlobRefs(cloneRecord(body))
	if len(refs) == 0 {
		return nil
	}

	host, ok := repoHostURL(repo)
	if !ok {
		return fmt.Errorf("verifying blobs for %s: the author repo exposes no host URL", oldURI)
	}

	client := r.blobClient()
	for _, ref := range refs {
		present, err := client.Present(ctx, host, repo.DID(), ref.cid)
		if err != nil {
			// "I could not ask" is not "it is not there". Reporting a transport
			// failure as absence would refuse a healthy record; reporting it as
			// presence would license deleting the only copy of the bytes.
			return fmt.Errorf("checking whether embed blob %s is present in the author repo of %s: %w", ref.cid, repo.DID(), err)
		}
		if !present {
			return fmt.Errorf("embed blob %s is not present in the author repo of %s after the postv2 write; refusing to proceed toward the delete", ref.cid, repo.DID())
		}
	}
	return nil
}

// communityHostURL is the PDS host the COMMUNITY's repo lives on — where its
// blobs are actually served from.
func (r *Rematerializer) communityHostURL(ctx context.Context, communityDID string) (string, error) {
	if r.CommunityRepos == nil {
		return "", fmt.Errorf("no community-repo factory is wired, so the host holding %s's blobs cannot be resolved", communityDID)
	}
	repo, err := r.CommunityRepos(ctx, communityDID)
	if err != nil {
		return "", fmt.Errorf("opening the community repo of %s: %w", communityDID, err)
	}
	host, ok := hostURLOf(repo)
	if !ok {
		return "", fmt.Errorf("the community repo of %s exposes no host URL to fetch its blobs from", communityDID)
	}
	return host, nil
}

// rematerializeBlobRef is one embed blob to copy: its CID and MIME type, both
// read from the blob reference in the record.
type rematerializeBlobRef struct {
	cid      string
	mimeType string
}

// extractBlobRefs walks a decoded record tree and collects every blob reference —
// an object whose $type is "blob" carrying a ref CID link. It descends maps and
// arrays so a blob nested anywhere in the embed union (images, video, external
// thumb) is found.
func extractBlobRefs(node any) []rematerializeBlobRef {
	var out []rematerializeBlobRef
	switch v := node.(type) {
	case map[string]any:
		if t, _ := v["$type"].(string); t == "blob" {
			if cid := blobLinkCID(v); cid != "" {
				mimeType, _ := v["mimeType"].(string)
				out = append(out, rematerializeBlobRef{cid: cid, mimeType: mimeType})
			}
		}
		for _, child := range v {
			out = append(out, extractBlobRefs(child)...)
		}
	case []any:
		for _, child := range v {
			out = append(out, extractBlobRefs(child)...)
		}
	}
	return out
}

// blobLinkCID reads the CID out of a decoded blob reference, tolerating both the
// canonical {"ref":{"$link":cid}} shape and a bare string ref.
func blobLinkCID(blob map[string]any) string {
	switch ref := blob["ref"].(type) {
	case map[string]any:
		if link, ok := ref["$link"].(string); ok {
			return link
		}
	case string:
		return ref
	}
	return ""
}

// httpRematerializeBlobClient is the production RematerializeBlobClient: bounded
// HTTP against com.atproto.sync.getBlob.
type httpRematerializeBlobClient struct {
	client *http.Client
	// maxBytes is the copy cap. It is a field rather than the package constant so
	// the overrun path can be exercised without moving 100 MiB through a test.
	maxBytes int
}

// DefaultRematerializeBlobClient is the production blob client, over an HTTP
// client with its OWN timeout and an ADDRESS GUARD, and it is the dev gate for
// this call site.
//
// http.DefaultClient has none, and a batch tool that hangs on a half-open socket
// is a batch tool the operator must kill and re-run — mid-migration, without
// knowing where it stopped. "Which client may this tool dial with" has a second
// half, and the guard is it. THE TWO METHODS DIAL DIFFERENT HOSTS, and both are
// data rather than config:
//
//   - Fetch takes communityHostURL's answer — hostURLOf on the COMMUNITY's repo,
//     which NewCommunityRepoFactory builds from the community's PDSURL database
//     column, written when a community is created or federated in. A federated
//     community's PDSURL is whatever the remote instance said it was.
//   - Present takes repoHostURL's answer — hostURLOf on the AUTHOR's repo, which
//     is the serviceEndpoint of the author's DID document, and minting a did:plc
//     with any endpoint is free.
//
// Either way it is the same attacker-chosen input class as the image proxy's
// pdsURL, arriving by a route that reads like internal plumbing. That this is an
// operator-run batch tool does not shrink the exposure: an attacker plants the
// endpoint and waits for the migration, which runs once, by hand, with thousands
// of records scrolling past and a refused address looking exactly like a dialled
// one.
//
// # BOTH OF THIS SITE'S OWN LIMITS ARE RE-APPLIED, AND ONE OF THEM IS A TRAP
//
// rematerializeBlobFetchTimeout is two MINUTES against the shared client's 15s,
// because a single blob may be 100 MiB — inheriting the shared ceiling would not
// hurry those copies, it would fail them.
//
// maxRematerializeBlobBytes is 100 MiB against oauth.DefaultMaxResponseBytes's
// 32 MiB — SMALLER — so unlike every other conversion in this remediation,
// adopting the shared client here TIGHTENS the limit unless WithMaxResponseBytes
// raises it, and the failure would arrive as an error on a blob between the two
// numbers, after the postv2 is written and before the legacy record is deleted.
// The transport cap and c.maxBytes are separate controls: the transport refuses
// an announced Content-Length before a byte of body is read, so setting only the
// field would leave a client whose cap says 100 MiB unable to receive 40.
//
// # THE DEV GATE IS THE ONLY THING A CALLER MAY SAY
//
// It takes the gate and nothing else, so "only allowPrivateHosts opens the
// guard" is a fact about the signature rather than a description of current call
// sites. It used to take `opts ...covesoauth.Option` as a test seam — an
// EXPORTED type, and covesoauth.WithPrivateAddressesAllowed() is an exported
// option, so any package in the tree could open the guard on the tool that
// copies blobs from whatever host a federated community's PDSURL or an author's
// DID document names, with nothing for the audit to grep. The seam is real and
// still needed (a guard test that builds its own client proves only that
// internal/atproto/oauth works), so it lives on
// newGuardedRematerializeBlobClient, which is unexported and reachable only from
// this package's own tests. users.NewProfileBackfillClient is the same shape.
func DefaultRematerializeBlobClient(allowPrivateHosts bool) RematerializeBlobClient {
	return newGuardedRematerializeBlobClient(allowPrivateHosts, maxRematerializeBlobBytes)
}

// newGuardedRematerializeBlobClient builds the production client at a copy cap
// the caller names, so the RELATIONSHIP between the two caps can be driven by a
// test without moving 100 MiB through one.
//
// # THE TRANSPORT CAP IS ONE BYTE ABOVE THE COPY CAP, DELIBERATELY
//
// Fetch reads maxBytes+1 through an io.LimitReader because io.ReadAll cannot
// tell "the body ended" from "the limit was reached" — the extra byte's
// existence IS the overrun signal. A transport cap set to exactly maxBytes
// clips that byte, so `len(data) > c.maxBytes` becomes unreachable in
// production and every overrun surfaces as a generic ErrResponseTooLarge
// naming a limit no operator configured, instead of the error explaining that a
// truncated copy is DIFFERENT bytes under a DIFFERENT CID. Not a hole — the
// oversized blob is refused either way — but the wrong message, during a
// one-shot migration that has already written the postv2 and is about to delete
// the only intact copy. imageproxy/fetcher.go:118 is the same trap, sprung the
// same way.
//
// The two caps are separate controls and both are needed: the transport refuses
// an announced Content-Length before a byte of body is read, while c.maxBytes is
// what a chunked body — which reports no length at all — is measured against.
func newGuardedRematerializeBlobClient(
	allowPrivateHosts bool, maxBytes int, opts ...covesoauth.Option,
) *httpRematerializeBlobClient {
	options := append(covesoauth.PrivateAddressOptions(allowPrivateHosts),
		covesoauth.WithMaxResponseBytes(int64(maxBytes)+1))
	client := covesoauth.NewSSRFSafeHTTPClient(append(options, opts...)...)
	client.Timeout = rematerializeBlobFetchTimeout
	return newRematerializeBlobClient(client, maxBytes)
}

// newRematerializeBlobClient is the constructor the tests use to shrink the cap.
func newRematerializeBlobClient(client *http.Client, maxBytes int) *httpRematerializeBlobClient {
	return &httpRematerializeBlobClient{client: client, maxBytes: maxBytes}
}

// Fetch downloads a blob's bytes via com.atproto.sync.getBlob.
//
// IT READS ONE BYTE PAST THE CAP ON PURPOSE. io.ReadAll over a LimitReader
// cannot tell "the body ended" from "the limit was reached", so a blob larger
// than the cap used to be uploaded TRUNCATED — under a different CID, referenced
// by a postv2 that would then be verified against the wrong bytes. Over-reading
// by one byte makes the overrun observable, and it is an error.
func (c *httpRematerializeBlobClient) Fetch(ctx context.Context, host, did, cid string) ([]byte, error) {
	resp, err := c.get(ctx, host, did, cid)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("getBlob returned status %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(c.maxBytes)+1))
	if err != nil {
		return nil, fmt.Errorf("reading blob bytes: %w", err)
	}
	if len(data) > c.maxBytes {
		return nil, fmt.Errorf(
			"blob %s exceeds the %d-byte copy cap; a truncated copy is DIFFERENT bytes, so it would be stored under a different CID and the postv2 would point at media the repo does not serve",
			cid, c.maxBytes)
	}
	return data, nil
}

// Present reports whether the repo serves the blob. A transport failure is an
// error, never a false.
func (c *httpRematerializeBlobClient) Present(ctx context.Context, host, did, cid string) (bool, error) {
	resp, err := c.get(ctx, host, did, cid)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, int64(c.maxBytes)))
	switch {
	case resp.StatusCode == http.StatusOK:
		return true, nil
	case resp.StatusCode == http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("getBlob for %s in %s returned status %d, which is neither 'here' nor 'absent'", cid, did, resp.StatusCode)
	}
}

func (c *httpRematerializeBlobClient) get(ctx context.Context, host, did, cid string) (*http.Response, error) {
	blobURL := blobs.HydrateBlobURL(host, did, cid)
	if blobURL == "" {
		return nil, fmt.Errorf("could not build a getBlob URL for %s / %s on %q", did, cid, host)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, blobURL, nil)
	if err != nil {
		return nil, fmt.Errorf("building the getBlob request for %s: %w", cid, err)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("requesting blob %s from %s: %w", cid, did, err)
	}
	return resp, nil
}

// repoHostURL extracts the PDS host the author repo is bound to, when the concrete
// repo exposes one. The production pds.Client does; the state-machine fakes do
// not, and they never need it because their records carry no blobs.
func repoHostURL(repo AuthorRepo) (string, bool) {
	return hostURLOf(repo)
}

// hostURLOf reads the PDS host off anything that exposes one.
func hostURLOf(repo any) (string, bool) {
	if h, ok := repo.(interface{ HostURL() string }); ok {
		if host := h.HostURL(); host != "" {
			return host, true
		}
	}
	return "", false
}

// cloneRecord deep-copies a decoded record through a JSON round-trip, which both
// detaches it from the caller's map and NORMALISES any typed value (a struct blob
// ref, a json.Number) into the plain map/slice/string/float64 tree the conversion
// and the blob walk expect.
func cloneRecord(record map[string]any) map[string]any {
	raw, err := json.Marshal(record)
	if err != nil {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]any{}
	}
	return out
}

// sameRecordBody reports whether two decoded records are byte-identical once
// canonicalised. json.Marshal sorts map keys, so two equal records — regardless
// of how their values were originally typed — serialise to the same bytes.
func sameRecordBody(a, b map[string]any) bool {
	aj, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bj, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return string(aj) == string(bj)
}

// legacyFromLedgerRow reconstructs the LegacyPost the reconcile pass needs to
// finish a row past the postv2 write. It carries no RawRecord — the reconcile
// path re-reads the record from the source when it needs the body — but it does
// carry the community DID and the source CID, which are what the scope check and
// the guarded delete are made of.
func legacyFromLedgerRow(row RematerializeLedgerRow) LegacyPost {
	return LegacyPost{
		URI:          row.OldURI,
		CID:          row.SourceCID,
		CommunityDID: row.CommunityDID,
		AuthorDID:    row.AuthorDID,
	}
}

// distinct returns the unique values of a slice, order-preserving.
func distinct(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range values {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// joinFirst renders at most n values for an error message, saying how many more
// there are.
func joinFirst(values []string, n int) string {
	if len(values) <= n {
		return fmt.Sprint(values)
	}
	return fmt.Sprintf("%v and %d more", values[:n], len(values)-n)
}
