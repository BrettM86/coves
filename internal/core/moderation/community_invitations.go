package moderation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/communities"
	"Coves/internal/core/posts"
	"Coves/internal/core/users"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

// Fingerprint operations of the community invitation requests.
const (
	inviteModeratorOperation   = "invite-moderator"
	offerOwnershipOperation    = "offer-ownership"
	acceptInvitationOperation  = "accept-invitation"
	declineInvitationOperation = "decline-invitation"
)

// roleRecordCreatedAtLayout is the createdAt of a role record an accept
// writes: RFC 3339 UTC at millisecond precision.
const roleRecordCreatedAtLayout = "2006-01-02T15:04:05.000Z"

// CommunityInvitationConfig wires the community invitation service.
type CommunityInvitationConfig struct {
	Authorizer     communities.RoleAuthorizer
	Store          CommunityActionStore
	Invitations    communities.InvitationReader
	Resolver       InviteeResolver // handle -> DID through the configured identity resolution
	Now            func() time.Time
	IdempotencyTTL time.Duration
}

// InviteeResolver resolves an invitee handle to a DID. users.UserService
// satisfies it.
type InviteeResolver interface {
	ResolveHandleToDID(ctx context.Context, handle string) (string, error)
}

// CommunityInvitationService creates, declines and lists community role
// invitations. Accepting one is CommunityActionService.AcceptInvitation.
// Invitations are AppView state: none of these requests writes to the
// community repo, logs a moderation action or changes the roster version.
type CommunityInvitationService struct {
	cfg CommunityInvitationConfig
}

// NewCommunityInvitationService builds the service. Every dependency is
// required and IdempotencyTTL must be positive.
func NewCommunityInvitationService(cfg CommunityInvitationConfig) *CommunityInvitationService {
	if cfg.Authorizer == nil || cfg.Store == nil || cfg.Invitations == nil || cfg.Resolver == nil || cfg.Now == nil {
		panic("moderation.NewCommunityInvitationService: Authorizer, Store, Invitations, Resolver and Now are required")
	}
	if cfg.IdempotencyTTL <= 0 {
		panic("moderation.NewCommunityInvitationService: IdempotencyTTL must be positive")
	}
	return &CommunityInvitationService{cfg: cfg}
}

// InviteRequest asks to invite an account to a community role. Invitee is a
// handle or a DID.
type InviteRequest struct {
	ActorDID, CommunityDID string
	Invitee                string
	ExpectedVersion        int64
	IdempotencyKey         string
}

// DeclineInvitationRequest asks to decline the caller's invitation.
type DeclineInvitationRequest struct {
	ActorDID, InvitationID string
	ExpectedVersion        int64
	IdempotencyKey         string
}

// AcceptInvitationRequest asks to accept the caller's invitation.
type AcceptInvitationRequest struct {
	ActorDID, InvitationID string
	ExpectedVersion        int64
	IdempotencyKey         string
}

// InviteModerator invites an account to moderate the community. The caller
// must be the owner or an instance admin; the invitee must hold no role.
func (s *CommunityInvitationService) InviteModerator(ctx context.Context, req InviteRequest) (*CommunityActionResult, error) {
	return s.invite(ctx, req, communities.RoleModerator, inviteModeratorOperation)
}

// OfferOwnership offers the community's ownership to an account. The owner
// may offer it to a current moderator; an instance admin to any account but
// the owner.
func (s *CommunityInvitationService) OfferOwnership(ctx context.Context, req InviteRequest) (*CommunityActionResult, error) {
	return s.invite(ctx, req, communities.RoleOwner, offerOwnershipOperation)
}

