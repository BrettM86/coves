package posts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"Coves/internal/api/middleware"
	"Coves/internal/atproto/pds"
	"Coves/internal/atproto/utils"
	"Coves/internal/core/aggregators"
	"Coves/internal/core/blobs"
	"Coves/internal/core/blueskypost"
	"Coves/internal/core/communities"
	"Coves/internal/core/richtext"
	"Coves/internal/core/unfurl"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

type postService struct {
	repo              Repository
	communityService  communities.Service
	aggregatorService aggregators.Service
	blobService       blobs.Service
	unfurlService     unfurl.Service
	blueskyService    blueskypost.Service
	blockChecker      BlockChecker
	admission         *AdmissionPolicy
	pdsURL            string

	// The author-owned write path (§4.2). authorRepos opens the AUTHOR's repo
	// under the author's own credentials; admissions and acceptor are the
	// local-community fast path — the row the post is seeded into and the
	// engine that settles it. See postv2.go.
	authorRepos AuthorRepoFactory
	admissions  AdmissionRepository
	acceptor    SubmissionAcceptor

	// acceptanceWithdrawal is the delete path's mirror of acceptor: the writer
	// that retracts, from a hosted community's repo, the acceptance the fast
	// path put there. See compensateAuthorDelete.
	acceptanceWithdrawal AcceptanceWithdrawer
}

// PostServiceOption configures optional postService dependencies. Options keep the
// constructor stable as new optional collaborators are added.
type PostServiceOption func(*postService)

// WithBlockChecker enables viewer block enforcement on GetPosts. When set and a viewer
// DID is present on the request, posts authored by users the viewer has blocked are
// returned as BlockedPost union members instead of full views, keeping permalink/
// cold-load reads consistent with feed/timeline block filtering.
func WithBlockChecker(checker BlockChecker) PostServiceOption {
	return func(s *postService) { s.blockChecker = checker }
}

// NewPostService creates a new post service
// aggregatorService, blobService, unfurlService, and blueskyService can be nil if not needed (e.g., in tests or minimal setups)
func NewPostService(
	repo Repository,
	communityService communities.Service,
	aggregatorService aggregators.Service, // Optional: can be nil
	blobService blobs.Service, // Optional: can be nil
	unfurlService unfurl.Service, // Optional: can be nil
	blueskyService blueskypost.Service, // Optional: can be nil
	pdsURL string,
	opts ...PostServiceOption,
) Service {
	s := &postService{
		repo:              repo,
		communityService:  communityService,
		aggregatorService: aggregatorService,
		blobService:       blobService,
		unfurlService:     unfurlService,
		blueskyService:    blueskyService,
		pdsURL:            pdsURL,
	}
	for _, opt := range opts {
		opt(s)
	}

	// THE DELETE COMPENSATION IS DEFAULT-ON. There is no wiring in which an
	// AppView should hold a community's credentials and still decline to
	// withdraw its acceptance of a post the author has deleted (§5.3), so
	// leaving it to an option would mean one omitted line silently restores the
	// bug: a signed acceptance left citing a record nobody can fetch, with
	// nothing anywhere to notice.
	//
	// The writer is a pure derivation of communityService, which is mandatory
	// here, and the factory's hosting test is CREDENTIAL PRESENCE — so for every
	// community this instance does not host it answers ErrCommunityNotHosted and
	// the delete path skips, which is the common case.
	if s.acceptanceWithdrawal == nil && communityService != nil {
		s.acceptanceWithdrawal = NewCommunityRecordWriter(NewCommunityRepoFactory(communityService), time.Now)
	}

	// The admission policy is mandatory, and a missing or partial one panics
	// here rather than defaulting to no-ops: a post service whose ban check and
	// quota silently do not exist is a wiring bug, not a configuration.
	mustCompleteAdmissionPolicy(s.admission)
	return s
}

// CreatePost writes a new post into the AUTHOR's repository (§4.2).
// Flow:
//  1. Validate input (and normalize embed/facet URIs)
//  2. Verify the authenticated DID matches the request's author DID
//  3. Classify the actor: trusted aggregator, registered aggregator, or user
//  4. Admission: one decision over community existence, visibility, ban,
//     aggregator authorization, dedupe and the per-author quota (admitPost)
//  5. Open the AUTHOR's repository under the author's own credentials
//  6. Build the postv2 record
//  7. Enhance external embeds (unfurl, and the thumbnail blob into the
//     author's own storage)
//  8. Create-only write at the deterministic rkey
//  9. If aggregator: record post for rate limiting
//  10. Seed the admission row and, for a community we host, settle it
//  11. Return URI/CID/status (AppView indexes asynchronously via Jetstream)

// THE COMMUNITY'S OWN CREDENTIALS ARE NOT ON THIS PATH AT ALL, and that is the
// point of the flip rather than an oversight. Both writes that used them — the
// post record and its thumbnail blob — are the AUTHOR's now. A refresh step
// retained here would fail for every community indexed from someone else's
// firehose, since those carry no stored tokens, and so would refuse exactly the
// remote-community submissions §4.2 step 5 exists to accept.
//
// Admission runs BEFORE the credentials, the blob uploads and the PDS write,
// so a refused submission costs a few lookups rather than an upload — and,
// more to the point, leaves no record in a community that refused it. Every
// failure AFTER admission (steps 5-8) must release the ledger reservation the
// admission took, or the failure costs the author a quota slot and refuses
// their retry as a duplicate.
//
// NOTHING AFTER THE RECORD COMMITS MAY FAIL THE REQUEST. The record is the
// author's and it exists; a failed acceptance, a failed row seed or a failed
// meter is degraded service, never data loss, and never a reason to withdraw
// someone else's record (§4.2).
func (s *postService) CreatePost(ctx context.Context, session *oauth.ClientSessionData, req CreatePostRequest) (*CreatePostResponse, error) {
	// 1. Validate basic input, and normalize the fields the lexicon declares as
	// `format: uri`, before any of them reach the record. Both live in the
	// SHARED gate (normalizeAndValidatePostContent) rather than here, so the
	// edit path cannot get a weaker version of either. It runs ahead of the
	// community and PDS work, so an unrecoverable URI fails fast without burning
	// a DB lookup or an unfurl fetch, and it mutates req in place.
	if err := s.validateCreateRequest(&req); err != nil {
		return nil, err
	}

	// 2. SECURITY: Extract authenticated DID from context (set by JWT middleware)
	// Defense-in-depth: verify service layer receives correct DID even if handler is bypassed
	authenticatedDID := middleware.GetAuthenticatedDID(ctx)
	if authenticatedDID == "" {
		return nil, fmt.Errorf("no authenticated DID in context - authentication required")
	}

	// SECURITY: Verify request DID matches authenticated DID from JWT
	// This prevents DID spoofing where a malicious client or compromised handler
	// could provide a different DID than what was authenticated
	if authenticatedDID != req.AuthorDID {
		log.Printf("[SECURITY] DID mismatch: authenticated=%s, request=%s", authenticatedDID, req.AuthorDID)
		return nil, fmt.Errorf("authenticated DID does not match author DID")
	}

	// 3. Determine actor type: trusted aggregator, other aggregator, or regular user.
	// The allowlist is read through the shared helper rather than inline, so
	// this path and the acceptance engine's decider cannot drift into disagreeing
	// about who is trusted — see TrustedAggregatorDIDs.
	isTrustedAggregator := TrustedAggregatorDIDs()[req.AuthorDID]

	// Check if this is a non-trusted aggregator (requires database lookup)
	var isOtherAggregator bool
	if !isTrustedAggregator && s.aggregatorService != nil {
		isAggregator, err := s.aggregatorService.IsAggregator(ctx, req.AuthorDID)
		if err != nil {
			log.Printf("[POST-CREATE] Warning: failed to check if DID is aggregator: %v", err)
			// Don't fail the request - treat as regular user if check fails
			isOtherAggregator = false
		} else {
			isOtherAggregator = isAggregator
		}
	}

	// The classification the admission decision is made against. It is resolved
	// HERE, at the call site, rather than inside admitPost: "who is trusted"
	// comes out of the process environment, and a decision function that read it
	// itself would hide the most consequential input to a security decision from
	// the place that makes it.
	actor := ActorUser
	switch {
	case isTrustedAggregator:
		log.Printf("[POST-CREATE] Trusted aggregator detected: %s posting to community: %s", req.AuthorDID, req.Community)
		actor = ActorTrustedAggregator
	case isOtherAggregator:
		actor = ActorRegisteredAggregator
	}

	// 4. ADMISSION: the single decision over community existence, visibility,
	// ban, aggregator authorization, dedupe and the per-author quota (§4.1, §8).
	//
	// The fingerprint is taken from the record as the CLIENT sent it, before
	// unfurl enhancement rewrites the embed: two submissions of the same content
	// must hash the same, and an enriched embed varies with whatever the remote
	// page served at the time. The thumbnail URL rides along as submitted; the
	// client-typed community identifier and the per-attempt timestamp are
	// excluded inside submissionFingerprint (see its doc comment).
	fingerprint := submissionFingerprint(postRecordFor(req, req.Community, ""), req.ThumbnailURL)
	decision, err := admitPost(ctx, s.admissionDeps(), AdmissionRequest{
		Actor:       actor,
		AuthorDID:   req.AuthorDID,
		Community:   req.Community,
		Fingerprint: fingerprint,
	})
	if err != nil {
		return nil, err
	}
	if !decision.Admitted() {
		log.Printf("[POST-CREATE] Refused: author=%s, community=%s, actor=%s, code=%s",
			req.AuthorDID, req.Community, actor, decision.Code)
		return nil, refusalError(decision)
	}

	communityDID := decision.Community.DID

	// From here on the submission holds a ledger row. Every path that fails
	// before the record exists has to give it back, or a transient failure
	// permanently costs the author a quota slot AND blocks them from retrying
	// the same content until the dedupe window rolls.
	releaseOnFailure := func() {
		if decision.Reservation != nil {
			releaseReservation(ctx, s.admission.Ledger, *decision.Reservation)
		}
	}

	// 5. Open the AUTHOR's repository, which is where the record goes now — and
	// the only credential this path needs at all.
	authorRepo, err := s.openAuthorRepo(ctx, req.AuthorDID, session)
	if err != nil {
		releaseOnFailure()
		return nil, err
	}

	// 6. Build post record for PDS
	postRecord := postRecordFor(req, communityDID, time.Now().UTC().Format(time.RFC3339))

	// 7. Enhance external embeds.
	//
	// It cannot fail, so there is nothing to release here — see its doc comment.
	// A future step added inside it that CAN fail must return an error, and this
	// call must then release the reservation before returning it, exactly as
	// steps 5 and 8 do.
	s.enhanceExternalEmbed(ctx, &postRecord, req, authorRepo, actor == ActorTrustedAggregator)

	// 8. Write to the author's PDS repository.
	//
	// A failure here is the case the reservation was designed around: the row
	// went in before the write precisely so two concurrent identical submissions
	// would collide on the unique key, and the cost of that ordering is that a
	// write which never happened owes the author their slot back. Without it, a
	// PDS hiccup would consume a quota slot AND refuse the retry as a duplicate,
	// turning a transient outage into a per-author lockout that outlives it.
	rkey := SubmissionRkey(communityDID, fingerprint, decision.DedupeBucket, s.admission.Limits.DedupeWindow)
	uri, cid, converged, err := createAuthorRecord(ctx, authorRepo, rkey, postV2From(postRecord))
	if err != nil {
		releaseOnFailure()
		return nil, fmt.Errorf("failed to write post to PDS: %w", err)
	}

	// 9. Record aggregator post for rate limiting (non-Kagi aggregators only)
	// Kagi is exempted from rate limiting via env var (temporary)
	//
	// A CONVERGED RETRY IS NOT A NEW POST AND MUST NOT BE BILLED AS ONE. The
	// write above found the aggregator's own record already standing and handed
	// back its URI; metering that again would charge the quota a second time for
	// one post, so an aggregator whose responses are being lost would be rate
	// limited for posts it never made — and the retry that finally got through
	// would be the one refused.
	if isOtherAggregator && !converged && s.aggregatorService != nil {
		if recordErr := s.aggregatorService.RecordAggregatorPost(ctx, req.AuthorDID, communityDID, uri, cid); recordErr != nil {
			// Log but don't fail - post was already created successfully
			log.Printf("[POST-CREATE] Warning: failed to record aggregator post for rate limiting: %v", recordErr)
		}
	}

	// 10. Seed the admission row and, for a community this AppView hosts,
	// settle it before answering.
	status := s.settleSubmission(ctx, communityDID, uri, cid)

	// 11. Return response (AppView will index via Jetstream consumer)
	log.Printf("[POST-CREATE] Author: %s (trustedKagi=%v, otherAggregator=%v), Community: %s, URI: %s, Status: %s",
		req.AuthorDID, isTrustedAggregator, isOtherAggregator, communityDID, uri, status)

	return &CreatePostResponse{
		URI:    uri,
		CID:    cid,
		Status: status,
	}, nil
}

