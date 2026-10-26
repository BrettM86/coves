package media

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"Coves/internal/api/middleware"
	covesoauth "Coves/internal/atproto/oauth"
	"Coves/internal/atproto/pds"
	"Coves/internal/core/aggregators"
	"Coves/internal/core/blobs"
	coremedia "Coves/internal/core/media"
	"Coves/internal/core/posts"
	"Coves/tests/testkit"

	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"golang.org/x/image/webp"
)

const (
	uploaderDID = "did:plc:uploadimagecaller"
	pdsBlobCID  = "bafkreihdwdcefgh4dqkjv67uzcmw7ojee6xedzdetojuzjevtenxquvyku"
)

type receivedUpload struct {
	data     []byte
	mimeType string
}

// fakeAuthorRepo stands in for the caller's PDS repo. Only UploadBlob and DID
// are reachable from this handler; the embedded nil interface makes any other
// call panic.
type fakeAuthorRepo struct {
	posts.AuthorRepo
	uploads   []receivedUpload
	returned  []blobs.BlobRef
	uploadErr error
}

func (r *fakeAuthorRepo) UploadBlob(_ context.Context, data []byte, mimeType string) (*blobs.BlobRef, error) {
	r.uploads = append(r.uploads, receivedUpload{data: append([]byte(nil), data...), mimeType: mimeType})
	if r.uploadErr != nil {
		return nil, r.uploadErr
	}
	ref := blobs.BlobRef{Type: "blob", Ref: map[string]string{"$link": pdsBlobCID}, MimeType: mimeType, Size: len(data)}
	r.returned = append(r.returned, ref)
	return &ref, nil
}

func (r *fakeAuthorRepo) DID() string { return uploaderDID }

type factoryCall struct {
	did     string
	session *oauth.ClientSessionData
}

type fakePDS struct {
	repo       *fakeAuthorRepo
	calls      []factoryCall
	factoryErr error
}

func newFakePDS() *fakePDS { return &fakePDS{repo: &fakeAuthorRepo{}} }

func (f *fakePDS) factory(_ context.Context, did string, session *oauth.ClientSessionData) (posts.AuthorRepo, error) {
	f.calls = append(f.calls, factoryCall{did: did, session: session})
	if f.factoryErr != nil {
		return nil, f.factoryErr
	}
	return f.repo, nil
}

// fakeSessionLoader stands in for the aggregator stored-session store: it
// records which DIDs it was asked for and answers with a scripted result.
type fakeSessionLoader struct {
	session   *oauth.ClientSessionData
	err       error
	requested []string
}

func (l *fakeSessionLoader) StoredSession(_ context.Context, aggregatorDID string) (*oauth.ClientSessionData, error) {
	l.requested = append(l.requested, aggregatorDID)
	return l.session, l.err
}

// unconsultedLoader fails the test if the handler asks it for anything. Every
// request built by uploadRequest carries the caller's own OAuth session, which
// must be used as-is: an OAuth caller never has its stored session loaded.
type unconsultedLoader struct{ t *testing.T }

func (l unconsultedLoader) StoredSession(_ context.Context, aggregatorDID string) (*oauth.ClientSessionData, error) {
	l.t.Errorf("stored-session loader asked for %s; a caller with its own OAuth session must not consult it", aggregatorDID)
	return nil, errors.New("unconsultedLoader answers nothing")
}

func newHandler(t *testing.T, pds *fakePDS) *UploadImageHandler {
	t.Helper()
	return newHandlerWithLoader(t, pds, unconsultedLoader{t: t})
}

func newHandlerWithLoader(t *testing.T, pds *fakePDS, loader StoredSessionLoader) *UploadImageHandler {
	t.Helper()
	pipeline, err := coremedia.NewPipeline(coremedia.Config{MaxConcurrent: 2, MaxLongEdge: coremedia.DefaultMaxLongEdge})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return NewUploadImageHandler(defaultUploadService(t, pipeline), pds.factory, loader)
}

// newHandlerWithMaxLongEdge is newHandler with a pipeline that downscales
// uploads to a long edge of at most maxLongEdge pixels.
func newHandlerWithMaxLongEdge(t *testing.T, pds *fakePDS, maxLongEdge int) *UploadImageHandler {
	t.Helper()
	pipeline, err := coremedia.NewPipeline(coremedia.Config{MaxConcurrent: 2, MaxLongEdge: maxLongEdge})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return NewUploadImageHandler(defaultUploadService(t, pipeline), pds.factory, unconsultedLoader{t: t})
}

func sessionWithScopes(scopes ...string) *oauth.ClientSessionData {
	return &oauth.ClientSessionData{
		AccountDID:  syntax.DID(uploaderDID),
		SessionID:   "upload-image-session",
		HostURL:     "https://pds.upload.test",
		Scopes:      scopes,
		AccessToken: "upload-image-access-token",
	}
}

func blobSession() *oauth.ClientSessionData {
	return sessionWithScopes("atproto", "repo:social.coves.community.post", "blob:*/*")
}