func (s *CommunityInvitationService) invite(ctx context.Context, req InviteRequest, role, operation string) (*CommunityActionResult, error) {
	if req.ActorDID == "" {
		return nil, ErrAuthRequired
	}
	if _, err := syntax.ParseDID(req.CommunityDID); err != nil {
		return nil, fmt.Errorf("%w: community must be a DID", ErrInvalidRequest)
	}
	if strings.TrimSpace(req.Invitee) == "" {
		return nil, fmt.Errorf("%w: subject must be a handle or a DID", ErrInvalidRequest)
	}
	if err := validateKeyAndVersion(req.IdempotencyKey, req.ExpectedVersion); err != nil {
		return nil, err
	}
	if _, err := authorizeRole(ctx, s.cfg.Authorizer, req.CommunityDID, req.ActorDID, communities.RoleOwner); err != nil {
		return nil, err
	}
	inviteeDID, err := s.resolveInvitee(ctx, req.Invitee)
	if err != nil {
		return nil, err
	}
	fingerprint, err := requestFingerprint(operation, req.CommunityDID, inviteeDID, req.ExpectedVersion)
	if err != nil {
		return nil, err
	}
	scope := idempotencyScope{actorDID: req.ActorDID, communityDID: req.CommunityDID, key: req.IdempotencyKey, fingerprint: fingerprint}

	tx, err := s.cfg.Store.BeginAction(ctx, newCommunityActionLock(req.CommunityDID,
		[]string{req.ActorDID, inviteeDID}, nil, []string{inviteeDID}))
	if err != nil {
		return nil, fmt.Errorf("%w: opening the community action transaction: %w", ErrModerationUnavailable, err)
	}
	defer func() { _ = tx.Rollback() }()

	auth, err := authorizeRole(ctx, s.cfg.Authorizer, req.CommunityDID, req.ActorDID, communities.RoleOwner)
	if err != nil {
		return nil, err
	}
	now := s.cfg.Now()
	if replay, err := storedResult(ctx, tx, scope, now); replay != nil || err != nil {
		return replay, err
	}
	if err := refuseOpenIntentUnderKey(ctx, tx, scope); err != nil {
		return nil, err
	}
	if err := refuseAcceptInFlight(ctx, tx, req.CommunityDID, inviteeDID); err != nil {
		return nil, err
	}
	roster, err := readRosterInTx(ctx, tx, req.CommunityDID)
	if err != nil {
		return nil, err
	}
	if err := inviteRules(role, auth, roster, inviteeDID); err != nil {
		return nil, err
	}
	if err := refuseBannedInvitee(ctx, tx, req.CommunityDID, inviteeDID, now); err != nil {
		return nil, err
	}
	version, err := checkRosterVersion(ctx, tx, req.CommunityDID, req.ExpectedVersion)
	if err != nil {
		return nil, err
	}

	id, err := randomHexID("community invitation ID")
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrModerationUnavailable, err)
	}
	created := now.UTC().Truncate(time.Microsecond)
	if err := tx.CreateInvitation(ctx, communities.Invitation{
		ID: id, CommunityDID: req.CommunityDID, InviteeDID: inviteeDID, Role: role,
		InviterDID: req.ActorDID, InviterAdmin: auth.InstanceAdmin, State: communities.InvitationOpen,
		CreatedAt: created, ExpiresAt: created.Add(communities.InvitationLifetime),
	}); err != nil {
		if errors.Is(err, communities.ErrInvitationPartyErased) {
			return nil, fmt.Errorf("%w: %w", ErrAccountNotFound, err)
		}
		return nil, fmt.Errorf("%w: creating the invitation: %w", ErrModerationUnavailable, err)
	}
	result := CommunityActionResult{CommunityDID: req.CommunityDID, SubjectDID: inviteeDID, Version: version, InvitationID: id}
	return s.commitResult(ctx, tx, scope, result)
}

// resolveInvitee is the invitee's DID: a "did:" value is parsed, anything else
// is a handle resolved through the configured identity resolution.
func (s *CommunityInvitationService) resolveInvitee(ctx context.Context, invitee string) (string, error) {
	invitee = strings.TrimSpace(invitee)
	if strings.HasPrefix(invitee, "did:") {
		if _, err := syntax.ParseDID(invitee); err != nil {
			return "", fmt.Errorf("%w: subject is not a valid DID", ErrInvalidRequest)
		}
		return invitee, nil
	}
	did, err := s.cfg.Resolver.ResolveHandleToDID(ctx, invitee)
	if err == nil {
		return did, nil
	}
	var invalid *users.InvalidHandleError
	switch {
	case errors.Is(err, users.ErrUserNotFound):
		return "", fmt.Errorf("%w: %w", ErrAccountNotFound, err)
	case errors.As(err, &invalid):
		return "", fmt.Errorf("%w: subject is not a valid handle", ErrInvalidRequest)
	}
	return "", fmt.Errorf("%w: resolving the invitee handle: %w", ErrModerationUnavailable, err)
}

// inviteRules refuses an invitation the roster rules forbid. A moderator
// invitation needs an invitee without a role. An ownership offer never goes
// to the owner, and the owner (not acting as an instance admin) offers it
// only to a current moderator.
func inviteRules(role string, auth communities.RoleAuthorization, roster communities.Roster, inviteeDID string) error {
	held := rosterRoleOf(roster, inviteeDID)
	if role == communities.RoleModerator {
		if held != "" {
			return fmt.Errorf("%w: the invitee already holds the %s role", ErrInviteeHasRole, held)
		}
		return nil
	}
	if held == communities.RoleOwner {
		return fmt.Errorf("%w: the invitee is already the owner", ErrInviteeHasRole)
	}
	if !auth.InstanceAdmin && held != communities.RoleModerator {
		return fmt.Errorf("%w: the owner offers ownership only to a current moderator", ErrInviteeNotModerator)
	}
	return nil
}

