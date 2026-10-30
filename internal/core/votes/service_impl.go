package votes

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"Coves/internal/atproto/pds"

	"github.com/bluesky-social/indigo/atproto/atclient"
	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"

	oauthclient "Coves/internal/atproto/oauth"
)

const (
	// voteCollection is the AT Protocol collection for vote records
	voteCollection                 = "social.coves.feed.vote"
	voteReconciliationTimeout      = 5 * time.Second
	voteReconciliationPollInterval = 500 * time.Millisecond
)

// PDSClient requires the repository operations used by vote writes, including
// atomic replacement. Factories cannot supply a single-write-only client.
type PDSClient interface {
	pds.Client
	ApplyWrites(context.Context, []pds.Write, string) (*pds.ApplyWritesResult, error)
}

// PDSClientFactory creates batch-capable PDS clients from session data.
// Used to allow injection of different auth mechanisms (OAuth for production, password for tests).
type PDSClientFactory func(ctx context.Context, session *oauth.ClientSessionData) (PDSClient, error)

// voteService implements the Service interface for vote operations
type voteService struct {
	repo             Repository
	oauthClient      *oauthclient.OAuthClient
	oauthStore       oauth.ClientAuthStore
	logger           *slog.Logger
	pdsClientFactory PDSClientFactory // Optional, for testing. If nil, uses OAuth.
	now              func() time.Time
	overlay          *recentWriteOverlay
	writeLocks       *viewerSubjectLocks
}

// NewService creates a new vote service instance
func NewService(repo Repository, oauthClient *oauthclient.OAuthClient, oauthStore oauth.ClientAuthStore, logger *slog.Logger) Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &voteService{
		repo:        repo,
		oauthClient: oauthClient,
		oauthStore:  oauthStore,
		now:         time.Now,
		overlay:     newRecentWriteOverlay(),
		writeLocks:  newViewerSubjectLocks(),
		logger:      logger,
	}
}

// NewServiceWithPDSFactory creates a vote service with a custom PDS client factory.
// This is primarily for testing with password-based authentication.
func NewServiceWithPDSFactory(repo Repository, logger *slog.Logger, factory PDSClientFactory) Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &voteService{
		repo:             repo,
		now:              time.Now,
		overlay:          newRecentWriteOverlay(),
		writeLocks:       newViewerSubjectLocks(),
		logger:           logger,
		pdsClientFactory: factory,
	}
}

// getPDSClient creates a PDS client from an OAuth session.
// If a custom factory was provided (for testing), uses that.
// Otherwise, uses DPoP authentication via indigo's APIClient for proper OAuth token handling.
func (s *voteService) getPDSClient(ctx context.Context, session *oauth.ClientSessionData) (PDSClient, error) {
	// Use custom factory if provided (e.g., for testing with password auth)
	if s.pdsClientFactory != nil {
		return s.pdsClientFactory(ctx, session)
	}

	// Production path: use OAuth with DPoP
	if s.oauthClient == nil || s.oauthClient.ClientApp == nil {
		return nil, fmt.Errorf("OAuth client not configured")
	}

	client, err := pds.NewFromOAuthSession(ctx, s.oauthClient.ClientApp, session)
	if err != nil {
		return nil, fmt.Errorf("failed to create PDS client: %w", err)
	}

	return client, nil
}

