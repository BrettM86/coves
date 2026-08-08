package posts

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// The re-materialization tool: the cutover step that moves every legacy
// social.coves.community.post record (written into a COMMUNITY's repo under the
// community's credentials) to an author-owned social.coves.community.postv2 in
// the AUTHOR's repo, plus the community's acceptance that pins it
// (docs/PRD_AUTHOR_OWNED_POSTS.md §10.1 step 5 and §11 the rev-2.8 deploy
// runbook).
//
// # THIS FILE IS A RED STUB
//
// Every exported symbol here exists so tests/e2e/rematerialize_contract_test.go
// and the T1 state-machine tests compile and FAIL for the right reason. The
// bodies return the not-implemented sentinel; GREEN fills them in. Nothing in
// this file decides anything yet.
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
// migrated → done; the two fallback states are terminal.
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
	RematerializeVerified RematerializeState = "verified"

	// RematerializeMigrated is the CHECKPOINT BEFORE DELETE, distinct from done
	// on purpose: it means "verified safe to delete, and the OLD record is still
	// present". A crash after this checkpoint resumes by retrying ONLY the delete
	// (§11 step 4).
	RematerializeMigrated RematerializeState = "migrated"

	// RematerializeDone: the old community.post record has been deleted.
	RematerializeDone RematerializeState = "done"

	// RematerializeFallbackLeftLegacy is the terminal state for a record whose
	// author credentials could not be restored: the postv2 is NOT written, the
	// old record is NOT deleted, the row is logged. §11 step 3 is explicit that
	// the fallback NEVER forges authorship (no admin-signed postv2), because
	// forging reintroduces the exact §2 impersonation liability the whole flip
	// removes.
	RematerializeFallbackLeftLegacy RematerializeState = "fallback_left_legacy"

	// RematerializeFallbackNoCreds is reserved terminal vocabulary for
	// distinguishing "the author-repo factory reported no credentials" from a
	// human-operator-flagged leave-as-legacy. Cycle 1 pins only the behaviour
	// (a no-creds author is left legacy and the run is not complete); which of
	// the two strings a given cause writes is a GREEN/owner decision — see the
	// ambiguity flag in the RED report.
	RematerializeFallbackNoCreds RematerializeState = "fallback_no_creds"
)

// IsFallback reports whether a state is one of the terminal fallback states, so
// the census can gate "complete" on any of them surviving without enumerating
// each string at every call site.
func IsFallback(state RematerializeState) bool {
	return state == RematerializeFallbackLeftLegacy || state == RematerializeFallbackNoCreds
}

// LegacyPost is one deprecated social.coves.community.post record discovered for
// migration.
type LegacyPost struct {
	// URI is the OLD record's AT-URI — at://<communityDID>/social.coves.community.post/<rkey>.
	// It is the ledger's primary key and the material the deterministic postv2
	// rkey is derived from.
	URI string

	// CID is the old record's content CID, carried for audit only. Authorship
	// and content come from the author's repo once the postv2 is written; the
	// old CID is never pinned by the new acceptance.
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
	// The conversion must run off RawRecord instead; this field is retained only
	// until GREEN removes the lossy path.
	Record PostRecord

	// RawRecord is the legacy record EXACTLY as it stands in the community repo —
	// the lossless source the postv2 is built from. The conversion drops only the
	// `author` field and re-stamps `$type`; every other field (including langs,
	// tags, crosspostOf, crosspostChain, bridgedStats, facets, embed, labels) is
	// carried through byte-for-byte, and createdAt is preserved so the
	// re-materialized post keeps its original time.
	RawRecord map[string]any
}

// LegacySource enumerates and deletes the deprecated community.post records.
//
// It is a seam so the T1 state machine runs against an in-memory source while
// the T2 contract and production run it against real community repos on the PDS
// (listRecords over social.coves.community.post, delete via the community's own
// credentials). The DELETE is idempotent by contract — a delete of an
// already-gone record reports success — because it is the resumed step a crash
// after the migrated checkpoint retries.
type LegacySource interface {
	ListLegacyPosts(ctx context.Context) ([]LegacyPost, error)
	DeleteLegacyPost(ctx context.Context, legacy LegacyPost) error
}