func uploadRequest(body []byte, contentType string, session *oauth.ClientSessionData) *http.Request {
	return uploadRequestAs(uploaderDID, body, contentType, session)
}

// uploadRequestAs is uploadRequest authenticated as did.
func uploadRequestAs(did string, body []byte, contentType string, session *oauth.ClientSessionData) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.media.uploadImage", bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	ctx := middleware.SetTestUserDID(req.Context(), did)
	ctx = middleware.SetTestOAuthSession(ctx, session)
	return req.WithContext(ctx)
}

const aggregatorCallerDID = "did:plc:uploadimageaggregator"

// aggregatorRequest is a request the way DualAuthMiddleware passes it on after
// validating an API key or service JWT: the aggregator's DID and auth method
// in the context, and no OAuth session.
func aggregatorRequest(body io.Reader, contentType, authMethod string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.media.uploadImage", body)
	req.Header.Set("Content-Type", contentType)
	ctx := context.WithValue(req.Context(), middleware.UserDIDKey, aggregatorCallerDID)
	ctx = context.WithValue(ctx, middleware.IsAggregatorAuthKey, true)
	ctx = context.WithValue(ctx, middleware.AuthMethodKey, authMethod)
	return req.WithContext(ctx)
}

// storedAggregatorSession is the aggregator's stored OAuth session.
func storedAggregatorSession(scopes ...string) *oauth.ClientSessionData {
	session := sessionWithScopes(scopes...)
	session.AccountDID = syntax.DID(aggregatorCallerDID)
	session.SessionID = "apikey"
	return session
}

// countingBody counts reads, so a test can tell whether the handler touched
// the request body.
type countingBody struct {
	reader io.Reader
	reads  int
}

func (b *countingBody) Read(p []byte) (int, error) {
	b.reads++
	return b.reader.Read(p)
}

func serve(handler http.Handler, req *http.Request) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

type uploadResponse struct {
	Blob        *blobs.BlobRef `json:"blob"`
	MimeType    string         `json:"mimeType"`
	Size        int            `json:"size"`
	AspectRatio *struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	} `json:"aspectRatio"`
}

func expectError(t *testing.T, recorder *httptest.ResponseRecorder, status int, name string) {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(recorder.Body.Bytes(), &body)
	if recorder.Code != status || body.Error != name {
		t.Fatalf("response: got %d error %q, want %d error %q (body %q)", recorder.Code, body.Error, status, name, recorder.Body.String())
	}
}

func expectNoUploads(t *testing.T, pds *fakePDS) {
	t.Helper()
	if len(pds.repo.uploads) != 0 {
		t.Errorf("uploads reaching the PDS: got %d, want 0", len(pds.repo.uploads))
	}
}

// A lossless 32x16 WebP (VP8X with the EXIF flag, VP8L pixels, an EXIF chunk
// with orientation 6), assembled once from the media package's quadrant WebP
// fixture. The test checks it decodes as 32x16 before using it.
const orientedWebPBase64 = "UklGRn4AAABXRUJQVlA4WAoAAAAIAAAAHwAADwAAVlA4TCwAAAAvH8ADAB8gEEjaH3qN+RcQFPk/2vwHXySAINtmkpOc5AEuENH/2DbVH0DVD0VYSUYsAAAASUkqAAgAAAACABIBAwABAAAABgAAACWIBAABAAAAJgAAAAAAAAAAAAAAAAA="

func orientedWebP(t *testing.T) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(orientedWebPBase64)
	if err != nil {
		t.Fatalf("decode oriented WebP fixture: %v", err)
	}
	config, err := webp.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width != 32 || config.Height != 16 {
		t.Fatalf("oriented WebP fixture: %dx%d, error %v; want 32x16", config.Width, config.Height, err)
	}
	if !bytes.Contains(data, []byte("EXIF")) {
		t.Fatal("oriented WebP fixture lacks its EXIF chunk")
	}
	return data
}

