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