// openAuthorRepo resolves the credentials the record is signed under.
//
// A service with no factory wired answers ErrNoAuthorCredentials rather than a
// nil-pointer panic: it is the same condition the production factory reports
// for an aggregator whose stored session is gone, and the boundary already
// knows how to answer it.
func (s *postService) openAuthorRepo(ctx context.Context, authorDID string, session *oauth.ClientSessionData) (AuthorRepo, error) {
	// DEFENCE IN DEPTH over a boundary the PDS also enforces. The session's own
	// DID is what the credentials will actually write as, so a request that
	// named a different author has already failed CreatePost's spoofing check —
	// this refuses the same thing one layer down, where a future caller that
	// skipped that check would otherwise reach the factory with an
	// author-supplied repo DID.
	if session != nil && session.AccountDID.String() != authorDID {
		log.Printf("[SECURITY] Author-repo session mismatch: session=%s, author=%s",
			session.AccountDID.String(), authorDID)
		return nil, ErrNotAuthorized
	}

	if s.authorRepos == nil {
		return nil, fmt.Errorf("opening the repository of %s: %w", authorDID, ErrNoAuthorCredentials)
	}

	repo, err := s.authorRepos(ctx, authorDID, session)
	if err != nil {
		return nil, err
	}
	if repo == nil {
		return nil, fmt.Errorf("opening the repository of %s: the factory returned no repo: %w",
			authorDID, ErrNoAuthorCredentials)
	}
	return repo, nil
}

// createAuthorRecord writes the post at its derived key, create-only, and
// reports the record that stands afterwards.
//
// THE GUARD IS THE IDEMPOTENCE. swapRecord "" means "there must be nothing
// here", so a retry of a submission whose first response was lost meets
// ErrSwapConflict instead of overwriting its own post — and is answered with
// the standing record's URI and CID rather than a fresh one. Re-putting an
// identical record would look harmless and be anything but: a new record CID
// dangles every strongRef built from the first response, and the second commit
// reaches every consumer as an EDIT, which drops an already-accepted post out
// of the community it was accepted into.
//
// converged reports that no new record was written — the caller is looking at a
// post that already existed.
//
// record is `any` rather than PostV2Record because the two callers assemble the
// body differently and both are correct: the write path passes a typed
// PostV2Record (postV2From), while the re-materialization tool passes the legacy
// record's lossless map so no published field is dropped in the conversion. Both
// serialise to the same postv2 shape; the guard and read-back are identical.
func createAuthorRecord(ctx context.Context, repo AuthorRepo, rkey string, record any) (uri, cid string, converged bool, err error) {
	commit, err := repo.PutRecordWithCommit(ctx, PostV2Collection, rkey, record, "")
	if err == nil {
		return commit.URI, commit.CID, false, nil
	}

	// TWO ANSWERS MEAN "IT IS ALREADY THERE", because the PDS orders its checks
	// that way (verified against a live one): a put of bytes IDENTICAL to what
	// stands is a no-op with no commit, and only a put of DIFFERENT bytes
	// reaches the swap guard and comes back InvalidSwap. Both are the retry
	// meeting its own first attempt, and both are answered the same way — by
	// reporting the record that stands.
	if !errors.Is(err, pds.ErrSwapConflict) && !errors.Is(err, pds.ErrNoCommit) {
		return "", "", false, err
	}

	standing, readErr := repo.GetRecord(ctx, PostV2Collection, rkey)
	if readErr != nil {
		// The record exists — that is what the swap conflict said — and we
		// cannot name it. Reporting the read failure rather than the conflict
		// keeps the cause the operator needs; the caller's retry converges on
		// the same key and will find it.
		return "", "", false, fmt.Errorf("the post already exists at %s but could not be read back: %w", rkey, readErr)
	}
	return standing.URI, standing.CID, true, nil
}

// settleSubmission seeds the admission row for the post just written and, for a
// community this AppView hosts, settles it synchronously (§4.2 steps 4 and 5).
//
// IT CANNOT FAIL THE REQUEST, and every return path here reflects that. The
// author's record has committed; the worst outcome available is that the
// community still owes a decision, which the firehose engine will make when the
// post reaches it. A rollback would be wrong twice over: the record is the
// AUTHOR's to withdraw, and a rollback whose own delete failed would leave a
// post nobody has a row for.
func (s *postService) settleSubmission(ctx context.Context, communityDID, postURI, postCID string) string {
	// Both or neither, by construction (WithSyncAcceptance). A service without
	// them is one whose posts wait for the firehose, which is the pre-flip
	// behaviour and a legitimate wiring.
	if s.admissions == nil || s.acceptor == nil {
		return PostStatusPending
	}

	// THE ROW COMES FIRST. The engine settles a row, so there has to be one —
	// and the URI it is seeded under must be byte-identical to the one the
	// firehose consumer builds from the same commit, or the two index one post
	// as two subjects and neither ever settles.
	seeded, err := s.admissions.UpsertPending(ctx, UpsertPendingCommand{
		CommunityDID: communityDID,
		PostURI:      postURI,
		EvaluatedCID: postCID,
		// A SEED, not an observation: it may create the row or re-affirm its own
		// CID, and may never overwrite content the firehose has already recorded.
		// The firehose is live in the window between the commit above and this
		// call, so the row may already hold a LATER version — see IsSeed.
		IsSeed: true,
	})
	if err != nil {
		log.Printf("[POST-CREATE] Warning: failed to seed the admission of %s in %s: %v",
			postURI, communityDID, err)
		return PostStatusPending
	}

	// ALREADY ACCEPTED, which is what a retry of a settled post finds. The row
	// is the AppView's answer about a post that exists, so reporting pending
	// here would show the author a "waiting for the community" state over a post
	// the community accepted — and a client that resubmitted would be handed the
	// same URI again, forever.
	if seeded.Admission != nil && seeded.Admission.Status == AdmissionStatusAccepted {
		return PostStatusAccepted
	}

	outcome, err := s.acceptor.AcceptSubmission(ctx, communityDID, postURI, postCID)
	if err != nil {
		if errors.Is(err, ErrCommunityNotHosted) {
			// §4.2 step 5, and NOT a failure: this AppView has no authoritative
			// view of that community's bans, visibility or quotas, so it must
			// not decide for it. The community decides when the post reaches it.
			log.Printf("[POST-CREATE] %s is hosted elsewhere; %s waits for its decision", communityDID, postURI)
			return PostStatusPending
		}
		log.Printf("[POST-CREATE] Warning: the synchronous acceptance of %s in %s failed, leaving it "+
			"pending for the firehose engine to retry: %v", postURI, communityDID, err)
		return PostStatusPending
	}

	if outcome == EngineAccepted {
		return PostStatusAccepted
	}
	return PostStatusPending
}

