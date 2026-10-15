package actor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"Coves/internal/core/comments"
	"Coves/internal/core/posts"
	"Coves/internal/core/users"
)

// scopedActorResolutions is the actor-identifier resolver both actor endpoints
// are wired to below. Any identifier it does not model is an infrastructure
// failure, so a handler that rewrites the actor before resolving it fails.
func scopedActorResolutions(_ context.Context, identifier string) (string, error) {
	switch identifier {
	case "Alice_B@lemmy.world":
		return "did:plc:alicebridged000000000000", nil
	case "Carol@example.social":
		return "", fmt.Errorf("resolve Carol@example.social: %w", users.ErrAmbiguousUser)
	case "nobody@lemmy.world":
		return "", fmt.Errorf("resolve nobody@lemmy.world: %w", users.ErrUserNotFound)
	case "alice@not a host":
		return "", &users.InvalidHandleError{Handle: identifier, Reason: "invalid host"}
	case "outage@lemmy.world":
		return "", fmt.Errorf("resolving actor outage@lemmy.world: %w",
			errors.New("pq: connection refused to db.internal:5432"))
	case "not found@lemmy.world":
		return "", fmt.Errorf("resolving actor not found@lemmy.world: %w",
			errors.New("connection refused"))
	}
	return "", fmt.Errorf("unmodeled actor identifier %q", identifier)
}

// legacyHandleResolution answers the handle-only resolver with a DID no row
// expects, so a handler still calling it fails on the outcome.
func legacyHandleResolution(context.Context, string) (string, error) {
	return "did:plc:legacypath000000000000", nil
}

// TestActorEndpoints_ResolveScopedActorIdentifier covers getPosts and
// getComments resolving `actor` through the actor-identifier resolver: a
// name@host actor reaches the content service as the resolved DID, and the
// resolver's ambiguity, not-found and validation errors keep distinct answers.
func TestActorEndpoints_ResolveScopedActorIdentifier(t *testing.T) {
	endpoints := []struct {
		name  string
		serve func(actor string) (rec *httptest.ResponseRecorder, receivedDID string)
	}{
		{
			name: "getPosts",
			serve: func(actor string) (*httptest.ResponseRecorder, string) {
				var receivedDID string
				postService := &mockPostService{
					getAuthorPostsFunc: func(_ context.Context, req posts.GetAuthorPostsRequest) (*posts.GetAuthorPostsResponse, error) {
						receivedDID = req.ActorDID
						return &posts.GetAuthorPostsResponse{Feed: []*posts.FeedViewPost{}}, nil
					},
				}
				userService := &mockUserService{
					resolveHandleToDIDFunc:          legacyHandleResolution,
					resolveActorIdentifierToDIDFunc: scopedActorResolutions,
				}
				handler := NewGetPostsHandler(postService, userService, &mockVoteService{}, &mockBlueskyService{})
				rec := httptest.NewRecorder()
				handler.HandleGetPosts(rec, httptest.NewRequest(http.MethodGet,
					"/xrpc/social.coves.actor.getPosts?actor="+url.QueryEscape(actor), nil))
				return rec, receivedDID
			},
		},
		{
			name: "getComments",
			serve: func(actor string) (*httptest.ResponseRecorder, string) {
				var receivedDID string
				commentService := &mockCommentService{
					getActorCommentsFunc: func(_ context.Context, req *comments.GetActorCommentsRequest) (*comments.GetActorCommentsResponse, error) {
						receivedDID = req.ActorDID
						return &comments.GetActorCommentsResponse{Comments: []*comments.CommentView{}}, nil
					},
				}
				userService := &mockUserServiceForComments{
					resolveHandleToDIDFunc:          legacyHandleResolution,
					resolveActorIdentifierToDIDFunc: scopedActorResolutions,
				}
				handler := NewGetCommentsHandler(commentService, userService, &mockVoteServiceForComments{})
				rec := httptest.NewRecorder()
				handler.HandleGetComments(rec, httptest.NewRequest(http.MethodGet,
					"/xrpc/social.coves.actor.getComments?actor="+url.QueryEscape(actor), nil))
				return rec, receivedDID
			},
		},
	}

	tests := []struct {
		name            string
		actor           string
		wantStatus      int
		wantError       string
		wantReceivedDID string
		// wantBodyExcludes is text the response body must not contain.
		wantBodyExcludes string
	}{
		{
			name:            "name@host reaches the service as the resolved DID",
			actor:           "Alice_B@lemmy.world",
			wantStatus:      http.StatusOK,
			wantReceivedDID: "did:plc:alicebridged000000000000",
		},
		{
			name:       "ambiguous address is a 409 AmbiguousActor",
			actor:      "Carol@example.social",
			wantStatus: http.StatusConflict,
			wantError:  "AmbiguousActor",
		},
		{
			name:       "unknown address is a 404 ActorNotFound",
			actor:      "nobody@lemmy.world",
			wantStatus: http.StatusNotFound,
			wantError:  "ActorNotFound",
		},
		{
			name:       "invalid address is a 400 InvalidRequest",
			actor:      "alice@not a host",
			wantStatus: http.StatusBadRequest,
			wantError:  "InvalidRequest",
		},
		{
			name:             "resolver outage is a 500 that does not leak the cause",
			actor:            "outage@lemmy.world",
			wantStatus:       http.StatusInternalServerError,
			wantError:        "InternalServerError",
			wantBodyExcludes: "db.internal",
		},
		{
			name:             "outage whose message contains the actor's \"not found\" is still a 500",
			actor:            "not found@lemmy.world",
			wantStatus:       http.StatusInternalServerError,
			wantError:        "InternalServerError",
			wantBodyExcludes: "connection refused",
		},
	}

	for _, endpoint := range endpoints {
		for _, tc := range tests {
			t.Run(endpoint.name+"/"+tc.name, func(t *testing.T) {
				rec, receivedDID := endpoint.serve(tc.actor)

				if rec.Code != tc.wantStatus {
					t.Fatalf("status = %d, want %d (body: %s)", rec.Code, tc.wantStatus, rec.Body.String())
				}
				if receivedDID != tc.wantReceivedDID {
					t.Errorf("service received actor DID %q, want %q", receivedDID, tc.wantReceivedDID)
				}
				if tc.wantBodyExcludes != "" && strings.Contains(rec.Body.String(), tc.wantBodyExcludes) {
					t.Errorf("response body leaks %q: %s", tc.wantBodyExcludes, rec.Body.String())
				}
				if tc.wantError == "" {
					return
				}
				var body ErrorResponse
				if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
					t.Fatalf("decode error body: %v", err)
				}
				if body.Error != tc.wantError {
					t.Errorf("error = %q, want %q", body.Error, tc.wantError)
				}
			})
		}
	}
}
