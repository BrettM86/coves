package moderation

import (
	"context"
	"errors"
	"fmt"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

// NewService builds the moderation service.
func NewService(reader SubjectReader, store Store, config Config) Service {
	return &service{reader: reader, store: store, config: config}
}

type service struct {
	reader SubjectReader
	store  Store
	config Config
}

func (s *service) RemoveContent(ctx context.Context, actorDID string, request RemoveContentRequest) (*MutationResult, error) {
	return s.removeContent(ctx, actorDID, request)
}

func (s *service) RestoreContent(ctx context.Context, actorDID string, request RestoreContentRequest) (*MutationResult, error) {
	return s.restoreContent(ctx, actorDID, request)
}

func (s *service) LabelContent(ctx context.Context, actorDID string, request LabelContentRequest) (*MutationResult, error) {
	return s.labelContent(ctx, actorDID, request)
}

func (s *service) RetractContentLabel(ctx context.Context, actorDID string, request RetractContentLabelRequest) (*MutationResult, error) {
	return s.retractContentLabel(ctx, actorDID, request)
}

func (s *service) GetSubjectState(ctx context.Context, subject string) (*SubjectState, error) {
	if accountDID, ok := accountSubjectDID(subject); ok {
		return s.accountSubjectState(ctx, subject, accountDID)
	}
	uri, err := syntax.ParseATURI(subject)
	if err != nil || !uri.Authority().IsDID() || !IsSubjectCollection(uri.Collection().String()) || uri.RecordKey().String() == "" {
		return nil, fmt.Errorf("%w: expected a record URI with a DID authority and supported collection", ErrInvalidSubject)
	}

	record, err := s.reader.ReadSubject(ctx, subject)
	if err != nil && !errors.Is(err, ErrSubjectNotIndexed) {
		return nil, fmt.Errorf("%w: %w", ErrModerationUnavailable, err)
	}
	if err == nil && record == nil {
		return nil, fmt.Errorf("%w: subject reader returned no record", ErrModerationUnavailable)
	}
	stored, err := s.store.SubjectModeration(ctx, s.config.InstanceDID, subject)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrModerationUnavailable, err)
	}
	if stored == nil {
		return nil, fmt.Errorf("%w: subject store returned no state", ErrModerationUnavailable)
	}
	recordState := RecordStateUnavailable
	var current *StrongRef
	if record != nil {
		if record.Deleted {
			recordState = RecordStateDeleted
		} else {
			recordState = RecordStatePresent
			current = &StrongRef{URI: record.URI, CID: record.CID}
		}
	}
	state, err := newSubjectState(subject, stored.Version, recordState, current, stored.ActiveRemoval, stored.ActiveLabels, s.config.InstanceDID)
	if err != nil {
		return nil, err
	}
	return &state, nil
}

// accountSubjectState is GetSubjectState for an at://<did> subject. An active
// decision's recorded subject kind picks an account's or a community's state;
// with none, an indexed community's state, or else an account's.
func (s *service) accountSubjectState(ctx context.Context, subject, accountDID string) (*SubjectState, error) {
	stored, err := s.store.SubjectModeration(ctx, s.config.InstanceDID, subject)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrModerationUnavailable, err)
	}
	if stored == nil {
		return nil, fmt.Errorf("%w: subject store returned no state", ErrModerationUnavailable)
	}
	isCommunity := false
	if stored.ActiveRemoval != nil {
		isCommunity = stored.ActiveRemoval.SubjectKind == SubjectKindCommunity
	} else {
		isCommunity, err = s.isIndexedCommunity(ctx, accountDID)
		if err != nil {
			return nil, err
		}
	}
	var state SubjectState
	if isCommunity {
		state, err = newSubjectState(subject, stored.Version, RecordStateUnavailable, nil, communityRemoval(stored.ActiveRemoval), nil, s.config.InstanceDID)
	} else {
		state, err = newAccountSubjectState(subject, stored.Version, stored.ActiveRemoval, s.config.InstanceDID, s.now())
	}
	if err != nil {
		return nil, err
	}
	return &state, nil
}
