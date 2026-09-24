package moderation

import (
	"context"
	"errors"
	"fmt"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

// NewService builds the moderation read service over a SubjectReader.
func NewService(reader SubjectReader) Service {
	return &service{reader: reader}
}

type service struct {
	reader SubjectReader
}

func (s *service) GetSubjectState(ctx context.Context, subject string) (*SubjectState, error) {
	uri, err := syntax.ParseATURI(subject)
	if err != nil || !uri.Authority().IsDID() || !IsSubjectCollection(uri.Collection().String()) || uri.RecordKey().String() == "" {
		return nil, fmt.Errorf("%w: expected a record URI with a DID authority and supported collection", ErrInvalidSubject)
	}

	state := &SubjectState{
		Subject:    subject,
		Version:    InitialVersion,
		Moderation: ModerationView{State: ModerationStateClear},
	}
	record, err := s.reader.ReadSubject(ctx, subject)
	if errors.Is(err, ErrSubjectNotIndexed) {
		state.RecordState = RecordStateUnavailable
		return state, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrModerationUnavailable, err)
	}
	if record == nil {
		return nil, fmt.Errorf("%w: subject reader returned no record", ErrModerationUnavailable)
	}
	if record.Deleted {
		state.RecordState = RecordStateDeleted
	} else {
		state.RecordState = RecordStatePresent
		state.CurrentSubject = &StrongRef{URI: record.URI, CID: record.CID}
	}
	return state, nil
}
