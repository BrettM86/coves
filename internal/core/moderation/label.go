package moderation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

func validateLabelRequest(request LabelContentRequest) error {
	uri, err := syntax.ParseATURI(request.Subject.URI)
	if err != nil || !uri.Authority().IsDID() || uri.RecordKey().String() == "" {
		return fmt.Errorf("%w: expected a post record URI with a DID authority", ErrInvalidSubject)
	}
	if uri.Collection().String() != PostV2Collection && uri.Collection().String() != LegacyPostCollection {
		return fmt.Errorf("%w: unsupported subject collection", ErrInvalidSubject)
	}
	if _, err := syntax.ParseCID(request.Subject.CID); err != nil {
		return fmt.Errorf("%w: invalid subject CID", ErrInvalidRequest)
	}
	if err := validateMutationBaseFields(request.IdempotencyKey, request.ExpectedVersion, request.PrivateNote); err != nil {
		return err
	}
	if !validOpaqueField(request.LabelValue) {
		return fmt.Errorf("%w: labelValue must be 1-128 UTF-8 bytes", ErrInvalidRequest)
	}
	return validateOptionalReason(request.Reason)
}

func (s *service) labelContent(ctx context.Context, actorDID string, request LabelContentRequest) (*MutationResult, error) {
	if err := validateLabelRequest(request); err != nil {
		return nil, err
	}
	now := time.Now()
	if s.config.Now != nil {
		now = s.config.Now()
	}
	fingerprint := mutationFingerprint(ActionLabel, "", request.Subject.URI, request.Subject.CID, request.ExpectedVersion, request.Reason, request.LabelValue, request.PrivateNote)
	var result *MutationResult
	var ruleError error
	err := s.store.InTransaction(ctx, func(ctx context.Context, tx Transaction) error {
		fail := func(err error) error {
			ruleError = err
			return err
		}
		unavailable := func(err error) error { return fmt.Errorf("%w: %w", ErrModerationUnavailable, err) }
		if err := tx.LockActor(ctx, actorDID); err != nil {
			return unavailable(err)
		}
		stored, err := tx.LiveIdempotencyRecord(ctx, actorDID, s.config.InstanceDID, request.IdempotencyKey, now)
		if err != nil {
			return unavailable(err)
		}
		if stored != nil {
			if stored.Fingerprint != fingerprint {
				return fail(ErrIdempotencyConflict)
			}
			copy := stored.Result
			result = &copy
			return nil
		}
		if request.LabelValue != LabelNSFW {
			return fail(ErrUnsupportedLabel)
		}
		count, err := tx.CountLiveIdempotencyKeys(ctx, actorDID, now)
		if err != nil {
			return unavailable(err)
		}
		if count >= s.config.MaxLiveIdempotencyKeys {
			return fail(fmt.Errorf("%w: live idempotency key limit %d reached", ErrInvalidRequest, s.config.MaxLiveIdempotencyKeys))
		}
		version, err := tx.LockSubject(ctx, request.Subject.URI)
		if err != nil {
			return unavailable(err)
		}
		if request.ExpectedVersion != versionToken(version) {
			return fail(ErrStateConflict)
		}
		subject, err := readIndexedSubject(ctx, tx, request.Subject.URI)
		if errors.Is(err, ErrSubjectNotIndexed) {
			return fail(ErrSubjectNotFound)
		}
		if err != nil {
			return unavailable(err)
		}
		if subject == nil {
			return unavailable(errors.New("indexed subject missing"))
		}
		if subject.AuthorDeleted {
			return fail(ErrSubjectNotFound)
		}
		if subject.CID != request.Subject.CID {
			return fail(ErrContentChanged)
		}
		labels, err := tx.ActiveLabels(ctx, s.config.InstanceDID, request.Subject.URI)
		if err != nil {
			return unavailable(err)
		}
		active, err := tx.ActiveRemoval(ctx, s.config.InstanceDID, request.Subject.URI)
		if err != nil {
			return unavailable(err)
		}
		current := &StrongRef{URI: subject.URI, CID: subject.CID}
		alreadyActive := false
		for _, label := range labels {
			if label.LabelValue == LabelNSFW {
				alreadyActive = true
				break
			}
		}
		if alreadyActive {
			state, err := newSubjectState(request.Subject.URI, version, RecordStatePresent, current, active, labels, s.config.InstanceDID)
			if err != nil {
				return err
			}
			result = &MutationResult{Outcome: OutcomeUnchanged, State: state}
		} else {
			action, err := tx.InsertAction(ctx, Action{
				ActorDID: actorDID, AuthorityDID: s.config.InstanceDID,
				ScopeKind: ScopeInstance, SubjectURI: request.Subject.URI,
				SubjectCollection: subject.Collection, SubjectCommunityDID: subject.CommunityDID,
				ObservedCID: subject.CID, Action: ActionLabel, LabelValue: LabelNSFW,
				Reason: request.Reason, PrivateNote: request.PrivateNote,
				Origin: OriginLocal, CreatedAt: now,
			})
			if err != nil {
				return unavailable(err)
			}
			if action == nil || action.ID == "" {
				return unavailable(errors.New("action insert returned no identifier"))
			}
			if err := tx.SetLabelDecision(ctx, s.config.InstanceDID, request.Subject.URI, LabelNSFW, action.ID, true); err != nil {
				return unavailable(err)
			}
			if err := tx.SetSubjectVersion(ctx, request.Subject.URI, version+1); err != nil {
				return unavailable(err)
			}
			appliedLabels, err := tx.ActiveLabels(ctx, s.config.InstanceDID, request.Subject.URI)
			if err != nil {
				return unavailable(err)
			}
			state, err := newSubjectState(request.Subject.URI, version+1, RecordStatePresent, current, active, appliedLabels, s.config.InstanceDID)
			if err != nil {
				return err
			}
			result = &MutationResult{Outcome: OutcomeApplied, State: state, Action: action}
		}
		return tx.SaveIdempotencyRecord(ctx, IdempotencyRecord{
			ActorDID: actorDID, AuthorityDID: s.config.InstanceDID, Key: request.IdempotencyKey,
			Fingerprint: fingerprint, Result: *result, CreatedAt: now, ExpiresAt: now.Add(s.config.IdempotencyRetention),
		})
	})
	if err != nil {
		if ruleError != nil && errors.Is(err, ruleError) {
			return nil, ruleError
		}
		return nil, fmt.Errorf("%w: %w", ErrModerationUnavailable, err)
	}
	return result, nil
}
