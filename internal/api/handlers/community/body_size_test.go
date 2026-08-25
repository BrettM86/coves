package community

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"Coves/internal/api/middleware"
	"Coves/internal/api/reqbody"
	"Coves/internal/core/communities"
)

// Create and update are the two community endpoints that carry inline base64
// image bytes (avatarBlob/bannerBlob, up to 1MB + 2MB raw per the lexicon).
// These tests pin both sides of the image tier: a realistic avatar upload
// must decode (the regression a text-sized cap silently introduced — caught
// in review, since no test crossed the HTTP decode path with a blob), and a
// body over the tier must 413 without reaching the service.

// realisticAvatar is comfortably above the 100 KiB text tier that would have
// rejected it, and far below the image tier.
const realisticAvatarSize = 500_000

func postCommunity(t *testing.T, handler http.HandlerFunc, path string, payload map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshaling request payload: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserDIDKey, "did:plc:author"))
	w := httptest.NewRecorder()
	handler(w, req)
	return w
}

func TestCreateHandler_AcceptsARealisticAvatarUpload(t *testing.T) {
	t.Parallel()

	var received communities.CreateCommunityRequest
	handler := NewCreateHandler(&mockCommunityService{
		createFunc: func(_ context.Context, req communities.CreateCommunityRequest) (*communities.Community, error) {
			received = req
			return &communities.Community{DID: "did:plc:test", Handle: "c-test.coves.social"}, nil
		},
	}, nil)

	rec := postCommunity(t, handler.HandleCreate, "/xrpc/social.coves.community.create", map[string]any{
		"name":           "photography",
		"description":    "a community with an avatar",
		"visibility":     "public",
		"avatarBlob":     bytes.Repeat([]byte{0xAB}, realisticAvatarSize),
		"avatarMimeType": "image/png",
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("a %dKB avatar upload must decode, got %d: %.200s",
			realisticAvatarSize/1000, rec.Code, rec.Body.String())
	}
	if len(received.AvatarBlob) != realisticAvatarSize {
		t.Fatalf("service received %d avatar bytes, want %d", len(received.AvatarBlob), realisticAvatarSize)
	}
}

func TestCreateHandler_OversizedBodyIsRefusedBeforeTheService(t *testing.T) {
	t.Parallel()

	handler := NewCreateHandler(&mockCommunityService{
		createFunc: func(context.Context, communities.CreateCommunityRequest) (*communities.Community, error) {
			t.Error("the service was called for a body over the image tier")
			return nil, nil
		},
	}, nil)

	// 8MB raw becomes ~10.7MB of base64 — past LimitImage.
	rec := postCommunity(t, handler.HandleCreate, "/xrpc/social.coves.community.create", map[string]any{
		"name":       "toolarge",
		"avatarBlob": bytes.Repeat([]byte{0xAB}, 8_000_000),
	})

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	var resp struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Error != "PayloadTooLarge" {
		t.Fatalf("error code = %q (unmarshal err %v), want PayloadTooLarge", resp.Error, err)
	}
}

// updateBodyService lets the update tests observe whether the service was
// reached; every other method panics via the embedded nil-safe mock.
type updateBodyService struct {
	mockCommunityService
	updateFunc func(ctx context.Context, req communities.UpdateCommunityRequest) (*communities.Community, error)
}

func (s *updateBodyService) UpdateCommunity(ctx context.Context, req communities.UpdateCommunityRequest) (*communities.Community, error) {
	return s.updateFunc(ctx, req)
}

func TestUpdateHandler_AcceptsARealisticAvatarUpload(t *testing.T) {
	t.Parallel()

	var received communities.UpdateCommunityRequest
	handler := NewUpdateHandler(&updateBodyService{
		updateFunc: func(_ context.Context, req communities.UpdateCommunityRequest) (*communities.Community, error) {
			received = req
			return &communities.Community{DID: "did:plc:test", Handle: "c-test.coves.social"}, nil
		},
	})

	rec := postCommunity(t, handler.HandleUpdate, "/xrpc/social.coves.community.update", map[string]any{
		"communityDid":   "did:plc:test",
		"avatarBlob":     bytes.Repeat([]byte{0xCD}, realisticAvatarSize),
		"avatarMimeType": "image/png",
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("a %dKB avatar update must decode, got %d: %.200s",
			realisticAvatarSize/1000, rec.Code, rec.Body.String())
	}
	if len(received.AvatarBlob) != realisticAvatarSize {
		t.Fatalf("service received %d avatar bytes, want %d", len(received.AvatarBlob), realisticAvatarSize)
	}
}

func TestUpdateHandler_OversizedBodyIsRefusedBeforeTheService(t *testing.T) {
	t.Parallel()

	handler := NewUpdateHandler(&updateBodyService{
		updateFunc: func(context.Context, communities.UpdateCommunityRequest) (*communities.Community, error) {
			t.Error("the service was called for a body over the image tier")
			return nil, nil
		},
	})

	rec := postCommunity(t, handler.HandleUpdate, "/xrpc/social.coves.community.update", map[string]any{
		"communityDid": "did:plc:test",
		"avatarBlob":   bytes.Repeat([]byte{0xCD}, 8_000_000),
	})

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}

// TestCommunityBodiesFitTheTier documents WHY these two handlers sit on the
// image tier: the lexicon's own maxima (1MB avatar + 2MB banner, base64) must
// fit under it with room for the text fields.
func TestCommunityBodiesFitTheTier(t *testing.T) {
	t.Parallel()
	const base64Factor = 4.0 / 3.0
	lexiconMax := int64(float64(communities.MaxAvatarBlobSize+communities.MaxBannerBlobSize)*base64Factor) + 64_000
	if int64(reqbody.LimitImage) < lexiconMax {
		t.Fatalf("LimitImage (%d) cannot fit the lexicon's maximum community payload (~%d)",
			int64(reqbody.LimitImage), lexiconMax)
	}
}
