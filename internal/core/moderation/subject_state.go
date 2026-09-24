package moderation

import "fmt"

// newSubjectState constructs a state with a current strong reference exactly
// when the indexed record is present.
func newSubjectState(subject string, version int64, recordState RecordState, current *StrongRef, activeRemoval *Action, authorityDID string) (SubjectState, error) {
	if (recordState == RecordStatePresent) != (current != nil) ||
		(recordState != RecordStatePresent && recordState != RecordStateDeleted && recordState != RecordStateUnavailable) || version < 0 {
		return SubjectState{}, fmt.Errorf("%w: inconsistent subject state", ErrModerationUnavailable)
	}
	state := SubjectState{
		Subject:        subject,
		Version:        versionToken(version),
		Moderation:     ModerationView{State: ModerationStateClear},
		RecordState:    recordState,
		CurrentSubject: current,
	}
	if activeRemoval != nil {
		state.Moderation.State = ModerationStateRemoved
		state.LocalRemoval = &ActionRef{ServiceDID: authorityDID, ActionID: activeRemoval.ID}
	}
	return state, nil
}
