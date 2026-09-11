package pds

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"

	covesoauth "Coves/internal/atproto/oauth"
)

type sessionAuth struct {
	app       *oauth.ClientApp
	did       syntax.DID
	sessionID string
}

func (auth *sessionAuth) DoWithAuth(_ *http.Client, request *http.Request, endpoint syntax.NSID) (*http.Response, error) {
	var response *http.Response
	err := covesoauth.RunSessionOperation(request.Context(), auth.app, auth.did, auth.sessionID, func(session *oauth.ClientSession) error {
		var err error
		response, err = session.DoWithAuth(session.Client, request, endpoint)
		return err
	})
	if isDeadSession(err) {
		err = fmt.Errorf("OAuth session unavailable: %w: %w", ErrSessionExpired, err)
	} else if errors.Is(err, covesoauth.ErrSessionBusy) {
		err = fmt.Errorf("OAuth session held by another operation: %w: %w", ErrSessionBusy, err)
	}
	if err != nil && response != nil {
		_ = response.Body.Close()
		response = nil
	}
	return response, err
}

// A dead session is one whose stored row is gone, has just been invalidated by
// a rejected refresh grant, or holds credentials that can never sign a request.
func isDeadSession(err error) bool {
	return errors.Is(err, covesoauth.ErrRefreshRejected) ||
		errors.Is(err, covesoauth.ErrSessionNotFound) ||
		errors.Is(err, covesoauth.ErrSessionCorrupt)
}