// CreateVote creates a new vote or toggles off an existing vote
// Implements the toggle behavior:
// - No existing vote → Create new vote with given direction
// - Vote exists with same direction → Delete vote (toggle off)
// - Vote exists with different direction → Update to new direction
func (s *voteService) CreateVote(ctx context.Context, session *oauth.ClientSessionData, req CreateVoteRequest) (*CreateVoteResponse, error) {
	// Validate direction
	if req.Direction != "up" && req.Direction != "down" {
		return nil, ErrInvalidDirection
	}

	// Validate subject URI format
	if req.Subject.URI == "" {
		return nil, ErrInvalidSubject
	}
	if !strings.HasPrefix(req.Subject.URI, "at://") {
		return nil, ErrInvalidSubject
	}

	// Validate subject CID is provided
	if req.Subject.CID == "" {
		return nil, ErrInvalidSubject
	}

	// Create PDS client for this session
	pdsClient, err := s.getPDSClient(ctx, session)
	if err != nil {
		s.logger.Error("failed to create PDS client",
			"error", err,
			"voter", session.AccountDID)
		return nil, fmt.Errorf("failed to create PDS client: %w", err)
	}

	// Note: We intentionally don't validate subject existence here.
	// The vote record goes to the user's PDS regardless, which avoids race
	// conditions and eventual consistency issues on the write path.
	//
	// The consumer handles the resulting ordering: a vote that reaches the
	// AppView before its subject is refused transiently and replayed by the
	// dead letter redriver once the subject lands (the ordering gate in
	// vote_consumer.go's indexVoteAndUpdateCounts). Contract:
	// TestVoteBeforeSubjectIsCountedOnceSubjectIndexed
	// (tests/e2e/vote_contract_test.go).

	// Lookup, PDS write and overlay update run under one lock per viewer and
	// subject, so two taps can never both create a record.
	key := viewerSubjectKey{viewerDID: session.AccountDID.String(), subjectURI: req.Subject.URI}
	release, err := s.writeLocks.acquire(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("waiting for the vote write lock: %w", err)
	}
	defer release()

	existing, ambiguous, err := s.findExistingVote(ctx, pdsClient, key)
	if err != nil {
		s.logger.Error("failed to check existing vote",
			"error", err,
			"voter", session.AccountDID,
			"subject", req.Subject.URI)
		return nil, fmt.Errorf("failed to check existing vote: %w", err)
	}

	// A retry of an ambiguous CreateVote in the same direction repeats it
	// (D6): if the PDS already holds its result, return that and write nothing.
	if ambiguous.direction == req.Direction {
		if ambiguous.operation == ambiguousOperationCast && existing != nil && existing.Direction == req.Direction {
			s.logger.Info("vote retry found the ambiguous vote cast",
				"voter", session.AccountDID,
				"subject", req.Subject.URI,
				"uri", existing.URI)
			s.overlay.record(key, recentWrite{state: recentWriteVoted, uri: existing.URI, direction: existing.Direction, rkey: existing.RKey}, s.now())
			return &CreateVoteResponse{URI: existing.URI, CID: existing.CID}, nil
		}
		if ambiguous.operation == ambiguousOperationToggleOff && existing == nil {
			s.logger.Info("vote retry found the ambiguous toggle-off applied",
				"voter", session.AccountDID,
				"subject", req.Subject.URI)
			s.overlay.record(key, recentWrite{state: recentWriteRemoved, uri: ambiguous.removedURI}, s.now())
			return &CreateVoteResponse{URI: "", CID: ""}, nil
		}
	}

	// Toggle logic
	if existing != nil {
		// Vote exists - check if same direction
		if existing.Direction == req.Direction {
			// Same direction - toggle off (delete)
			if err := pdsClient.DeleteRecord(ctx, voteCollection, existing.RKey); err != nil {
				s.logger.Error("failed to delete vote on PDS",
					"error", err,
					"voter", session.AccountDID,
					"rkey", existing.RKey)
				s.recordAmbiguousWrite(key, ambiguousRequest{
					operation:  ambiguousOperationToggleOff,
					direction:  req.Direction,
					removedURI: existing.URI,
				}, err)
				if pds.IsAuthError(err) {
					return nil, fmt.Errorf("%w: %w", ErrNotAuthorized, err)
				}
				return nil, fmt.Errorf("failed to delete vote: %w", err)
			}

			s.logger.Info("vote toggled off",
				"voter", session.AccountDID,
				"subject", req.Subject.URI,
				"direction", req.Direction)

			s.overlay.record(key, recentWrite{state: recentWriteRemoved, uri: existing.URI}, s.now())

			// Return empty response to indicate deletion
			return &CreateVoteResponse{
				URI: "",
				CID: "",
			}, nil
		}
	}

	// Write a first vote or atomically replace an opposite-direction vote.
	uri, cid, rkey, err := s.writeVoteRecord(ctx, pdsClient, key, req, existing, ambiguous.createRKey)
	if err != nil {
		s.logger.Error("failed to create vote on PDS",
			"error", err,
			"voter", session.AccountDID,
			"subject", req.Subject.URI,
			"direction", req.Direction)
		if pds.IsAuthError(err) {
			return nil, fmt.Errorf("%w: %w", ErrNotAuthorized, err)
		}
		return nil, fmt.Errorf("failed to create vote: %w", err)
	}

	s.logger.Info("vote created",
		"voter", session.AccountDID,
		"subject", req.Subject.URI,
		"direction", req.Direction,
		"uri", uri,
		"cid", cid)

	s.overlay.record(key, recentWrite{
		state:     recentWriteVoted,
		uri:       uri,
		direction: req.Direction,
		rkey:      rkey,
	}, s.now())

	return &CreateVoteResponse{
		URI: uri,
		CID: cid,
	}, nil
}

