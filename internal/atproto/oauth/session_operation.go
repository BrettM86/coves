package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

// ErrRefreshRejected means the authorization server definitively rejected the
// refresh grant and the specific stored session has been invalidated.
var ErrRefreshRejected = errors.New("OAuth refresh grant rejected")

// ErrSessionCorrupt means the stored session's DPoP private key cannot be
// parsed, so no request could ever be signed with it. The row has been
// deleted; the user must sign in again.
var ErrSessionCorrupt = errors.New("OAuth session credentials corrupt")

// ErrSessionMoveNotUndone means MoveSession's commit (or the move itself)
// failed and the move could not be undone, so the stored sessions are not as
// they were before the call.
var ErrSessionMoveNotUndone = errors.New("the OAuth session move could not be undone")

// Rotated credentials must be written even when the request that triggered
// the rotation has been abandoned: the authorization server has already
// consumed the previous refresh token.
const sessionPersistenceTimeout = 5 * time.Second

// RunSessionOperation reloads credentials for one operation. Its transport
// refuses further requests if Indigo's otherwise fire-and-forget persistence
// callback fails, so a rotated token cannot authorize a write before it is saved.
func RunSessionOperation(ctx context.Context, app *oauth.ClientApp, did syntax.DID, sessionID string, operation func(*oauth.ClientSession) error) error {
	if coordinator, ok := app.Store.(sessionOperationCoordinator); ok {
		return coordinator.withSessionOperation(ctx, did, sessionID, func(ctx context.Context) error {
			return runSessionOperation(ctx, app, did, sessionID, operation)
		})
	}
	return runSessionOperation(ctx, app, did, sessionID, operation)
}

// MoveSession renames a stored session from fromSessionID to toSessionID,
// replacing any session already stored under toSessionID, and then runs commit
// with the moved credentials. The source row is reloaded under the
// coordination of both sessions (locked in from-then-to order), so a refresh
// in flight on either finishes before the move reads the row. A missing source
// row is returned as the store reports it, with nothing written.
//
// If the move or commit fails, the move is undone: the source row is put back
// and the destination gets back the session it held before, if any, and the
// failure is returned. With the Postgres store the rename and its undo are
// each one transaction, so the credentials are never held by two rows. If the
// undo fails, the returned error also wraps ErrSessionMoveNotUndone.
func MoveSession(ctx context.Context, app *oauth.ClientApp, did syntax.DID, fromSessionID, toSessionID string, commit func(context.Context, *oauth.ClientSessionData) error) error {
	if fromSessionID == toSessionID {
		return fmt.Errorf("move OAuth session: source and destination are both %q", fromSessionID)
	}
	coordinator, ok := app.Store.(sessionOperationCoordinator)
	if !ok {
		return moveSession(ctx, app, did, fromSessionID, toSessionID, commit)
	}
	return coordinator.withSessionOperation(ctx, did, fromSessionID, func(ctx context.Context) error {
		return coordinator.withSessionOperation(ctx, did, toSessionID, func(ctx context.Context) error {
			return moveSession(ctx, app, did, fromSessionID, toSessionID, commit)
		})
	})
}

// sessionRenamer is a store that can rename a session row atomically.
// renameSession returns the session the destination held before (nil if
// none); undoSessionRename reverses the rename and restores that session.
type sessionRenamer interface {
	renameSession(ctx context.Context, did syntax.DID, fromSessionID, toSessionID string) (previousDestination *oauth.ClientSessionData, err error)
	undoSessionRename(ctx context.Context, did syntax.DID, fromSessionID, toSessionID string, previousDestination *oauth.ClientSessionData) error
}

func storeSessionRenamer(store oauth.ClientAuthStore) (sessionRenamer, bool) {
	if wrapper, ok := store.(*MobileAwareStoreWrapper); ok {
		store = wrapper.ClientAuthStore
	}
	renamer, ok := store.(sessionRenamer)
	return renamer, ok
}

