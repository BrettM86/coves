package communities_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"Coves/internal/core/blobs"
	"Coves/internal/core/communities"
	"Coves/internal/core/media"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

const mediaCommunityDID = "did:plc:mediafixturecommunity"

type mediaPDSRequest struct {
	path string
	body map[string]any
}

type mediaBlobUpload struct {
	owner    blobs.BlobOwner
	data     []byte
	mimeType string
}

type recordingMediaBlobs struct {
	uploads []mediaBlobUpload
}

func (b *recordingMediaBlobs) UploadBlob(_ context.Context, owner blobs.BlobOwner, data []byte, mimeType string) (*blobs.BlobRef, error) {
	b.uploads = append(b.uploads, mediaBlobUpload{owner: owner, data: append([]byte(nil), data...), mimeType: mimeType})
	return &blobs.BlobRef{
		Type: "blob", Ref: map[string]string{"$link": "bafytestimage"},
		MimeType: mimeType, Size: len(data),
	}, nil
}

func (b *recordingMediaBlobs) FetchImageForURL(context.Context, string) ([]byte, error) {
	panic("community media test must not fetch a URL")
}

type communityMediaHarness struct {
	service  communities.Service
	repo     *fakeCommunityRepo
	blobs    *recordingMediaBlobs
	requests []mediaPDSRequest
	mu       sync.Mutex
	stored   *communities.Community
}