func TestUploadImageStoresSanitizedImageInCallersRepo(t *testing.T) {
	rows := []struct {
		name                  string
		body                  []byte
		contentType           string
		wantMimeType          string // "" means: the sniffed format of the stored bytes, never image/webp
		wantWidth, wantHeight int
	}{
		{"JPEG with Exif orientation 6 declared as PNG", testkit.JPEGWithSegments(testkit.TestJPEG(48, 32), testkit.JPEGExifGPSegment(6)), "image/png", "image/jpeg", 32, 48},
		{"PNG", testkit.TestPNG(40, 20), "image/png", "image/png", 40, 20},
		{"WebP with Exif orientation 6", orientedWebP(t), "image/webp", "", 16, 32},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			pds := newFakePDS()
			session := blobSession()

			recorder := serve(newHandler(t, pds), uploadRequest(row.body, row.contentType, session))

			if recorder.Code != http.StatusOK {
				t.Fatalf("status: got %d, want 200 (body %q)", recorder.Code, recorder.Body.String())
			}
			if len(pds.calls) != 1 || pds.calls[0].did != uploaderDID || pds.calls[0].session != session {
				t.Fatalf("repo factory calls: got %+v, want one for %s with the request's session", pds.calls, uploaderDID)
			}
			if len(pds.repo.uploads) != 1 || len(pds.repo.returned) != 1 {
				t.Fatalf("uploads on the caller's repo: got %d, want 1", len(pds.repo.uploads))
			}
			stored := pds.repo.uploads[0]
			var response uploadResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode response %q: %v", recorder.Body.String(), err)
			}

			wantMimeType := row.wantMimeType
			if wantMimeType == "" {
				wantMimeType = http.DetectContentType(stored.data)
				if wantMimeType == "image/webp" {
					t.Fatalf("oriented WebP was stored as WebP; want it re-encoded")
				}
			}
			if response.MimeType != wantMimeType || stored.mimeType != wantMimeType || http.DetectContentType(stored.data) != wantMimeType {
				t.Errorf("mimeType: response %q, upload %q, stored bytes %q; want %q", response.MimeType, stored.mimeType, http.DetectContentType(stored.data), wantMimeType)
			}
			if response.Blob == nil {
				t.Fatal("response has no blob")
			}
			if got, want := *response.Blob, pds.repo.returned[0]; got.Type != want.Type || got.Ref["$link"] != want.Ref["$link"] || len(got.Ref) != len(want.Ref) || got.MimeType != want.MimeType || got.Size != want.Size {
				t.Errorf("blob: got %+v, want the PDS's %+v", got, want)
			}
			if response.Size != len(stored.data) {
				t.Errorf("size: got %d, want %d (bytes the PDS received)", response.Size, len(stored.data))
			}
			if response.AspectRatio == nil || response.AspectRatio.Width != row.wantWidth || response.AspectRatio.Height != row.wantHeight {
				t.Errorf("aspectRatio: got %+v, want {%d %d}", response.AspectRatio, row.wantWidth, row.wantHeight)
			}
			decoded, format, err := image.Decode(bytes.NewReader(stored.data))
			if err != nil || "image/"+format != wantMimeType || decoded.Bounds() != image.Rect(0, 0, row.wantWidth, row.wantHeight) {
				t.Errorf("stored bytes: format %q bounds %v error %v; want %s %dx%d", format, imageBounds(decoded), err, wantMimeType, row.wantWidth, row.wantHeight)
			}
		})
	}
}

func imageBounds(img image.Image) image.Rectangle {
	if img == nil {
		return image.Rectangle{}
	}
	return img.Bounds()
}

func TestUploadImageStoresSanitizedGIF(t *testing.T) {
	dirty, clean := testkit.AnimatedGIFWithMetadata()
	pds := newFakePDS()

	recorder := serve(newHandler(t, pds), uploadRequest(dirty, "image/gif", blobSession()))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (body %q)", recorder.Code, recorder.Body.String())
	}
	var response uploadResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response %q: %v", recorder.Body.String(), err)
	}
	if response.MimeType != "image/gif" {
		t.Errorf("response mimeType: got %q, want image/gif", response.MimeType)
	}
	if response.AspectRatio == nil || response.AspectRatio.Width != 4 || response.AspectRatio.Height != 3 {
		t.Errorf("aspectRatio: got %+v, want {4 3}", response.AspectRatio)
	}
	if len(pds.repo.uploads) != 1 {
		t.Fatalf("uploads on the caller's repo: got %d, want 1", len(pds.repo.uploads))
	}
	stored := pds.repo.uploads[0]
	if stored.mimeType != "image/gif" {
		t.Errorf("uploaded mimeType: got %q, want image/gif", stored.mimeType)
	}
	if !bytes.Equal(stored.data, clean) {
		t.Errorf("uploaded bytes: got %d bytes, want the sanitized GIF (%d bytes)", len(stored.data), len(clean))
	}
}

func TestUploadImageDescribesTheDownscaledImageItStored(t *testing.T) {
	body, err := base64.StdEncoding.DecodeString(testkit.TransparentQuadrantWebP300x200Base64)
	if err != nil {
		t.Fatalf("decode WebP fixture: %v", err)
	}
	if fixture, err := webp.Decode(bytes.NewReader(body)); err != nil || imageBounds(fixture) != image.Rect(0, 0, 300, 200) {
		t.Fatalf("WebP fixture: bounds %v, error %v; want 300x200", imageBounds(fixture), err)
	} else if _, _, _, alpha := fixture.At(225, 50).RGBA(); alpha != 0 {
		t.Fatalf("WebP fixture top-right alpha: got %d, want 0", alpha)
	}
	pds := newFakePDS()

	recorder := serve(newHandlerWithMaxLongEdge(t, pds, 120), uploadRequest(body, "image/webp", blobSession()))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (body %q)", recorder.Code, recorder.Body.String())
	}
	if len(pds.repo.uploads) != 1 {
		t.Fatalf("uploads on the caller's repo: got %d, want 1", len(pds.repo.uploads))
	}
	stored := pds.repo.uploads[0]
	var response uploadResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response %q: %v", recorder.Body.String(), err)
	}
	if response.MimeType != "image/png" || stored.mimeType != "image/png" {
		t.Errorf("mimeType: response %q, upload %q; want image/png", response.MimeType, stored.mimeType)
	}
	if response.Size != len(stored.data) {
		t.Errorf("size: got %d, want %d (bytes the PDS received)", response.Size, len(stored.data))
	}
	if response.AspectRatio == nil || response.AspectRatio.Width != 120 || response.AspectRatio.Height != 80 {
		t.Errorf("aspectRatio: got %+v, want {120 80}", response.AspectRatio)
	}
	decoded, format, err := image.Decode(bytes.NewReader(stored.data))
	if err != nil || format != "png" || decoded.Bounds() != image.Rect(0, 0, 120, 80) {
		t.Errorf("stored bytes: format %q bounds %v error %v; want png 120x80", format, imageBounds(decoded), err)
	}
}