func moveSession(ctx context.Context, app *oauth.ClientApp, did syntax.DID, fromSessionID, toSessionID string, commit func(context.Context, *oauth.ClientSessionData) error) error {
	original, err := app.Store.GetSession(ctx, did, fromSessionID)
	if err != nil {
		return err
	}
	if original == nil {
		return ErrSessionNotFound
	}
	moved := *original
	moved.SessionID = toSessionID

	if renamer, ok := storeSessionRenamer(app.Store); ok {
		previous, err := renamer.renameSession(ctx, did, fromSessionID, toSessionID)
		if err != nil {
			return fmt.Errorf("move OAuth session: %w", err)
		}
		if err := commit(ctx, &moved); err != nil {
			undoContext, cancel := persistenceContext(ctx)
			defer cancel()
			if undoErr := renamer.undoSessionRename(undoContext, did, fromSessionID, toSessionID, previous); undoErr != nil {
				return errors.Join(err, fmt.Errorf("%w: %w", ErrSessionMoveNotUndone, undoErr))
			}
			return err
		}
		return nil
	}

	previous, err := app.Store.GetSession(ctx, did, toSessionID)
	if errors.Is(err, ErrSessionNotFound) {
		previous = nil
	} else if err != nil {
		return fmt.Errorf("read OAuth session move destination: %w", err)
	}
	if err := app.Store.SaveSession(ctx, moved); err != nil {
		return fmt.Errorf("save moved OAuth session: %w", err)
	}
	if err := app.Store.DeleteSession(ctx, did, fromSessionID); err != nil {
		return errors.Join(fmt.Errorf("delete moved OAuth session source: %w", err),
			restoreMovedSession(ctx, app, did, *original, toSessionID, previous))
	}
	if err := commit(ctx, &moved); err != nil {
		return errors.Join(err, restoreMovedSession(ctx, app, did, *original, toSessionID, previous))
	}
	return nil
}

// restoreMovedSession undoes a move on a store that cannot rename atomically.
// It puts the source row back first and only then restores the destination
// (its previous session, or no row), so a failure leaves the credentials in
// at least one row. Any failure is returned wrapped in ErrSessionMoveNotUndone.
func restoreMovedSession(ctx context.Context, app *oauth.ClientApp, did syntax.DID, original oauth.ClientSessionData, toSessionID string, previous *oauth.ClientSessionData) error {
	restoreContext, cancel := persistenceContext(ctx)
	defer cancel()
	if err := app.Store.SaveSession(restoreContext, original); err != nil {
		return fmt.Errorf("%w: restore OAuth session source: %w", ErrSessionMoveNotUndone, err)
	}
	if previous != nil {
		if err := app.Store.SaveSession(restoreContext, *previous); err != nil {
			return fmt.Errorf("%w: restore OAuth session destination: %w", ErrSessionMoveNotUndone, err)
		}
		return nil
	}
	if err := app.Store.DeleteSession(restoreContext, did, toSessionID); err != nil && !errors.Is(err, ErrSessionNotFound) {
		return fmt.Errorf("%w: remove OAuth session destination: %w", ErrSessionMoveNotUndone, err)
	}
	return nil
}

// ResumeSession rebuilds a stored session's DPoP signer without coordinating
// with other operations on it. Store failures are returned as they are, so a
// database outage stays retryable, while a key that cannot be parsed deletes
// that one row and reports ErrSessionCorrupt. Anything that may refresh tokens
// must go through RunSessionOperation instead.
func ResumeSession(ctx context.Context, app *oauth.ClientApp, did syntax.DID, sessionID string) (*oauth.ClientSession, error) {
	data, err := app.Store.GetSession(ctx, did, sessionID)
	if err != nil {
		return nil, err
	}
	privateKey, err := atcrypto.ParsePrivateMultibase(data.DPoPPrivateKeyMultibase)
	if err != nil {
		deleteContext, cancel := persistenceContext(ctx)
		defer cancel()
		if deleteErr := app.Store.DeleteSession(deleteContext, did, sessionID); deleteErr != nil {
			return nil, fmt.Errorf("invalidate corrupt OAuth session: %w", deleteErr)
		}
		slog.Error("deleted OAuth session with unparseable DPoP key", "did", did, "session_id", sessionID, "error", err)
		return nil, ErrSessionCorrupt
	}
	return &oauth.ClientSession{Client: app.Client, Config: app.Config, Data: data, DPoPPrivateKey: privateKey}, nil
}