// DeclineInvitation declines the caller's open invitation. It writes nothing
// to the community repo and logs no action.
func (s *CommunityInvitationService) DeclineInvitation(ctx context.Context, req DeclineInvitationRequest) (*CommunityActionResult, error) {
	if req.ActorDID == "" {
		return nil, ErrAuthRequired
	}
	if !validOpaqueField(req.InvitationID) {
		return nil, fmt.Errorf("%w: invitation must be 1-128 UTF-8 bytes", ErrInvalidRequest)
	}
	if err := validateKeyAndVersion(req.IdempotencyKey, req.ExpectedVersion); err != nil {
		return nil, err
	}
	inv, err := inviteeInvitation(ctx, s.cfg.Store.InvitationByID, req.InvitationID, req.ActorDID)
	if err != nil {
		return nil, err
	}
	fingerprint, err := requestFingerprint(declineInvitationOperation, inv.CommunityDID, inv.ID, req.ExpectedVersion)
	if err != nil {
		return nil, err
	}
	scope := idempotencyScope{actorDID: req.ActorDID, communityDID: inv.CommunityDID, key: req.IdempotencyKey, fingerprint: fingerprint}

	tx, err := s.cfg.Store.BeginAction(ctx, newCommunityActionLock(inv.CommunityDID, []string{req.ActorDID}, nil, nil))
	if err != nil {
		return nil, fmt.Errorf("%w: opening the community action transaction: %w", ErrModerationUnavailable, err)
	}
	defer func() { _ = tx.Rollback() }()

	now := s.cfg.Now()
	if replay, err := storedResult(ctx, tx, scope, now); replay != nil || err != nil {
		return replay, err
	}
	if err := refuseOpenIntentUnderKey(ctx, tx, scope); err != nil {
		return nil, err
	}
	if err := refuseAcceptInFlight(ctx, tx, inv.CommunityDID, req.ActorDID); err != nil {
		return nil, err
	}
	current, err := inviteeInvitation(ctx, tx.InvitationByID, req.InvitationID, req.ActorDID)
	if err != nil {
		return nil, err
	}
	if err := requireOpenInvitation(*current, now); err != nil {
		return nil, err
	}
	version, err := checkRosterVersion(ctx, tx, inv.CommunityDID, req.ExpectedVersion)
	if err != nil {
		return nil, err
	}
	if err := tx.SetInvitationState(ctx, inv.ID, communities.InvitationDeclined); err != nil {
		return nil, fmt.Errorf("%w: declining the invitation: %w", ErrModerationUnavailable, err)
	}
	result := CommunityActionResult{CommunityDID: inv.CommunityDID, SubjectDID: req.ActorDID, Version: version, InvitationID: inv.ID}
	return s.commitResult(ctx, tx, scope, result)
}

// refuseOpenIntentUnderKey refuses with IdempotencyConflict a request whose
// key names another request's open write intent: that write's completion
// stores its own result under the key.
func refuseOpenIntentUnderKey(ctx context.Context, tx CommunityActionTx, scope idempotencyScope) error {
	open, err := tx.IntentByKey(ctx, scope.actorDID, scope.communityDID, scope.key)
	if err != nil {
		return fmt.Errorf("%w: reading the key's open intent: %w", ErrModerationUnavailable, err)
	}
	if open != nil && open.State == CommunityIntentOpen && open.Fingerprint != scope.fingerprint {
		return fmt.Errorf("%w: idempotency key %q was used for a different request", ErrIdempotencyConflict, scope.key)
	}
	return nil
}

// refuseAcceptInFlight refuses with StateConflict while an accept of one of
// the invitee's invitations in the community has an open intent: its write
// may have committed, and the intent's completion accepts the invitation.
// The roster lock the caller holds keeps a new accept intent from opening;
// the retry, the firehose copy or the sweep settles the open one.
func refuseAcceptInFlight(ctx context.Context, tx CommunityActionTx, communityDID, inviteeDID string) error {
	open, err := tx.OpenInvitationIntent(ctx, communityDID, inviteeDID)
	if err != nil {
		return fmt.Errorf("%w: reading the invitee's open accept intent: %w", ErrModerationUnavailable, err)
	}
	if open != nil {
		return fmt.Errorf("%w: an acceptance of invitation %s has an unknown outcome", ErrStateConflict, open.InvitationID)
	}
	return nil
}