func TestUploadImageRequiresTheBlobScope(t *testing.T) {
	rows := []struct {
		name    string
		session *oauth.ClientSessionData
		body    []byte
	}{
		{"atproto and repo scopes only", sessionWithScopes("atproto", "repo:social.coves.community.post"), testkit.TestJPEG(8, 8)},
		{"narrower blob:image/png uploading a PNG", sessionWithScopes("atproto", "blob:image/png"), testkit.TestPNG(8, 8)},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			pds := newFakePDS()

			recorder := serve(newHandler(t, pds), uploadRequest(row.body, "image/png", row.session))

			expectError(t, recorder, http.StatusForbidden, "MissingBlobScope")
			if len(pds.calls) != 0 {
				t.Errorf("repo factory calls: got %d, want 0", len(pds.calls))
			}
			expectNoUploads(t, pds)
		})
	}
}

func TestUploadImageCapsTheRequestBody(t *testing.T) {
	jpegWithJunk := testkit.TestJPEG(16, 16)
	jpegWithJunk = append(jpegWithJunk, bytes.Repeat([]byte{0}, 10_000_001-len(jpegWithJunk))...)
	rows := []struct {
		name          string
		body          []byte
		unknownLength bool
		status        int
		error         string
	}{
		{"JPEG padded with trailing junk to 10,000,001 bytes", jpegWithJunk, false, http.StatusRequestEntityTooLarge, "ImageTooLarge"},
		{"same body with unknown Content-Length", jpegWithJunk, true, http.StatusRequestEntityTooLarge, "ImageTooLarge"},
		{"exactly 10,000,000 non-image bytes reach the pipeline", bytes.Repeat([]byte("x"), 10_000_000), false, http.StatusBadRequest, "UnsupportedImageFormat"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			pds := newFakePDS()
			req := uploadRequest(row.body, "image/jpeg", blobSession())
			if row.unknownLength {
				req.ContentLength = -1
			}

			recorder := serve(newHandler(t, pds), req)

			expectError(t, recorder, row.status, row.error)
			expectNoUploads(t, pds)
		})
	}
}

func heicHeader() []byte {
	box := binary.BigEndian.AppendUint32(nil, 20)
	box = append(box, "ftypheic"...)
	box = binary.BigEndian.AppendUint32(box, 0)
	return append(box, "heic"...)
}

func pngChunk(name string, payload []byte) []byte {
	chunk := binary.BigEndian.AppendUint32(nil, uint32(len(payload)))
	chunk = append(chunk, name...)
	chunk = append(chunk, payload...)
	return binary.BigEndian.AppendUint32(chunk, crc32.ChecksumIEEE(chunk[4:]))
}

func pngHeader(width, height uint32) []byte {
	ihdr := binary.BigEndian.AppendUint32(nil, width)
	ihdr = binary.BigEndian.AppendUint32(ihdr, height)
	ihdr = append(ihdr, 8, 6, 0, 0, 0)
	file := []byte("\x89PNG\r\n\x1a\n")
	file = append(file, pngChunk("IHDR", ihdr)...)
	return append(file, pngChunk("IDAT", []byte{0x78, 0x9c})...)
}

func truncatedJPEG(t *testing.T) []byte {
	t.Helper()
	data := testkit.TestJPEG(48, 32)
	_, scan, err := testkit.WalkJPEGSegments(data)
	if err != nil {
		t.Fatalf("walk encoded JPEG: %v", err)
	}
	return data[:len(data)-len(scan)+len(scan)/2]
}

func TestUploadImageMapsRejectedImagesToErrors(t *testing.T) {
	rows := []struct {
		name   string
		body   []byte
		status int
		error  string
	}{
		{"HEIC", heicHeader(), http.StatusBadRequest, "UnsupportedImageFormat"},
		{"PNG declaring 8000x8000", pngHeader(8000, 8000), http.StatusBadRequest, "ImageDimensionsTooLarge"},
		{"truncated JPEG", truncatedJPEG(t), http.StatusBadRequest, "InvalidImage"},
		{"JPEG whose rotated re-encode exceeds 10,000,000 bytes", testkit.JPEGThatGrowsPastTenMegabytesWhenRotated(), http.StatusRequestEntityTooLarge, "ImageTooLarge"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			pds := newFakePDS()

			recorder := serve(newHandler(t, pds), uploadRequest(row.body, "image/jpeg", blobSession()))

			expectError(t, recorder, row.status, row.error)
			expectNoUploads(t, pds)
		})
	}
}

