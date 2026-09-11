//go:build integration

package oauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"Coves/tests/testkit"
	indigooauth "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandleRefreshWaitsForExistingSessionOperation(t *testing.T) {
	db := testkit.DB(t)
	owner, data := coordinationClient(t, db, false)
	contender, _ := coordinationClient(t, db, true)
	handler := lifecycleHandler(contender)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	result := make(chan error, 1)
	go func() {
		result <- RunSessionOperation(ctx, owner, data.AccountDID, data.SessionID, func(*indigooauth.ClientSession) error {
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("owner never entered operation")
	}
	waiterContext, cancelWaiter := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancelWaiter()
	response := httptest.NewRecorder()
	handler.HandleRefresh(response, lifecycleRefreshRequest(t, handler, data, waiterContext))
	assert.GreaterOrEqual(t, response.Code, 500, "explicit refresh must wait for the same-session owner and report its canceled wait as retryable")
	assert.Less(t, response.Code, 600)
	cancel()
	require.ErrorIs(t, <-result, context.Canceled)
	saved, err := contender.Store.GetSession(context.Background(), data.AccountDID, data.SessionID)
	require.NoError(t, err)
	assert.True(t, saved.RefreshToken == "initial-refresh", "canceled explicit refresh must not rotate the owner's credentials")
}

func TestHandleLogoutCannotBeUndoneByDelayedSessionPersistence(t *testing.T) {
	db := testkit.DB(t)
	owner, data := coordinationClient(t, db, false)
	contender, _ := coordinationClient(t, db, true)
	handler := lifecycleHandler(contender)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	entered := make(chan struct{})
	release := make(chan struct{})
	ownerResult := make(chan error, 1)
	go func() {
		ownerResult <- RunSessionOperation(ctx, owner, data.AccountDID, data.SessionID, func(session *indigooauth.ClientSession) error {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			_, err := session.RefreshTokens(ctx)
			return err
		})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("owner never entered operation")
	}
	sealed, err := handler.client.SealSession(data.AccountDID.String(), data.SessionID, time.Hour)
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost, "/oauth/logout", nil).WithContext(ctx)
	request.AddCookie(&http.Cookie{Name: "coves_session", Value: sealed})
	response := httptest.NewRecorder()
	logoutDone := make(chan struct{})
	go func() { handler.HandleLogout(response, request); close(logoutDone) }()
	// The owner is deliberately held. A short bounded observation is sufficient
	// to catch an early completed logout; regardless of scheduling the final row
	// assertion proves that delayed persistence cannot resurrect its session.
	early := false
	select {
	case <-logoutDone:
		early = true
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	require.NoError(t, <-ownerResult)
	select {
	case <-logoutDone:
	case <-ctx.Done():
		t.Fatal("logout did not finish after the owner released the session")
	}
	assert.False(t, early, "logout must not report completion while an earlier operation can still persist the same session")
	require.Equal(t, http.StatusOK, response.Code)
	_, err = contender.Store.GetSession(context.Background(), data.AccountDID, data.SessionID)
	require.ErrorIs(t, err, ErrSessionNotFound, "delayed token persistence must not resurrect a logged-out session")
}