// commitResult stores result under scope and commits the transaction.
func (s *CommunityInvitationService) commitResult(ctx context.Context, tx CommunityActionTx, scope idempotencyScope, result CommunityActionResult) (*CommunityActionResult, error) {
	if err := tx.SaveIdempotencyRecord(ctx, scope.actorDID, scope.communityDID, scope.key,
		CommunityIdempotencyRecord{Fingerprint: scope.fingerprint, Result: result}, s.cfg.IdempotencyTTL); err != nil {
		return nil, fmt.Errorf("%w: storing the idempotency record: %w", ErrModerationUnavailable, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("%w: committing the invitation: %w", ErrModerationUnavailable, err)
	}
	return &result, nil
}

// ListCommunityInvitations lists the community's pending invitations to its
// owner or an instance admin.
func (s *CommunityInvitationService) ListCommunityInvitations(ctx context.Context, actorDID, communityDID string) ([]communities.Invitation, error) {
	if actorDID == "" {
		return nil, ErrAuthRequired
	}
	if _, err := syntax.ParseDID(communityDID); err != nil {
		return nil, fmt.Errorf("%w: community must be a DID", ErrInvalidRequest)
	}
	if _, err := authorizeRole(ctx, s.cfg.Authorizer, communityDID, actorDID, communities.RoleOwner); err != nil {
		return nil, err
	}
	invitations, err := s.cfg.Invitations.ListPendingInvitationsForCommunity(ctx, communityDID, s.cfg.Now())
	if err != nil {
		return nil, fmt.Errorf("%w: listing the community's invitations: %w", ErrModerationUnavailable, err)
	}
	return invitations, nil
}

// ListViewerInvitations lists the viewer's own pending invitations.
func (s *CommunityInvitationService) ListViewerInvitations(ctx context.Context, viewerDID string) ([]communities.Invitation, error) {
	if viewerDID == "" {
		return nil, ErrAuthRequired
	}
	invitations, err := s.cfg.Invitations.ListPendingInvitationsForInvitee(ctx, viewerDID, s.cfg.Now())
	if err != nil {
		return nil, fmt.Errorf("%w: listing the viewer's invitations: %w", ErrModerationUnavailable, err)
	}
	return invitations, nil
}

// acceptance is one accept request with the invitation it names.
type acceptance struct {
	invitation      communities.Invitation
	expectedVersion int64
	scope           idempotencyScope
}

// acceptedChange is what accepting the invitation writes, planned from the
// effective roster: the invitee's record (created, or updated when the invitee
// holds one) and, for a transfer with an owner, the owner's record rewritten
// as moderator.
type acceptedChange struct {
	inviteeRow *communities.CommunityRole // the invitee's roster row; nil when it holds none
	targetURI  string
	op         pds.WriteOp
	owner      *communities.CommunityRole // the previous owner of a transfer; nil otherwise
	ownerURI   string
	// erasedCID is the CID of an unindexed record at the invitee's key whose
	// queued delete (left by an erasure) is guarded on it; the accept
	// replaces that record. "" otherwise.
	erasedCID string
}

// AcceptInvitation accepts the caller's invitation through the community
// write pipeline: a moderator invitation creates the invitee's moderator
// record; an ownership offer writes the invitee's owner record and rewrites
// the previous owner's as moderator in the same commit.
func (s *CommunityActionService) AcceptInvitation(ctx context.Context, req AcceptInvitationRequest) (*CommunityActionResult, error) {
	if req.ActorDID == "" {
		return nil, ErrAuthRequired
	}
	if !validOpaqueField(req.InvitationID) {
		return nil, fmt.Errorf("%w: invitation must be 1-128 UTF-8 bytes", ErrInvalidRequest)
	}
	if err := validateKeyAndVersion(req.IdempotencyKey, req.ExpectedVersion); err != nil {
		return nil, err
	}
	inv, err := inviteeInvitation(ctx, s.cfg.Store.InvitationByID, req.InvitationID, req.ActorDID)
	if err != nil {
		return nil, err
	}
	fingerprint, err := requestFingerprint(acceptInvitationOperation, inv.CommunityDID, inv.ID, req.ExpectedVersion)
	if err != nil {
		return nil, err
	}
	accept := acceptance{
		invitation:      *inv,
		expectedVersion: req.ExpectedVersion,
		scope:           idempotencyScope{actorDID: req.ActorDID, communityDID: inv.CommunityDID, key: req.IdempotencyKey, fingerprint: fingerprint},
	}
	if err := s.requireHosted(ctx, inv.CommunityDID); err != nil {
		if errors.Is(err, ErrForbidden) {
			return s.replayRefused(ctx, accept.scope, err)
		}
		return nil, err
	}
	if s.cfg.Hooks.AfterAuthorize != nil {
		s.cfg.Hooks.AfterAuthorize(ctx)
	}
	// The first locks are a guess at the change: the invitee's record and, for
	// an owner's offer, the inviter's. An attempt that finds the roster needs
	// more restarts under the wider locks.
	inviteeURI := roleRecordURI(inv.CommunityDID, communities.RoleRecordKey(inv.InviteeDID))
	accounts := []string{inv.InviteeDID}
	targets := []string{inviteeURI}
	if inv.Role == communities.RoleOwner && !inv.InviterAdmin {
		accounts = append(accounts, inv.InviterDID)
		targets = append(targets, roleRecordURI(inv.CommunityDID, communities.RoleRecordKey(inv.InviterDID)))
	}
	lock := newCommunityActionLock(inv.CommunityDID, accounts, targets, []string{inv.InviteeDID})
	// The inviter's erasure deletes the invitation this accept locks and
	// marks accepted, so the accept holds the inviter's shared erasure gate
	// and that erasure waits at its gate rather than on the invitation row.
	lock.ErasureGateDIDs = []string{inv.InviterDID}
	return s.runAttempts(lock, inviteeURI, func(lock CommunityActionLock) (*CommunityActionResult, *restart, error) {
		return s.acceptAttempt(ctx, accept, lock)
	})
}

// requireHosted refuses (Forbidden) a community this instance does not host
// or has removed.
func (s *CommunityActionService) requireHosted(ctx context.Context, communityDID string) error {
	if err := s.cfg.Authorizer.RequireHostedCommunity(ctx, communityDID); err != nil {
		return authorizationError(err)
	}
	return nil
}

func (s *CommunityActionService) acceptAttempt(ctx context.Context, accept acceptance, lock CommunityActionLock) (*CommunityActionResult, *restart, error) {
	communityDID := accept.invitation.CommunityDID
	tx, err := s.cfg.Store.BeginAction(ctx, lock)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: opening the community action transaction: %w", ErrModerationUnavailable, err)
	}
	defer func() { _ = tx.Rollback() }()

	now := s.cfg.Now()
	if err := s.requireHosted(ctx, communityDID); err != nil {
		if errors.Is(err, ErrForbidden) {
			result, err := replayOrRefuse(ctx, tx, accept.scope, now, err)
			return result, nil, err
		}
		return nil, nil, err
	}
	if replay, err := storedResult(ctx, tx, accept.scope, now); replay != nil || err != nil {
		return replay, nil, err
	}
	if result, again, done, err := s.reconcileOwnIntent(ctx, tx, accept.scope, lock); done {
		return result, again, err
	}
	inv, err := inviteeInvitation(ctx, tx.InvitationByID, accept.invitation.ID, accept.scope.actorDID)
	if err != nil {
		return nil, nil, err
	}
	if err := requireOpenInvitation(*inv, now); err != nil {
		return nil, nil, err
	}
	roster, err := readRosterInTx(ctx, tx, communityDID)
	if err != nil {
		return nil, nil, err
	}
	// Another key's open ownership transfer is settled before planning, even
	// when its targets are disjoint from this one's: it may have committed an
	// owner record the index does not hold yet.
	if inv.Role == communities.RoleOwner {
		other, err := tx.OpenIntentByAction(ctx, communityDID, ActionTransferOwnership)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: reading the community's open ownership transfer: %w", ErrModerationUnavailable, err)
		}
		if other != nil {
			again, err := s.settleOtherIntent(ctx, tx, lock, *other)
			return nil, again, err
		}
	}
	change := planAcceptance(*inv, roster)
	if needed := change.lock(*inv); !lockCovers(lock, needed) {
		return nil, &restart{lock: mergeLocks(lock, needed)}, nil
	}
	// Another key's open intent on a target is settled before the targets are
	// read, as for every community action.
	for _, uri := range change.targets() {
		other, err := tx.OpenIntentByTarget(ctx, uri)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: reading the target's open intent: %w", ErrModerationUnavailable, err)
		}
		if other != nil {
			again, err := s.settleOtherIntent(ctx, tx, lock, *other)
			return nil, again, err
		}
	}
	if err := acceptRules(*inv, roster); err != nil {
		return nil, nil, err
	}
	if err := refuseBannedInvitee(ctx, tx, communityDID, inv.InviteeDID, now); err != nil {
		return nil, nil, err
	}
	if err := refuseUnresolvedBan(ctx, tx, communityDID, inv.InviteeDID); err != nil {
		return nil, nil, err
	}
	if _, err := checkRosterVersion(ctx, tx, communityDID, accept.expectedVersion); err != nil {
		return nil, nil, err
	}

	repo, err := s.cfg.Repos(ctx, communityDID)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: opening the community repo: %w", ErrModerationUnavailable, err)
	}
	head, err := repo.GetLatestCommit(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: reading the community repo head: %w", ErrModerationUnavailable, err)
	}
	if err := planInviteeRecord(ctx, tx, repo, communityDID, &change); err != nil {
		return nil, nil, err
	}
	if change.owner != nil {
		if err := requireRepoMatchesIndex(ctx, repo, change.ownerURI, change.owner); err != nil {
			return nil, nil, err
		}
	}

	intent, err := s.acceptIntent(accept, *inv, change, head, now)
	if err != nil {
		return nil, nil, err
	}
	if err := s.cfg.Store.SaveIntent(ctx, intent); err != nil {
		if errors.Is(err, ErrStateConflict) {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("%w: storing the community write intent: %w", ErrModerationUnavailable, err)
	}
	result, err := s.write(ctx, tx, repo, intent, false)
	return result, nil, err
}

