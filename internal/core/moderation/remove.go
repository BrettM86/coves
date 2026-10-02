package moderation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

const (
	illegalContentReason = "social.coves.moderation.defs#reasonIllegalContent"
	doxingReason         = "social.coves.moderation.defs#reasonDoxing"
)

// removeReasons is every known moderation reason. removeContent and
// restoreContent require one; labelContent and retractContentLabel accept one
// optionally. Restore, label and retract-label reject the illegal-content and
// doxing reasons on top of this set.
var removeReasons = map[string]struct{}{
	"social.coves.moderation.defs#reasonSpam":       {},
	"social.coves.moderation.defs#reasonHarassment": {},
	doxingReason:         {},
	illegalContentReason: {},
	"social.coves.moderation.defs#reasonRuleViolation":       {},
	"social.coves.moderation.defs#reasonModeratorDiscretion": {},
}

func validateRemoveRequest(request RemoveContentRequest) error {
	uri, err := syntax.ParseATURI(request.Subject.URI)
	if err != nil || !uri.Authority().IsDID() || uri.RecordKey().String() == "" {
		return fmt.Errorf("%w: expected a content record URI with a DID authority", ErrInvalidSubject)
	}
	switch uri.Collection().String() {
	case CommentCollection, PostV2Collection, LegacyPostCollection:
	default:
		return fmt.Errorf("%w: unsupported subject collection", ErrInvalidSubject)
	}
	if _, err := syntax.ParseCID(request.Subject.CID); err != nil {
		return fmt.Errorf("%w: invalid subject CID", ErrInvalidRequest)
	}
	return validateMutationFields(request.IdempotencyKey, request.ExpectedVersion, request.Reason, request.PrivateNote)
}

func (s *service) removeContent(ctx context.Context, actorDID string, request RemoveContentRequest) (*MutationResult, error) {
	if err := validateRemoveRequest(request); err != nil {
		return nil, err
	}
	now := time.Now()
	if s.config.Now != nil {
		now = s.config.Now()
	}
	fingerprint := mutationFingerprint(ActionRemove, "", request.Subject.URI, request.Subject.CID, request.ExpectedVersion, request.Reason, "", request.PrivateNote)
	var result *MutationResult
	var newlyBlocked []MediaBlock
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
		if !subject.AuthorDeleted && subject.CID != request.Subject.CID {
			return fail(ErrContentChanged)
		}
		active, err := tx.ActiveRemoval(ctx, s.config.InstanceDID, request.Subject.URI)
		if err != nil {
			return unavailable(err)
		}
		activeLabels, err := tx.ActiveLabels(ctx, s.config.InstanceDID, request.Subject.URI)
		if err != nil {
			return unavailable(err)
		}
		recordState := RecordStatePresent
		var current *StrongRef
		if subject.AuthorDeleted {
			recordState = RecordStateDeleted
		} else {
			current = &StrongRef{URI: subject.URI, CID: subject.CID}
		}
		if active != nil {
			state, err := newSubjectState(request.Subject.URI, version, recordState, current, active, activeLabels, s.config.InstanceDID)
			if err != nil {
				return err
			}
			result = &MutationResult{Outcome: OutcomeUnchanged, State: state}
		} else {
			action, err := tx.InsertAction(ctx, Action{
				ActorDID: actorDID, AuthorityDID: s.config.InstanceDID,
				ScopeKind: ScopeInstance, SubjectURI: request.Subject.URI,
				SubjectCollection: subject.Collection, SubjectCommunityDID: subject.CommunityDID,
				ObservedCID: subject.CID, Action: ActionRemove, Reason: request.Reason,
				PrivateNote: request.PrivateNote, Origin: OriginLocal, CreatedAt: now,
			})
			if err != nil {
				return unavailable(err)
			}
			if action == nil || action.ID == "" {
				return unavailable(errors.New("action insert returned no identifier"))
			}
			if err := tx.SetRemovalDecision(ctx, s.config.InstanceDID, request.Subject.URI, action.ID, true); err != nil {
				return unavailable(err)
			}
			if err := tx.SetSubjectVersion(ctx, request.Subject.URI, version+1); err != nil {
				return unavailable(err)
			}
			newlyBlocked = imageMediaBlocks(subject, action)
			if len(newlyBlocked) > 0 {
				if err := tx.InsertMediaBlocks(ctx, newlyBlocked); err != nil {
					return unavailable(err)
				}
			}
			state, err := newSubjectState(request.Subject.URI, version+1, recordState, current, action, activeLabels, s.config.InstanceDID)
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
	purgeMediaBlocks(s.config.Purger, newlyBlocked)
	purgeCDNMediaBlocks(context.WithoutCancel(ctx), s.config.CDNPurger, newlyBlocked)
	return result, nil
}
