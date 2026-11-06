package moderation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"Coves/internal/core/accountbans"
	"Coves/internal/core/labels"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

// Fingerprint operations of the account mutations. They differ from every
// content operation, so a key reused across the two kinds is a conflict.
const (
	banAccountOperation   = "ban-account"
	unbanAccountOperation = "unban-account"
)

// AccountSubjectURI is the moderation subject of an account: at://<did>.
func AccountSubjectURI(accountDID string) string {
	return "at://" + accountDID
}

// accountSubjectDID returns the DID of an at://<did> account subject.
func accountSubjectDID(subject string) (string, bool) {
	did, found := strings.CutPrefix(subject, "at://")
	if !found {
		return "", false
	}
	if _, err := syntax.ParseDID(did); err != nil {
		return "", false
	}
	return did, true
}

// accountBanInForce reports whether an active account removal applies at now.
func accountBanInForce(removal *Action, now time.Time) bool {
	if removal == nil {
		return false
	}
	ban := accountbans.Ban{ActionID: removal.ID, Reason: removal.Reason, ExpiresAt: removal.ExpiresAt}
	return ban.InForce(now)
}

// newAccountSubjectState is the state of an account subject. The account has
// no indexed record here, so its record state is unavailable. Its removal
// decision shows while active; the account reads as removed only while the
// ban is in force.
func newAccountSubjectState(subject string, version int64, activeRemoval *Action, authorityDID string, now time.Time) (SubjectState, error) {
	state, err := newSubjectState(subject, version, RecordStateUnavailable, nil, activeRemoval, nil, authorityDID)
	if err != nil {
		return SubjectState{}, err
	}
	if activeRemoval != nil {
		state.LocalRemoval.ExpiresAt = activeRemoval.ExpiresAt
		if !accountBanInForce(activeRemoval, now) {
			state.Moderation.State = ModerationStateClear
		}
	}
	return state, nil
}

// accountMutationFingerprint binds an account mutation's request fields,
// including the normalized ban expiry, to its idempotency key.
func accountMutationFingerprint(operation, subjectURI, expectedVersion, reason, expiresAt, privateNote string) string {
	payload, _ := json.Marshal([6]string{operation, subjectURI, expectedVersion, reason, expiresAt, privateNote})
	hash := sha256.Sum256(payload)
	return hex.EncodeToString(hash[:])
}

func validateAccountSubject(subject string) error {
	if _, err := syntax.ParseDID(subject); err != nil {
		return fmt.Errorf("%w: subject must be an account DID", ErrInvalidRequest)
	}
	return nil
}

func (s *service) now() time.Time {
	if s.config.Now != nil {
		return s.config.Now()
	}
	return time.Now()
}