// UpdatePost edits a post in place in the author's repository.
//
// THE READ IS NOT A CONVENIENCE. community and createdAt are taken from the
// STANDING RECORD rather than from the request — the first because the lexicon
// calls it immutable, so an edit that changed it would be discarded entire by
// every consumer, and the second because every feed orders by it, so
// re-stamping it would jump a three-year-old post corrected for a typo to the
// top of every sort. The CID that read returns is also the swap guard, which is
// what makes a concurrent edit a detected conflict rather than a silent
// overwrite of a change its author never saw.
//
// THE SUBMISSION LEDGER IS NOT TOUCHED. An edit is not a submission: it
// consumes no quota, and writing the edited content's fingerprint would let the
// ORIGINAL content be resubmitted inside its own dedupe window.
func (s *postService) UpdatePost(ctx context.Context, session *oauth.ClientSessionData, req UpdatePostRequest) (*UpdatePostResponse, error) {
	if session == nil {
		return nil, NewValidationError("session", "OAuth session required")
	}
	userDID := session.AccountDID.String()

	if req.URI == "" {
		return nil, NewValidationError("uri", "post URI is required")
	}
	authority, rkey, err := parsePostURIParts(req.URI, "uri")
	if err != nil {
		return nil, err
	}
	if err := requireDIDAuthority(authority, "uri"); err != nil {
		return nil, err
	}

	// Only postv2 records are editable, and the reason is not squeamishness
	// about the deprecated collection: a community.post record lives in the
	// COMMUNITY's repo, so an edit would have to be signed by the community —
	// the AppView asserting a change to someone else's words. Task 8
	// re-materializes those posts into their authors' repos; until then they
	// are readable and deletable, not editable.
	if collection := CollectionOfPostURI(req.URI); collection != PostV2Collection {
		return nil, NewValidationError("uri", fmt.Sprintf(
			"editing a post is only supported for %s URIs, got %s", PostV2Collection, collection))
	}

	// THE URI'S AUTHORITY IS THE OWNER, so authorization is decided here,
	// before anything is fetched. The credentials the edit goes out on cannot
	// reach another author's repo even if this check were wrong, which is
	// exactly why it must be proven to exist rather than quietly removed.
	if authority != userDID {
		log.Printf("[SECURITY] Post update authorization failed: user=%s, authority=%s, uri=%s",
			userDID, authority, req.URI)
		return nil, ErrNotAuthorized
	}

	repo, err := s.openAuthorRepo(ctx, userDID, session)
	if err != nil {
		return nil, err
	}

	standing, err := repo.GetRecord(ctx, PostV2Collection, rkey)
	if err != nil {
		if errors.Is(err, pds.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("failed to fetch the post being edited: %w", err)
	}

	record, err := decodePostV2Record(standing.Value)
	if err != nil {
		return nil, fmt.Errorf("failed to decode the post being edited: %w", err)
	}

	// REFUSED, NOT SILENTLY IGNORED. Both leave the record's community intact,
	// but only one tells the client that the thing it asked for did not happen —
	// and a client that believed it had moved a post would show its author a
	// community the post is not in (§3.1: retargeting means a NEW post record).
	if req.Community != "" && req.Community != record.Community {
		return nil, NewValidationError("community",
			"a post's community is immutable; submitting it elsewhere means creating a new post")
	}

	applyPostV2Edit(&record, req)

	// NORMALIZED AND VALIDATED AFTER THE MERGE AND BEFORE THE PUT, and every
	// half of that is load-bearing. After the merge, because a facet's byte
	// range is meaningful only against the content the record will actually
	// hold — an edit sending facets and no content is checked against the
	// standing content, and one sending shorter content and no facets re-checks
	// the standing facets against it. Before the put, because a check that ran
	// afterwards would return exactly this error over a record already signed
	// and on the wire.
	//
	// THE SAME CALL THE CREATE PATH MAKES, which is the point: it is the one
	// function that refuses a javascript: embed or facet URI, caps the sources
	// array and percent-encodes what the lexicon declares `format: uri`. When
	// those ran at CreatePost's call site instead, an author could post a clean
	// link and then edit it into a URI create would never have accepted. It
	// rewrites record.Embed and record.Facets in place, so the put below carries
	// the normalized values.
	if err := normalizeAndValidatePostContent(postContent{
		Title:   record.Title,
		Content: record.Content,
		Facets:  record.Facets,
		Labels:  record.Labels,
		Embed:   record.Embed,
		Langs:   record.Langs,
		Tags:    record.Tags,
	}); err != nil {
		return nil, err
	}

	// The swap guard is the CID that was just read. An edit landing between the
	// two is ErrConcurrentModification: the edit was composed against content
	// that no longer stands, and re-applying it would erase a change its author
	// never saw. Retrying is the client's decision, not the server's.
	commit, err := repo.PutRecordWithCommit(ctx, PostV2Collection, rkey, record, standing.CID)
	if err != nil {
		// A PUT OF IDENTICAL BYTES IS A SUCCESS, not a failure. The PDS answers
		// a no-op write with a 200 carrying no commit (pds.ErrNoCommit, verified
		// against a live PDS on the create path), and three ordinary things
		// produce one: a client retrying after a lost response, a UI that saves
		// on blur whether or not anything changed, and an author who opens the
		// editor and saves without typing. In every case the record already
		// holds precisely what the client asked for — which is what it means for
		// the request to have succeeded — so the standing CID is the answer.
		if errors.Is(err, pds.ErrNoCommit) {
			log.Printf("[POST-UPDATE] No-op edit (the record already holds this content): %s", req.URI)
			return &UpdatePostResponse{URI: req.URI, CID: standing.CID}, nil
		}
		if errors.Is(err, pds.ErrSwapConflict) {
			return nil, fmt.Errorf("editing %s: %w", req.URI, ErrConcurrentModification)
		}
		return nil, fmt.Errorf("failed to write the edited post to PDS: %w", err)
	}

	log.Printf("[POST-UPDATE] Author: %s, URI: %s, CID: %s", userDID, commit.URI, commit.CID)

	return &UpdatePostResponse{URI: commit.URI, CID: commit.CID}, nil
}

// decodePostV2Record reads a standing record into the typed shape an edit is
// applied to.
//
// It round-trips through JSON rather than reading the map by hand so that the
// struct's tags stay the single description of the record: a field spelled one
// way in the writer and another in the reader is a field an edit silently
// erases, and postv2_record_test.go pins the struct against the lexicon for
// exactly that reason.
func decodePostV2Record(value map[string]any) (PostV2Record, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return PostV2Record{}, fmt.Errorf("re-encoding the standing record: %w", err)
	}
	var record PostV2Record
	if err := json.Unmarshal(encoded, &record); err != nil {
		return PostV2Record{}, fmt.Errorf("decoding the standing record: %w", err)
	}
	// A record read out of a repo may predate a $type being written, and the
	// collection it was fetched from is the authority on what it is.
	record.Type = PostV2Collection
	return record, nil
}

// applyPostV2Edit overwrites the mutable surfaces the request named, and only
// those. A nil field is "leave it alone" rather than "clear it": the update
// lexicon has no way to spell a deletion, so treating absence as removal would
// have a client editing a title silently drop the post's embed.
func applyPostV2Edit(record *PostV2Record, req UpdatePostRequest) {
	if req.Title != nil {
		record.Title = req.Title
	}
	if req.Content != nil {
		record.Content = req.Content
	}
	if req.Facets != nil {
		record.Facets = req.Facets
	}
	if req.Embed != nil {
		record.Embed = req.Embed
	}
	if req.Labels != nil {
		record.Labels = req.Labels
	}
	if req.Langs != nil {
		record.Langs = req.Langs
	}
	if req.Tags != nil {
		record.Tags = req.Tags
	}
}

// postRecordFor builds the record a request describes, stamped with the given
// community identifier and creation time.
//
// It is shared by the submission fingerprint and the record actually written,
// so that the thing dedupe hashes and the thing that lands in a repo cannot
// drift into describing different posts. The two callers differ in exactly the
// two arguments: the fingerprint is taken before the community identifier has
// been resolved and with no timestamp at all (createdAt is stamped per attempt,
// so including it would make every retry look new).
//
// IT IS STILL THE DEPRECATED SHAPE, and the write converts at the boundary
// (postV2From). Two things are typed against it that a postv2 record cannot
// carry today: the fingerprint, whose stability across the flip keeps the
// ledger's live dedupe rows valid, and the embed-enhancement pipeline, which is
// unchanged content work. Retyping both belongs to TASK 8, which re-materializes
// these records and is therefore the one moment a fingerprint repartition is
// free — see submissionFingerprint. Nothing behavioural rides on it: the bytes
// that reach the PDS are postV2From's.
func postRecordFor(req CreatePostRequest, community, createdAt string) PostRecord {
	return PostRecord{
		Type:           LegacyPostCollection,
		Community:      community,
		Author:         req.AuthorDID,
		Title:          req.Title,
		Content:        req.Content,
		Facets:         req.Facets,
		Embed:          req.Embed, // Start with user-provided embed
		Labels:         req.Labels,
		Langs:          req.Langs,
		Tags:           req.Tags,
		OriginalAuthor: req.OriginalAuthor,
		FederatedFrom:  req.FederatedFrom,
		Location:       req.Location,
		CreatedAt:      createdAt,
	}
}

// postV2From is the write boundary: the record the pipeline assembled, in the
// shape the AUTHOR's repository receives.
//
// THE AUTHOR FIELD IS DROPPED HERE, and this is the one place it happens. Under
// §3.1 authorship is the repository the record lives in — a claim a verifying
// relay or a DID-resolved fetch can check — so carrying the old self-asserted
// field alongside it would give consumers two answers to one question with no
// rule for which wins. The $type is re-stamped for the same reason: the
// collection a record is written to and the type it declares must agree.
func postV2From(record PostRecord) PostV2Record {
	return PostV2Record{
		Type:           PostV2Collection,
		Community:      record.Community,
		CreatedAt:      record.CreatedAt,
		Title:          record.Title,
		Content:        record.Content,
		Facets:         record.Facets,
		Embed:          record.Embed,
		Labels:         record.Labels,
		Langs:          record.Langs,
		Tags:           record.Tags,
		OriginalAuthor: record.OriginalAuthor,
		FederatedFrom:  record.FederatedFrom,
		Location:       record.Location,
	}
}

