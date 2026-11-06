package moderation

import (
	"context"
	"errors"
	"fmt"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

// Fingerprint operations of the community mutations. They differ from every
// other operation, so a key reused across kinds is a conflict.
const (
	removeCommunityOperation  = "remove-community"
	restoreCommunityOperation = "restore-community"
)

// requireIndexedCommunity refuses a DID that is not an indexed community.
func (s *service) requireIndexedCommunity(ctx context.Context, did string) error {
	isCommunity, err := s.isIndexedCommunity(ctx, did)
	if err != nil {
		return err
	}
	if !isCommunity {
		return fmt.Errorf("%w: subject is not an indexed community", ErrInvalidSubject)
	}
	return nil
}

// isIndexedCommunity reports whether did is an indexed community.
func (s *service) isIndexedCommunity(ctx context.Context, did string) (bool, error) {
	if s.config.Communities == nil {
		return false, fmt.Errorf("%w: community lookup is not configured", ErrModerationUnavailable)
	}
	isCommunity, err := s.config.Communities.ExistsByDID(ctx, did)
	if err != nil {
		return false, fmt.Errorf("%w: %w", ErrModerationUnavailable, err)
	}
	return isCommunity, nil
}

// communityRemoval is the community removal among an active instance removal
// decision on at://<did>: the decision when its recorded subject kind is
// community, matching the read rule in communityRemovedSQL, and nil for an
// account ban, even on a DID indexed as a community afterwards.
func communityRemoval(active *Action) *Action {
	if active == nil || active.SubjectKind != SubjectKindCommunity {
		return nil
	}
	return active
}

func (s *service) RemoveCommunity(ctx context.Context, actorDID string, request RemoveCommunityRequest) (*MutationResult, error) {
	if _, err := syntax.ParseDID(request.Subject); err != nil {
		return nil, fmt.Errorf("%w: subject must be a community DID", ErrInvalidRequest)
	}
	if err := validateMutationFields(request.IdempotencyKey, request.ExpectedVersion, request.Reason, request.PrivateNote); err != nil {
		return nil, err
	}
	if err := s.requireIndexedCommunity(ctx, request.Subject); err != nil {
		return nil, err
	}
	now := s.now()
	subjectURI := AccountSubjectURI(request.Subject)
	fingerprint := accountMutationFingerprint(removeCommunityOperation, subjectURI, request.ExpectedVersion, request.Reason, "", request.PrivateNote)
	return s.accountMutation(ctx, actorDID, request.IdempotencyKey, fingerprint, SubjectKindCommunity, now, func(ctx context.Context, tx Transaction, fail func(error) error) (*MutationResult, error) {
		version, err := tx.LockSubject(ctx, subjectURI)
		if err != nil {
			return nil, unavailableError(err)
		}
		if request.ExpectedVersion != versionToken(version) {
			return nil, fail(ErrStateConflict)
		}
		active, err := tx.ActiveRemoval(ctx, s.config.InstanceDID, subjectURI)
		if err != nil {
			return nil, unavailableError(err)
		}
		if removal := communityRemoval(active); removal != nil {
			state, err := newSubjectState(subjectURI, version, RecordStateUnavailable, nil, removal, nil, s.config.InstanceDID)
			if err != nil {
				return nil, err
			}
			return &MutationResult{Outcome: OutcomeUnchanged, State: state}, nil
		}
		// An account ban in force is lifted with unbanAccount first; a lapsed
		// one is superseded by the removal.
		if accountBanInForce(active, now) {
			return nil, fail(fmt.Errorf("%w: the subject has an account ban in force", ErrStateConflict))
		}
		action, err := tx.InsertAction(ctx, Action{
			ActorDID: actorDID, AuthorityDID: s.config.InstanceDID,
			ScopeKind: ScopeInstance, SubjectURI: subjectURI, SubjectKind: SubjectKindCommunity,
			Action: ActionRemove, Reason: request.Reason, PrivateNote: request.PrivateNote,
			Origin: OriginLocal, CreatedAt: now,
		})
		if err != nil {
			return nil, unavailableError(err)
		}
		if action == nil || action.ID == "" {
			return nil, unavailableError(errors.New("action insert returned no identifier"))
		}
		if err := tx.SetRemovalDecision(ctx, s.config.InstanceDID, subjectURI, action.ID, true); err != nil {
			return nil, unavailableError(err)
		}
		// A community label names the bare community DID (atproto label spec).
		if err := s.publishLabel(ctx, tx, request.Subject, takedownLabelValue, false, action.ID, now); err != nil {
			return nil, unavailableError(err)
		}
		if err := tx.SetSubjectVersion(ctx, subjectURI, version+1); err != nil {
			return nil, unavailableError(err)
		}
		state, err := newSubjectState(subjectURI, version+1, RecordStateUnavailable, nil, action, nil, s.config.InstanceDID)
		if err != nil {
			return nil, err
		}
		return &MutationResult{Outcome: OutcomeApplied, State: state, Action: action}, nil
	})
}

func (s *service) RestoreCommunity(ctx context.Context, actorDID string, request RestoreCommunityRequest) (*MutationResult, error) {
	if _, err := syntax.ParseDID(request.Subject); err != nil {
		return nil, fmt.Errorf("%w: subject must be a community DID", ErrInvalidRequest)
	}
	if err := validateMutationFields(request.IdempotencyKey, request.ExpectedVersion, request.Reason, request.PrivateNote); err != nil {
		return nil, err
	}
	// As for a restore: a hidden reason would drop the restore from the public
	// history while it reversed a public removal.
	if request.Reason == illegalContentReason || request.Reason == doxingReason {
		return nil, fmt.Errorf("%w: a community restore cannot use the illegal-content or doxing reason", ErrUnsupportedReason)
	}
	// A community removal whose community row was deleted is still lifted
	// here, so the community lookup only decides the refusal when the subject
	// has no active community removal.
	// This pre-lock lookup only picks the refusal code (InvalidSubject vs
	// StateConflict); it never decides whether a decision is applied.
	isCommunity, err := s.isIndexedCommunity(ctx, request.Subject)
	if err != nil {
		return nil, err
	}
	now := s.now()
	subjectURI := AccountSubjectURI(request.Subject)
	fingerprint := accountMutationFingerprint(restoreCommunityOperation, subjectURI, request.ExpectedVersion, request.Reason, "", request.PrivateNote)
	return s.accountMutation(ctx, actorDID, request.IdempotencyKey, fingerprint, SubjectKindCommunity, now, func(ctx context.Context, tx Transaction, fail func(error) error) (*MutationResult, error) {
		version, err := tx.LockSubject(ctx, subjectURI)
		if err != nil {
			return nil, unavailableError(err)
		}
		removal, err := tx.ActiveRemoval(ctx, s.config.InstanceDID, subjectURI)
		if err != nil {
			return nil, unavailableError(err)
		}
		removal = communityRemoval(removal)
		if removal == nil && !isCommunity {
			return nil, fail(fmt.Errorf("%w: subject is not an indexed community", ErrInvalidSubject))
		}
		if request.ExpectedVersion != versionToken(version) {
			return nil, fail(ErrStateConflict)
		}
		if removal == nil {
			return nil, fail(fmt.Errorf("%w: the community has no active removal", ErrStateConflict))
		}
		action, err := tx.InsertAction(ctx, Action{
			ActorDID: actorDID, AuthorityDID: s.config.InstanceDID,
			ScopeKind: ScopeInstance, SubjectURI: subjectURI, SubjectKind: SubjectKindCommunity,
			Action: ActionRestore, Reason: request.Reason, PrivateNote: request.PrivateNote,
			ReversesActionID: removal.ID, ReversedActionReason: removal.Reason,
			Origin: OriginLocal, CreatedAt: now,
		})
		if err != nil {
			return nil, unavailableError(err)
		}
		if action == nil || action.ID == "" {
			return nil, unavailableError(errors.New("action insert returned no identifier"))
		}
		if err := tx.SetRemovalDecision(ctx, s.config.InstanceDID, subjectURI, removal.ID, false); err != nil {
			return nil, unavailableError(err)
		}
		if err := s.publishLabel(ctx, tx, request.Subject, takedownLabelValue, true, action.ID, now); err != nil {
			return nil, unavailableError(err)
		}
		if err := tx.SetSubjectVersion(ctx, subjectURI, version+1); err != nil {
			return nil, unavailableError(err)
		}
		state, err := newSubjectState(subjectURI, version+1, RecordStateUnavailable, nil, nil, nil, s.config.InstanceDID)
		if err != nil {
			return nil, err
		}
		return &MutationResult{Outcome: OutcomeApplied, State: state, Action: action}, nil
	})
}
