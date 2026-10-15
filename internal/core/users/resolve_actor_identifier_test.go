package users

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"Coves/internal/atproto/identity"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// TestResolveActorIdentifierToDID_ScopedForm covers the name@host form: it is
// answered from the stored (origin_username, origin) pairs alone. The repo fake
// answers only the exact pairs below, so a service that lowercases the name or
// splits at the first @ gets ErrUserNotFound instead of the DID. No row may
// reach the identity resolver: an unknown bridged address is never looked up
// over the network. A name Postgres would reject (invalid UTF-8, a control
// character) or that ingest could never have stored (over 256 bytes) is a
// validation error before the repository is asked; a repository failure that
// is neither sentinel keeps its identity so it is never answered as a 404.
func TestResolveActorIdentifierToDID_ScopedForm(t *testing.T) {
	errConnectionRefused := errors.New("connection refused")
	nameAtTheIngestCap := strings.Repeat("a", 256)

	tests := []struct {
		name       string
		identifier string
		wantDID    string
		assertErr  func(t *testing.T, err error)
		// repoNotCalled asserts the repository was never asked.
		repoNotCalled bool
		// wantRepoUsername, when set, asserts the repository was asked for it.
		wantRepoUsername string
	}{
		{
			name:       "name@host resolves to the stored pair",
			identifier: "Alice_B@lemmy.world",
			wantDID:    "did:plc:alicebridged000000000000",
		},
		{
			name:       "leading @ and a mixed-case host resolve to the same pair",
			identifier: "@Alice_B@Lemmy.World",
			wantDID:    "did:plc:alicebridged000000000000",
		},
		{
			name:       "the name is trimmed",
			identifier: "Alice_B @lemmy.world",
			wantDID:    "did:plc:alicebridged000000000000",
		},
		{
			name:       "split at the last @",
			identifier: "Alice@Bridge@lemmy.world",
			wantDID:    "did:plc:aliceatbridge00000000000",
		},
		{
			name:       "unknown pair is ErrUserNotFound",
			identifier: "nobody@lemmy.world",
			assertErr: func(t *testing.T, err error) {
				assert.ErrorIs(t, err, ErrUserNotFound)
			},
		},
		{
			name:       "ambiguous pair is ErrAmbiguousUser, not ErrUserNotFound",
			identifier: "Carol@example.social",
			assertErr: func(t *testing.T, err error) {
				assert.ErrorIs(t, err, ErrAmbiguousUser)
				assert.NotErrorIs(t, err, ErrUserNotFound)
			},
		},
		{
			name:       "empty name is a validation error",
			identifier: "@lemmy.world",
			assertErr:  assertInvalidHandle,
		},
		{
			name:       "empty host is a validation error",
			identifier: "alice@",
			assertErr:  assertInvalidHandle,
		},
		{
			name:       "invalid host is a validation error",
			identifier: "alice@not a host",
			assertErr:  assertInvalidHandle,
		},
		{
			name:       "whitespace-only name is a validation error",
			identifier: "   @lemmy.world",
			assertErr:  assertInvalidHandle,
		},
		{
			name:       "whitespace-only name after stripping the leading @ is a validation error",
			identifier: "@ @lemmy.world",
			assertErr:  assertInvalidHandle,
		},
		{
			name:          "invalid UTF-8 in the name is a validation error",
			identifier:    "\xff@lemmy.world",
			assertErr:     assertInvalidHandle,
			repoNotCalled: true,
		},
		{
			name:          "a NUL in the name is a validation error",
			identifier:    "a\x00b@lemmy.world",
			assertErr:     assertInvalidHandle,
			repoNotCalled: true,
		},
		{
			name:          "a name over the 256-byte ingest cap is a validation error",
			identifier:    nameAtTheIngestCap + "a@lemmy.world",
			assertErr:     assertInvalidHandle,
			repoNotCalled: true,
		},
		{
			name:       "a name at the 256-byte ingest cap still reaches the repository",
			identifier: nameAtTheIngestCap + "@lemmy.world",
			assertErr: func(t *testing.T, err error) {
				assert.ErrorIs(t, err, ErrUserNotFound)
			},
			wantRepoUsername: nameAtTheIngestCap,
		},
		{
			name:       "a repository failure is passed through, not ErrUserNotFound",
			identifier: "Outage@lemmy.world",
			assertErr: func(t *testing.T, err error) {
				assert.ErrorIs(t, err, errConnectionRefused)
				assert.NotErrorIs(t, err, ErrUserNotFound, "a database outage is a 500, not a 404")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockRepo := new(MockUserRepository)
			mockResolver := new(MockIdentityResolver)

			mockRepo.On("GetDIDByOriginUsername", mock.Anything, "Alice_B", "lemmy.world").
				Return("did:plc:alicebridged000000000000", nil)
			mockRepo.On("GetDIDByOriginUsername", mock.Anything, "Alice@Bridge", "lemmy.world").
				Return("did:plc:aliceatbridge00000000000", nil)
			mockRepo.On("GetDIDByOriginUsername", mock.Anything, "Carol", "example.social").
				Return("", fmt.Errorf("two users share this address: %w", ErrAmbiguousUser))
			mockRepo.On("GetDIDByOriginUsername", mock.Anything, "Outage", "lemmy.world").
				Return("", errConnectionRefused)
			mockRepo.On("GetDIDByOriginUsername", mock.Anything, mock.Anything, mock.Anything).
				Return("", ErrUserNotFound)
			mockRepo.On("GetByHandle", mock.Anything, mock.Anything).Return(nil, ErrUserNotFound)
			mockResolver.On("ResolveHandle", mock.Anything, mock.Anything).
				Return("did:plc:networklookup00000000000", "https://pds.example", nil)

			service := NewUserService(mockRepo, mockResolver, "https://default.pds", nil, "")

			did, err := service.ResolveActorIdentifierToDID(context.Background(), tt.identifier)

			if tt.assertErr == nil {
				assert.NoError(t, err)
				assert.Equal(t, tt.wantDID, did)
			} else {
				assert.Error(t, err)
				assert.Empty(t, did)
				tt.assertErr(t, err)
			}
			assert.Empty(t, mockResolver.Calls, "a name@host identifier must never reach the identity resolver")
			if tt.repoNotCalled {
				mockRepo.AssertNotCalled(t, "GetDIDByOriginUsername", mock.Anything, mock.Anything, mock.Anything)
			}
			if tt.wantRepoUsername != "" {
				mockRepo.AssertCalled(t, "GetDIDByOriginUsername", mock.Anything, tt.wantRepoUsername, "lemmy.world")
			}
		})
	}
}