// enhanceExternalEmbed applies the external-embed handling that has to happen
// against a live network: Bluesky URL conversion and unfurl enrichment with its
// blob uploads.
//
// IT CANNOT FAIL, AND THE SIGNATURE SAYS SO. It used to return an error for the
// four thumb-validation exits it owned; those moved into the shared content gate
// (validateExternalThumb), which runs before admission — a client mistake is
// answerable with a 400 without costing a ledger reservation to discover. What
// is left is pure enhancement, and enhancement has never been able to fail a
// post: an unfurl that times out, a thumbnail that will not fetch and a Bluesky
// URL that will not resolve are all logged and stepped over, because the author
// asked to publish a post, not to publish a preview.
//
// It kept the error return for a while afterwards, with the caller holding a
// `releaseOnFailure()` branch for it, and both were dead. They are gone rather
// than kept as a seam, because an untestable branch is not a safety net — no
// fixture can make this function fail, so nothing could ever prove the branch
// worked. THE INVARIANT IT GUARDED IS STILL WRITTEN DOWN, at the call site and
// in CreatePost's own doc comment: this runs with a submission reservation on
// the ledger, so anything added here that CAN fail must return that error and
// the caller must release the reservation before returning it. Re-introducing
// the error return is the change that forces the caller to be edited, which is
// the point.
//
// trusted marks a trusted aggregator, which supplies its own metadata and is
// unfurled only for a thumbnail it did not provide.
func (s *postService) enhanceExternalEmbed(ctx context.Context, postRecord *PostRecord, req CreatePostRequest, authorRepo AuthorRepo, trusted bool) {
	if postRecord.Embed != nil {
		embedType, typeOk := postRecord.Embed["$type"].(string)
		if typeOk && embedType == "social.coves.embed.external" {
			if external, extOk := postRecord.Embed["external"].(map[string]interface{}); extOk {
				// Check if this is a Bluesky post URL and convert to post embed
				if !s.tryConvertBlueskyURLToPostEmbed(ctx, external, postRecord) {
					// Not a Bluesky URL or conversion failed - continue with normal external embed processing.
					// The thumb's shape was already checked in validateCreateRequest,
					// which needs no network and so must not wait for one.

					// TRUSTED AGGREGATOR: Allow Kagi aggregator to provide thumbnail URLs directly
					// This bypasses unfurl for more accurate RSS-sourced thumbnails
					if req.ThumbnailURL != nil && *req.ThumbnailURL != "" && trusted {
						log.Printf("[AGGREGATOR-THUMB] Trusted aggregator provided thumbnail: %s", *req.ThumbnailURL)

						blob, blobErr := s.uploadThumbnail(ctx, authorRepo, *req.ThumbnailURL)
						if blobErr != nil {
							log.Printf("[AGGREGATOR-THUMB] Failed to upload thumbnail: %v", blobErr)
							// No fallback - aggregators only use RSS feed thumbnails
						} else if blob != nil {
							external["thumb"] = blob
							log.Printf("[AGGREGATOR-THUMB] Successfully uploaded thumbnail from trusted aggregator")
						}
					}

					// Unfurl enhancement (optional, only if URL is supported)
					// For trusted aggregators: only unfurl for thumbnail if they didn't provide one
					// For regular users: full unfurl for all metadata
					needsThumbnailUnfurl := trusted && external["thumb"] == nil && (req.ThumbnailURL == nil || *req.ThumbnailURL == "")
					needsFullUnfurl := !trusted

					if needsThumbnailUnfurl || needsFullUnfurl {
						if uri, ok := external["uri"].(string); ok && uri != "" {
							// Check if we support unfurling this URL
							if s.unfurlService != nil && s.unfurlService.IsSupported(uri) {
								log.Printf("[POST-CREATE] Unfurling URL: %s (thumbnailOnly=%v)", uri, needsThumbnailUnfurl)

								// Unfurl with timeout (non-fatal if it fails)
								unfurlCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
								defer cancel()

								result, err := s.unfurlService.UnfurlURL(unfurlCtx, uri)
								if err != nil {
									// Log but don't fail - user can still post with manual metadata
									log.Printf("[POST-CREATE] Warning: Failed to unfurl URL %s: %v", uri, err)
								} else {
									// For regular users: enhance embed with fetched metadata
									// For trusted aggregators: skip metadata, they provide their own
									if needsFullUnfurl {
										// Enhance embed with fetched metadata (only if client didn't provide)
										// Note: We respect client-provided values, even empty strings
										// If client sends title="", we assume they want no title
										if external["title"] == nil {
											external["title"] = result.Title
										}
										if external["description"] == nil {
											external["description"] = result.Description
										}
										// Always set metadata fields (provider, domain, type)
										external["embedType"] = result.Type
										external["provider"] = result.Provider
										external["domain"] = result.Domain
									}

									// Upload thumbnail from unfurl if client didn't provide one
									// (Thumb validation already happened above)
									if external["thumb"] == nil && result.ThumbnailURL != "" {
										blob, blobErr := s.uploadThumbnail(ctx, authorRepo, result.ThumbnailURL)
										if blobErr != nil {
											log.Printf("[POST-CREATE] Warning: Failed to upload thumbnail for %s: %v", uri, blobErr)
										} else if blob != nil {
											external["thumb"] = blob
											log.Printf("[POST-CREATE] Uploaded thumbnail blob for %s", uri)
										}
									}

									if needsFullUnfurl {
										log.Printf("[POST-CREATE] Successfully enhanced embed with unfurl data (provider: %s, type: %s)",
											result.Provider, result.Type)
									} else {
										log.Printf("[POST-CREATE] Fetched thumbnail via unfurl for trusted aggregator")
									}
								}
							}
						}
					}
				}
			}
		}
	}
}

// uploadThumbnail fetches a remote thumbnail through the blob service's guard
// and puts it into the AUTHOR's own repository.
//
// THE GUARD RUNS FIRST AND THE UPLOAD ONLY HAPPENS IF IT PASSES. FetchImageForURL
// bounds the fetch with a timeout, refuses a Content-Type outside the image
// allowlist and caps the body at 6MB; a refusal returns before the author's PDS
// has been touched at all. Discarding a bad blob after uploading it would be the
// easy mistake and the wrong one — it pays the whole cost of not having a cap
// (the fetch, the transfer, the storage write, the author's quota) and only
// declines to show the result.
//
// A nil blob with a nil error means there was nothing to do: no blob service is
// wired, or no author repo is available. Both are wiring states a post survives
// — a thumbnail is an enhancement, and it has never been able to fail a post.
func (s *postService) uploadThumbnail(ctx context.Context, authorRepo AuthorRepo, imageURL string) (*blobs.BlobRef, error) {
	if s.blobService == nil || authorRepo == nil || imageURL == "" {
		return nil, nil
	}

	// One budget for the fetch and the upload together, as the single
	// UploadBlobFromURL call used to have: the pair is one enhancement, and
	// letting each half have its own would double the worst case a post waits.
	blobCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	data, mimeType, err := s.blobService.FetchImageForURL(blobCtx, imageURL)
	if err != nil {
		return nil, err
	}

	return authorRepo.UploadBlob(blobCtx, data, mimeType)
}

// validateCreateRequest validates basic input requirements, and normalizes the
// request's `format: uri` fields in place through the shared content gate.
func (s *postService) validateCreateRequest(req *CreatePostRequest) error {
	// The two fields only a SUBMISSION has. They are deliberately not in
	// normalizeAndValidatePostContent: an edit carries neither — the community is immutable
	// by lexicon and the author is the repository — so a validator that demanded
	// them would refuse every legitimate edit.
	if req.Community == "" {
		return NewValidationError("community", "community is required")
	}
	if req.AuthorDID == "" {
		return NewValidationError("authorDid", "authorDid must be set from authenticated user")
	}

	// Normalizes req.Embed and req.Facets IN PLACE as well as checking them —
	// the record built later reads these same values, and the unfurl step then
	// works from the encoded URI, which dereferences identically.
	return normalizeAndValidatePostContent(postContent{
		Title:   req.Title,
		Content: req.Content,
		Facets:  req.Facets,
		Labels:  req.Labels,
		Embed:   req.Embed,
		Langs:   req.Langs,
		Tags:    req.Tags,
	})
}

// postContent is the mutable surface of a post — everything an edit may change
// and a create may set, and nothing that identifies WHICH post it is.
//
// EVERY FIELD AN EDIT CAN WRITE HAS TO BE HERE. A field the record carries and
// this struct does not is a field the shared gate cannot see, and therefore one
// that reaches a signed record with no validation at all — which is exactly what
// happened to langs and tags before they were added.
type postContent struct {
	Title   *string
	Content *string
	Facets  []interface{}
	Labels  *SelfLabels
	Embed   map[string]interface{}
	Langs   []string
	Tags    []string
}

// Lexicon caps on the two list fields, from social.coves.community.postv2 (and
// matched by both the post.create and post.update procedure lexicons).
const (
	// maxLangs is the postv2 `langs` array maxLength.
	maxLangs = 3
	// maxTags is the postv2 `tags` array maxLength.
	maxTags = 8
	// maxTagLength is the postv2 per-tag maxLength, in BYTES.
	//
	// The lexicon also declares maxGraphemes 64, which is NOT checked here —
	// the same known gap the title check carries and names, and it is left as
	// one gap rather than two half-solutions. The byte cap is the one that
	// bounds what gets written; a grapheme cap needs a unicode segmentation
	// library and belongs with the title's when that lands.
	maxTagLength = 640
)

// normalizeAndValidatePostContent is the definition of a well-formed Coves post,
// and it is SHARED by the create and the edit path rather than duplicated across
// them.
//
// IT NORMALIZES IN PLACE AS WELL AS VALIDATING, which is why the name says so.
// The `format: uri` fields are repaired on the value the caller goes on to
// write — see the normalization section below — so a caller that skipped this
// function would not merely miss a check, it would sign un-encoded URIs.
//
// # THE APP LAYER IS THE ONLY GATE THERE IS
//
// Every write goes out as putRecord with `validate: false`, because the Coves
// lexicons are ones no PDS has been taught. That is deliberate and correct, and
// it has a consequence worth stating plainly: the PDS will sign and publish
// literally any JSON handed to it. There is no second opinion downstream, so
// whatever this function refuses IS the definition of a well-formed post — and a
// path that skipped it would be a hole straight through all of it, signed by the
// author's own key and published to the firehose as a valid record.
//
// # WHY SHARED AND NOT COPIED
//
// Two copies drift, and the copy that drifts is the one nobody is looking at.
// An edit path with its own slightly-different rules is how a client comes to be
// able to post cleanly and then edit into a record no validation would ever have
// allowed.
//
// # THE ARGUMENT IS THE MERGED RECORD
//
// Callers pass what the record WILL hold, not what the request happened to
// carry. It matters for facets, whose byte ranges are meaningful only against
// the content they annotate: an edit sending new facets and no content must be
// checked against the STANDING content, and one sending shorter content and no
// facets must re-check the STANDING facets against it. Either omission signs a
// record whose annotations slice outside its own text, which is a renderer crash
// on somebody else's client.
//
// # AND THE NORMALIZATION IS PART OF THE SAME GATE
//
// The `format: uri` fields — external.uri, each external.sources[].uri, and each
// facet #link uri — are normalized here rather than at one call site, because
// that is where they were and the edit path did not have one. Nothing else
// refuses a javascript: URI: validateEmbed only asks that external.uri be a
// non-empty string and never looks at sources at all, and richtext's structural
// check deliberately has no #link arm.
//
// Be precise about what that buys, because it is easy to overclaim: an author
// can write whatever record they like straight into their own PDS repo without
// going near this API, and the firehose ingest path does not scheme-check what
// it indexes. This is not the system's only defence against a hostile URI. What
// it IS: the guarantee that a record THIS AppView signs conforms to the lexicon
// it claims, identically on both paths that produce one.
func normalizeAndValidatePostContent(post postContent) error {
	// Global content limits (from lexicon)
	const (
		maxContentLength = 100000 // 100k characters - matches the postv2 lexicon
		maxTitleLength   = 3000   // 3k bytes
	)

	if post.Content != nil && len(*post.Content) > maxContentLength {
		return NewValidationError("content",
			fmt.Sprintf("content too long (max %d characters)", maxContentLength))
	}

	if post.Title != nil && len(*post.Title) > maxTitleLength {
		return NewValidationError("title",
			fmt.Sprintf("title too long (max %d bytes)", maxTitleLength))
		// Simplified grapheme check (actual implementation would need unicode library)
		// For Alpha, byte length check is sufficient
	}

	// Validate facets structurally against the content they annotate.
	// Catches out-of-range byte slices at the API boundary instead of
	// persisting a record whose annotations slice outside the content.
	contentByteLen := 0
	if post.Content != nil {
		contentByteLen = len(*post.Content)
	}
	if err := richtext.ValidateFacets(post.Facets, contentByteLen); err != nil {
		return NewValidationError("facets", err.Error())
	}

	// Validate content labels are from known values
	if post.Labels != nil {
		validLabels := map[string]bool{
			"nsfw":     true,
			"spoiler":  true,
			"violence": true,
		}
		for _, label := range post.Labels.Values {
			if !validLabels[label.Val] {
				return NewValidationError("labels",
					fmt.Sprintf("unknown content label: %s (valid: nsfw, spoiler, violence)", label.Val))
			}
		}
	}

	if err := validatePostLangs(post.Langs); err != nil {
		return err
	}

	if err := validatePostTags(post.Tags); err != nil {
		return err
	}

	// Validate the embed (if provided) matches a known lexicon union member.
	// Catches malformed embeds at the API boundary instead of silently
	// persisting an unrenderable record to the PDS.
	if err := validateEmbed(post.Embed); err != nil {
		return err
	}

	// And the external embed's thumbnail, which validateEmbed deliberately does
	// not look at: it checks the union's SHAPE, and the thumb is a blob whose
	// parts a client gets wrong in four distinct ways worth naming separately.
	if err := validateExternalThumb(post.Embed); err != nil {
		return err
	}

	// NORMALIZATION RUNS LAST, on the structure the checks above established:
	// normalizeEmbedURIs walks an embed validateEmbed has already shaped, and
	// NormalizeLinkURIs walks facets ValidateFacets has already shaped. Both
	// rewrite in place, so the caller's record carries the encoded values.
	if err := normalizeEmbedURIs(post.Embed); err != nil {
		return err
	}
	if err := richtext.NormalizeLinkURIs(post.Facets); err != nil {
		return NewValidationErrorFrom("facets", err)
	}
	return nil
}