// DeleteVote removes a vote on the specified subject
func (s *voteService) DeleteVote(ctx context.Context, session *oauth.ClientSessionData, req DeleteVoteRequest) error {
	// Validate subject URI format
	if req.Subject.URI == "" {
		return ErrInvalidSubject
	}
	if !strings.HasPrefix(req.Subject.URI, "at://") {
		return ErrInvalidSubject
	}

	// Create PDS client for this session
	pdsClient, err := s.getPDSClient(ctx, session)
	if err != nil {
		s.logger.Error("failed to create PDS client",
			"error", err,
			"voter", session.AccountDID)
		return fmt.Errorf("failed to create PDS client: %w", err)
	}

	key := viewerSubjectKey{viewerDID: session.AccountDID.String(), subjectURI: req.Subject.URI}
	release, err := s.writeLocks.acquire(ctx, key)
	if err != nil {
		return fmt.Errorf("waiting for the vote write lock: %w", err)
	}
	defer release()

	existing, ambiguous, err := s.findExistingVote(ctx, pdsClient, key)
	if err != nil {
		s.logger.Error("failed to find existing vote",
			"error", err,
			"voter", session.AccountDID,
			"subject", req.Subject.URI)
		return fmt.Errorf("failed to find vote: %w", err)
	}
	if existing == nil {
		// A retry of an ambiguous DeleteVote repeats it (D6): the vote is gone.
		if ambiguous.operation == ambiguousOperationDelete {
			s.logger.Info("vote retry found the ambiguous delete applied",
				"voter", session.AccountDID,
				"subject", req.Subject.URI)
			s.overlay.record(key, recentWrite{state: recentWriteRemoved, uri: ambiguous.removedURI}, s.now())
			return nil
		}
		return ErrVoteNotFound
	}

	// Delete the vote record from user's PDS
	if err := pdsClient.DeleteRecord(ctx, voteCollection, existing.RKey); err != nil {
		s.logger.Error("failed to delete vote on PDS",
			"error", err,
			"voter", session.AccountDID,
			"rkey", existing.RKey)
		s.recordAmbiguousWrite(key, ambiguousRequest{operation: ambiguousOperationDelete, removedURI: existing.URI}, err)
		if pds.IsAuthError(err) {
			return fmt.Errorf("%w: %w", ErrNotAuthorized, err)
		}
		return fmt.Errorf("failed to delete vote: %w", err)
	}

	s.logger.Info("vote deleted",
		"voter", session.AccountDID,
		"subject", req.Subject.URI,
		"uri", existing.URI)

	s.overlay.record(key, recentWrite{state: recentWriteRemoved, uri: existing.URI}, s.now())

	return nil
}

// recordAmbiguousWrite marks the key Unverified with the failed request,
// unless the PDS definitively refused it (D5). A refused write cannot have
// committed, so the overlay keeps what it knew.
func (s *voteService) recordAmbiguousWrite(key viewerSubjectKey, request ambiguousRequest, err error) {
	if !definitiveVoteRefusal(err) {
		s.recordUnverified(key, request)
	}
}

// recordUnverified marks the key's last write as having an unknown outcome.
// request is the write, or the zero value when no retry should repeat it.
func (s *voteService) recordUnverified(key viewerSubjectKey, request ambiguousRequest) {
	s.overlay.record(key, recentWrite{state: recentWriteUnverified, request: request}, s.now())
}

// writeVoteRecord writes a vote, replacing an existing direction in one commit,
// and returns the new vote's URI, CID and record key. A plain create reuses
// createRKey when an earlier ambiguous create left one.
func (s *voteService) writeVoteRecord(ctx context.Context, pdsClient PDSClient, key viewerSubjectKey, req CreateVoteRequest, existing *existingVote, createRKey string) (string, string, string, error) {
	// Generate TID for the record key
	tid := syntax.NewTIDNow(0)

	// Build vote record following the lexicon schema
	record := VoteRecord{
		Type: voteCollection,
		Subject: StrongRef{
			URI: req.Subject.URI,
			CID: req.Subject.CID,
		},
		Direction: req.Direction,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}

	if existing != nil {
		uri, cid, err := s.replaceVoteRecord(ctx, pdsClient, key, existing.RKey, tid.String(), record)
		return uri, cid, tid.String(), err
	}

	if createRKey == "" {
		createRKey = tid.String()
	}
	request := ambiguousRequest{operation: ambiguousOperationCast, direction: req.Direction, createRKey: createRKey}
	uri, cid, err := pdsClient.CreateRecord(ctx, voteCollection, createRKey, record)
	if err != nil {
		s.recordAmbiguousWrite(key, request, err)
		return "", "", "", fmt.Errorf("createRecord failed: %w", err)
	}
	// A success naming another record is an unknown write outcome.
	if uri != "at://"+pdsClient.DID()+"/"+voteCollection+"/"+createRKey {
		s.recordUnverified(key, request)
		return "", "", "", fmt.Errorf("PDS returned a vote create result naming another record")
	}

	return uri, cid, createRKey, nil
}

