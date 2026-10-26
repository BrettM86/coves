package routes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"testing"

	"Coves/internal/api/middleware"
	"Coves/internal/core/aggregators"
	"Coves/internal/core/media"
	"Coves/internal/core/posts"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/go-chi/chi/v5"
)

// uploadImageLexiconPath is the schema clients generate from, relative to this
// package's directory.
const uploadImageLexiconPath = "../../atproto/lexicon/social/coves/media/uploadImage.json"

// A request with no credentials never reaches the upload handler: the shared
// RequireAuth middleware answers it, with the error name that middleware uses
// for every route. That name is not the handler's own AuthRequired (sent for a
// session the user's PDS no longer accepts), so the lexicon has to declare it
// separately. This drives the real router and checks the answer against the
// lexicon's declared errors, so the schema and the route cannot drift apart.
func TestUploadImageRoute_NoCredentialsErrorIsDeclaredInTheLexicon(t *testing.T) {
	router := theRouter()

	request := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.media.uploadImage", nil)
	request.RemoteAddr = "192.0.2.201:1234"
	recorder := httptest.NewRecorder()
	router.mux.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("POST uploadImage without credentials: status %d, want %d; body %s",
			recorder.Code, http.StatusUnauthorized, recorder.Body.String())
	}

	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the 401 body %q: %v", recorder.Body.String(), err)
	}
	if body.Error == "" {
		t.Fatalf("the 401 body names no error: %s", recorder.Body.String())
	}

	declared := uploadImageDeclaredErrors(t)

	if !slices.Contains(declared, body.Error) {
		t.Errorf("POST uploadImage without credentials answers 401 %q, which "+
			"social.coves.media.uploadImage does not declare (declared: %v); a client "+
			"generated from the lexicon cannot recognise it", body.Error, declared)
	}
}

// uploadImageDeclaredErrors returns the error names the uploadImage lexicon
// declares.
func uploadImageDeclaredErrors(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(uploadImageLexiconPath)
	if err != nil {
		t.Fatalf("reading the uploadImage lexicon: %v", err)
	}
	var lexicon struct {
		Defs struct {
			Main struct {
				Errors []struct {
					Name string `json:"name"`
				} `json:"errors"`
			} `json:"main"`
		} `json:"defs"`
	}
	if err := json.Unmarshal(raw, &lexicon); err != nil {
		t.Fatalf("parsing the uploadImage lexicon: %v", err)
	}
	var declared []string
	for _, declaredError := range lexicon.Defs.Main.Errors {
		declared = append(declared, declaredError.Name)
	}
	return declared
}

const (
	uploadImageAggregatorDID = "did:plc:uploadimageroutebot"
	uploadImageAPIKey        = "ckapi_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// singleKeyValidator accepts exactly one API key, as the aggregator's.
type singleKeyValidator struct{}

func (singleKeyValidator) ValidateKey(_ context.Context, plainKey string) (string, error) {
	if plainKey != uploadImageAPIKey {
		return "", errors.New("unknown API key")
	}
	return uploadImageAggregatorDID, nil
}

func (singleKeyValidator) RefreshTokensIfNeeded(context.Context, string) error { return nil }

// deadSessionLoader records which DIDs it was asked for and reports every
// stored session as dead.
type deadSessionLoader struct{ requested []string }

func (l *deadSessionLoader) StoredSession(_ context.Context, aggregatorDID string) (*oauth.ClientSessionData, error) {
	l.requested = append(l.requested, aggregatorDID)
	return nil, fmt.Errorf("stored session: %w", aggregators.ErrOAuthSessionDead)
}

// An aggregator authenticating with an API key reaches the upload handler
// through the real DualAuthMiddleware, which hands it no OAuth session; the
// handler loads the aggregator's stored session by DID.
func TestUploadImageRoute_AcceptsAggregatorAPIKeys(t *testing.T) {
	rows := []struct {
		name          string
		authorization string
		status        int
		error         string
		wantRequested []string
	}{
		{"valid API key", "Bearer " + uploadImageAPIKey, http.StatusServiceUnavailable, "NoAuthorCredentials", []string{"did:plc:uploadimageroutebot"}},
		{"no credentials", "", http.StatusUnauthorized, "AuthenticationRequired", nil},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			pipeline, err := media.NewPipeline(media.Config{MaxConcurrent: 1, MaxLongEdge: media.DefaultMaxLongEdge})
			if err != nil {
				t.Fatalf("NewPipeline: %v", err)
			}
			repoFactory := func(_ context.Context, did string, _ *oauth.ClientSessionData) (posts.AuthorRepo, error) {
				t.Errorf("repo factory called for %s; a dead stored session must stop the upload first", did)
				return nil, errors.New("repo factory must not be called")
			}
			loader := &deadSessionLoader{}
			dualAuth := middleware.NewDualAuthMiddleware(nil, nil, nil, nil).WithAPIKeyValidator(singleKeyValidator{})
			router := chi.NewRouter()
			RegisterMediaRoutes(router, dualAuth, media.NewUploadService(pipeline, defaultUploadLimiter(t)), repoFactory, loader)

			request := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.media.uploadImage", nil)
			request.Header.Set("Content-Type", "image/jpeg")
			if row.authorization != "" {
				request.Header.Set("Authorization", row.authorization)
			}
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)

			var body struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(recorder.Body.Bytes(), &body)
			if recorder.Code != row.status || body.Error != row.error {
				t.Errorf("response: got %d error %q, want %d error %q (body %q)", recorder.Code, body.Error, row.status, row.error, recorder.Body.String())
			}
			if !slices.Equal(loader.requested, row.wantRequested) {
				t.Errorf("stored sessions requested: got %v, want %v", loader.requested, row.wantRequested)
			}
			if declared := uploadImageDeclaredErrors(t); !slices.Contains(declared, row.error) {
				t.Errorf("uploadImage answers %d %q, which social.coves.media.uploadImage does not declare (declared: %v)", row.status, row.error, declared)
			}
		})
	}
}

// defaultUploadLimiter builds a per-DID upload count limiter with the default cap.
func defaultUploadLimiter(t *testing.T) *media.UploadLimiter {
	t.Helper()
	limiter, err := media.NewUploadLimiter(media.UploadLimitConfig{UploadsPerTenMinutes: media.DefaultUploadsPerTenMinutes, BytesPerDay: 500000000, WarnFraction: 0.8})
	if err != nil {
		t.Fatalf("creating the upload limiter: %v", err)
	}
	return limiter
}

func TestUploadImageLexiconDeclaresUploadRateLimited(t *testing.T) {
	if declared := uploadImageDeclaredErrors(t); !slices.Contains(declared, "UploadRateLimited") {
		t.Fatalf("social.coves.media.uploadImage does not declare UploadRateLimited (declared: %v)", declared)
	}
}