// validatePostLangs enforces the postv2 lexicon's `langs`: at most maxLangs
// entries, each a real language tag.
//
// The format check is indigo's own parser rather than a hand-rolled one,
// because `format: language` means what the atProto spec says it means, and the
// package that ships ParseLanguage is the package every other implementation
// validates our records with.
func validatePostLangs(langs []string) error {
	if len(langs) > maxLangs {
		return NewValidationError("langs",
			fmt.Sprintf("too many langs: %d (max %d)", len(langs), maxLangs))
	}
	for i, lang := range langs {
		if _, err := syntax.ParseLanguage(lang); err != nil {
			return NewValidationError("langs",
				fmt.Sprintf("langs[%d] %q is not a valid language tag: %v", i, lang, err))
		}
	}
	return nil
}

// validatePostTags enforces the postv2 lexicon's `tags`: at most maxTags
// entries, each at most maxTagLength bytes.
//
// The empty-string refusal is deliberately STRICTER than the lexicon, which
// declares no minLength: a tag with no characters is an unusable entry that
// renders as a blank chip and matches nothing, the same reasoning that already
// refuses an embed source carrying no uri. Being stricter about what we SIGN is
// safe; consumers still accept whatever the schema allows.
func validatePostTags(tags []string) error {
	if len(tags) > maxTags {
		return NewValidationError("tags",
			fmt.Sprintf("too many tags: %d (max %d)", len(tags), maxTags))
	}
	for i, tag := range tags {
		if tag == "" {
			return NewValidationError("tags", fmt.Sprintf("tags[%d] must not be empty", i))
		}
		if len(tag) > maxTagLength {
			return NewValidationError("tags",
				fmt.Sprintf("tags[%d] too long: %d bytes (max %d)", i, len(tag), maxTagLength))
		}
	}
	return nil
}

// validateExternalThumb enforces that a social.coves.embed.external carries a
// real atProto blob reference in `thumb`, or nothing at all.
//
// Clients repeatedly send a URL STRING here, because that is what the rendered
// post looks like, and accepting one writes a record no other atProto
// implementation can read. Each of the four rejections names the part that is
// missing, because the message is the only thing telling a client which one it
// left out.
//
// IT RUNS AT VALIDATION TIME, before admission and before any credential is
// resolved, because it needs nothing but the request: a client mistake must be
// answerable with a 400 whether or not the author's repository can be opened,
// and it must not cost a ledger reservation to discover.
func validateExternalThumb(embed map[string]interface{}) error {
	if embed == nil {
		return nil
	}
	if embedType, ok := embed["$type"].(string); !ok || embedType != embedTypeExternal {
		return nil
	}
	external, ok := embed["external"].(map[string]interface{})
	if !ok {
		return nil
	}
	thumb := external["thumb"]
	if thumb == nil {
		// The common case: a bare link whose thumbnail unfurl fills in later.
		return nil
	}

	if thumbStr, isString := thumb.(string); isString {
		return NewValidationError("thumb",
			fmt.Sprintf("thumb must be a blob reference (with $type, ref, mimeType, size), not URL string: %s", thumbStr))
	}

	thumbMap, isMap := thumb.(map[string]interface{})
	if !isMap {
		return NewValidationError("thumb",
			fmt.Sprintf("thumb must be a blob object, got: %T", thumb))
	}
	if thumbType, ok := thumbMap["$type"].(string); !ok || thumbType != "blob" {
		return NewValidationError("thumb",
			fmt.Sprintf("thumb must have $type: blob (got: %v)", thumbType))
	}
	if _, hasRef := thumbMap["ref"]; !hasRef {
		return NewValidationError("thumb", "thumb blob missing required 'ref' field")
	}
	if _, hasMimeType := thumbMap["mimeType"]; !hasMimeType {
		return NewValidationError("thumb", "thumb blob missing required 'mimeType' field")
	}
	return nil
}

// tryConvertBlueskyURLToPostEmbed attempts to convert a Bluesky URL in an external embed to a post embed.
// Returns true if the conversion was successful and the postRecord was modified.
// Returns false if the URL is not a Bluesky URL or if conversion failed (caller should continue with external embed).
//
// A strongRef is an AT Protocol reference containing both URI (at://did/collection/rkey) and CID
// (content identifier hash). This function resolves the Bluesky URL to obtain both values,
// enabling rich embedded quote posts instead of plain external links.
func (s *postService) tryConvertBlueskyURLToPostEmbed(ctx context.Context, external map[string]interface{}, postRecord *PostRecord) bool {
	// 1. Check if blueskyService is available
	if s.blueskyService == nil {
		log.Printf("[POST-CREATE] BlueskyService unavailable, keeping as external embed")
		return false
	}

	// 2. Extract and validate URL
	url, ok := external["uri"].(string)
	if !ok || url == "" {
		return false
	}

	// 3. Check if it's a Bluesky URL
	if !s.blueskyService.IsBlueskyURL(url) {
		return false
	}

	// 4. Parse URL to AT-URI (resolves handle to DID if needed)
	atURI, err := s.blueskyService.ParseBlueskyURL(ctx, url)
	if err != nil {
		// Differentiate between timeout and other errors
		if errors.Is(err, context.DeadlineExceeded) {
			log.Printf("[POST-CREATE] WARN: Bluesky URL parse timed out, keeping as external embed: %s", url)
		} else {
			log.Printf("[POST-CREATE] Failed to parse Bluesky URL %s: %v", url, err)
		}
		return false // Fall back to external embed
	}

	// 5. Resolve post to get CID
	result, err := s.blueskyService.ResolvePost(ctx, atURI)
	if err != nil {
		// Differentiate error types for better debugging
		if errors.Is(err, blueskypost.ErrCircuitOpen) {
			log.Printf("[POST-CREATE] WARN: Bluesky circuit breaker OPEN, keeping as external embed: %s", atURI)
		} else {
			log.Printf("[POST-CREATE] Failed to resolve Bluesky post %s: %v", atURI, err)
		}
		return false // Fall back to external embed
	}

	if result == nil {
		log.Printf("[POST-CREATE] ERROR: ResolvePost returned nil result for %s", atURI)
		return false
	}

	// 6. Handle unavailable posts - keep as external embed since we can't get a valid CID
	if result.Unavailable {
		log.Printf("[POST-CREATE] Bluesky post unavailable, keeping as external embed: %s (reason: %s)", atURI, result.Message)
		return false
	}

	// 7. Validate we have both URI and CID
	if result.URI == "" || result.CID == "" {
		log.Printf("[POST-CREATE] ERROR: Bluesky post missing URI or CID (internal bug): uri=%q, cid=%q", result.URI, result.CID)
		return false
	}

	// 8. Convert embed to social.coves.embed.post with strongRef
	postRecord.Embed = map[string]interface{}{
		"$type": "social.coves.embed.post",
		"post": map[string]interface{}{
			"uri": result.URI,
			"cid": result.CID,
		},
	}

	log.Printf("[POST-CREATE] Converted Bluesky URL to post embed: %s (cid: %s)", result.URI, result.CID)
	return true
}

// GetAuthorPosts retrieves posts by a specific author with optional filtering
// Supports filtering by: posts_with_replies, posts_no_replies, posts_with_media
// Optionally filter to a specific community
func (s *postService) GetAuthorPosts(ctx context.Context, req GetAuthorPostsRequest) (*GetAuthorPostsResponse, error) {
	// 1. Validate request
	if err := s.validateGetAuthorPostsRequest(&req); err != nil {
		return nil, err
	}

	// 2. If community is provided, resolve it to DID
	if req.Community != "" {
		communityDID, err := s.communityService.ResolveCommunityIdentifier(ctx, req.Community)
		if err != nil {
			if communities.IsNotFound(err) {
				return nil, ErrCommunityNotFound
			}
			if communities.IsValidationError(err) {
				return nil, NewValidationError("community", err.Error())
			}
			return nil, fmt.Errorf("failed to resolve community identifier: %w", err)
		}
		req.Community = communityDID
	}

	// 3. Fetch posts from repository
	postViews, cursor, err := s.repo.GetByAuthor(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("failed to get author posts: %w", err)
	}

	// 4. Wrap PostViews in FeedViewPost
	feed := make([]*FeedViewPost, len(postViews))
	for i, postView := range postViews {
		feed[i] = &FeedViewPost{
			Post: postView,
		}
	}

	// 5. Return response
	return &GetAuthorPostsResponse{
		Feed:   feed,
		Cursor: cursor,
	}, nil
}