func assertInvalidHandle(t *testing.T, err error) {
	t.Helper()
	var invalidHandle *InvalidHandleError
	assert.ErrorAs(t, err, &invalidHandle)
	assert.NotErrorIs(t, err, ErrUserNotFound, "a malformed identifier is a 400, not a 404")
}

// TestResolveActorIdentifierToDID_DIDAndHandle covers the two forms that keep
// today's behaviour: a DID passes through untouched, and a handle takes the
// ResolveHandleToDID path (local index first, then the identity resolver).
func TestResolveActorIdentifierToDID_DIDAndHandle(t *testing.T) {
	tests := []struct {
		name           string
		identifier     string
		wantDID        string
		wantRepoHandle string
		resolverCalled bool
	}{
		{
			name:       "DID passes through with no lookups",
			identifier: "did:plc:abc123",
			wantDID:    "did:plc:abc123",
		},
		{
			name:           "indexed handle is normalized and answered locally",
			identifier:     "  Known.Example.COM ",
			wantDID:        "did:plc:knownindexed0000000000",
			wantRepoHandle: "known.example.com",
		},
		{
			name:           "unindexed handle falls through to the identity resolver",
			identifier:     "someone.example.com",
			wantDID:        "did:plc:resolvedexternal00000000",
			wantRepoHandle: "someone.example.com",
			resolverCalled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockRepo := new(MockUserRepository)
			mockResolver := new(MockIdentityResolver)

			mockRepo.On("GetByHandle", mock.Anything, "known.example.com").
				Return(&User{DID: "did:plc:knownindexed0000000000", Handle: "known.example.com"}, nil)
			mockRepo.On("GetByHandle", mock.Anything, mock.Anything).Return(nil, ErrUserNotFound)
			mockRepo.On("GetDIDByOriginUsername", mock.Anything, mock.Anything, mock.Anything).
				Return("", ErrUserNotFound)
			mockResolver.On("ResolveHandle", mock.Anything, "someone.example.com").
				Return("did:plc:resolvedexternal00000000", "https://pds.example", nil)
			mockResolver.On("ResolveHandle", mock.Anything, mock.Anything).
				Return("", "", &identity.ErrNotFound{Identifier: "unexpected", Reason: "not modeled"})

			service := NewUserService(mockRepo, mockResolver, "https://default.pds", nil, "")

			did, err := service.ResolveActorIdentifierToDID(context.Background(), tt.identifier)
			assert.NoError(t, err)
			assert.Equal(t, tt.wantDID, did)

			if tt.wantRepoHandle != "" {
				mockRepo.AssertCalled(t, "GetByHandle", mock.Anything, tt.wantRepoHandle)
			} else {
				assert.Empty(t, mockRepo.Calls, "a DID needs no repository lookup")
			}
			if tt.resolverCalled {
				mockResolver.AssertCalled(t, "ResolveHandle", mock.Anything, "someone.example.com")
			} else {
				assert.Empty(t, mockResolver.Calls, "the identity resolver must not be called")
			}
		})
	}
}