// recheckAcceptance rechecks an uncommitted accept intent's preconditions in
// tx before a same-key retry writes it again: the invitation still open and
// not lapsed, the invitee not banned and the roster rules.
func (s *CommunityActionService) recheckAcceptance(ctx context.Context, tx CommunityActionTx, intent CommunityIntent) error {
	now := s.cfg.Now()
	inv, err := inviteeInvitation(ctx, tx.InvitationByID, intent.InvitationID, intent.ActorDID)
	if err != nil {
		return err
	}
	if err := requireOpenInvitation(*inv, now); err != nil {
		return err
	}
	if err := refuseBannedInvitee(ctx, tx, intent.CommunityDID, inv.InviteeDID, now); err != nil {
		return err
	}
	roster, err := readRosterInTx(ctx, tx, intent.CommunityDID)
	if err != nil {
		return err
	}
	return acceptRules(*inv, roster)
}

// planAcceptance plans the writes accepting inv makes on roster.
func planAcceptance(inv communities.Invitation, roster communities.Roster) acceptedChange {
	var change acceptedChange
	change.inviteeRow = rosterRow(roster, inv.InviteeDID)
	rkey := communities.RoleRecordKey(inv.InviteeDID)
	change.op = pds.WriteOpCreate
	if change.inviteeRow != nil {
		rkey = change.inviteeRow.RecordKey
		change.op = pds.WriteOpUpdate
	}
	change.targetURI = roleRecordURI(inv.CommunityDID, rkey)
	if inv.Role == communities.RoleOwner && roster.Owner != nil && roster.Owner.SubjectDID != inv.InviteeDID {
		owner := *roster.Owner
		change.owner = &owner
		change.ownerURI = roleRecordURI(inv.CommunityDID, owner.RecordKey)
	}
	return change
}