// RematerializeLedgerRow is one row of the migration-037 ledger.
type RematerializeLedgerRow struct {
	OldURI    string
	State     RematerializeState
	AuthorDID string

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
type RematerializeLedger interface {
	// Discover upserts the row for oldURI in state discovered, idempotently: a
	// re-run finds the existing row (whatever state it is in) rather than
	// resetting it.
	Discover(ctx context.Context, oldURI, authorDID string) (RematerializeLedgerRow, error)

	// Get reads one row. found is false when the URI has never been discovered.
	Get(ctx context.Context, oldURI string) (row RematerializeLedgerRow, found bool, err error)

	// ListResumable returns every row in a non-terminal state (not done, not a
	// fallback). It is what makes crash-resume drive off the LEDGER rather than
	// the source listing (whole-branch review, P7): a record whose delete
	// succeeded but whose MarkDone crashed is GONE from the community repo, so a
	// re-run's listRecords can never rediscover it — only the ledger row proves it
	// is owed a final MarkDone.
	ListResumable(ctx context.Context) ([]RematerializeLedgerRow, error)

	// RecordPostV2Written moves discovered → postv2_written and records the
	// postv2 coordinates.
	RecordPostV2Written(ctx context.Context, oldURI, newURI, newCID, newRkey string) error

	// MarkVerified moves postv2_written → verified.
	MarkVerified(ctx context.Context, oldURI string) error

	// MarkMigrated moves verified → migrated — the checkpoint before delete.
	MarkMigrated(ctx context.Context, oldURI string) error

	// MarkDone moves migrated → done, after the old record is deleted.
	MarkDone(ctx context.Context, oldURI string) error

	// MarkFallback moves the row to a terminal fallback state with a reason.
	MarkFallback(ctx context.Context, oldURI string, state RematerializeState, reason string) error

	// CountByState is the census: how many rows sit in each state, so the run
	// can refuse "complete" while any fallback survives.
	CountByState(ctx context.Context) (map[RematerializeState]int, error)
}

// RematerializeReport is the census a run returns.
type RematerializeReport struct {
	Discovered int
	Done       int
	Fallbacks  int
	ByState    map[RematerializeState]int

	// Complete is false while any fallback row survives — the gate on the
	// separate, manual legacy-removal follow-up (§11 step 6).
	Complete bool
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
}

// RematerializeRkey is the postv2 record key the tool writes a legacy record at.
//
// IT IS A PURE, STABLE FUNCTION OF THE OLD RECORD'S URI — the single
// highest-risk detail in the tool (§11 step 4). A re-run must recompute the
// IDENTICAL key so createAuthorRecord converges by read instead of minting a
// second postv2. It therefore CANNOT be SubmissionRkey, which needs the
// submission-time fingerprint and dedupe bucket the migration does not have — a
// re-run would draw a different key and duplicate the post.
//
// The scheme is the SubjectRkey digest scheme applied to the OLD URI: a total,
// collision-free (SHA-256) function that the write path already trusts. It is
// SubjectRkey verbatim, not a re-derivation, so the tool and the write path
// cannot come to disagree about what key a subject hashes to.
func RematerializeRkey(legacyPostURI string) string { return SubjectRkey(legacyPostURI) }

// Run discovers every legacy record and drives each to a terminal state,
// returning the census.
//
// A per-record error FAILS THE RUN rather than being logged and skipped: the
// safety properties are all ordering ones, and continuing past a record the tool
// could not verify would let the operator read a "done"-heavy census as
// permission to run the irreversible legacy-removal step while a record sits
// half-migrated. A no-creds fallback is NOT such an error — it is an expected
// terminal outcome the census counts — so it lets the run continue while still
// holding Complete false.
func (r *Rematerializer) Run(ctx context.Context) (RematerializeReport, error) {
	legacies, err := r.Source.ListLegacyPosts(ctx)
	if err != nil {
		return RematerializeReport{}, fmt.Errorf("enumerating legacy posts: %w", err)
	}

	for _, legacy := range legacies {
		if _, err := r.RematerializeOne(ctx, legacy); err != nil {
			return RematerializeReport{}, fmt.Errorf("re-materializing %s: %w", legacy.URI, err)
		}
	}

	byState, err := r.Ledger.CountByState(ctx)
	if err != nil {
		return RematerializeReport{}, fmt.Errorf("taking the census: %w", err)
	}

	report := RematerializeReport{ByState: byState}
	for state, n := range byState {
		report.Discovered += n
		if state == RematerializeDone {
			report.Done += n
		}
		if IsFallback(state) {
			report.Fallbacks += n
		}
	}
	// The gate on the separate, irreversible legacy-removal follow-up (§11 step
	// 6): a surviving fallback is a post still living only as a legacy record, so
	// the run must not tell the operator the migration is finished.
	report.Complete = report.Fallbacks == 0

	return report, nil
}

// RematerializeOne drives a single legacy record from wherever its ledger row
// stands to a terminal state, and returns the state it reached.
//
// The steps are guarded on the ledger state each moves FROM, so a resumed run
// re-enters at exactly the step its predecessor stopped before and re-does none
// of the completed ones. The load-bearing ordering is VERIFY BEFORE DELETE: the
// old record is deleted only after the postv2 and its acceptance are confirmed to
// pin the same CID, and the migrated checkpoint is persisted BEFORE the delete so
// a crash there retries only the delete.
func (r *Rematerializer) RematerializeOne(ctx context.Context, legacy LegacyPost) (RematerializeState, error) {
	row, err := r.Ledger.Discover(ctx, legacy.URI, legacy.AuthorDID)
	if err != nil {
		return "", err
	}

	// A row already in a terminal fallback state is left exactly as it stands: the
	// credential census reached its verdict on a prior pass, and re-opening it
	// would be the one thing §11 step 3 forbids — a second chance to forge.
	if IsFallback(row.State) {
		return row.State, nil
	}

	// Step 1 — postv2_written. Write the author-owned postv2 at the deterministic
	// rkey. createAuthorRecord is create-only and converges by read, so a resume
	// that re-enters here (it will not, but the guard is honest) would find its own
	// first attempt rather than mint a second post.
	if row.State == RematerializeDiscovered {
		repo, err := r.AuthorRepos(ctx, legacy.AuthorDID, nil)
		if err != nil {
			// NO CREDENTIALS IS A TERMINAL FALLBACK, NEVER A FORGERY. An author whose
			// repo cannot be restored is left as legacy — the postv2 is not written and
			// the old record survives — because re-authoring under any other identity
			// reintroduces the §2 impersonation the whole flip removes.
			if errors.Is(err, ErrNoAuthorCredentials) {
				reason := fmt.Sprintf("author %s has no restorable repo credentials: %v", legacy.AuthorDID, err)
				if markErr := r.Ledger.MarkFallback(ctx, legacy.URI, RematerializeFallbackLeftLegacy, reason); markErr != nil {
					return row.State, markErr
				}
				return RematerializeFallbackLeftLegacy, nil
			}
			return row.State, fmt.Errorf("opening the author repo of %s: %w", legacy.AuthorDID, err)
		}

		rkey := RematerializeRkey(legacy.URI)
		newURI, newCID, _, err := createAuthorRecord(ctx, repo, rkey, postV2From(legacy.Record))
		if err != nil {
			return row.State, fmt.Errorf("writing the postv2 for %s: %w", legacy.URI, err)
		}

		if err := r.Ledger.RecordPostV2Written(ctx, legacy.URI, newURI, newCID, rkey); err != nil {
			return row.State, err
		}
		row.State = RematerializePostV2Written
		row.NewURI, row.NewCID, row.NewRkey = newURI, newCID, rkey
	}

	// Step 2 — verified. Write the community's acceptance DIRECT (never through the
	// engine — see the type's doc) pinning the NEW postv2 CID, then RE-READ the
	// postv2 and confirm it still pins that CID. Verification reads the standing
	// record rather than trusting the write's returned CID, so a concurrent edit
	// landing in the write→verify window is caught here — before anything is
	// deleted.
	if row.State == RematerializePostV2Written {
		if _, err := r.Acceptances.WriteAcceptance(ctx, CommunityWriteCommand{
			CommunityDID: legacy.CommunityDID,
			PostURI:      row.NewURI,
			PostCID:      row.NewCID,
		}); err != nil {
			return row.State, fmt.Errorf("writing the acceptance for %s: %w", row.NewURI, err)
		}

		repo, err := r.AuthorRepos(ctx, legacy.AuthorDID, nil)
		if err != nil {
			return row.State, fmt.Errorf("re-opening the author repo of %s to verify: %w", legacy.AuthorDID, err)
		}
		standing, err := repo.GetRecord(ctx, PostV2Collection, row.NewRkey)
		if err != nil {
			return row.State, fmt.Errorf("verifying the postv2 for %s: %w", row.NewURI, err)
		}
		if standing.CID != row.NewCID {
			// VERIFY BEFORE DELETE fails closed: the acceptance now pins a CID the
			// postv2 no longer carries, so deleting the old record would destroy the
			// only copy of a post whose new attestation points at content that no
			// longer stands. No checkpoint, no delete — the row stays at
			// postv2_written for a later pass to re-verify.
			return row.State, fmt.Errorf(
				"verifying the postv2 for %s: the standing record pins %s but the acceptance pinned %s (a concurrent edit landed mid-verify)",
				row.NewURI, standing.CID, row.NewCID)
		}

		if err := r.Ledger.MarkVerified(ctx, legacy.URI); err != nil {
			return row.State, err
		}
		row.State = RematerializeVerified
	}

	// Step 3 — migrated. The checkpoint BEFORE the delete: postv2 and acceptance
	// verified, old record still present. Persisting it as its own state is what
	// lets a crash on the delete retry ONLY the delete.
	if row.State == RematerializeVerified {
		if err := r.Ledger.MarkMigrated(ctx, legacy.URI); err != nil {
			return row.State, err
		}
		row.State = RematerializeMigrated
	}

	// Step 4 — done. Delete the old community.post; a delete of an already-gone
	// record is success (the source's contract), so a resumed delete is idempotent.
	if row.State == RematerializeMigrated {
		if err := r.Source.DeleteLegacyPost(ctx, legacy); err != nil {
			return row.State, fmt.Errorf("deleting the old record %s: %w", legacy.URI, err)
		}
		if err := r.Ledger.MarkDone(ctx, legacy.URI); err != nil {
			return row.State, err
		}
		row.State = RematerializeDone
	}

	return row.State, nil
}