// MaxGetPostsURIs is the maximum number of URIs accepted by a single
// social.coves.community.post.get request (matches the lexicon maxLength).
// Exported so the handler layer reuses the same bound (single source of truth).
const MaxGetPostsURIs = 25

// GetPosts batch-fetches post views by AT-URI for feed hydration and permalink
// (cold-load) rendering. Implements social.coves.community.post.get.
//
// URIs must be canonical DID-based AT-URIs (at://<community-did>/social.coves.community.post/<rkey>).
// Handle-based authorities are rejected: handles are mutable, so a handle-based URI
// would break (or, if the handle is later reassigned, mis-resolve to the wrong community)
// after a rename. Resolving a human-readable handle to a DID is the caller's job, done
// once at the edge. DIDs are permanent, so DID-based URIs stay valid forever.
//
// Flow:
//  1. Validate the URI count (1..25) and that every URI is a well-formed DID-based URI.
//     A malformed or handle-based URI is a client error -> InvalidRequest, not a silent miss.
//  2. Batch fetch views for the (deduped) URIs.
//  3. Assemble results in request order; valid-but-absent URIs become notFoundPost.
//
// Viewer state (vote) and embed/blob transforms are applied by the handler layer.
func (s *postService) GetPosts(ctx context.Context, req GetPostsRequest) ([]*PostResult, error) {
	// 1. Validate batch size
	if len(req.URIs) == 0 {
		return nil, NewValidationError("uris", "at least one URI is required")
	}
	if len(req.URIs) > MaxGetPostsURIs {
		return nil, NewValidationError("uris", fmt.Sprintf("too many URIs (max %d)", MaxGetPostsURIs))
	}

	// Validate every URI up front and dedup for the batch fetch. A malformed or
	// handle-based URI fails the whole request with a clear error rather than silently
	// degrading to notFound, which would hide client bugs.
	uniqueSet := make(map[string]struct{}, len(req.URIs))
	for _, uri := range req.URIs {
		if err := validatePostURI(uri); err != nil {
			return nil, err
		}
		uniqueSet[uri] = struct{}{}
	}

	// 2. Batch fetch the (deduped) URIs
	unique := make([]string, 0, len(uniqueSet))
	for uri := range uniqueSet {
		unique = append(unique, uri)
	}
	// The viewer scopes the visibility gate. An anonymous permalink read passes
	// "" and gets accepted content only; an AUTHOR reading their own
	// pending/rejected/removed post gets it, exactly as actor.getPosts, the feeds
	// and the getComments thread header already give it to them (PRD §6.2). This
	// is the same req.ViewerDID the block filter below runs on — post.get used to
	// consult it for blocks and ignore it for admission, which made the permalink
	// the one surface that told an author their own post did not exist.
	views, err := s.repo.GetViewsByURIs(ctx, unique, req.ViewerDID)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch post views: %w", err)
	}

	// 3. Assemble results in request order. A visible view is a postView; an
	// absent URI is a notFoundPost — UNLESS its own community removed it, in
	// which case it becomes a #removedPost tombstone carrying the removal code
	// (PRD §3.4/§6.2). The visibility predicate hides a removed post from
	// GetViewsByURIs exactly as it hides a pending one, so the removal is
	// recovered here from the admission row rather than from the (absent) view.
	removed, err := s.removedMarkers(ctx, req.URIs, views)
	if err != nil {
		return nil, err
	}
	results := make([]*PostResult, len(req.URIs))
	for i, uri := range req.URIs {
		switch {
		case views[uri] != nil:
			results[i] = foundResult(views[uri])
		default:
			if code, ok := removed[uri]; ok {
				results[i] = removedResult(uri, code)
			} else {
				results[i] = notFoundResult(uri)
			}
		}
	}

	// 4. Enforce viewer block visibility: posts authored by someone the viewer has
	// blocked become blockedPost markers, consistent with feed/timeline filtering.
	// Only runs for an authenticated viewer when a block checker is wired.
	if req.ViewerDID != "" && s.blockChecker != nil {
		if err := s.applyViewerBlocks(ctx, req.ViewerDID, results); err != nil {
			return nil, err
		}
	}

	return results, nil
}

// removedMarkers returns, for the requested URIs absent from the visible view
// set, the removal code of any whose OWN community removed it — so post.get can
// serve a #removedPost tombstone (PRD §3.4) instead of collapsing a moderator
// removal into an indistinguishable notFoundPost. The presence of a URI in the
// returned map is the removed signal; the value is the code (possibly empty).
//
// It is a no-op when the admissions store is not wired (minimal setups and unit
// tests), leaving every absent URI a plain notFound — the pre-task-7 behavior.
// That is a CONFIGURATION fact, known before any lookup runs, and it is the only
// thing that silently degrades to notFound.
//
// A LOOKUP FAILURE IS AN ERROR, NOT A NOTFOUND. Both lookups here used to be
// best-effort: a database blip turned a standing removal into notFoundPost, so
// the same request answered with a different union member depending on the
// health of the database, and a client (or a moderator checking their own
// removal) could not tell "this post was taken down" from "we could not find
// out". post.get answering 5xx is the honest response to "we do not know";
// silently downgrading the tombstone is not, and it is unfalsifiable from the
// wire. Callers propagate the error.
func (s *postService) removedMarkers(ctx context.Context, uris []string, views map[string]*PostView) (map[string]string, error) {
	markers := make(map[string]string)
	if s.admissions == nil {
		return markers, nil
	}

	// Collect the absent URIs once (deduped), then resolve their admissions in a
	// single batched lookup rather than one round-trip per URI.
	seen := make(map[string]struct{}, len(uris))
	absent := make([]string, 0, len(uris))
	for _, uri := range uris {
		if views[uri] != nil {
			continue // visible — not a candidate for a tombstone
		}
		if _, done := seen[uri]; done {
			continue
		}
		seen[uri] = struct{}{}
		absent = append(absent, uri)
	}
	if len(absent) == 0 {
		return markers, nil
	}

	admissionsByURI, err := s.admissions.GetByPostURIs(ctx, absent)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve removal state for post.get: %w", err)
	}

	// The post rows are fetched in ONE batched round trip. Looping a per-URI
	// lookup here was an N+1 on a public endpoint whose URI list the caller
	// controls: 25 URIs (MaxGetPostsURIs) meant up to 25 sequential queries per
	// request, all of them for URIs the visibility predicate had already refused.
	//
	// These are RAW rows on purpose — the predicate has already hidden every URI
	// in `absent`, so a gated read would return nothing and there would be no
	// removal to report. The raw row is used for exactly two facts, both checked
	// below and neither of them content: which community owns the post, and
	// whether its author withdrew it.
	postsByURI, err := s.repo.GetRawIndexedRowsByURIs(ctx, absent)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve removal state for post.get: %w", err)
	}

	for _, uri := range absent {
		// A removal is an admission-state change, not a soft delete, so the post
		// row still stands and its own community — the key the admission is scoped
		// by — comes straight off it. A URI with no row is genuinely not-indexed
		// and stays a notFound.
		post := postsByURI[uri]
		if post == nil {
			continue
		}
		// A soft-deleted post is GONE, not a tombstone: the author withdrew it, so
		// even a standing removal must render as notFound rather than advertising
		// a moderation reason for a post its own author took down.
		if post.DeletedAt != nil {
			continue
		}

		for _, admission := range admissionsByURI[uri] {
			// The community half of this comparison is the fork oracle, and it is
			// load-bearing: a post can carry a removal from a community that FORKED
			// it while its own community has said nothing. Emitting that as a
			// tombstone would let any community publish a moderation verdict about
			// a post it does not host.
			if admission.CommunityDID == post.CommunityDID && admission.Status == AdmissionStatusRemoved {
				code := ""
				if admission.DecisionCode != nil {
					code = *admission.DecisionCode
				}
				markers[uri] = code
				break
			}
		}
	}
	return markers, nil
}

// applyViewerBlocks rewrites found posts whose author the viewer has blocked into
// blockedPost results (blockedBy "author"). It batches the block lookup over the unique
// author DIDs in the result set. On lookup failure it returns an error rather than
// emitting the posts: the endpoint fails closed so it can never surface content the
// viewer's block list would have hidden.
func (s *postService) applyViewerBlocks(ctx context.Context, viewerDID string, results []*PostResult) error {
	// Collect the unique author DIDs of the found posts.
	seen := make(map[string]struct{}, len(results))
	authorDIDs := make([]string, 0, len(results))
	for _, r := range results {
		if r.Post == nil || r.Post.Author == nil {
			continue
		}
		did := r.Post.Author.DID
		if did == "" {
			continue
		}
		if _, ok := seen[did]; ok {
			continue
		}
		seen[did] = struct{}{}
		authorDIDs = append(authorDIDs, did)
	}
	if len(authorDIDs) == 0 {
		return nil
	}

	blocked, err := s.blockChecker.AreBlocked(ctx, viewerDID, authorDIDs)
	if err != nil {
		return fmt.Errorf("failed to check viewer blocks: %w", err)
	}

	for i, r := range results {
		if r.Post == nil || r.Post.Author == nil {
			continue
		}
		if blocked[r.Post.Author.DID] {
			results[i] = blockedByAuthorResult(r.Post.URI, r.Post.Author.DID)
		}
	}
	return nil
}

// parsePostURIParts splits a post AT-URI into its authority and rkey, validating the
// scheme, structure, collection, and that both authority and rkey are present. The
// authority may be a DID or a handle; callers enforce the DID requirement (if any) via
// requireDIDAuthority. field names the request parameter for error attribution (e.g.
// "uri" or "uris"). This is the single source of truth for post-URI structure rules,
// shared by validatePostURI (get) and parsePostURI (delete). Pure (no I/O), so unit-testable.
func parsePostURIParts(uri, field string) (authority string, rkey string, err error) {
	if !strings.HasPrefix(uri, "at://") {
		return "", "", NewValidationError(field, "invalid AT-URI: must start with at://")
	}
	parts := strings.Split(strings.TrimPrefix(uri, "at://"), "/")
	if len(parts) != 3 {
		return "", "", NewValidationError(field, "invalid post URI format: expected at://authority/"+LegacyPostCollection+"/rkey")
	}
	authority, collection, rkey := parts[0], parts[1], parts[2]
	if authority == "" {
		return "", "", NewValidationError(field, "invalid post URI: missing authority")
	}
	// EITHER post collection is a well-formed post URI. A post now lives in the
	// author's repo under social.coves.community.postv2 (§3.1), while every post
	// written before the flip is still at the deprecated community-repo NSID, and
	// a reader has to be able to name both — refusing postv2 here made the new
	// records unfetchable by the endpoint that hydrates every feed.
	//
	// What the authority MEANS differs between them — the community for the old
	// collection, the author for the new — so a caller that goes on to use it as
	// one or the other must narrow this itself. parsePostURI does, because it
	// writes to the repo the authority names.
	if collection != LegacyPostCollection && collection != PostV2Collection {
		return "", "", NewValidationError(field, fmt.Sprintf("invalid collection in URI: expected %s or %s, got %s",
			LegacyPostCollection, PostV2Collection, collection))
	}
	if rkey == "" {
		return "", "", NewValidationError(field, "invalid post URI: missing rkey")
	}
	return authority, rkey, nil
}