// targets is every record URI the change writes.
func (c acceptedChange) targets() []string {
	if c.owner == nil {
		return []string{c.targetURI}
	}
	return []string{c.targetURI, c.ownerURI}
}

// lock is the locks the change needs: the roster, the invitee's and the
// previous owner's accounts, every target, and the invitee's ban subject.
func (c acceptedChange) lock(inv communities.Invitation) CommunityActionLock {
	accounts := []string{inv.InviteeDID}
	if c.owner != nil {
		accounts = append(accounts, c.owner.SubjectDID)
	}
	return newCommunityActionLock(inv.CommunityDID, accounts, c.targets(), []string{inv.InviteeDID})
}

// acceptRules rechecks the invitation against the roster under the roster
// lock. A moderator invitation needs an invitee without a role; an ownership
// offer never goes to the owner; an owner's offer needs the invitee still a
// moderator and the inviter still the owner.
func acceptRules(inv communities.Invitation, roster communities.Roster) error {
	held := rosterRoleOf(roster, inv.InviteeDID)
	switch inv.Role {
	case communities.RoleModerator:
		if held != "" {
			return fmt.Errorf("%w: the invitee now holds the %s role", ErrInviteeHasRole, held)
		}
		return nil
	case communities.RoleOwner:
		if held == communities.RoleOwner {
			return fmt.Errorf("%w: the invitee is already the owner", ErrInviteeHasRole)
		}
		if inv.InviterAdmin {
			return nil
		}
		if held != communities.RoleModerator {
			return fmt.Errorf("%w: the invitee is no longer a moderator", ErrInviteeNotModerator)
		}
		if roster.Owner == nil || roster.Owner.SubjectDID != inv.InviterDID {
			return fmt.Errorf("%w: the inviter is no longer the owner", ErrInvitationStale)
		}
		return nil
	}
	return fmt.Errorf("%w: invitation %s has an unknown role %q", ErrModerationUnavailable, inv.ID, inv.Role)
}

// requireRepoMatchesIndex refuses with StateConflict a target record the
// repo holds differently from the index: present when not indexed, absent
// when indexed, or at another CID.
func requireRepoMatchesIndex(ctx context.Context, repo posts.CommunityRepo, uri string, indexed *communities.CommunityRole) error {
	rkey, err := roleRecordKeyOf(uri)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrModerationUnavailable, err)
	}
	record, err := repo.GetRecord(ctx, communities.RoleCollection, rkey)
	if err != nil && !errors.Is(err, pds.ErrNotFound) {
		return fmt.Errorf("%w: reading the role record: %w", ErrModerationUnavailable, err)
	}
	switch {
	case indexed == nil && err == nil:
		return fmt.Errorf("%w: the role record %s is in the community repo but not indexed", ErrStateConflict, uri)
	case indexed != nil && err != nil:
		return fmt.Errorf("%w: the role record %s is indexed but gone from the community repo", ErrStateConflict, uri)
	case indexed != nil && record.CID != indexed.RecordCID:
		return fmt.Errorf("%w: the indexed role record %s is behind the community repo", ErrStateConflict, uri)
	}
	return nil
}