func TestUploadImageReportsPDSFailures(t *testing.T) {
	rows := []struct {
		name        string
		uploadErr   error
		factoryErr  error
		wantUploads int
	}{
		{"PDS upload fails", errors.New("pds rejected blob"), nil, 1},
		{"opening the caller's repo fails", nil, errors.New("no session to resume"), 0},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			pds := newFakePDS()
			pds.repo.uploadErr = row.uploadErr
			pds.factoryErr = row.factoryErr

			recorder := serve(newHandler(t, pds), uploadRequest(testkit.TestJPEG(16, 16), "image/jpeg", blobSession()))

			expectError(t, recorder, http.StatusBadGateway, "PDSUploadFailed")
			if len(pds.repo.uploads) != row.wantUploads {
				t.Errorf("uploads reaching the PDS: got %d, want %d", len(pds.repo.uploads), row.wantUploads)
			}
		})
	}
}

// captureDefaultLog routes slog's default logger (and, through it, the log
// package) into a buffer until the test ends.
func captureDefaultLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	previous, previousWriter, previousFlags := slog.Default(), log.Writer(), log.Flags()
	t.Cleanup(func() {
		slog.SetDefault(previous)
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	})
	var captured bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&captured, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return &captured
}

func TestUploadImageLogsNoCredentialsOrPDSErrorText(t *testing.T) {
	const (
		accessToken     = "sentinel-access-token-a71f"
		refreshToken    = "sentinel-refresh-token-c93e"
		hostNonce       = "sentinel-dpop-host-nonce-5d02"
		authServerNonce = "sentinel-dpop-authserver-nonce-8b44"
		dpopKey         = "sentinel-dpop-private-key-e6a1"
		pdsErrorText    = "sentinel-pds-error-text-3f7c"
	)
	session := blobSession()
	session.AccessToken = accessToken
	session.RefreshToken = refreshToken
	session.DPoPHostNonce = hostNonce
	session.DPoPAuthServerNonce = authServerNonce
	session.DPoPPrivateKeyMultibase = dpopKey

	captured := captureDefaultLog(t)
	rows := []struct {
		name      string
		uploadErr error
	}{
		{"successful upload", nil},
		{"failed PDS upload", errors.New("upload refused: " + pdsErrorText)},
	}
	for _, row := range rows {
		captured.Reset()
		pds := newFakePDS()
		pds.repo.uploadErr = row.uploadErr

		serve(newHandler(t, pds), uploadRequest(testkit.TestJPEG(16, 16), "image/jpeg", session))

		output := captured.String()
		if !strings.Contains(output, uploaderDID) {
			t.Errorf("%s: log output has no entry naming the caller's DID; got %q", row.name, output)
		}
		for _, sentinel := range []string{accessToken, refreshToken, hostNonce, authServerNonce, dpopKey, pdsErrorText} {
			if strings.Contains(output, sentinel) {
				t.Errorf("%s: log output contains %q", row.name, sentinel)
			}
		}
	}
}

func TestUploadImageMapsSessionAndPDSErrors(t *testing.T) {
	rows := []struct {
		name       string
		uploadErr  error
		factoryErr error
		status     int
		error      string
	}{
		{"upload: session expired", fmt.Errorf("refreshing session: %w", pds.ErrSessionExpired), nil, http.StatusUnauthorized, "AuthRequired"},
		{"upload: PDS answers 401", fmt.Errorf("uploadBlob: %w", pds.ErrUnauthorized), nil, http.StatusUnauthorized, "AuthRequired"},
		{"factory: session expired", nil, fmt.Errorf("resuming session: %w", pds.ErrSessionExpired), http.StatusUnauthorized, "AuthRequired"},
		{"factory: PDS answers 401", nil, fmt.Errorf("resuming session: %w", pds.ErrUnauthorized), http.StatusUnauthorized, "AuthRequired"},
		{"upload: session busy", fmt.Errorf("refreshing session: %w", pds.ErrSessionBusy), nil, http.StatusServiceUnavailable, "SessionBusy"},
		{"upload: PDS rate limited", fmt.Errorf("uploadBlob: %w", pds.ErrRateLimited), nil, http.StatusTooManyRequests, "RateLimitExceeded"},
		{"upload: PDS payload too large", fmt.Errorf("uploadBlob: %w", pds.ErrPayloadTooLarge), nil, http.StatusRequestEntityTooLarge, "PayloadTooLarge"},
		{"upload: PDS forbidden", fmt.Errorf("uploadBlob: %w", pds.ErrForbidden), nil, http.StatusForbidden, "PermissionDenied"},
		{"upload: PDS server error", fmt.Errorf("uploadBlob: %w", pds.ErrServerError), nil, http.StatusBadGateway, "PDSUploadFailed"},
		{"upload: PDS bad request", fmt.Errorf("uploadBlob: %w", pds.ErrBadRequest), nil, http.StatusBadGateway, "PDSUploadFailed"},
		{"upload: PDS not found", fmt.Errorf("uploadBlob: %w", pds.ErrNotFound), nil, http.StatusBadGateway, "PDSUploadFailed"},
		{"upload: PDS conflict", fmt.Errorf("uploadBlob: %w", pds.ErrConflict), nil, http.StatusBadGateway, "PDSUploadFailed"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			pds := newFakePDS()
			pds.repo.uploadErr = row.uploadErr
			pds.factoryErr = row.factoryErr

			recorder := serve(newHandler(t, pds), uploadRequest(testkit.TestJPEG(16, 16), "image/jpeg", blobSession()))

			expectError(t, recorder, row.status, row.error)
		})
	}
}