// CollectionOfPostURI returns the collection segment of an at:// record URI, or
// "" when the URI is not shaped like one.
//
// It is exported because the two post collections are now indexed into one
// table, so every layer that renders or narrows a post has to ask the same
// question of the same URI — the repository decides which record shape to
// build from it, and this path decides which writes it will accept. A second
// spelling of the split is a second place for the two to disagree.
func CollectionOfPostURI(uri string) string {
	parts := strings.Split(strings.TrimPrefix(uri, "at://"), "/")
	if len(parts) != 3 {
		return ""
	}
	return parts[1]
}

// IsPostCollection reports whether an NSID names a post record — either the
// author-repo PostV2Collection or the deprecated community-repo one.
//
// IT ANSWERS ONE QUESTION: does a record in this collection live in the `posts`
// table? Both do, and they will for as long as the flip's dual-collection window
// is open: §10.1 truncates and re-indexes rather than migrating the schema, so a
// legacy record and an author-owned one produce rows that differ only in what
// their URI says, and everything downstream of the row — the counters, the feed
// queries, the rendered view — treats them alike. The prod drain (§11) is what
// finally closes the window, and until it has run, production holds records of
// both kinds and this AppView consumes both.
//
// It exists as one predicate rather than as a pair of comparisons at each site
// because the sites are the aggregation paths — the vote consumer, the comment
// consumer, the reconciliation tool — and their failure mode when they disagree
// is SILENT. A subject URI whose collection is not recognised falls to an
// "unsupported collection" branch that indexes the vote or the comment and
// simply never touches the counter: no error, no dead letter, just a post whose
// score never moves. One predicate is one place to delete when §11's follow-up
// retires the legacy NSID, and one place to be wrong in the meantime.
func IsPostCollection(collection string) bool {
	return collection == PostV2Collection || collection == LegacyPostCollection
}

// requireDIDAuthority enforces that a parsed post-URI authority is a DID (not a handle).
// Handles are mutable, so a handle-based URI would break after a community rename, or
// mis-resolve if the handle is later reassigned. field names the request parameter for errors.
func requireDIDAuthority(authority, field string) error {
	if !strings.HasPrefix(authority, "did:") {
		return NewValidationError(field, fmt.Sprintf("post URI authority must be a DID, got handle %q (resolve the community handle to its DID before calling)", authority))
	}
	if err := validateDIDFormat(authority); err != nil {
		return NewValidationError(field, fmt.Sprintf("invalid community DID in URI: %s", err.Error()))
	}
	return nil
}

// validatePostURI verifies that uri is a well-formed, canonical (DID-based) post AT-URI:
//
//	at://<community-did>/social.coves.community.post/<rkey>
//
// Used by GetPosts; callers must resolve handles to DIDs before calling.
func validatePostURI(uri string) error {
	authority, _, err := parsePostURIParts(uri, "uris")
	if err != nil {
		return err
	}
	return requireDIDAuthority(authority, "uris")
}

// validateGetAuthorPostsRequest validates the GetAuthorPosts request
func (s *postService) validateGetAuthorPostsRequest(req *GetAuthorPostsRequest) error {
	// Validate actor DID is set
	if req.ActorDID == "" {
		return NewValidationError("actor", "actor is required")
	}

	// Validate DID format - AT Protocol supports did:plc and did:web
	if err := validateDIDFormat(req.ActorDID); err != nil {
		return NewValidationError("actor", err.Error())
	}

	// Validate and set defaults for filter
	// Legacy snake_case values are normalized so pre-rename clients keep working
	req.Filter = strings.ReplaceAll(req.Filter, "_", "-")
	validFilters := map[string]bool{
		FilterPostsWithReplies: true,
		FilterPostsNoReplies:   true,
		FilterPostsWithMedia:   true,
	}
	if req.Filter == "" {
		req.Filter = FilterPostsWithReplies // Default
	}
	if !validFilters[req.Filter] {
		return NewValidationError("filter", "filter must be one of: posts-with-replies, posts-no-replies, posts-with-media")
	}

	// Validate and set defaults for limit
	if req.Limit <= 0 {
		req.Limit = 50 // Default
	}
	if req.Limit > 100 {
		req.Limit = 100 // Max
	}

	return nil
}

// validateDIDFormat validates that a string is a properly formatted DID
// Supports did:plc: (24 char base32 identifier) and did:web: (domain-based)
func validateDIDFormat(did string) error {
	const maxDIDLength = 2048

	if len(did) > maxDIDLength {
		return fmt.Errorf("DID exceeds maximum length")
	}

	switch {
	case strings.HasPrefix(did, "did:plc:"):
		// did:plc: format - identifier is 24 lowercase alphanumeric chars
		identifier := strings.TrimPrefix(did, "did:plc:")
		if len(identifier) == 0 {
			return fmt.Errorf("invalid did:plc format: missing identifier")
		}
		// Base32 uses lowercase a-z and 2-7
		for _, c := range identifier {
			if !((c >= 'a' && c <= 'z') || (c >= '2' && c <= '7')) {
				return fmt.Errorf("invalid did:plc format: identifier contains invalid characters")
			}
		}
		return nil

	case strings.HasPrefix(did, "did:web:"):
		// did:web: format - domain-based identifier
		domain := strings.TrimPrefix(did, "did:web:")
		if len(domain) == 0 {
			return fmt.Errorf("invalid did:web format: missing domain")
		}
		// Basic domain validation - must contain at least one dot or be localhost
		if !strings.Contains(domain, ".") && domain != "localhost" {
			return fmt.Errorf("invalid did:web format: invalid domain")
		}
		return nil

	default:
		return fmt.Errorf("unsupported DID method: must be did:plc or did:web")
	}
}

// communityCredentialFailure reports that the *community's* stored PDS
// credentials were rejected, deliberately severing the pds sentinel from the
// chain with %v rather than %w.
//
// Posts live in the community's repo, so deletes authenticate with the
// community's service token, not the caller's OAuth session. If that token were
// allowed to surface pds.ErrUnauthorized, the API boundary would read it as the
// caller's session being dead and answer 401 — telling a user with a perfectly
// healthy session to sign in again over a server-side credential problem they
// cannot fix, and hiding a real outage from 5xx alerting. Unclassified is the
// correct answer here: it becomes a logged 500.
func communityCredentialFailure(operation, communityDID string, err error) error {
	return fmt.Errorf("community PDS credentials rejected during %s for %s: %v",
		operation, communityDID, err)
}

// DeletePost removes a post record from the repository that holds it.
// SECURITY: Only the post author can delete their own posts.
//
// THE TWO COLLECTIONS AUTHORIZE DIFFERENTLY BECAUSE THEY LIVE IN DIFFERENT
// REPOS, and collapsing them would break one or the other:
//
//   - A postv2 URI names the AUTHOR's repo, so the URI's authority IS the
//     owner. The check is local, decided before anything is fetched, and the
//     credentials the delete goes out on cannot reach another author's repo
//     even if it were wrong.
//   - A deprecated community.post URI names the COMMUNITY's repo, where the
//     caller has no credentials at all. The delete goes out on the community's
//     service token — which could delete anyone's post — so the record's
//     `author` field has to be fetched and compared. That path survives until
//     task 8 re-materializes those posts into their authors' repos; every one
//     of them is standing in a community repo right now with a delete button
//     that has to keep working.
func (s *postService) DeletePost(ctx context.Context, session *oauth.ClientSessionData, req DeletePostRequest) error {
	// 1. Validate session
	if session == nil {
		return NewValidationError("session", "OAuth session required")
	}
	userDID := session.AccountDID.String()

	// 2. Validate URI shape, before anything reaches the network
	if err := s.validateDeleteRequest(&req); err != nil {
		return err
	}
	authority, rkey, err := parsePostURIParts(req.URI, "uri")
	if err != nil {
		return err
	}
	if err := requireDIDAuthority(authority, "uri"); err != nil {
		return err
	}
	// Defense-in-depth: verify rkey extraction is consistent with the utils helper.
	if extractedRkey := utils.ExtractRKeyFromURI(req.URI); extractedRkey != rkey {
		return NewValidationError("uri", "URI parsing inconsistency")
	}

	// 3. Route on the collection. parsePostURIParts has already refused
	// anything that is neither post collection.
	if CollectionOfPostURI(req.URI) == PostV2Collection {
		return s.deleteAuthorPost(ctx, session, userDID, authority, rkey, req.URI)
	}
	return s.deleteCommunityPost(ctx, userDID, authority, rkey, req.URI)
}

// deleteAuthorPost removes a postv2 record from its author's own repository.
func (s *postService) deleteAuthorPost(ctx context.Context, session *oauth.ClientSessionData, userDID, authorDID, rkey, uri string) error {
	// AUTHORIZATION IS THE URI'S AUTHORITY, decided before anything is fetched.
	//
	// Refused as UNAUTHORIZED rather than "not found", which is what the
	// pre-flip path answered for an authority it could not look up: a 404 there
	// would tell an attacker that the DID they aimed at is one this AppView has
	// never seen, and would answer 404 to a probe that deserves 403.
	if authorDID != userDID {
		log.Printf("[SECURITY] Post delete authorization failed: user=%s, authority=%s, uri=%s",
			userDID, authorDID, uri)
		return ErrNotAuthorized
	}

	repo, err := s.openAuthorRepo(ctx, userDID, session)
	if err != nil {
		return err
	}

	if err := repo.DeleteRecord(ctx, PostV2Collection, rkey); err != nil {
		if errors.Is(err, pds.ErrNotFound) {
			// Already deleted or never existed — the retried delete after a lost
			// response succeeds.
			//
			// AND IT STILL COMPENSATES. This is the shape a RETRY arrives in:
			// the first attempt removed the record and then failed part way
			// through the compensation below, so the second finds nothing left
			// to delete. Returning here would make the retry the client is
			// offered the one path that can never finish the work.
			log.Printf("[POST-DELETE] Post not found in the author's repo (already deleted?): %s", uri)
			return s.compensateAuthorDelete(ctx, uri)
		}
		return fmt.Errorf("failed to delete post from PDS: %w", err)
	}

	log.Printf("[POST-DELETE] Successfully deleted post: uri=%s, author=%s", uri, userDID)

	// The AppView's own half of the deletion, performed now rather than left to
	// a firehose event that may never arrive. See compensateAuthorDelete.
	return s.compensateAuthorDelete(ctx, uri)
}