// planInviteeRecord checks the record at the invitee's key against the
// index, as requireRepoMatchesIndex does, with one exception: an unindexed
// record left by the invitee's erasure, whose queued delete carries exactly
// its CID, is replaced by the accept (an update at the key) rather than
// refused. The queued delete then conflicts on the new CID and ends.
func planInviteeRecord(ctx context.Context, tx CommunityActionTx, repo posts.CommunityRepo, communityDID string, change *acceptedChange) error {
	if change.inviteeRow != nil {
		return requireRepoMatchesIndex(ctx, repo, change.targetURI, change.inviteeRow)
	}
	rkey, err := roleRecordKeyOf(change.targetURI)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrModerationUnavailable, err)
	}
	record, err := readRoleRecord(ctx, repo, rkey)
	if err != nil || record == nil {
		return err
	}
	pending, err := tx.PendingRoleRecordDeleteCID(ctx, communityDID, rkey)
	if err != nil {
		return fmt.Errorf("%w: reading the queued role record delete: %w", ErrModerationUnavailable, err)
	}
	if pending != "" && pending == record.CID {
		change.op = pds.WriteOpUpdate
		change.erasedCID = record.CID
		return nil
	}
	return fmt.Errorf("%w: the role record %s is in the community repo but not indexed", ErrStateConflict, change.targetURI)
}

// roleRecordBody is a role record an accept writes.
type roleRecordBody struct {
	Type      string `json:"$type"`
	Subject   string `json:"subject"`
	Role      string `json:"role"`
	CreatedAt string `json:"createdAt"`
}

func encodeRoleRecord(subjectDID, role, createdAt string) (json.RawMessage, error) {
	body, err := json.Marshal(roleRecordBody{Type: communities.RoleCollection, Subject: subjectDID, Role: role, CreatedAt: createdAt})
	if err != nil {
		return nil, fmt.Errorf("%w: encoding the role record: %w", ErrModerationUnavailable, err)
	}
	return body, nil
}

// acceptIntent is the open intent of the accept, stored before the write. Its
// requester is the invitee; its audit actor is the inviter.
func (s *CommunityActionService) acceptIntent(accept acceptance, inv communities.Invitation, change acceptedChange, head *pds.LatestCommit, now time.Time) (CommunityIntent, error) {
	createdAt := now.UTC().Format(roleRecordCreatedAtLayout)
	record, err := encodeRoleRecord(inv.InviteeDID, inv.Role, createdAt)
	if err != nil {
		return CommunityIntent{}, err
	}
	id, err := newCommunityIntentID()
	if err != nil {
		return CommunityIntent{}, fmt.Errorf("%w: %w", ErrModerationUnavailable, err)
	}
	action := ActionGrantRole
	if inv.Role == communities.RoleOwner {
		action = ActionTransferOwnership
	}
	intent := CommunityIntent{
		ID:                   id,
		Action:               action,
		ActorDID:             accept.scope.actorDID,
		AuditActorDID:        inv.InviterDID,
		AdminActing:          inv.InviterAdmin,
		CommunityDID:         inv.CommunityDID,
		SubjectDID:           inv.InviteeDID,
		TargetURI:            change.targetURI,
		Op:                   string(change.op),
		Record:               record,
		GuardCommitCID:       head.CID,
		GuardRev:             head.Rev,
		InvitationID:         inv.ID,
		IdempotencyKey:       accept.scope.key,
		Fingerprint:          accept.scope.fingerprint,
		State:                CommunityIntentOpen,
		IdempotencyRetention: s.cfg.IdempotencyTTL,
		CreatedAt:            now,
		ExpiresAt:            now.Add(s.cfg.IntentTTL),
	}
	if change.inviteeRow != nil {
		intent.ReplacedCID = change.inviteeRow.RecordCID
	} else if change.erasedCID != "" {
		intent.ReplacedCID = change.erasedCID
	}
	if change.owner != nil {
		paired, err := encodeRoleRecord(change.owner.SubjectDID, communities.RoleModerator, createdAt)
		if err != nil {
			return CommunityIntent{}, err
		}
		intent.PairedTargetURI = change.ownerURI
		intent.PairedRecord = paired
		intent.PairedReplacedCID = change.owner.RecordCID
	}
	return intent, nil
}

// inviteeInvitation reads the invitation through read and refuses with
// InvitationNotFound an unknown one or one addressed to another account, so a
// caller learns nothing about invitations it did not receive.
func inviteeInvitation(ctx context.Context, read func(context.Context, string) (*communities.Invitation, error), invitationID, actorDID string) (*communities.Invitation, error) {
	inv, err := read(ctx, invitationID)
	if err != nil {
		return nil, fmt.Errorf("%w: reading the invitation: %w", ErrModerationUnavailable, err)
	}
	if inv == nil || inv.InviteeDID != actorDID {
		return nil, fmt.Errorf("%w: %s", ErrInvitationNotFound, invitationID)
	}
	return inv, nil
}