func (s *service) BanAccount(ctx context.Context, actorDID string, request BanAccountRequest) (*MutationResult, error) {
	if err := validateAccountSubject(request.Subject); err != nil {
		return nil, err
	}
	if err := validateMutationFields(request.IdempotencyKey, request.ExpectedVersion, request.Reason, request.PrivateNote); err != nil {
		return nil, err
	}
	// Labels and stored expiries carry milliseconds, so the ban does too.
	var expiresAt *time.Time
	var expiresAtToken string
	if request.ExpiresAt != nil {
		normalized := request.ExpiresAt.UTC().Truncate(time.Millisecond)
		expiresAt = &normalized
		expiresAtToken = normalized.Format(labels.CreatedAtLayout)
	}
	now := s.now()
	subjectURI := AccountSubjectURI(request.Subject)
	fingerprint := accountMutationFingerprint(banAccountOperation, subjectURI, request.ExpectedVersion, request.Reason, expiresAtToken, request.PrivateNote)
	return s.accountMutation(ctx, actorDID, request.IdempotencyKey, fingerprint, SubjectKindAccount, now, func(ctx context.Context, tx Transaction, fail func(error) error) (*MutationResult, error) {
		// Checked after the idempotency lookup: a replay of a ban whose expiry
		// has since passed still returns its original result.
		if expiresAt != nil && !expiresAt.After(now) {
			return nil, fail(fmt.Errorf("%w: expiresAt must be after the server's current time", ErrInvalidRequest))
		}
		version, err := tx.LockSubject(ctx, subjectURI)
		if err != nil {
			return nil, unavailableError(err)
		}
		// An indexed community DID is banned as an account only when it also
		// has account activity and is not a trusted bridge's community;
		// otherwise removing it is the separate community procedure. The
		// refusal is applied after the idempotency lookup, so a replay of a ban
		// made before the DID was indexed as a community still returns its
		// original result, and before the version check, so an unbannable
		// subject is refused whatever version the request carries. Only the
		// facts read under the subject lock decide: communityRepo.Create and
		// userRepo.Create take the same lock, so the facts hold until commit.
		facts, err := tx.AccountSubjectFacts(ctx, request.Subject)
		if err != nil {
			return nil, unavailableError(err)
		}
		if facts.IsCommunity {
			if !facts.HasAccountActivity {
				return nil, fail(fmt.Errorf("%w: subject is a community with no account activity, not an account", ErrInvalidSubject))
			}
			// The exemption is decided on the stored communities.handle and
			// pds_url, which the community consumer records from its verified
			// identity resolution, by the rule it applied when it indexed the
			// community despite account activity.
			if s.config.BridgeTrust != nil && s.config.BridgeTrust.TrustsCommunity(facts.CommunityHandle, facts.CommunityPDSURL) {
				return nil, fail(fmt.Errorf("%w: subject is a trusted bridge's community; use removeCommunity", ErrInvalidSubject))
			}
		}
		if request.ExpectedVersion != versionToken(version) {
			return nil, fail(ErrStateConflict)
		}
		// A community removal whose community row was deleted is still a
		// community decision: a ban never replaces it.
		active, err := tx.ActiveRemoval(ctx, s.config.InstanceDID, subjectURI)
		if err != nil {
			return nil, unavailableError(err)
		}
		if active != nil && active.SubjectKind == SubjectKindCommunity {
			return nil, fail(fmt.Errorf("%w: the subject has an active community removal", ErrStateConflict))
		}
		// A ban in force, or one that has lapsed, is replaced: the decision
		// moves to the new action, whatever expiry it carries.
		action, err := tx.InsertAction(ctx, Action{
			ActorDID: actorDID, AuthorityDID: s.config.InstanceDID,
			ScopeKind: ScopeInstance, SubjectURI: subjectURI, SubjectKind: SubjectKindAccount,
			Action: ActionRemove, Reason: request.Reason, PrivateNote: request.PrivateNote,
			Origin: OriginLocal, CreatedAt: now, ExpiresAt: expiresAt,
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
		// An account label names the bare DID (atproto label spec).
		if err := s.publishExpiringLabel(ctx, tx, request.Subject, takedownLabelValue, false, action.ID, now, action.ExpiresAt); err != nil {
			return nil, unavailableError(err)
		}
		if err := tx.SetSubjectVersion(ctx, subjectURI, version+1); err != nil {
			return nil, unavailableError(err)
		}
		state, err := newAccountSubjectState(subjectURI, version+1, action, s.config.InstanceDID, now)
		if err != nil {
			return nil, err
		}
		return &MutationResult{Outcome: OutcomeApplied, State: state, Action: action}, nil
	})
}

func (s *service) UnbanAccount(ctx context.Context, actorDID string, request UnbanAccountRequest) (*MutationResult, error) {
	if err := validateAccountSubject(request.Subject); err != nil {
		return nil, err
	}
	if err := validateMutationFields(request.IdempotencyKey, request.ExpectedVersion, request.Reason, request.PrivateNote); err != nil {
		return nil, err
	}
	// As for a restore: a hidden reason would drop the unban from the public
	// history while it reversed a public ban.
	if request.Reason == illegalContentReason || request.Reason == doxingReason {
		return nil, fmt.Errorf("%w: an unban cannot use the illegal-content or doxing reason", ErrUnsupportedReason)
	}
	// An account ban on a DID indexed as a community afterwards is still
	// lifted here, so the community lookup only decides the refusal when the
	// subject has no active ban.
	// This pre-lock lookup only picks the refusal code (InvalidSubject vs
	// StateConflict); it never decides whether a decision is applied.
	isCommunity, err := s.isIndexedCommunity(ctx, request.Subject)
	if err != nil {
		return nil, err
	}
	now := s.now()
	subjectURI := AccountSubjectURI(request.Subject)
	fingerprint := accountMutationFingerprint(unbanAccountOperation, subjectURI, request.ExpectedVersion, request.Reason, "", request.PrivateNote)
	return s.accountMutation(ctx, actorDID, request.IdempotencyKey, fingerprint, SubjectKindAccount, now, func(ctx context.Context, tx Transaction, fail func(error) error) (*MutationResult, error) {
		version, err := tx.LockSubject(ctx, subjectURI)
		if err != nil {
			return nil, unavailableError(err)
		}
		// A lapsed ban is still an active decision and is lifted like one.
		ban, err := tx.ActiveRemoval(ctx, s.config.InstanceDID, subjectURI)
		if err != nil {
			return nil, unavailableError(err)
		}
		if ban != nil && ban.SubjectKind != SubjectKindAccount {
			ban = nil
		}
		if ban == nil && isCommunity {
			return nil, fail(fmt.Errorf("%w: subject is a community, not an account", ErrInvalidSubject))
		}
		if request.ExpectedVersion != versionToken(version) {
			return nil, fail(ErrStateConflict)
		}
		if ban == nil {
			return nil, fail(fmt.Errorf("%w: the account has no active ban", ErrStateConflict))
		}
		action, err := tx.InsertAction(ctx, Action{
			ActorDID: actorDID, AuthorityDID: s.config.InstanceDID,
			ScopeKind: ScopeInstance, SubjectURI: subjectURI, SubjectKind: SubjectKindAccount,
			Action: ActionRestore, Reason: request.Reason, PrivateNote: request.PrivateNote,
			ReversesActionID: ban.ID, ReversedActionReason: ban.Reason,
			Origin: OriginLocal, CreatedAt: now,
		})
		if err != nil {
			return nil, unavailableError(err)
		}
		if action == nil || action.ID == "" {
			return nil, unavailableError(errors.New("action insert returned no identifier"))
		}
		if err := tx.SetRemovalDecision(ctx, s.config.InstanceDID, subjectURI, ban.ID, false); err != nil {
			return nil, unavailableError(err)
		}
		if err := s.publishLabel(ctx, tx, request.Subject, takedownLabelValue, true, action.ID, now); err != nil {
			return nil, unavailableError(err)
		}
		if err := tx.SetSubjectVersion(ctx, subjectURI, version+1); err != nil {
			return nil, unavailableError(err)
		}
		state, err := newAccountSubjectState(subjectURI, version+1, nil, s.config.InstanceDID, now)
		if err != nil {
			return nil, err
		}
		return &MutationResult{Outcome: OutcomeApplied, State: state, Action: action}, nil
	})
}

func unavailableError(err error) error {
	return fmt.Errorf("%w: %w", ErrModerationUnavailable, err)
}

// accountMutation runs an account mutation under the shared idempotency
// rules: the actor lock, replay or conflict of a live key, and the live-key
// limit, then apply, whose result is stored against the key. apply reports a
// rule refusal through fail so it reaches the caller unwrapped. A stored result
// does not carry its action's subject kind, so a replay sets it to kind, the
// kind every action of the mutation records.
func (s *service) accountMutation(ctx context.Context, actorDID, key, fingerprint string, kind SubjectKind, now time.Time,
	apply func(ctx context.Context, tx Transaction, fail func(error) error) (*MutationResult, error),
) (*MutationResult, error) {
	var result *MutationResult
	var ruleError error
	err := s.store.InTransaction(ctx, func(ctx context.Context, tx Transaction) error {
		fail := func(err error) error {
			ruleError = err
			return err
		}
		if err := tx.LockActor(ctx, actorDID); err != nil {
			return unavailableError(err)
		}
		stored, err := tx.LiveIdempotencyRecord(ctx, actorDID, s.config.InstanceDID, key, now)
		if err != nil {
			return unavailableError(err)
		}
		if stored != nil {
			if stored.Fingerprint != fingerprint {
				return fail(ErrIdempotencyConflict)
			}
			storedResult := stored.Result
			if storedResult.Action != nil {
				action := *storedResult.Action
				action.SubjectKind = kind
				storedResult.Action = &action
			}
			result = &storedResult
			return nil
		}
		count, err := tx.CountLiveIdempotencyKeys(ctx, actorDID, now)
		if err != nil {
			return unavailableError(err)
		}
		if count >= s.config.MaxLiveIdempotencyKeys {
			return fail(fmt.Errorf("%w: live idempotency key limit %d reached", ErrInvalidRequest, s.config.MaxLiveIdempotencyKeys))
		}
		applied, err := apply(ctx, tx, fail)
		if err != nil {
			return err
		}
		result = applied
		return tx.SaveIdempotencyRecord(ctx, IdempotencyRecord{
			ActorDID: actorDID, AuthorityDID: s.config.InstanceDID, Key: key,
			Fingerprint: fingerprint, Result: *result, CreatedAt: now, ExpiresAt: now.Add(s.config.IdempotencyRetention),
		})
	})
	if err != nil {
		if ruleError != nil && errors.Is(err, ruleError) {
			return nil, ruleError
		}
		if errors.Is(err, ErrModerationUnavailable) {
			return nil, err
		}
		return nil, unavailableError(err)
	}
	return result, nil
}