func TestUploadImageLogsClassifiedCauseWithoutErrorText(t *testing.T) {
	const (
		sessionIDSentinel    = "sentinel-session-id-xyz"
		unclassifiedSentinel = "sentinel-unclassified-text-91b3"
		pdsErrorText         = "sentinel-pds-error-text-77d1"
	)
	rows := []struct {
		name       string
		uploadErr  error
		factoryErr error
		wantLevel  string
		wantCause  string
		sentinels  []string
	}{
		{
			name:       "factory: no author credentials",
			factoryErr: fmt.Errorf("failed to resume OAuth session for DID=%s, sessionID=%s: %w", uploaderDID, sessionIDSentinel, posts.ErrNoAuthorCredentials),
			wantLevel:  "level=ERROR",
			wantCause:  "cause=no_author_credentials",
			sentinels:  []string{sessionIDSentinel},
		},
		{
			name:       "factory: unclassified failure",
			factoryErr: errors.New("session store query failed: " + unclassifiedSentinel),
			wantLevel:  "level=ERROR",
			wantCause:  "cause=unclassified",
			sentinels:  []string{unclassifiedSentinel},
		},
		{
			name:      "upload: session expired",
			uploadErr: fmt.Errorf("refreshing session %s: %w", pdsErrorText, pds.ErrSessionExpired),
			wantLevel: "level=WARN",
			wantCause: "cause=session_expired",
			sentinels: []string{pdsErrorText},
		},
		{
			name:      "upload: PDS not found",
			uploadErr: fmt.Errorf("uploadBlob %s: %w", pdsErrorText, pds.ErrNotFound),
			wantLevel: "level=WARN",
			wantCause: "cause=pds_not_found",
			sentinels: []string{pdsErrorText},
		},
		{
			name:      "upload: PDS conflict",
			uploadErr: fmt.Errorf("uploadBlob %s: %w", pdsErrorText, pds.ErrConflict),
			wantLevel: "level=WARN",
			wantCause: "cause=pds_conflict",
			sentinels: []string{pdsErrorText},
		},
	}
	captured := captureDefaultLog(t)
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			captured.Reset()
			pds := newFakePDS()
			pds.repo.uploadErr = row.uploadErr
			pds.factoryErr = row.factoryErr

			serve(newHandler(t, pds), uploadRequest(testkit.TestJPEG(16, 16), "image/jpeg", blobSession()))

			output := captured.String()
			for _, want := range []string{row.wantLevel, row.wantCause, uploaderDID} {
				if !strings.Contains(output, want) {
					t.Errorf("log output lacks %q; got %q", want, output)
				}
			}
			for _, sentinel := range row.sentinels {
				if strings.Contains(output, sentinel) {
					t.Errorf("log output contains error text %q; got %q", sentinel, output)
				}
			}
		})
	}
}

// failingBody yields data once, then fails every later read with err.
type failingBody struct {
	data []byte
	err  error
}

func (b *failingBody) Read(p []byte) (int, error) {
	if len(b.data) > 0 {
		n := copy(p, b.data)
		b.data = b.data[n:]
		return n, nil
	}
	return 0, b.err
}

func TestUploadImageReportsBodyReadFailures(t *testing.T) {
	rows := []struct {
		name      string
		readErr   error
		status    int
		error     string
		wantCause string
	}{
		{"read deadline exceeded", fmt.Errorf("read tcp: %w", os.ErrDeadlineExceeded), http.StatusRequestTimeout, "RequestTimeout", "cause=timeout"},
		{"connection closed mid-body", io.ErrUnexpectedEOF, http.StatusBadRequest, "InvalidRequest", "cause=unexpected_eof"},
	}
	captured := captureDefaultLog(t)
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			captured.Reset()
			pds := newFakePDS()
			req := uploadRequest(nil, "image/jpeg", blobSession())
			req.Body = io.NopCloser(&failingBody{data: testkit.TestJPEG(16, 16)[:64], err: row.readErr})
			req.ContentLength = -1

			recorder := serve(newHandler(t, pds), req)

			expectError(t, recorder, row.status, row.error)
			if len(pds.calls) != 0 {
				t.Errorf("repo factory calls: got %d, want 0", len(pds.calls))
			}
			output := captured.String()
			for _, want := range []string{uploaderDID, row.wantCause} {
				if !strings.Contains(output, want) {
					t.Errorf("log output lacks %q; got %q", want, output)
				}
			}
		})
	}
}

