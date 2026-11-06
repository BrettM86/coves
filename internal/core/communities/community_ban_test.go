package communities_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"Coves/internal/core/communities"
)

type communityBanKey struct{ community, subject string }

// fakeBanReader is an in-memory ban index keyed by (community, subject).
type fakeBanReader struct {
	bans map[communityBanKey]communities.CommunityBan
	err  error
}

func (f fakeBanReader) GetCommunityBan(_ context.Context, communityDID, subjectDID string) (*communities.CommunityBan, error) {
	if f.err != nil {
		return nil, f.err
	}
	ban, ok := f.bans[communityBanKey{communityDID, subjectDID}]
	if !ok {
		return nil, communities.ErrCommunityBanNotFound
	}
	return &ban, nil
}

func TestActiveCommunityBan(t *testing.T) {
	const (
		community      = "did:plc:bancommunity1"
		otherCommunity = "did:plc:bancommunity2"
		subject        = "did:plc:troublemaker456"
	)
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	at := func(instant time.Time) *time.Time { return &instant }
	banIn := func(communityDID string, expiresAt *time.Time) map[communityBanKey]communities.CommunityBan {
		return map[communityBanKey]communities.CommunityBan{
			{communityDID, subject}: {
				CommunityDID: communityDID,
				SubjectDID:   subject,
				RecordKey:    "a5uzm6rn7ba5hnqtdjrd22rg53skdtu6ksiksa5eini4ychhrcpa",
				RecordCID:    "bafyreib2rxk3rh6kzwq",
				Reason:       "spam",
				ExpiresAt:    expiresAt,
			},
		}
	}
	readerFailure := errors.New("ban index unavailable")

	tests := []struct {
		name       string
		reader     fakeBanReader
		wantBanned bool
		wantErr    error
	}{
		{name: "no ban", reader: fakeBanReader{}},
		{name: "permanent ban", reader: fakeBanReader{bans: banIn(community, nil)}, wantBanned: true},
		{name: "expires one second after now", reader: fakeBanReader{bans: banIn(community, at(now.Add(time.Second)))}, wantBanned: true},
		{name: "expires exactly at now", reader: fakeBanReader{bans: banIn(community, at(now))}},
		{name: "expired an hour ago", reader: fakeBanReader{bans: banIn(community, at(now.Add(-time.Hour)))}},
		{name: "ban only in another community", reader: fakeBanReader{bans: banIn(otherCommunity, nil)}},
		{name: "reader error", reader: fakeBanReader{err: readerFailure}, wantErr: readerFailure},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ban, err := communities.ActiveCommunityBan(context.Background(), tt.reader, community, subject, now)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				assert.Nil(t, ban)
				return
			}
			require.NoError(t, err)
			if !tt.wantBanned {
				assert.Nil(t, ban)
				return
			}
			require.NotNil(t, ban, "expected the ban to be in force")
			assert.Equal(t, community, ban.CommunityDID)
			assert.Equal(t, subject, ban.SubjectDID)
			assert.Equal(t, "spam", ban.Reason)
		})
	}
}

// The literal is the unpadded lowercase RFC 4648 base32 of SHA-256 over the
// DID string, computed outside this code:
// printf '%s' 'did:plc:troublemaker456' | openssl dgst -sha256 -binary | base32 | tr -d '=' | tr 'A-Z' 'a-z'
func TestBanRecordKey(t *testing.T) {
	assert.Equal(t, "a5uzm6rn7ba5hnqtdjrd22rg53skdtu6ksiksa5eini4ychhrcpa",
		communities.BanRecordKey("did:plc:troublemaker456"))
}
