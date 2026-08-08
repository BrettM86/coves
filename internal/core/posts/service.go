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
//  6. Ensure the community has fresh PDS credentials — a step with no consumer
//     left on this path; see the note at the call site
//  7. Build the postv2 record
//  8. Validate and enhance external embeds (thumb validation, unfurl, blobs)
//  9. Create-only write at the deterministic rkey
//  10. If aggregator: record post for rate limiting
//  11. Seed the admission row and, for a community we host, settle it
//  12. Return URI/CID/status (AppView indexes asynchronously via Jetstream)
//
// Admission runs BEFORE the credentials, the blob uploads and the PDS write,
// so a refused submission costs a few lookups rather than an upload — and,
// more to the point, leaves no record in a community that refused it. Every
// failure AFTER admission (steps 5-9) must release the ledger reservation the
// admission took, or the failure costs the author a quota slot and refuses
// their retry as a duplicate.
//
// NOTHING AFTER THE RECORD COMMITS MAY FAIL THE REQUEST. The record is the
// author's and it exists; a failed acceptance, a failed row seed or a failed
// meter is degraded service, never data loss, and never a reason to withdraw
// someone else's record (§4.2).
func (s *postService) CreatePost(ctx context.Context, session *oauth.ClientSessionData, req CreatePostRequest) (*CreatePostResponse, error) {
	// 1. Validate basic input (before DID checks to give clear validation errors)
	if err := s.validateCreateRequest(&req); err != nil {
		return nil, err
	}

	// 1b. Normalize the fields the lexicon declares as `format: uri` before any
	// of them reach the record. Runs here, ahead of the community and PDS work,
	// so an unrecoverable URI fails fast without burning a DB lookup or an
	// unfurl fetch. Mutates req in place; the record built below reads these
	// same values, and the unfurl step then works from the encoded URI,
	// which dereferences identically.
	if err := normalizeEmbedURIs(req.Embed); err != nil {
		return nil, err
	}
	if err := richtext.NormalizeLinkURIs(req.Facets); err != nil {
		return nil, NewValidationErrorFrom("facets", err)
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

	community := decision.Community
	communityDID := community.DID

	// From here on the submission holds a ledger row. Every path that fails
	// before the record exists has to give it back, or a transient failure
	// permanently costs the author a quota slot AND blocks them from retrying
	// the same content until the dedupe window rolls.
	releaseOnFailure := func() {
		if decision.Reservation != nil {
			releaseReservation(ctx, s.admission.Ledger, *decision.Reservation)
		}
	}

	// 5. Open the AUTHOR's repository, which is where the record goes now.
	//
	// AHEAD OF THE COMMUNITY'S CREDENTIALS, because this is the credential the
	// post cannot be written without: a community whose token will not refresh
	// costs a link preview, while an author we cannot authenticate as has no
	// post at all. Ordering it second would report a community-side outage for
	// a signed-out user.
	authorRepo, err := s.openAuthorRepo(ctx, req.AuthorDID, session)
	if err != nil {
		releaseOnFailure()
		return nil, err
	}

	// 6. Ensure community has fresh PDS credentials (token refresh if needed).
	//
	// THIS STEP NOW HAS NO CONSUMER ON THIS PATH, and it should go. It existed
	// for the two writes that used the community's token — the post record and
	// its thumbnail blob — and both have moved to the author's repository. The
	// refreshed community is not read again below; only communityDID is, and
	// that was captured before the call.
	//
	// It survives because service_admission_test.go's token-refresh case still
	// requires a failed refresh to fail the submission, and that test is not
	// mine to retire. Removing it is a four-line deletion the moment it is.
	// Leaving it is not free: a community whose stored refresh token has rotted
	// currently blocks its authors from posting for no reason any longer
	// present in the code.
	community, err = s.communityService.EnsureFreshToken(ctx, community)
	if err != nil {
		releaseOnFailure()
		return nil, fmt.Errorf("failed to refresh community credentials: %w", err)
	}

	// 7. Build post record for PDS
	postRecord := postRecordFor(req, communityDID, time.Now().UTC().Format(time.RFC3339))

	// 8. Validate and enhance external embeds
	if err := s.enhanceExternalEmbed(ctx, &postRecord, req, authorRepo, actor == ActorTrustedAggregator); err != nil {
		releaseOnFailure()
		return nil, err
	}

	// 9. Write to the author's PDS repository.
	//
	// A failure here is the case the reservation was designed around: the row
	// went in before the write precisely so two concurrent identical submissions
	// would collide on the unique key, and the cost of that ordering is that a
	// write which never happened owes the author their slot back. Without it, a
	// PDS hiccup would consume a quota slot AND refuse the retry as a duplicate,
	// turning a transient outage into a per-author lockout that outlives it.
	rkey := SubmissionRkey(communityDID, fingerprint, decision.DedupeBucket, s.admission.Limits.DedupeWindow)
	uri, cid, err := createAuthorRecord(ctx, authorRepo, rkey, postV2From(postRecord))
	if err != nil {
		releaseOnFailure()
		return nil, fmt.Errorf("failed to write post to PDS: %w", err)
	}

	// 10. Record aggregator post for rate limiting (non-Kagi aggregators only)
	// Kagi is exempted from rate limiting via env var (temporary)
	if isOtherAggregator && s.aggregatorService != nil {
		if recordErr := s.aggregatorService.RecordAggregatorPost(ctx, req.AuthorDID, communityDID, uri, cid); recordErr != nil {
			// Log but don't fail - post was already created successfully
			log.Printf("[POST-CREATE] Warning: failed to record aggregator post for rate limiting: %v", recordErr)
		}
	}

	// 11. Seed the admission row and, for a community this AppView hosts,
	// settle it before answering.
	status := s.settleSubmission(ctx, communityDID, uri, cid)

	// 12. Return response (AppView will index via Jetstream consumer)
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
func createAuthorRecord(ctx context.Context, repo AuthorRepo, rkey string, record PostV2Record) (uri, cid string, err error) {
	commit, err := repo.PutRecordWithCommit(ctx, PostV2Collection, rkey, record, "")
	if err == nil {
		return commit.URI, commit.CID, nil
	}

	// TWO ANSWERS MEAN "IT IS ALREADY THERE", because the PDS orders its checks
	// that way (verified against a live one): a put of bytes IDENTICAL to what
	// stands is a no-op with no commit, and only a put of DIFFERENT bytes
	// reaches the swap guard and comes back InvalidSwap. Both are the retry
	// meeting its own first attempt, and both are answered the same way — by
	// reporting the record that stands.
	if !errors.Is(err, pds.ErrSwapConflict) && !errors.Is(err, pds.ErrNoCommit) {
		return "", "", err
	}

	standing, readErr := repo.GetRecord(ctx, PostV2Collection, rkey)
	if readErr != nil {
		// The record exists — that is what the swap conflict said — and we
		// cannot name it. Reporting the read failure rather than the conflict
		// keeps the cause the operator needs; the caller's retry converges on
		// the same key and will find it.
		return "", "", fmt.Errorf("the post already exists at %s but could not be read back: %w", rkey, readErr)
	}
	return standing.URI, standing.CID, nil
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

	// The swap guard is the CID that was just read. An edit landing between the
	// two is ErrConcurrentModification: the edit was composed against content
	// that no longer stands, and re-applying it would erase a change its author
	// never saw. Retrying is the client's decision, not the server's.
	commit, err := repo.PutRecordWithCommit(ctx, PostV2Collection, rkey, record, standing.CID)
	if err != nil {
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
// unchanged content work. Retyping both is a task-6 cycle-2 obligation, not a
// behavioural one — the bytes that reach the PDS are postV2From's.
func postRecordFor(req CreatePostRequest, community, createdAt string) PostRecord {
	return PostRecord{
		Type:           postCollection,
		Community:      community,
		Author:         req.AuthorDID,
		Title:          req.Title,
		Content:        req.Content,
		Facets:         req.Facets,
		Embed:          req.Embed, // Start with user-provided embed
		Labels:         req.Labels,
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
		OriginalAuthor: record.OriginalAuthor,
		FederatedFrom:  record.FederatedFrom,
		Location:       record.Location,
	}
}

// enhanceExternalEmbed applies the external-embed handling that has to happen
// against a live network: Bluesky URL conversion, client thumb validation, and
// unfurl enrichment with its blob uploads.
//
// It is a method rather than inline steps because every failure inside it now
// happens with a submission reservation already on the ledger, and a caller
// that has one error return to handle can give the reservation back in one
// place instead of at each of the four validation exits.
//
// trusted marks a trusted aggregator, which supplies its own metadata and is
// unfurled only for a thumbnail it did not provide.
func (s *postService) enhanceExternalEmbed(ctx context.Context, postRecord *PostRecord, req CreatePostRequest, authorRepo AuthorRepo, trusted bool) error {
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

	return nil
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

// validateCreateRequest validates basic input requirements
func (s *postService) validateCreateRequest(req *CreatePostRequest) error {
	// Global content limits (from lexicon)
	const (
		maxContentLength  = 100000 // 100k characters - matches social.coves.community.post lexicon
		maxTitleLength    = 3000   // 3k bytes
		maxTitleGraphemes = 300    // 300 graphemes (simplified check)
	)

	// Validate community required
	if req.Community == "" {
		return NewValidationError("community", "community is required")
	}

	// Validate author DID set by handler
	if req.AuthorDID == "" {
		return NewValidationError("authorDid", "authorDid must be set from authenticated user")
	}

	// Validate content length
	if req.Content != nil && len(*req.Content) > maxContentLength {
		return NewValidationError("content",
			fmt.Sprintf("content too long (max %d characters)", maxContentLength))
	}

	// Validate title length
	if req.Title != nil {
		if len(*req.Title) > maxTitleLength {
			return NewValidationError("title",
				fmt.Sprintf("title too long (max %d bytes)", maxTitleLength))
		}
		// Simplified grapheme check (actual implementation would need unicode library)
		// For Alpha, byte length check is sufficient
	}

	// Validate facets structurally against the content they annotate.
	// Catches out-of-range byte slices at the API boundary instead of
	// persisting a record whose annotations slice outside the content.
	contentByteLen := 0
	if req.Content != nil {
		contentByteLen = len(*req.Content)
	}
	if err := richtext.ValidateFacets(req.Facets, contentByteLen); err != nil {
		return NewValidationError("facets", err.Error())
	}

	// Validate content labels are from known values
	if req.Labels != nil {
		validLabels := map[string]bool{
			"nsfw":     true,
			"spoiler":  true,
			"violence": true,
		}
		for _, label := range req.Labels.Values {
			if !validLabels[label.Val] {
				return NewValidationError("labels",
					fmt.Sprintf("unknown content label: %s (valid: nsfw, spoiler, violence)", label.Val))
			}
		}
	}

	// Validate the embed (if provided) matches a known lexicon union member.
	// Catches malformed embeds at the API boundary instead of silently
	// persisting an unrenderable record to the PDS.
	if err := validateEmbed(req.Embed); err != nil {
		return err
	}

	// And the external embed's thumbnail, which validateEmbed deliberately does
	// not look at: it checks the union's SHAPE, and the thumb is a blob whose
	// parts a client gets wrong in four distinct ways worth naming separately.
	if err := validateExternalThumb(req.Embed); err != nil {
		return err
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

// Bounds for the social.coves.community.post.get endpoint.
const (
	postCollection = "social.coves.community.post"
	// MaxGetPostsURIs is the maximum number of URIs accepted by a single
	// social.coves.community.post.get request (matches the lexicon maxLength).
	// Exported so the handler layer reuses the same bound (single source of truth).
	MaxGetPostsURIs = 25
)

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
	views, err := s.repo.GetViewsByURIs(ctx, unique)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch post views: %w", err)
	}

	// 3. Assemble results in request order; valid-but-absent URIs become notFoundPost
	results := make([]*PostResult, len(req.URIs))
	for i, uri := range req.URIs {
		if view := views[uri]; view != nil {
			results[i] = foundResult(view)
		} else {
			results[i] = notFoundResult(uri)
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
		return "", "", NewValidationError(field, "invalid post URI format: expected at://authority/"+postCollection+"/rkey")
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
	if collection != postCollection && collection != PostV2Collection {
		return "", "", NewValidationError(field, fmt.Sprintf("invalid collection in URI: expected %s or %s, got %s",
			postCollection, PostV2Collection, collection))
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
			log.Printf("[POST-DELETE] Post not found in the author's repo (already deleted?): %s", uri)
			return nil
		}
		return fmt.Errorf("failed to delete post from PDS: %w", err)
	}

	log.Printf("[POST-DELETE] Successfully deleted post: uri=%s, author=%s", uri, userDID)
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
	record, err := pdsClient.GetRecord(ctx, postCollection, rkey)
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
	if err := pdsClient.DeleteRecord(ctx, postCollection, rkey); err != nil {
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