func TestUploadImageUsesTheAggregatorsStoredSession(t *testing.T) {
	rows := []struct {
		name       string
		authMethod string
	}{
		{"API key", middleware.AuthMethodAPIKey},
		{"service JWT", middleware.AuthMethodServiceJWT},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			pds := newFakePDS()
			stored := storedAggregatorSession("atproto", "repo:social.coves.community.post", "blob:*/*")
			loader := &fakeSessionLoader{session: stored}

			recorder := serve(newHandlerWithLoader(t, pds, loader),
				aggregatorRequest(bytes.NewReader(testkit.TestPNG(40, 20)), "image/png", row.authMethod))

			if recorder.Code != http.StatusOK {
				t.Fatalf("status: got %d, want 200 (body %q)", recorder.Code, recorder.Body.String())
			}
			if len(loader.requested) != 1 || loader.requested[0] != "did:plc:uploadimageaggregator" {
				t.Errorf("stored sessions requested: got %v, want [did:plc:uploadimageaggregator]", loader.requested)
			}
			if len(pds.calls) != 1 || pds.calls[0].did != "did:plc:uploadimageaggregator" || pds.calls[0].session != nil {
				t.Fatalf("repo factory calls: got %+v, want one for did:plc:uploadimageaggregator with a nil session (resumed by the factory)", pds.calls)
			}
			if len(pds.repo.uploads) != 1 {
				t.Fatalf("uploads on the aggregator's repo: got %d, want 1", len(pds.repo.uploads))
			}
			var response uploadResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode response %q: %v", recorder.Body.String(), err)
			}
			if response.MimeType != "image/png" {
				t.Errorf("mimeType: got %q, want image/png", response.MimeType)
			}
			if response.Blob == nil || response.Blob.Ref["$link"] != pdsBlobCID {
				t.Errorf("blob: got %+v, want ref %s", response.Blob, pdsBlobCID)
			}
			if response.Size != len(pds.repo.uploads[0].data) {
				t.Errorf("size: got %d, want %d (bytes the PDS received)", response.Size, len(pds.repo.uploads[0].data))
			}
			if response.AspectRatio == nil || response.AspectRatio.Width != 40 || response.AspectRatio.Height != 20 {
				t.Errorf("aspectRatio: got %+v, want {40 20}", response.AspectRatio)
			}
		})
	}
}

func TestUploadImageRequiresTheBlobScopeOnTheStoredSession(t *testing.T) {
	pds := newFakePDS()
	loader := &fakeSessionLoader{session: storedAggregatorSession("atproto", "repo:social.coves.community.post")}
	body := &countingBody{reader: bytes.NewReader(testkit.TestPNG(8, 8))}

	recorder := serve(newHandlerWithLoader(t, pds, loader), aggregatorRequest(body, "image/png", middleware.AuthMethodAPIKey))

	expectError(t, recorder, http.StatusForbidden, "MissingBlobScope")
	if len(pds.calls) != 0 {
		t.Errorf("repo factory calls: got %d, want 0", len(pds.calls))
	}
	expectNoUploads(t, pds)
	if body.reads != 0 {
		t.Errorf("request body reads: got %d, want 0", body.reads)
	}
}

func TestUploadImageReportsStoredSessionFailures(t *testing.T) {
	const storeErrorText = "sentinel-session-store-error-6c1d"
	rows := []struct {
		name      string
		loaderErr error
		status    int
		error     string
	}{
		{"stored session is dead", fmt.Errorf("loading stored session: %w", aggregators.ErrOAuthSessionDead), http.StatusServiceUnavailable, "NoAuthorCredentials"},
		{"session store fails", errors.New("session store query failed: " + storeErrorText), http.StatusServiceUnavailable, "ServiceUnavailable"},
	}
	captured := captureDefaultLog(t)
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			captured.Reset()
			pds := newFakePDS()
			loader := &fakeSessionLoader{err: row.loaderErr}
			body := &countingBody{reader: bytes.NewReader(testkit.TestJPEG(16, 16))}

			recorder := serve(newHandlerWithLoader(t, pds, loader), aggregatorRequest(body, "image/jpeg", middleware.AuthMethodAPIKey))

			expectError(t, recorder, row.status, row.error)
			if len(pds.calls) != 0 {
				t.Errorf("repo factory calls: got %d, want 0", len(pds.calls))
			}
			expectNoUploads(t, pds)
			if body.reads != 0 {
				t.Errorf("request body reads: got %d, want 0", body.reads)
			}
			if strings.Contains(recorder.Body.String(), storeErrorText) {
				t.Errorf("response body contains the store's error text: %q", recorder.Body.String())
			}
			if strings.Contains(captured.String(), storeErrorText) {
				t.Errorf("log output contains the store's error text: %q", captured.String())
			}
		})
	}
}