func (s *voteService) replaceVoteRecord(ctx context.Context, client PDSClient, key viewerSubjectKey, oldRKey, newRKey string, record VoteRecord) (string, string, error) {
	// Votes remain immutable: the consumer indexes create/delete events, not
	// updates. A single batch prevents a refused create from deleting the vote.
	result, err := client.ApplyWrites(ctx, []pds.Write{
		{Op: pds.WriteOpDelete, Collection: voteCollection, RKey: oldRKey},
		{Op: pds.WriteOpCreate, Collection: voteCollection, RKey: newRKey, Record: record},
	}, "")
	expectedURI := "at://" + client.DID() + "/" + voteCollection + "/" + newRKey
	if err == nil {
		if result != nil && len(result.Results) == 2 &&
			result.Results[1].URI == expectedURI && result.Results[1].CID != "" {
			return result.Results[1].URI, result.Results[1].CID, nil
		}
		// A malformed success is an unknown write outcome, not a toggle-off.
		err = fmt.Errorf("PDS returned an invalid vote replacement result")
	}

	if definitiveVoteRefusal(err) {
		// State conflicts can expose a stale key, so the next write reads the
		// PDS. Other refusals leave the overlay usable and must not add reads
		// to an unauthorized/limited PDS.
		if errors.Is(err, pds.ErrNotFound) || errors.Is(err, pds.ErrConflict) || errors.Is(err, pds.ErrSwapConflict) {
			s.recordUnverified(key, ambiguousRequest{})
		}
		return "", "", fmt.Errorf("atomic vote replacement refused: %w", err)
	}

	// A timed-out batch may still be committing when the first read runs.
	// Poll only its exact key; replaying a toggle could reverse the user's vote.
	// Cancellation is detached so a client deadline does not cancel recovery.
	reconcileContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), voteReconciliationTimeout)
	defer cancel()
	ticker := time.NewTicker(voteReconciliationPollInterval)
	defer ticker.Stop()
	var readErr error
	reason := "read_failed"
	for {
		if reconcileContext.Err() != nil {
			readErr = reconcileContext.Err()
			reason = "deadline_exceeded"
			break
		}
		var stored *pds.RecordResponse
		stored, readErr = client.GetRecord(reconcileContext, voteCollection, newRKey)
		if readErr == nil {
			reason = record.reconciliationMismatch(stored, expectedURI)
			if reason == "" {
				s.logger.Warn("recovered vote replacement after lost response",
					"old_rkey", oldRKey, "new_rkey", newRKey, "batch_error", err)
				return stored.URI, stored.CID, nil
			}
			break
		}
		// A missing GET result is still ambiguous while the batch is running.
		// A refused GET must stop polling, especially for auth or rate limits.
		if !errors.Is(readErr, pds.ErrNotFound) && definitiveVoteRefusal(readErr) {
			break
		}
		select {
		case <-reconcileContext.Done():
		case <-ticker.C:
		}
	}

	// The bounded read cannot rule out a still-running commit. The next
	// write must read the PDS instead of trusting the overlay or the index,
	// and a retry of this switch returns the switched vote if it committed.
	s.recordUnverified(key, ambiguousRequest{operation: ambiguousOperationCast, direction: record.Direction})
	s.logger.Warn("failed to reconcile vote replacement",
		"old_rkey", oldRKey, "new_rkey", newRKey, "batch_error", err,
		"reason", reason, "error", readErr)
	return "", "", fmt.Errorf("atomic vote replacement failed: %w", err)
}

