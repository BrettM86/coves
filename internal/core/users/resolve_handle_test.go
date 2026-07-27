package users

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"Coves/internal/atproto/identity"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// TestResolveHandleToDID_ClassifiesResolverErrors covers the translation from
// the identity resolver's typed errors into this package's vocabulary.
//
// Without it the API boundary cannot tell "no such handle" from "resolution
// broke", and the most common failure of a public profile lookup — a handle
// that does not exist — reads as a server fault. The repo lookup deliberately
// swallows its own ErrUserNotFound and falls through to external resolution, so
// the sentinel has to be reintroduced here or it never appears at all.
func TestResolveHandleToDID_ClassifiesResolverErrors(t *testing.T) {
	const handle = "ghost.example.com"

	tests := []struct {
		name     string
		resolver error
		assert   func(t *testing.T, err error)
	}{
		{
			name:     "unresolvable handle is a not-found",
			resolver: &identity.ErrNotFound{Identifier: handle, Reason: "no DNS TXT record"},
			assert: func(t *testing.T, err error) {
				assert.ErrorIs(t, err, ErrUserNotFound,
					"an absent handle must be matchable as ErrUserNotFound so the boundary answers 404")
				var notFound *identity.ErrNotFound
				assert.ErrorAs(t, err, &notFound, "the resolver's cause should stay reachable for logs")
			},
		},
		{
			name:     "malformed handle is a client error, not a not-found",
			resolver: &identity.ErrInvalidIdentifier{Identifier: "not a handle", Reason: "contains a space"},
			assert: func(t *testing.T, err error) {
				var invalidHandle *InvalidHandleError
				assert.ErrorAs(t, err, &invalidHandle)
				assert.NotErrorIs(t, err, ErrUserNotFound,
					"a malformed handle is a 400, not a 404")
			},
		},
		{
			name:     "infrastructure failure must NOT look like a missing user",
			resolver: &identity.ErrResolutionFailed{Identifier: handle, Reason: "PLC directory timeout"},
			assert: func(t *testing.T, err error) {
				assert.NotErrorIs(t, err, ErrUserNotFound,
					"reporting an outage as 404 hides it from 5xx alerting and misleads the caller")
				var invalidHandle *InvalidHandleError
				assert.NotErrorAs(t, err, &invalidHandle)
			},
		},
		{
			name:     "unclassified resolver error stays unclassified",
			resolver: errors.New("connection reset by peer"),
			assert: func(t *testing.T, err error) {
				assert.NotErrorIs(t, err, ErrUserNotFound)
				assert.ErrorContains(t, err, "connection reset by peer")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockRepo := new(MockUserRepository)
			mockResolver := new(MockIdentityResolver)

			// Not indexed locally, so resolution falls through to the resolver.
			mockRepo.On("GetByHandle", mock.Anything, mock.Anything).Return(nil, ErrUserNotFound)
			mockResolver.On("ResolveHandle", mock.Anything, mock.Anything).
				Return("", "", tt.resolver)

			service := NewUserService(mockRepo, mockResolver, "https://default.pds", nil, "")

			did, err := service.ResolveHandleToDID(context.Background(), handle)
			assert.Empty(t, did)
			assert.Error(t, err)
			tt.assert(t, err)
		})
	}
}

// A handle already indexed locally must not touch the resolver at all.
func TestResolveHandleToDID_LocalHit(t *testing.T) {
	mockRepo := new(MockUserRepository)
	mockResolver := new(MockIdentityResolver)

	mockRepo.On("GetByHandle", mock.Anything, "known.example.com").
		Return(&User{DID: "did:plc:known", Handle: "known.example.com"}, nil)

	service := NewUserService(mockRepo, mockResolver, "https://default.pds", nil, "")

	did, err := service.ResolveHandleToDID(context.Background(), "known.example.com")
	assert.NoError(t, err)
	assert.Equal(t, "did:plc:known", did)
	mockResolver.AssertNotCalled(t, "ResolveHandle", mock.Anything, mock.Anything)
}

// A transient database error must not stop external resolution — the local
// lookup is only a cache.
func TestResolveHandleToDID_DatabaseErrorFallsThrough(t *testing.T) {
	mockRepo := new(MockUserRepository)
	mockResolver := new(MockIdentityResolver)

	mockRepo.On("GetByHandle", mock.Anything, mock.Anything).
		Return(nil, fmt.Errorf("pq: too many connections"))
	mockResolver.On("ResolveHandle", mock.Anything, mock.Anything).
		Return("did:plc:resolved", "https://pds.example", nil)

	service := NewUserService(mockRepo, mockResolver, "https://default.pds", nil, "")

	did, err := service.ResolveHandleToDID(context.Background(), "someone.example.com")
	assert.NoError(t, err)
	assert.Equal(t, "did:plc:resolved", did)
}