// compensateAuthorDelete retracts everything the create wrote about a post
// whose author has just deleted it (§5.3), without waiting for the firehose
// copy of the deletion to come back.
//
// IT IS THE MIRROR OF settleSubmission. A create writes the postv2 into the
// author's repo, seeds the admission row and — for a community this AppView
// hosts — writes the acceptance into the community's repo and stamps the row,
// all before it answers the author. A delete undoes exactly those, and it is
// reachable with exactly the same credentials: the acceptance being withdrawn
// here is one this instance signed moments or months ago.
//
// THE FIREHOSE IS NOT A GUARANTEE, which is why this exists at all. The
// consumer's tombstoneAuthorPost only runs for an author whose PDS is on a
// configured jetstream feed, and a feed reconfiguration, a migrated repo or a
// consumer down for an afternoon all end the same silent way: this AppView
// keeps serving a post its author withdrew, the community's repo keeps a
// signed acceptance citing a record nobody can fetch, and getStatus keeps
// telling the author their deleted post is live in the community.
//
// The firehose copy is NOT made redundant by any of this — it still reaches
// every other AppView, and it still reaches this one on redelivery. Both paths
// are idempotent by construction (a withdrawal of nothing is a skip, and the
// §5.2 CAS refuses a rev that does not win), so doing the work twice is a
// no-op while doing it zero times is the failure above.
//
// A FAILURE IS RETURNED, and that is the one place this deliberately departs
// from the consumer, which logs and swallows. The consumer must: an error
// there dead-letters an event whose local half already committed, and the rev
// gate would refuse the redrive, so the retry could never reach the sweep
// again. Here the CLIENT is the retry loop — the author's record is already
// gone, every step below is idempotent, and surfacing the failure is what gets
// the remaining work done. Reporting success over a half-finished compensation
// reproduces the exact silence this path exists to end.
func (s *postService) compensateAuthorDelete(ctx context.Context, uri string) error {
	// THE LOCAL TRUTH LANDS FIRST, in the consumer's order and for its reason:
	// the author asked for their post to be gone, and a community PDS that
	// cannot be reached must not keep this AppView serving it.
	//
	// The consumer gates its tombstone on the commit rev; this path needs no
	// gate. SoftDelete is monotonic — NULL → NOW() under `WHERE deleted_at IS
	// NULL` — and the create it could race is protected by the indexer's own
	// ON CONFLICT DO NOTHING, so applying it here and again on the firehose
	// copy leaves the same row either way.
	if err := s.repo.SoftDelete(ctx, uri); err != nil {
		return fmt.Errorf("soft-deleting the indexed row of %s: %w", uri, err)
	}

	// Without the admissions store there is nothing that says WHICH community
	// accepted this post — a deletion carries no record to read it from — and
	// without a withdrawer there are no credentials to withdraw it with. That
	// combination is the pre-flip wiring, where the firehose owns every
	// community-repo write, and the tombstone above is all this path can do.
	if s.admissions == nil || s.acceptanceWithdrawal == nil {
		return nil
	}

	admissionsByURI, err := s.admissions.GetByPostURIs(ctx, []string{uri})
	if err != nil {
		// SURFACED, never swallowed. A read that failed says nothing about
		// whether an acceptance stands, and treating "I could not look" as
		// "there is nothing there" would reintroduce the silent version of this
		// bug through the back door.
		return fmt.Errorf("resolving the admissions of %s: %w", uri, err)
	}

	// EVERY community that admitted this post, not just the one the record
	// names. A post can also carry a decision from a community that FORKED it
	// (the case removedMarkers reads the same map for), and every acceptance of
	// it now cites a record that no longer exists. The ones this AppView does
	// not host answer ErrCommunityNotHosted and cost a lookup.
	for _, admission := range admissionsByURI[uri] {
		if err := s.withdrawAcceptanceOf(ctx, admission); err != nil {
			return err
		}
	}
	return nil
}

// withdrawAcceptanceOf removes one community's acceptance record and stamps the
// admission row that pointed at it.
//
// The two are one unit: the record is what the community publishes, the row is
// what this AppView answers getStatus from, and a withdrawal that did only the
// first would leave the author told their deleted post is still live.
func (s *postService) withdrawAcceptanceOf(ctx context.Context, admission *Admission) error {
	// THE GUARD IS THE ACCEPTANCE URI, NOT THE STATUS — the same test the
	// firehose sweep makes. `accepted` and `pending_reacceptance` both have a
	// live acceptance record standing in the community's repo (the second
	// merely pins content the author has since edited), and both must be
	// withdrawn when the subject itself is deleted. A row holding no URI has
	// nothing standing to withdraw.
	if admission == nil || admission.AcceptanceURI == nil {
		return nil
	}

	withdrawn, err := s.acceptanceWithdrawal.DeleteAcceptance(ctx, CommunityAcceptanceDeleteCommand{
		CommunityDID: admission.CommunityDID,
		PostURI:      admission.PostURI,
	})
	switch {
	case errors.Is(err, ErrCommunityNotHosted):
		// Not this instance's community, and no retry changes that: the
		// acceptance lives in the community's repo and needs its keys. The
		// community's own AppView performs this cleanup when the deletion
		// reaches it over the firehose, so the author's delete succeeds here
		// rather than failing forever on work this instance cannot do.
		log.Printf("[POST-DELETE] %s is hosted elsewhere; its acceptance of %s is not ours to withdraw",
			admission.CommunityDID, admission.PostURI)
		return nil
	case err != nil:
		return fmt.Errorf("withdrawing the acceptance of %s in %s: %w",
			admission.PostURI, admission.CommunityDID, err)
	}

	// A SKIPPED WITHDRAWAL STILL STAMPS, on the catch-up rev the writer reports
	// (the repo HEAD it read before its pre-read). Nothing stood, so the row's
	// claim to an acceptance is precisely what needs clearing — this is how a
	// row stranded by an earlier pass that committed the delete and then failed
	// this stamp is caught up. The engine's accept path stamps a skip for the
	// same reason.
	if withdrawn.Rev == "" {
		// Defensive only: the writer contract reports a rev on every path,
		// committed or skipped. An empty one must not be stamped — the
		// repository refuses it as a fabricated watermark, correctly.
		return nil
	}

	if _, err := s.admissions.ApplyAcceptanceDelete(ctx, CommunityDeleteCommand{
		CommunityDID: admission.CommunityDID,
		PostURI:      admission.PostURI,
		// The rev the withdrawal COMMITTED in — the §5.2 watermark that makes
		// the firehose copy of this same deletion a no-op instead of a second
		// decision. OpRank is left zero deliberately: the repository derives
		// the rank from the operation, because the rank IS the operation's kind.
		Watermark: CommunityWatermark{Rev: withdrawn.Rev},
	}); err != nil {
		// The record is OUT of the community's repo and the row still names it.
		// The firehose copy would reconcile it eventually, but "eventually" is
		// the assumption this whole path refuses to make.
		return fmt.Errorf("stamping the withdrawal of %s in %s: %w",
			admission.PostURI, admission.CommunityDID, err)
	}

	log.Printf("[POST-DELETE] Withdrew the acceptance of %s in %s", admission.PostURI, admission.CommunityDID)
	return nil
}

// deleteCommunityPost removes a pre-flip social.coves.community.post record
// from the community's repository, on the community's own credentials.
func (s *postService) deleteCommunityPost(ctx context.Context, userDID, communityDID, rkey, uri string) error {
	// 4. Fetch community from AppView
	community, err := s.communityService.GetByDID(ctx, communityDID)
	if err != nil {
		if communities.IsNotFound(err) {
			return ErrCommunityNotFound
		}
		return fmt.Errorf("failed to fetch community: %w", err)
	}

	// 5. Ensure community has fresh PDS credentials
	community, err = s.communityService.EnsureFreshToken(ctx, community)
	if err != nil {
		return fmt.Errorf("failed to refresh community credentials: %w", err)
	}

	// 6. Create PDS client for community repository
	pdsClient, err := pds.NewFromAccessToken(community.PDSURL, community.DID, community.PDSAccessToken)
	if err != nil {
		return fmt.Errorf("failed to create PDS client: %w", err)
	}

	// 7. Fetch post record from PDS to verify author
	record, err := pdsClient.GetRecord(ctx, LegacyPostCollection, rkey)
	if err != nil {
		if errors.Is(err, pds.ErrNotFound) {
			// Post already deleted or never existed - idempotent success
			log.Printf("[POST-DELETE] Post not found on PDS (already deleted?): %s", uri)
			return nil
		}
		if pds.IsAuthError(err) {
			return communityCredentialFailure("fetch post", community.DID, err)
		}
		return fmt.Errorf("failed to fetch post from PDS: %w", err)
	}

	// 8. SECURITY: Verify the requesting user is the post author
	// The author field in the record must match the authenticated user's DID
	postAuthor, ok := record.Value["author"].(string)
	if !ok || postAuthor == "" {
		return fmt.Errorf("post record missing author field: %s", uri)
	}

	if postAuthor != userDID {
		log.Printf("[SECURITY] Post delete authorization failed: user=%s, author=%s, uri=%s",
			userDID, postAuthor, uri)
		return ErrNotAuthorized
	}

	// 9. Delete record from community's PDS
	if err := pdsClient.DeleteRecord(ctx, LegacyPostCollection, rkey); err != nil {
		if errors.Is(err, pds.ErrNotFound) {
			// Already deleted - idempotent success
			log.Printf("[POST-DELETE] Post already deleted from PDS: %s", uri)
			return nil
		}
		if pds.IsAuthError(err) {
			return communityCredentialFailure("delete post", community.DID, err)
		}
		return fmt.Errorf("failed to delete post from PDS: %w", err)
	}

	// 10. Log success (AppView will update via Jetstream consumer)
	log.Printf("[POST-DELETE] Successfully deleted post: uri=%s, author=%s, community=%s",
		uri, userDID, communityDID)

	return nil
}

// validateDeleteRequest validates the delete post request
func (s *postService) validateDeleteRequest(req *DeletePostRequest) error {
	if req.URI == "" {
		return NewValidationError("uri", "post URI is required")
	}

	// Basic URI format check
	if !strings.HasPrefix(req.URI, "at://") {
		return NewValidationError("uri", "invalid AT-URI format: must start with at://")
	}

	return nil
}
