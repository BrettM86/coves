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
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"

	"Coves/internal/core/blobs"
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

// Run discovers every legacy record and drives each to a terminal state,
// returning the census.
//
// It runs in THREE ordered passes:
//
//  1. THE CREDENTIAL CENSUS (P8). Every discovered author is resolved with NO
//     repo mutation, so an author whose credentials cannot be restored is marked
//     a fallback BEFORE a single record is written or deleted. Without this, a
//     mutate-as-you-go run would fully migrate and delete the early records
//     before ever discovering that a later author is stranded — the exact
//     ordering §11 step 3 forbids.
//  2. THE SOURCE PASS. Each listed record is driven to a terminal state.
//  3. THE LEDGER RECONCILE (P7). A row whose delete succeeded but whose MarkDone
//     crashed is GONE from the community repo, so the source's listRecords can
//     never rediscover it — only ListResumable can. Every non-terminal ledger
//     row past the postv2 write is finished from the ledger.
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
	for _, legacy := range legacies {
		row, err := r.Ledger.Discover(ctx, legacy.URI, legacy.AuthorDID)
		if err != nil {
			return RematerializeReport{}, err
		}
		if row.State != RematerializeDiscovered {
			continue
		}
		if _, err := r.AuthorRepos(ctx, legacy.AuthorDID, nil); err != nil {
			if errors.Is(err, ErrNoAuthorCredentials) {
				reason := fmt.Sprintf("author %s has no restorable repo credentials: %v", legacy.AuthorDID, err)
				if markErr := r.Ledger.MarkFallback(ctx, legacy.URI, RematerializeFallbackLeftLegacy, reason); markErr != nil {
					return RematerializeReport{}, markErr
				}
				continue
			}
			return RematerializeReport{}, fmt.Errorf("preflighting the credentials of %s: %w", legacy.AuthorDID, err)
		}
	}

	// Pass 2 — the source pass. A record whose census marked it a fallback is left
	// untouched by RematerializeOne (it returns early on a terminal row).
	for _, legacy := range legacies {
		if _, err := r.RematerializeOne(ctx, legacy); err != nil {
			return RematerializeReport{}, fmt.Errorf("re-materializing %s: %w", legacy.URI, err)
		}
	}

	// Pass 3 — the ledger reconcile. Finish any row the source could not present.
	resumable, err := r.Ledger.ListResumable(ctx)
	if err != nil {
		return RematerializeReport{}, fmt.Errorf("listing resumable rows: %w", err)
	}
	for _, ledgerRow := range resumable {
		// A row still at discovered needs the ORIGINAL record's bytes to build its
		// postv2, which the ledger does not hold — only a source listing carries
		// them. Such a row is genuinely incomplete and keeps Complete false; it is
		// left for a pass whose source can present it, not driven off an empty body.
		if ledgerRow.State == RematerializeDiscovered {
			continue
		}
		legacy, err := legacyFromLedgerRow(ledgerRow)
		if err != nil {
			return RematerializeReport{}, err
		}
		if _, err := r.RematerializeOne(ctx, legacy); err != nil {
			return RematerializeReport{}, fmt.Errorf("reconciling %s: %w", ledgerRow.OldURI, err)
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
	// COMPLETE MEANS EVERY ROW REACHED done — not merely that no fallback survives
	// (P7). A row stranded in any non-terminal state, or a surviving fallback, both
	// leave Done < Discovered, and the operator's irreversible legacy-removal step
	// (§11 step 6) must not run while either is true.
	report.Complete = report.Done == report.Discovered

	return report, nil
}

// RematerializeOne drives a single legacy record from wherever its ledger row
// stands to a terminal state, and returns the state it reached.
//
// The steps are guarded on the ledger state each moves FROM, so a resumed run
// re-enters at exactly the step its predecessor stopped before and re-does none
// of the completed ones. The load-bearing ordering is VERIFY BEFORE DELETE: the
// old record is deleted only after the postv2, its embed blobs, and its
// acceptance are all confirmed present and consistent, and the migrated
// checkpoint is persisted BEFORE the delete so a crash there retries only the
// delete.
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

	// Step 1 — postv2_written. Copy the embed blobs into the author's repo, build
	// the postv2 LOSSLESSLY from the raw legacy record, and write it at the
	// deterministic rkey. createAuthorRecord is create-only and converges by read,
	// so a resume that re-enters here finds its own first attempt rather than
	// minting a second post.
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

		// P5 — the conversion is built from the LOSSLESS raw record, dropping only
		// the author field and re-stamping $type. Building it through PostRecord
		// would silently strip langs/tags/crosspostOf/crosspostChain/bridgedStats,
		// which the old record can never be recovered from once it is deleted.
		intended, err := postV2Body(legacy)
		if err != nil {
			return row.State, err
		}

		// P4 — the embed's blob BYTES must live in the AUTHOR's repo before the old
		// record (and the community's blob store) can go, or the postv2's media
		// resolves against a repo that never held it. The bytes are UPLOADED here,
		// before the record that references them is written; the PDS only serves an
		// uploaded blob once a record pins it, so presence is VERIFIED after the
		// write, below.
		if err := r.uploadEmbedBlobs(ctx, repo, legacy); err != nil {
			return row.State, err
		}

		rkey := RematerializeRkey(legacy.URI)
		newURI, newCID, converged, err := createAuthorRecord(ctx, repo, rkey, intended)
		if err != nil {
			return row.State, fmt.Errorf("writing the postv2 for %s: %w", legacy.URI, err)
		}

		// P6 — a converged write means a record ALREADY stood at the deterministic
		// rkey. createAuthorRecord accepts it by CID, but a CID match alone would
		// adopt a DIFFERENT record that merely shares the key. Confirm the standing
		// record IS this legacy post's conversion before trusting it; otherwise the
		// legacy original would be deleted in favour of a foreign record.
		if converged {
			standing, err := repo.GetRecord(ctx, PostV2Collection, rkey)
			if err != nil {
				return row.State, fmt.Errorf("reading the converged postv2 for %s: %w", legacy.URI, err)
			}
			if !sameRecordBody(standing.Value, intended) {
				return row.State, fmt.Errorf(
					"a different record already stands at %s in the author's repo: its body is not this legacy post's conversion, so re-materializing would adopt a foreign record and delete the real one",
					rkey)
			}
		}

		// P4 — now the postv2 pins the blobs, so the author repo actually serves
		// them. Confirm every embed blob is present BEFORE recording the postv2 (well
		// before the migrated checkpoint): a blob the author repo does not serve is a
		// broken image the moment the community's copy is garbage-collected.
		if err := r.verifyEmbedBlobsPresent(ctx, repo, legacy); err != nil {
			return row.State, err
		}

		if err := r.Ledger.RecordPostV2Written(ctx, legacy.URI, newURI, newCID, rkey); err != nil {
			return row.State, err
		}
		row.State = RematerializePostV2Written
		row.NewURI, row.NewCID, row.NewRkey = newURI, newCID, rkey
	}

	// Step 2 — verified. Write the community's acceptance DIRECT (never through the
	// engine — see the type's doc) pinning the NEW postv2 CID, confirm the
	// acceptance actually stands against OUR subject, then RE-READ the postv2 and
	// confirm it still pins that CID. Both reads happen before anything is deleted.
	if row.State == RematerializePostV2Written {
		res, err := r.Acceptances.WriteAcceptance(ctx, CommunityWriteCommand{
			CommunityDID: legacy.CommunityDID,
			PostURI:      row.NewURI,
			PostCID:      row.NewCID,
		})
		if err != nil {
			return row.State, fmt.Errorf("writing the acceptance for %s: %w", row.NewURI, err)
		}

		// P6 — verify the acceptance's subject strongRef. Its record key is the
		// digest of the subject URI, so a matching rkey proves the acceptance is FOR
		// our postv2; an empty CID would mean no record stands at all. A write that
		// returned neither has not made the community's acceptance real, and deleting
		// the legacy record on the strength of it would drop the post out of its
		// community.
		if res.CID == "" || res.RKey != SubjectRkey(row.NewURI) {
			return row.State, fmt.Errorf(
				"the acceptance for %s did not stand against the expected subject (got rkey %q cid %q, want rkey %q)",
				row.NewURI, res.RKey, res.CID, SubjectRkey(row.NewURI))
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

	// Step 3 — migrated. The checkpoint BEFORE the delete: postv2, blobs and
	// acceptance verified, old record still present. Persisting it as its own state
	// is what lets a crash on the delete retry ONLY the delete.
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

// maxRematerializeBlobBytes caps a single blob copy. It is generous — larger than
// any post media the lexicons admit — because the bytes come from our own
// community repo, not an untrusted origin; the cap exists to bound a corrupt or
// runaway response, not to enforce a content policy.
const maxRematerializeBlobBytes = 100 << 20 // 100 MiB

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

// uploadEmbedBlobs copies every embed blob's BYTES from the community's blob store
// into the author's repo. It runs BEFORE the postv2 record is written, because a
// record's embed may only reference blobs the repo has already received.
//
// The bytes are fetched via com.atproto.sync.getBlob against the instance PDS
// (the host the author repo is bound to, which also hosts the community's repo on
// a Coves instance) and uploaded through the author's own credentialed
// UploadBlob. A blob left uncopied fails the record here rather than after the
// old bytes are gone.
func (r *Rematerializer) uploadEmbedBlobs(ctx context.Context, repo AuthorRepo, legacy LegacyPost) error {
	refs := extractBlobRefs(cloneRecord(legacy.RawRecord))
	if len(refs) == 0 {
		return nil
	}

	host, ok := repoHostURL(repo)
	if !ok {
		return fmt.Errorf("copying blobs for %s: the author repo exposes no host URL to fetch the community's blobs from", legacy.URI)
	}

	for _, ref := range refs {
		data, err := fetchBlobBytes(ctx, host, legacy.CommunityDID, ref.cid)
		if err != nil {
			return fmt.Errorf("fetching embed blob %s from %s: %w", ref.cid, legacy.CommunityDID, err)
		}
		mimeType := ref.mimeType
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		if _, err := repo.UploadBlob(ctx, data, mimeType); err != nil {
			return fmt.Errorf("uploading embed blob %s into the author repo of %s: %w", ref.cid, repo.DID(), err)
		}
	}
	return nil
}

// verifyEmbedBlobsPresent confirms every embed blob is served by the author's
// repo — the P4 guarantee that the postv2's media resolves against the author,
// not the community repo the old record is about to be deleted from. It runs
// AFTER the postv2 is written, because the PDS serves a blob only once a record
// pins it; a 200 from getBlob proves the bytes actually landed.
func (r *Rematerializer) verifyEmbedBlobsPresent(ctx context.Context, repo AuthorRepo, legacy LegacyPost) error {
	refs := extractBlobRefs(cloneRecord(legacy.RawRecord))
	if len(refs) == 0 {
		return nil
	}

	host, ok := repoHostURL(repo)
	if !ok {
		return fmt.Errorf("verifying blobs for %s: the author repo exposes no host URL", legacy.URI)
	}

	for _, ref := range refs {
		if !blobPresent(ctx, host, repo.DID(), ref.cid) {
			return fmt.Errorf("embed blob %s is not present in the author repo of %s after the postv2 write; refusing to proceed toward the delete", ref.cid, repo.DID())
		}
	}
	return nil
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

// fetchBlobBytes downloads a blob's bytes from a repo via com.atproto.sync.getBlob.
func fetchBlobBytes(ctx context.Context, host, did, cid string) ([]byte, error) {
	blobURL := blobs.HydrateBlobURL(host, did, cid)
	if blobURL == "" {
		return nil, fmt.Errorf("could not build a getBlob URL for %s / %s", did, cid)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, blobURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("getBlob returned status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxRematerializeBlobBytes))
}

// blobPresent reports whether a repo serves a blob — a 200 from getBlob proves the
// repo actually holds the bytes.
func blobPresent(ctx context.Context, host, did, cid string) bool {
	blobURL := blobs.HydrateBlobURL(host, did, cid)
	if blobURL == "" {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, blobURL, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxRematerializeBlobBytes))
	return resp.StatusCode == http.StatusOK
}

// repoHostURL extracts the PDS host the author repo is bound to, when the concrete
// repo exposes one. The production pds.Client does; the state-machine fakes do
// not, and they never need it because their records carry no blobs.
func repoHostURL(repo AuthorRepo) (string, bool) {
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

// legacyFromLedgerRow reconstructs the minimal LegacyPost the reconcile pass needs
// to finish a row past the postv2 write: the delete step keys off the old URI, and
// the community DID is parsed back out of it. It deliberately carries no
// RawRecord — a reconciled row is past the point where the record body is read.
func legacyFromLedgerRow(row RematerializeLedgerRow) (LegacyPost, error) {
	communityDID, err := communityDIDFromURI(row.OldURI)
	if err != nil {
		return LegacyPost{}, err
	}
	return LegacyPost{
		URI:          row.OldURI,
		CommunityDID: communityDID,
		AuthorDID:    row.AuthorDID,
	}, nil
}

// communityDIDFromURI extracts the repo authority (the community DID) from an
// at:// record URI.
func communityDIDFromURI(uri string) (string, error) {
	const scheme = "at://"
	if len(uri) <= len(scheme) || uri[:len(scheme)] != scheme {
		return "", fmt.Errorf("cannot extract a community DID from %q: not an at:// URI", uri)
	}
	rest := uri[len(scheme):]
	for i := 0; i < len(rest); i++ {
		if rest[i] == '/' {
			return rest[:i], nil
		}
	}
	return "", fmt.Errorf("cannot extract a community DID from %q: no collection path", uri)
}
