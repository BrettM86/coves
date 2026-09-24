package moderation_test

import (
	"context"
	"errors"
	"testing"

	"Coves/internal/core/moderation"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeSubjectReader struct {
	calls  []string
	record *moderation.IndexedRecord
	err    error
}

type fakeSubjectStore struct{}

func (fakeSubjectStore) InTransaction(_ context.Context, _ func(context.Context, moderation.Transaction) error) error {
	return nil
}

func (fakeSubjectStore) SubjectModeration(_ context.Context, _, _ string) (*moderation.SubjectModeration, error) {
	return &moderation.SubjectModeration{Version: 0}, nil
}

func newSubjectStateTestService(reader moderation.SubjectReader) moderation.Service {
	return moderation.NewService(reader, fakeSubjectStore{}, moderation.Config{InstanceDID: "did:web:test.coves.social"})
}

func (reader *fakeSubjectReader) ReadSubject(_ context.Context, uri string) (*moderation.IndexedRecord, error) {
	reader.calls = append(reader.calls, uri)
	return reader.record, reader.err
}

func assertInitialSubjectState(t *testing.T, state *moderation.SubjectState, uri string, recordState moderation.RecordState, current *moderation.StrongRef) {
	t.Helper()
	require.NotNil(t, state)
	assert.Equal(t, uri, state.Subject)
	assert.Equal(t, recordState, state.RecordState)
	assert.Equal(t, moderation.InitialVersion, state.Version)
	assert.Equal(t, moderation.ModerationStateClear, state.Moderation.State)
	assert.Equal(t, current, state.CurrentSubject)
	assert.Nil(t, state.LocalRemoval)
	assert.Empty(t, state.LocalLabels)
}

func TestGetSubjectStateRejectsInvalidSubjectsBeforeReading(t *testing.T) {
	for _, subject := range []string{
		"",
		"not a uri",
		"at://alice.test/social.coves.community.post/3kabc",
		"at://did:plc:x/social.coves.actor.profile/self",
		"at://did:plc:x/social.coves.community.comment",
		"at://did:plc:x",
		"https://example.com/x",
	} {
		t.Run(subject, func(t *testing.T) {
			reader := &fakeSubjectReader{}
			state, err := newSubjectStateTestService(reader).GetSubjectState(t.Context(), subject)
			assert.ErrorIs(t, err, moderation.ErrInvalidSubject)
			assert.Nil(t, state)
			assert.Empty(t, reader.calls, "invalid subjects must not reach the reader")
		})
	}
}

func TestGetSubjectStateNeverIndexed(t *testing.T) {
	uri := "at://did:plc:neverindexed/social.coves.community.postv2/3kabc"
	reader := &fakeSubjectReader{err: moderation.ErrSubjectNotIndexed}

	state, err := newSubjectStateTestService(reader).GetSubjectState(t.Context(), uri)
	require.NoError(t, err)
	assertInitialSubjectState(t, state, uri, moderation.RecordStateUnavailable, nil)
	assert.Equal(t, []string{uri}, reader.calls)
}

func TestGetSubjectStateIndexedPresent(t *testing.T) {
	for _, test := range []struct {
		name       string
		collection string
	}{
		{name: "comment", collection: moderation.CommentCollection},
		{name: "postv2", collection: moderation.PostV2Collection},
		{name: "legacy post", collection: moderation.LegacyPostCollection},
	} {
		t.Run(test.name, func(t *testing.T) {
			uri := "at://did:plc:x/" + test.collection + "/3kabc"
			cid := "bafy...distinct"
			reader := &fakeSubjectReader{record: &moderation.IndexedRecord{URI: uri, CID: cid}}

			state, err := newSubjectStateTestService(reader).GetSubjectState(t.Context(), uri)
			require.NoError(t, err)
			assertInitialSubjectState(t, state, uri, moderation.RecordStatePresent, &moderation.StrongRef{URI: uri, CID: cid})
			assert.Equal(t, []string{uri}, reader.calls)
		})
	}
}

func TestGetSubjectStateAuthorDeleted(t *testing.T) {
	uri := "at://did:plc:x/social.coves.community.comment/3kabc"
	reader := &fakeSubjectReader{record: &moderation.IndexedRecord{URI: uri, CID: "bafy...distinct", Deleted: true}}

	state, err := newSubjectStateTestService(reader).GetSubjectState(t.Context(), uri)
	require.NoError(t, err)
	assertInitialSubjectState(t, state, uri, moderation.RecordStateDeleted, nil)
	assert.Equal(t, []string{uri}, reader.calls)
}

func TestGetSubjectStateReaderFailure(t *testing.T) {
	uri := "at://did:plc:x/social.coves.community.postv2/3kabc"
	reader := &fakeSubjectReader{err: errors.New("db down")}

	state, err := newSubjectStateTestService(reader).GetSubjectState(t.Context(), uri)
	assert.ErrorIs(t, err, moderation.ErrModerationUnavailable)
	assert.Nil(t, state)
	assert.Equal(t, []string{uri}, reader.calls)
}

func TestGetSubjectStateNeverIndexedVersionIsStable(t *testing.T) {
	uri := "at://did:plc:neverindexed/social.coves.community.postv2/3kabc"
	reader := &fakeSubjectReader{err: moderation.ErrSubjectNotIndexed}
	service := newSubjectStateTestService(reader)

	first, err := service.GetSubjectState(t.Context(), uri)
	require.NoError(t, err)
	require.NotNil(t, first)
	second, err := service.GetSubjectState(t.Context(), uri)
	require.NoError(t, err)
	require.NotNil(t, second)
	assert.Equal(t, first.Version, second.Version)
	assert.Equal(t, []string{uri, uri}, reader.calls)
}