// requireOpenInvitation refuses an invitation that is no longer open
// (InvitationNotOpen) or has lapsed at now (InvitationLapsed).
func requireOpenInvitation(inv communities.Invitation, now time.Time) error {
	if inv.State != communities.InvitationOpen {
		return fmt.Errorf("%w: invitation %s is %s", ErrInvitationNotOpen, inv.ID, inv.State)
	}
	if inv.Lapsed(now) {
		return fmt.Errorf("%w: invitation %s lapsed at %s", ErrInvitationLapsed, inv.ID, inv.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return nil
}

// refuseBannedInvitee refuses an invitee with an in-force community ban, read
// in the transaction that holds the invitee's ban-subject lock.
func refuseBannedInvitee(ctx context.Context, tx CommunityActionTx, communityDID, inviteeDID string, now time.Time) error {
	ban, err := communities.ActiveCommunityBan(ctx, tx, communityDID, inviteeDID, now)
	if err != nil {
		return fmt.Errorf("%w: reading the invitee's community ban: %w", ErrModerationUnavailable, err)
	}
	if ban != nil {
		return fmt.Errorf("%w: community %s", ErrInviteeBanned, communityDID)
	}
	return nil
}

// refuseUnresolvedBan refuses an acceptance while a ban of the invitee in the
// community has an unknown outcome (its intent is open): the ban may have
// committed, and the role must not be granted to a banned account. It is read
// under the invitee's ban-subject lock, which the ban holds through its write,
// so no new ban intent appears meanwhile. The intent row is read without a
// lock: a firehose completion locks that row before the ban-subject lock, so
// waiting on it here would deadlock, and an intent that completion still holds
// reads as open, which refuses.
func refuseUnresolvedBan(ctx context.Context, tx CommunityActionTx, communityDID, inviteeDID string) error {
	open, err := tx.OpenIntentByTargetUnlocked(ctx, banRecordURI(communityDID, communities.BanRecordKey(inviteeDID)))
	if err != nil {
		return fmt.Errorf("%w: reading the invitee's open community ban intent: %w", ErrModerationUnavailable, err)
	}
	if open != nil && open.Action == ActionRemove {
		return fmt.Errorf("%w: a ban of the invitee in community %s has an unknown outcome", ErrStateConflict, communityDID)
	}
	return nil
}

// readRosterInTx is the community's effective roster read in the transaction.
func readRosterInTx(ctx context.Context, tx CommunityActionTx, communityDID string) (communities.Roster, error) {
	rows, err := tx.ListCommunityRoles(ctx, communityDID)
	if err != nil {
		return communities.Roster{}, fmt.Errorf("%w: reading the community roster: %w", ErrModerationUnavailable, err)
	}
	return communities.EffectiveRoster(rows), nil
}

// checkRosterVersion returns the roster version, or StateConflict when it is
// not expected.
func checkRosterVersion(ctx context.Context, tx CommunityActionTx, communityDID string, expected int64) (int64, error) {
	version, err := tx.RosterVersion(ctx, communityDID)
	if err != nil {
		return 0, fmt.Errorf("%w: reading the roster version: %w", ErrModerationUnavailable, err)
	}
	if version != expected {
		return 0, fmt.Errorf("%w: roster version is %d, not %d", ErrStateConflict, version, expected)
	}
	return version, nil
}

// validateKeyAndVersion checks an idempotency key and an expected roster
// version.
func validateKeyAndVersion(key string, expectedVersion int64) error {
	if !validOpaqueField(key) {
		return fmt.Errorf("%w: idempotencyKey must be 1-128 UTF-8 bytes", ErrInvalidRequest)
	}
	if expectedVersion < 0 {
		return fmt.Errorf("%w: expectedVersion must not be negative", ErrInvalidRequest)
	}
	return nil
}

// rosterRow is subjectDID's row in the effective roster, nil when it holds no
// role.
func rosterRow(roster communities.Roster, subjectDID string) *communities.CommunityRole {
	if roster.Owner != nil && roster.Owner.SubjectDID == subjectDID {
		owner := *roster.Owner
		return &owner
	}
	for _, row := range roster.Moderators {
		if row.SubjectDID == subjectDID {
			moderator := row
			return &moderator
		}
	}
	return nil
}

// rosterRoleOf is subjectDID's role in the effective roster, "" when none.
func rosterRoleOf(roster communities.Roster, subjectDID string) string {
	if row := rosterRow(roster, subjectDID); row != nil {
		return row.Role
	}
	return ""
}