// definitiveVoteRefusal excludes responses that could conceal a committed write.
func definitiveVoteRefusal(err error) bool {
	if pds.IsAuthError(err) || errors.Is(err, pds.ErrBadRequest) ||
		errors.Is(err, pds.ErrRateLimited) || errors.Is(err, pds.ErrPayloadTooLarge) ||
		errors.Is(err, pds.ErrNotFound) || errors.Is(err, pds.ErrConflict) || errors.Is(err, pds.ErrSwapConflict) ||
		errors.Is(err, pds.ErrSessionBusy) {
		return true
	}
	// Unclassified API errors retain their original status in the PDS wrapper.
	var apiError *atclient.APIError
	return errors.As(err, &apiError) && apiError.StatusCode >= 400 &&
		apiError.StatusCode < 500 && apiError.StatusCode != 408
}

// reconciliationMismatch reports a field name, never untrusted record contents.
func (record VoteRecord) reconciliationMismatch(stored *pds.RecordResponse, expectedURI string) string {
	if stored == nil {
		return "record"
	}
	if stored.URI != expectedURI {
		return "uri"
	}
	if stored.CID == "" {
		return "cid"
	}
	if stored.Value["$type"] != record.Type {
		return "type"
	}
	if stored.Value["direction"] != record.Direction {
		return "direction"
	}
	subject, ok := stored.Value["subject"].(map[string]any)
	if !ok {
		return "subject"
	}
	if subject["uri"] != record.Subject.URI {
		return "subject_uri"
	}
	if subject["cid"] != record.Subject.CID {
		return "subject_cid"
	}
	if stored.Value["createdAt"] != record.CreatedAt {
		return "created_at"
	}
	return ""
}

// existingVote represents a vote record found on the PDS
type existingVote struct {
	URI       string
	CID       string
	RKey      string
	Direction string
}

// findExistingVote finds the viewer's vote on the key's subject for a write
// (PRD §5.3). It resolves through the index and overlay, and scans the PDS
// only when the key's last write had an unknown outcome (§5.4). Any error
// fails the write; it never falls back to "no vote". After an ambiguous write
// it also returns that write's request, for a retry to repeat (D6) and a
// create to reuse its record key; otherwise the zero request.
func (s *voteService) findExistingVote(ctx context.Context, pdsClient pds.Client, key viewerSubjectKey) (*existingVote, ambiguousRequest, error) {
	resolved, err := s.resolveViewerVotes(ctx, key.viewerDID, []string{key.subjectURI})
	if err != nil {
		return nil, ambiguousRequest{}, err
	}
	resolution := resolved[key.subjectURI]
	if !resolution.unverified {
		return resolution.vote, ambiguousRequest{}, nil
	}

	s.logger.Info("vote lookup scanning the PDS after an ambiguous write",
		"voter", key.viewerDID)
	existing, err := s.findExistingVoteFromPDS(ctx, pdsClient, key.subjectURI)
	return existing, resolution.request, err
}

// findExistingVoteFromPDS queries the user's PDS directly to find an existing vote for a subject.
// It pages until the first match, and runs only after an ambiguous write.
// Returns the vote record with rkey, or nil if no vote exists for the subject.
func (s *voteService) findExistingVoteFromPDS(ctx context.Context, pdsClient pds.Client, subjectURI string) (*existingVote, error) {
	cursor := ""
	const pageSize = 100

	// Paginate through all vote records
	for {
		result, err := pdsClient.ListRecords(ctx, voteCollection, pageSize, cursor)
		if err != nil {
			// Check for auth errors using typed errors
			if pds.IsAuthError(err) {
				return nil, fmt.Errorf("%w: %w", ErrNotAuthorized, err)
			}
			return nil, fmt.Errorf("listRecords failed: %w", err)
		}

		// Search for the vote matching our subject in this page
		for _, rec := range result.Records {
			// Extract subject from record value
			subject, ok := rec.Value["subject"].(map[string]any)
			if !ok {
				continue
			}

			subjectURIValue, ok := subject["uri"].(string)
			if !ok {
				continue
			}

			if subjectURIValue == subjectURI {
				// Extract rkey from the URI (at://did/collection/rkey)
				parts := strings.Split(rec.URI, "/")
				if len(parts) < 5 {
					continue
				}
				rkey := parts[len(parts)-1]

				// Extract direction
				direction, _ := rec.Value["direction"].(string)

				return &existingVote{
					URI:       rec.URI,
					CID:       rec.CID,
					RKey:      rkey,
					Direction: direction,
				}, nil
			}
		}

		// Check if there are more pages
		if result.Cursor == "" {
			break // No more pages
		}
		cursor = result.Cursor
	}

	// No vote found for this subject after checking all pages
	return nil, nil
}
