package moderation

import (
	"context"
	"errors"
	"fmt"
	"time"
)

func validateRetractLabelRequest(request RetractContentLabelRequest) error {
	if !validOpaqueField(request.ActionID) {
		return fmt.Errorf("%w: actionId must be 1-128 UTF-8 bytes", ErrInvalidRequest)
	}
	if err := validateMutationBaseFields(request.IdempotencyKey, request.ExpectedVersion, request.PrivateNote); err != nil {
		return err
	}
	if err := validateOptionalReason(request.Reason); err != nil {
		return err
	}
	if request.ReviewedSubject != nil && !validContentStrongRef(*request.ReviewedSubject) {
		return fmt.Errorf("%w: reviewedSubject must be a content strongRef", ErrInvalidRequest)
	}
	return nil
}

func (s *service) retractContentLabel(ctx context.Context, actorDID string, request RetractContentLabelRequest) (*MutationResult, error) {
	if err := validateRetractLabelRequest(request); err != nil {
		return nil, err
	}
	now := time.Now()
	if s.config.Now != nil {
		now = s.config.Now()
	}
	var reviewedURI, reviewedCID string
	if request.ReviewedSubject != nil {
		reviewedURI = request.ReviewedSubject.URI
		reviewedCID = request.ReviewedSubject.CID
	}
	fingerprint := mutationFingerprint(ActionRetractLabel, request.ActionID, reviewedURI, reviewedCID, request.ExpectedVersion, request.Reason, "", request.PrivateNote)
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
		count, err := tx.CountLiveIdempotencyKeys(ctx, actorDID, now)
		if err != nil {
			return unavailable(err)
		}
		if count >= s.config.MaxLiveIdempotencyKeys {
			return fail(fmt.Errorf("%w: live idempotency key limit %d reached", ErrInvalidRequest, s.config.MaxLiveIdempotencyKeys))
		}
		label, err := tx.GetAction(ctx, request.ActionID)
		if errors.Is(err, ErrDecisionNotFound) {
			return fail(ErrDecisionNotFound)
		}
		if err != nil {
			return unavailable(err)
		}
		if label == nil {
			return unavailable(errors.New("action lookup returned no action"))
		}
		if label.Action != ActionLabel || label.AuthorityDID != s.config.InstanceDID || label.ScopeKind != ScopeInstance || label.Origin != OriginLocal {
			return fail(ErrInvalidDecision)
		}
		version, err := tx.LockSubject(ctx, label.SubjectURI)
		if err != nil {
			return unavailable(err)
		}
		activeLabels, err := tx.ActiveLabels(ctx, s.config.InstanceDID, label.SubjectURI)
		if err != nil {
			return unavailable(err)
		}
		found := false
		for _, activeLabel := range activeLabels {
			if activeLabel.ID == label.ID {
				found = true
				break
			}
		}
		if !found {
			return fail(ErrInvalidDecision)
		}
		if request.ExpectedVersion != versionToken(version) {
			return fail(ErrStateConflict)
		}
		if request.ReviewedSubject != nil && reviewedURI != label.SubjectURI {
			return fail(fmt.Errorf("%w: reviewedSubject URI differs from label subject", ErrInvalidRequest))
		}
		subject, err := readIndexedSubject(ctx, tx, label.SubjectURI)
		if err != nil && !errors.Is(err, ErrSubjectNotIndexed) {
			return unavailable(err)
		}
		recordState := RecordStateUnavailable
		var current *StrongRef
		var observedCID string
		if err == nil {
			if subject == nil {
				return unavailable(errors.New("indexed subject lookup returned no subject"))
			}
			observedCID = subject.CID
			if subject.AuthorDeleted {
				recordState = RecordStateDeleted
			} else {
				recordState = RecordStatePresent
				current = &StrongRef{URI: subject.URI, CID: subject.CID}
				if request.ReviewedSubject == nil {
					return fail(fmt.Errorf("%w: reviewedSubject is required for present content", ErrInvalidRequest))
				}
				if reviewedCID != subject.CID {
					return fail(ErrContentChanged)
				}
			}
		}
		action, err := tx.InsertAction(ctx, Action{
			ActorDID: actorDID, AuthorityDID: s.config.InstanceDID,
			ScopeKind: ScopeInstance, SubjectURI: label.SubjectURI,
			SubjectCollection: label.SubjectCollection, SubjectCommunityDID: label.SubjectCommunityDID,
			ObservedCID: observedCID, Action: ActionRetractLabel, LabelValue: label.LabelValue,
			Reason: request.Reason, PrivateNote: request.PrivateNote, ReversesActionID: label.ID,
			ReversedActionReason: label.Reason,
			Origin:               OriginLocal, CreatedAt: now,
		})
		if err != nil {
			return unavailable(err)
		}
		if action == nil || action.ID == "" {
			return unavailable(errors.New("action insert returned no identifier"))
		}
		if err := tx.SetLabelDecision(ctx, s.config.InstanceDID, label.SubjectURI, label.LabelValue, label.ID, false); err != nil {
			return unavailable(err)
		}
		if err := tx.SetSubjectVersion(ctx, label.SubjectURI, version+1); err != nil {
			return unavailable(err)
		}
		removal, err := tx.ActiveRemoval(ctx, s.config.InstanceDID, label.SubjectURI)
		if err != nil {
			return unavailable(err)
		}
		remainingLabels, err := tx.ActiveLabels(ctx, s.config.InstanceDID, label.SubjectURI)
		if err != nil {
			return unavailable(err)
		}
		state, err := newSubjectState(label.SubjectURI, version+1, recordState, current, removal, remainingLabels, s.config.InstanceDID)
		if err != nil {
			return err
		}
		result = &MutationResult{Outcome: OutcomeApplied, State: state, Action: action}
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