func newCommunityMediaHarness(t *testing.T) *communityMediaHarness {
	t.Helper()
	h := &communityMediaHarness{repo: newFakeCommunityRepo(), blobs: &recordingMediaBlobs{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/xrpc/com.atproto.repo.getRecord" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"uri":   "at://" + mediaCommunityDID + "/social.coves.community.profile/self",
				"cid":   "bafytestrecord",
				"value": map[string]any{"$type": "social.coves.community.profile"},
			})
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode PDS %s: %v", r.URL.Path, err)
			http.Error(w, "bad JSON", http.StatusBadRequest)
			return
		}
		h.mu.Lock()
		h.requests = append(h.requests, mediaPDSRequest{path: r.URL.Path, body: body})
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/xrpc/com.atproto.server.createAccount":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"did": mediaCommunityDID, "handle": "c-gardening." + fakeInstanceDomain,
				"accessJwt": mediaPipelineJWT(t), "refreshJwt": "refresh-token",
			})
		case "/xrpc/com.atproto.repo.createRecord", "/xrpc/com.atproto.repo.putRecord":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"uri": "at://" + mediaCommunityDID + "/social.coves.community.profile/self", "cid": "bafytestrecord",
			})
		default:
			t.Errorf("unexpected PDS request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	pipeline, err := media.NewPipeline(media.Config{MaxConcurrent: 2, MaxLongEdge: media.DefaultMaxLongEdge})
	require.NoError(t, err)
	h.service = communities.NewCommunityService(
		h.repo, server.URL, fakeInstanceDID, fakeInstanceDomain,
		communities.NewPDSAccountProvisioner(fakeInstanceDomain, server.URL, communities.PrivateHostOptions(true)...),
		nil, h.blobs, pipeline, communities.PrivateHostOptions(true)...,
	)
	h.stored = h.repo.seed(&communities.Community{
		DID: mediaCommunityDID, Handle: "c-gardening." + fakeInstanceDomain,
		Name: "gardening", OwnerDID: mediaCommunityDID, CreatedByDID: aValidCreateRequest().CreatedByDID,
		HostedByDID: fakeInstanceDID, PDSAccessToken: mediaPipelineJWT(t), PDSURL: server.URL,
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	return h
}

// This is untagged: the other JWT fixture lives in an integration-only test file.
func mediaPipelineJWT(t *testing.T) string {
	t.Helper()
	claims, err := json.Marshal(map[string]any{"exp": time.Now().Add(24 * time.Hour).Unix()})
	require.NoError(t, err)
	return base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." +
		base64.RawURLEncoding.EncodeToString(claims) + ".signature"
}

func (h *communityMediaHarness) requestsAt(path string) []mediaPDSRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	var requests []mediaPDSRequest
	for _, request := range h.requests {
		if request.path == path {
			requests = append(requests, request)
		}
	}
	return requests
}

func (h *communityMediaHarness) writeImage(ctx context.Context, operation string, avatar, banner []byte) error {
	if operation == "create" {
		req := aValidCreateRequest()
		req.AvatarBlob, req.BannerBlob = avatar, banner
		req.AvatarMimeType, req.BannerMimeType = "image/png", "image/png"
		_, err := h.service.CreateCommunity(ctx, req)
		return err
	}
	_, err := h.service.UpdateCommunity(ctx, communities.UpdateCommunityRequest{
		CommunityDID: mediaCommunityDID, UpdatedByDID: h.stored.CreatedByDID,
		AvatarBlob: avatar, BannerBlob: banner, AvatarMimeType: "image/png", BannerMimeType: "image/png",
	})
	return err
}

func jpegWithGPS(t *testing.T) (source, stripped []byte) {
	t.Helper()
	pixels := image.NewRGBA(image.Rect(0, 0, 32, 24))
	for y := 0; y < 24; y++ {
		for x := 0; x < 32; x++ {
			pixels.SetRGBA(x, y, color.RGBA{R: uint8(x * 7), G: uint8(y * 9), B: 80, A: 255})
		}
	}
	var encoded bytes.Buffer
	require.NoError(t, jpeg.Encode(&encoded, pixels, nil))
	stripped = encoded.Bytes()
	return testkit.JPEGWithSegments(stripped, testkit.JPEGExifGPSegment(1)), stripped
}

func gifImage(t *testing.T) []byte {
	t.Helper()
	var encoded bytes.Buffer
	require.NoError(t, gif.Encode(&encoded, image.NewPaletted(image.Rect(0, 0, 2, 2), []color.Color{color.Black}), nil))
	return encoded.Bytes()
}

// The input is small because of low-quality JPEG compression; the independently
// rotated, quality-92 re-encode has to exceed the lexicon's output-size cap.
func expandingOrientedJPEG(t *testing.T, width, height, capBytes int) []byte {
	t.Helper()
	pixels := image.NewRGBA(image.Rect(0, 0, width, height))
	random := rand.New(rand.NewSource(42))
	_, err := random.Read(pixels.Pix)
	require.NoError(t, err)
	for i := 3; i < len(pixels.Pix); i += 4 {
		pixels.Pix[i] = 255
	}
	var compressed bytes.Buffer
	require.NoError(t, jpeg.Encode(&compressed, pixels, &jpeg.Options{Quality: 20}))
	source := testkit.JPEGWithSegments(compressed.Bytes(), testkit.JPEGExifGPSegment(6))
	require.Less(t, len(source), capBytes, "source must be below the input cap")
	decoded, err := jpeg.Decode(bytes.NewReader(compressed.Bytes()))
	require.NoError(t, err)
	rotated := image.NewRGBA(image.Rect(0, 0, height, width))
	for y := 0; y < width; y++ {
		for x := 0; x < height; x++ {
			rotated.Set(x, y, decoded.At(y, height-1-x))
		}
	}
	var expanded bytes.Buffer
	require.NoError(t, jpeg.Encode(&expanded, rotated, &jpeg.Options{Quality: 92}))
	require.Greater(t, expanded.Len(), capBytes+100_000, "oriented output must exceed the cap with margin")
	return source
}

func TestCommunityMedia_UploadsSanitizedBytesWithSniffedMIME(t *testing.T) {
	source, expected := jpegWithGPS(t)
	for _, tc := range []struct{ operation, field string }{
		{"create", "avatar"},
		{"create", "banner"},
		{"update", "avatar"},
		{"update", "banner"},
	} {
		t.Run(tc.operation+"/"+tc.field, func(t *testing.T) {
			h := newCommunityMediaHarness(t)
			var avatar, banner []byte
			if tc.field == "avatar" {
				avatar = source
			} else {
				banner = source
			}
			require.NoError(t, h.writeImage(context.Background(), tc.operation, avatar, banner))
			require.Len(t, h.blobs.uploads, 1)
			require.Equal(t, "image/jpeg", h.blobs.uploads[0].mimeType)
			require.Equal(t, expected, h.blobs.uploads[0].data)
			path := "/xrpc/com.atproto.repo.createRecord"
			if tc.operation == "update" {
				path = "/xrpc/com.atproto.repo.putRecord"
			}
			writes := h.requestsAt(path)
			require.Len(t, writes, 1)
			record, ok := writes[0].body["record"].(map[string]any)
			require.True(t, ok)
			ref, ok := record[tc.field].(map[string]any)
			require.True(t, ok)
			require.Equal(t, "image/jpeg", ref["mimeType"])
			require.Equal(t, "blob", ref["$type"])
			require.Equal(t, map[string]any{"$link": "bafytestimage"}, ref["ref"])
			require.Equal(t, float64(len(expected)), ref["size"])
		})
	}
}

func TestCommunityMedia_RejectsImagesBeforeSideEffects(t *testing.T) {
	valid, _ := jpegWithGPS(t)
	corrupt := append([]byte(nil), valid[:len(valid)/2]...)
	gifBytes := gifImage(t)
	largeAvatar := expandingOrientedJPEG(t, 1700, 1300, communities.MaxAvatarBlobSize)
	largeBanner := expandingOrientedJPEG(t, 2600, 1900, communities.MaxBannerBlobSize)
	for _, tc := range []struct {
		name           string
		avatar, banner []byte
		field          string
	}{
		{"unsupported avatar", gifBytes, nil, "avatarBlob"},
		{"corrupt JPEG avatar", corrupt, nil, "avatarBlob"},
		{"valid avatar and unsupported banner", valid, gifBytes, "bannerBlob"},
		{"avatar expands beyond cap", largeAvatar, nil, "avatarBlob"},
		{"banner expands beyond cap", nil, largeBanner, "bannerBlob"},
	} {
		for _, operation := range []string{"create", "update"} {
			t.Run(operation+"/"+tc.name, func(t *testing.T) {
				h := newCommunityMediaHarness(t)
				before := *h.stored // fake repo returns this pointer; compare the saved value.
				err := h.writeImage(context.Background(), operation, tc.avatar, tc.banner)
				var validation *communities.ValidationError
				require.True(t, errors.As(err, &validation), "expected validation error, got %v", err)
				require.True(t, communities.IsValidationError(err))
				require.Equal(t, tc.field, validation.Field)
				require.Zero(t, len(h.blobs.uploads), "rejected images must not be uploaded")
				if operation == "create" {
					require.Empty(t, h.requestsAt("/xrpc/com.atproto.server.createAccount"))
					require.Empty(t, h.repo.callsTo("Create"))
				} else {
					require.Empty(t, h.requestsAt("/xrpc/com.atproto.repo.putRecord"))
					require.True(t, reflect.DeepEqual(before, *h.stored), "repo row changed on rejected update")
				}
			})
		}
	}
}

func TestCommunityMedia_CancellationIsNotInvalidImage(t *testing.T) {
	valid, _ := jpegWithGPS(t)
	for _, operation := range []string{"create", "update"} {
		t.Run(operation, func(t *testing.T) {
			h := newCommunityMediaHarness(t)
			before := *h.stored
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			err := h.writeImage(ctx, operation, valid, nil)
			require.ErrorIs(t, err, context.Canceled)
			require.False(t, communities.IsValidationError(err))
			require.Zero(t, len(h.blobs.uploads), "cancelled images must not be uploaded")
			require.Empty(t, h.requestsAt("/xrpc/com.atproto.repo.createRecord"))
			require.Empty(t, h.requestsAt("/xrpc/com.atproto.repo.putRecord"))
			if operation == "create" {
				require.Empty(t, h.requestsAt("/xrpc/com.atproto.server.createAccount"))
			} else {
				require.True(t, reflect.DeepEqual(before, *h.stored), "repo row changed on cancellation")
			}
		})
	}
}