func runSessionOperation(ctx context.Context, app *oauth.ClientApp, did syntax.DID, sessionID string, operation func(*oauth.ClientSession) error) error {
	session, err := ResumeSession(ctx, app, did, sessionID)
	if err != nil {
		return err
	}
	transport := &sessionOperationTransport{base: app.Client.Transport, tokenEndpoint: session.Data.AuthServerTokenEndpoint}
	if transport.base == nil {
		transport.base = http.DefaultTransport // coves:allow-bare-client: preserve the supplied ClientApp client's nil-transport behavior; production installs its guarded transport before this boundary
	}
	client := *app.Client
	client.Transport = transport
	session.Client = &client
	loadedRefreshToken := session.Data.RefreshToken
	session.PersistSessionCallback = func(_ context.Context, data *oauth.ClientSessionData) {
		if transport.persistenceError != nil {
			return
		}
		persistContext, cancel := persistenceContext(ctx)
		defer cancel()
		err := app.Store.SaveSession(persistContext, *data)
		if err == nil {
			return
		}
		transport.persistenceError = fmt.Errorf("persist OAuth session: %w", err)
		if data.RefreshToken != loadedRefreshToken {
			slog.Error("rotated OAuth credentials were not persisted; the session cannot be refreshed again", "did", did, "session_id", sessionID, "error", err)
		} else {
			slog.Warn("OAuth session nonce update was not persisted", "did", did, "session_id", sessionID, "error", err)
		}
	}
	err = operation(session)
	if transport.persistenceError != nil {
		return errors.Join(transport.persistenceError, err)
	}
	if errors.Is(err, ErrRefreshRejected) {
		deleteContext, cancel := persistenceContext(ctx)
		defer cancel()
		if deleteErr := app.Store.DeleteSession(deleteContext, did, sessionID); deleteErr != nil {
			return fmt.Errorf("invalidate OAuth session: %w", deleteErr)
		}
	}
	return err
}

// The returned context keeps ctx's values (including a coordinated session
// connection) but not its cancellation.
func persistenceContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), sessionPersistenceTimeout)
}

// Indigo flattens token endpoint errors to strings (including the nonce path).
// Inspect that boundary while the structured response is still available. Never
// pass an upstream error body to Indigo's body-logging error parser.
type sessionOperationTransport struct {
	base             http.RoundTripper
	tokenEndpoint    string
	persistenceError error
}

const maximumOAuthErrorBytes = 64 * 1024

func (transport *sessionOperationTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if transport.persistenceError != nil {
		return nil, transport.persistenceError
	}
	refresh := isRefreshRequest(request, transport.tokenEndpoint)
	response, err := transport.base.RoundTrip(request) // coves:allow-bare-client: delegate to the supplied ClientApp transport, retaining its SSRF guard
	if err != nil || !refresh || response.StatusCode < 400 {
		return response, err
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, maximumOAuthErrorBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read OAuth refresh response: %w", err)
	}
	if len(body) > maximumOAuthErrorBytes {
		return nil, errors.New("OAuth refresh error response exceeds limit")
	}
	var failure struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &failure); err != nil {
		return nil, errors.New("malformed OAuth refresh error response")
	}
	if response.StatusCode == http.StatusBadRequest && failure.Error == "invalid_grant" {
		return nil, ErrRefreshRejected
	}
	if response.StatusCode == http.StatusBadRequest && failure.Error == "use_dpop_nonce" && response.Header.Get("DPoP-Nonce") != "" {
		response.Body = io.NopCloser(bytes.NewBufferString(`{"error":"use_dpop_nonce"}`))
		return response, nil
	}
	return nil, fmt.Errorf("OAuth refresh failed (HTTP %d)", response.StatusCode)
}

func isRefreshRequest(request *http.Request, tokenEndpoint string) bool {
	if request.Method != http.MethodPost || request.URL.String() != tokenEndpoint || request.GetBody == nil {
		return false
	}
	body, err := request.GetBody()
	if err != nil {
		return false
	}
	defer func() { _ = body.Close() }()
	encoded, err := io.ReadAll(io.LimitReader(body, maximumOAuthErrorBytes+1))
	if err != nil || len(encoded) > maximumOAuthErrorBytes {
		return false
	}
	values, err := url.ParseQuery(string(encoded))
	return err == nil && values.Get("grant_type") == "refresh_token"
}