// An aggregator's stored session is used only for the scope check: its repo is
// opened the way post.create opens it, so a stored session that loads but
// cannot be resumed answers 503 NoAuthorCredentials, not 401 "sign in again".
func TestUploadImageReportsAggregatorRepoFailures(t *testing.T) {
	rows := []struct {
		name       string
		factoryErr error
		status     int
		error      string
	}{
		{
			"stored session corrupt", fmt.Errorf("resuming the stored session of %s: %w: %w",
				aggregatorCallerDID, posts.ErrNoAuthorCredentials, covesoauth.ErrSessionCorrupt),
			http.StatusServiceUnavailable, "NoAuthorCredentials",
		},
		{
			"stored session gone", fmt.Errorf("resuming the stored session of %s: %w: %w",
				aggregatorCallerDID, posts.ErrNoAuthorCredentials, covesoauth.ErrSessionNotFound),
			http.StatusServiceUnavailable, "NoAuthorCredentials",
		},
		{
			"credentials unavailable", fmt.Errorf("resuming the stored session of %s: %w: %w",
				aggregatorCallerDID, posts.ErrAuthorCredentialsUnavailable, errors.New("dial tcp: connection refused")),
			http.StatusServiceUnavailable, "ServiceUnavailable",
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			pds := newFakePDS()
			pds.factoryErr = row.factoryErr
			loader := &fakeSessionLoader{session: storedAggregatorSession("atproto", "repo:social.coves.community.post", "blob:*/*")}

			recorder := serve(newHandlerWithLoader(t, pds, loader),
				aggregatorRequest(bytes.NewReader(testkit.TestJPEG(16, 16)), "image/jpeg", middleware.AuthMethodAPIKey))

			expectError(t, recorder, row.status, row.error)
			if len(pds.calls) != 1 || pds.calls[0].did != aggregatorCallerDID || pds.calls[0].session != nil {
				t.Errorf("repo factory calls: got %+v, want one for %s with a nil session", pds.calls, aggregatorCallerDID)
			}
			expectNoUploads(t, pds)
		})
	}
}

// defaultUploadService wraps a pipeline in an upload service with the default
// per-DID count limit.
func defaultUploadService(t *testing.T, pipeline *coremedia.Pipeline) *coremedia.UploadService {
	t.Helper()
	limiter, err := coremedia.NewUploadLimiter(coremedia.UploadLimitConfig{UploadsPerTenMinutes: coremedia.DefaultUploadsPerTenMinutes, BytesPerDay: 500000000, WarnFraction: 0.8})
	if err != nil {
		t.Fatalf("creating the upload limiter: %v", err)
	}
	return coremedia.NewUploadService(pipeline, limiter)
}

// The limit counts the authenticated caller's uploads: one caller at the cap
// is refused before anything reaches its PDS, while another caller on the
// same handler still uploads.
func TestUploadImageRefusesACallerOverTheUploadLimit(t *testing.T) {
	const otherDID = "did:plc:uploadimageothercaller"
	pipeline, err := coremedia.NewPipeline(coremedia.Config{MaxConcurrent: 2, MaxLongEdge: coremedia.DefaultMaxLongEdge})
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	limiter, err := coremedia.NewUploadLimiter(coremedia.UploadLimitConfig{UploadsPerTenMinutes: 1, BytesPerDay: 500000000, WarnFraction: 0.8})
	if err != nil {
		t.Fatalf("NewUploadLimiter: %v", err)
	}
	pds := newFakePDS()
	handler := NewUploadImageHandler(coremedia.NewUploadService(pipeline, limiter), pds.factory, unconsultedLoader{t: t})

	first := serve(handler, uploadRequest(testkit.TestJPEG(16, 16), "image/jpeg", blobSession()))
	if first.Code != http.StatusOK {
		t.Fatalf("first upload: got %d, want 200 (body %q)", first.Code, first.Body.String())
	}

	second := serve(handler, uploadRequest(testkit.TestJPEG(16, 16), "image/jpeg", blobSession()))
	expectError(t, second, http.StatusTooManyRequests, "UploadRateLimited")
	if len(pds.repo.uploads) != 1 {
		t.Fatalf("uploads reaching the PDS after the refused upload: got %d, want 1", len(pds.repo.uploads))
	}

	otherSession := blobSession()
	otherSession.AccountDID = syntax.DID(otherDID)
	other := serve(handler, uploadRequestAs(otherDID, testkit.TestJPEG(16, 16), "image/jpeg", otherSession))
	if other.Code != http.StatusOK {
		t.Fatalf("another caller's upload: got %d, want 200 (body %q)", other.Code, other.Body.String())
	}
	if len(pds.repo.uploads) != 2 {
		t.Fatalf("uploads reaching the PDS after the other caller's upload: got %d, want 2", len(pds.repo.uploads))
	}
	if last := pds.calls[len(pds.calls)-1]; last.did != otherDID {
		t.Fatalf("repo opened for the other caller's upload: got %s, want %s", last.did, otherDID)
	}
}
