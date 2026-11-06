package user

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"Coves/internal/atproto/pds"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
)

// detachedWriteTimeout bounds a local write that outlives the request.
const detachedWriteTimeout = 5 * time.Second

// detachedWriteContext returns a context for a local write a client disconnect
// must not cancel: it keeps ctx's values, drops its cancellation and deadline,
// and expires after detachedWriteTimeout.
func detachedWriteContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), detachedWriteTimeout)
}

// profileChanges are the fields a profile write sets on the standing
// social.coves.actor.profile record. nil leaves a field as it is.
type profileChanges struct {
	DisplayName *string
	Description *string
	Avatar      map[string]interface{}
	Banner      map[string]interface{}
}

// newPDSClient builds the PDS client for a request: the custom factory when one
// was provided (e.g. E2E tests with password auth), otherwise OAuth with DPoP.
func newPDSClient(ctx context.Context, factory PDSClientFactory, oauthClient *oauth.ClientApp, session *oauth.ClientSessionData) (pds.CommitClient, error) {
	if factory != nil {
		return factory(ctx, session)
	}
	if oauthClient == nil {
		return nil, fmt.Errorf("OAuth client not configured")
	}
	return pds.NewFromOAuthSession(ctx, oauthClient, session)
}

// standingProfileRecord interprets the result of reading the user's profile
// record: a missing record is (nil, nil), any other failure is returned.
func standingProfileRecord(existing *pds.RecordResponse, err error) (*pds.RecordResponse, error) {
	if errors.Is(err, pds.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return existing, nil
}

// mergeProfileRecord builds the record to write from the standing record and
// the requested changes. Every key of the standing record the changes do not
// touch is kept, and the returned swap CID guards the write so concurrent
// editors cannot silently overwrite each other. A changed description drops
// descriptionFacets, whose byte offsets no longer apply.
func mergeProfileRecord(existing *pds.RecordResponse, changes profileChanges) (map[string]interface{}, string) {
	profile := map[string]interface{}{
		"$type": CovesProfileCollection,
	}
	swapRecord := ""
	if existing != nil {
		for key, value := range existing.Value {
			profile[key] = value
		}
		profile["$type"] = CovesProfileCollection
		swapRecord = existing.CID
	}

	if changes.DisplayName != nil {
		profile["displayName"] = *changes.DisplayName
	}
	if changes.Description != nil {
		existingDescription, hadDescription := profile["description"].(string)
		if !hadDescription || existingDescription != *changes.Description {
			delete(profile, "descriptionFacets")
		}
		profile["description"] = *changes.Description
	}
	if changes.Avatar != nil {
		profile["avatar"] = changes.Avatar
	}
	if changes.Banner != nil {
		profile["banner"] = changes.Banner
	}
	return profile, swapRecord
}

// commitProfileRecord writes the profile record with putRecord guarded by
// swapRecord, then writes it through to the users table when an indexer is
// set. On failure it writes the error response and returns false.
func commitProfileRecord(
	ctx context.Context,
	w http.ResponseWriter,
	pdsClient pds.CommitClient,
	indexer ProfileIndexer,
	userDID, hostURL string,
	profile map[string]interface{},
	swapRecord string,
) (*pds.RecordCommit, bool) {
	commit, err := pdsClient.PutRecordWithCommit(ctx, CovesProfileCollection, "self", profile, swapRecord)
	if err != nil {
		if !errors.Is(err, pds.ErrNoCommit) || commit == nil || commit.URI == "" || commit.CID == "" {
			slog.Error("failed to put profile record to PDS",
				slog.String("did", userDID),
				slog.String("pds_url", hostURL),
				slog.String("error", err.Error()),
			)
			putProfileMapper.Write(w, err)
			return nil, false
		}
	}

	// Write the committed record through to the users table so the user's
	// next read sees it. Jetstream may re-apply the record or skip it as already
	// applied (its recency guard compares against users.updated_at), so the
	// write-through mirrors the whole record the way the consumer does. A
	// failure is logged and not returned: the record is already on the PDS, and
	// Jetstream will index it. The record is committed, so a client disconnecting
	// now must not cancel the write-through.
	if indexer != nil {
		indexCtx, cancel := detachedWriteContext(ctx)
		defer cancel()
		if _, indexErr := indexer.UpdateProfile(indexCtx, userDID, profileIndexInput(profile)); indexErr != nil {
			slog.Warn("failed to write profile update through to the users table; Jetstream will index it",
				slog.String("did", userDID),
				slog.String("error", indexErr.Error()),
			)
		}
	}
	return commit, true
}
