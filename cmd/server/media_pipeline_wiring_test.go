package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"Coves/internal/api/middleware"
	"Coves/internal/config"
	"Coves/internal/core/blobs"
	"Coves/internal/core/media"
	"Coves/internal/core/userblocks"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/tests/testkit"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildApplicationRejectsInvalidMediaUploadConcurrency(t *testing.T) {
	app, err := buildApplication(context.Background(), &config.Config{
		Media: config.MediaConfig{Upload: media.Config{MaxConcurrent: 0}},
	}, nil, nil, nil, nil)
	assert.Nil(t, app)
	require.ErrorIs(t, err, media.ErrInvalidConfig)
}

type mediaWiringUserBlockRepo struct{ userblocks.Repository }

func TestUserRouteOptionsCarryMediaPipeline(t *testing.T) {
	pipeline, err := media.NewPipeline(media.Config{MaxConcurrent: 1, MaxLongEdge: media.DefaultMaxLongEdge})
	require.NoError(t, err)
	userBlockRepo := &mediaWiringUserBlockRepo{}
	a := &application{mediaPipeline: pipeline, userBlockRepo: userBlockRepo}

	options := a.userRouteOptions()
	require.NotNil(t, options)
	assert.Same(t, pipeline, options.MediaPipeline)
	assert.Same(t, userBlockRepo, options.UserBlockRepo)
}

// unreachableDatabaseConnector refuses every connection and counts the
// attempts, so building the application against it proves construction never
// touches the database.
type unreachableDatabaseConnector struct{ attempts atomic.Int64 }

var errUnreachableDatabase = errors.New("media wiring test: database must not be used")

func (c *unreachableDatabaseConnector) Connect(context.Context) (driver.Conn, error) {
	c.attempts.Add(1)
	return nil, errUnreachableDatabase
}

func (c *unreachableDatabaseConnector) Driver() driver.Driver { return c }

func (c *unreachableDatabaseConnector) Open(string) (driver.Conn, error) {
	c.attempts.Add(1)
	return nil, errUnreachableDatabase
}

// The production community service must sanitize avatars and banners through
// the application's pipeline. Built through buildApplication rather than a
// helper, so it also fails if the pipeline is created after the services.
func TestBuildApplicationCommunityServiceCarriesMediaPipeline(t *testing.T) {
	config.ClearEnvForTest(t)
	t.Setenv("IS_DEV_ENV", "true")
	t.Setenv("DATABASE_URL", "postgres://unused.invalid/unused")
	t.Setenv("IMAGE_PROXY_ENABLED", "false")
	t.Setenv("TELEGRAM_ALERTS_ENABLED", "")
	t.Setenv("TELEGRAM_ALERT_REASONS", "")
	cfg, err := config.Load()
	require.NoError(t, err)

	connector := &unreachableDatabaseConnector{}
	db := sql.OpenDB(connector)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })

	app, err := buildApplication(context.Background(), cfg, db, db, db, credentialciphertest.Fixed())
	require.NoError(t, err)
	t.Cleanup(app.Close)
	require.Zero(t, connector.attempts.Load(), "building the application must not connect to the database")

	require.NotNil(t, app.mediaPipeline)
	communityService, ok := app.communityService.(interface{ MediaPipeline() *media.Pipeline })
	require.True(t, ok, "community service %T does not expose its media pipeline", app.communityService)
	assert.Same(t, app.mediaPipeline, communityService.MediaPipeline())
}

// The production post service must sanitize link-card thumbnails through the
// application's pipeline.
func TestBuildApplicationPostServiceCarriesMediaPipeline(t *testing.T) {
	config.ClearEnvForTest(t)
	t.Setenv("IS_DEV_ENV", "true")
	t.Setenv("DATABASE_URL", "postgres://unused.invalid/unused")
	t.Setenv("IMAGE_PROXY_ENABLED", "false")
	t.Setenv("TELEGRAM_ALERTS_ENABLED", "")
	t.Setenv("TELEGRAM_ALERT_REASONS", "")
	cfg, err := config.Load()
	require.NoError(t, err)

	connector := &unreachableDatabaseConnector{}
	db := sql.OpenDB(connector)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })

	app, err := buildApplication(context.Background(), cfg, db, db, db, credentialciphertest.Fixed())
	require.NoError(t, err)
	t.Cleanup(app.Close)

	require.NotNil(t, app.mediaPipeline)
	postService, ok := app.postService.(interface{ MediaPipeline() *media.Pipeline })
	require.True(t, ok, "post service %T does not expose its media pipeline", app.postService)
	assert.Same(t, app.mediaPipeline, postService.MediaPipeline())
}

// mediaWiringAPIKeyValidator accepts every API key as one aggregator, so a
// request reaches the upload handler authenticated without an OAuth session.
type mediaWiringAPIKeyValidator struct{ aggregatorDID string }

func (validator mediaWiringAPIKeyValidator) ValidateKey(context.Context, string) (string, error) {
	return validator.aggregatorDID, nil
}

func (mediaWiringAPIKeyValidator) RefreshTokensIfNeeded(context.Context, string) error { return nil }

// An aggregator uploading with an API key or service JWT writes with its
// stored OAuth session, so the server must hand the upload route the
// application's stored-session loader. Without it the handler answers every
// such upload with 503 NoAuthorCredentials and never reads a stored session.
// Driven through the real XRPC route table: the loader's store sits on a
// database that refuses connections, so a connection attempt during the
// request proves the loader was wired and called.
func TestRegisterXRPCRoutesUploadImageLoadsStoredSession(t *testing.T) {
	config.ClearEnvForTest(t)
	t.Setenv("IS_DEV_ENV", "true")
	t.Setenv("DATABASE_URL", "postgres://unused.invalid/unused")
	t.Setenv("IMAGE_PROXY_ENABLED", "false")
	t.Setenv("TELEGRAM_ALERTS_ENABLED", "")
	t.Setenv("TELEGRAM_ALERT_REASONS", "")
	cfg, err := config.Load()
	require.NoError(t, err)

	connector := &unreachableDatabaseConnector{}
	db := sql.OpenDB(connector)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })

	app, err := buildApplication(context.Background(), cfg, db, db, db, credentialciphertest.Fixed())
	require.NoError(t, err)
	t.Cleanup(app.Close)
	require.NotNil(t, app.apiKeyService)

	// Only API key validation is replaced; the stored-session loader the
	// route receives is whatever registerXRPCRoutes passes it.
	app.dualAuth = middleware.NewDualAuthMiddleware(nil, nil, nil, nil).
		WithAPIKeyValidator(mediaWiringAPIKeyValidator{aggregatorDID: "did:plc:mediawiringaggregator"})

	router := chi.NewRouter()
	registerXRPCRoutes(router, app)
	require.Zero(t, connector.attempts.Load(), "registering routes must not connect to the database")

	request := httptest.NewRequest(http.MethodPost, "/xrpc/social.coves.media.uploadImage",
		strings.NewReader("not read before the stored session loads"))
	request.Header.Set("Authorization", "Bearer "+middleware.APIKeyPrefix+"mediawiring")
	request.Header.Set("Content-Type", "image/png")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	var errorBody struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(recorder.Body.Bytes(), &errorBody)
	require.NotEqual(t, http.StatusUnauthorized, recorder.Code, "the API key must authenticate: %s", recorder.Body.String())
	assert.NotEqual(t, "NoAuthorCredentials", errorBody.Error,
		"the upload route has no stored-session loader: %s", recorder.Body.String())
	assert.Positive(t, connector.attempts.Load(),
		"the upload route never read the aggregator's stored session through the application's loader")
}

// mediaWiringUploader stands in for a caller's PDS and counts stored blobs.
type mediaWiringUploader struct{ calls int }

func (u *mediaWiringUploader) UploadBlob(_ context.Context, data []byte, mimeType string) (*blobs.BlobRef, error) {
	u.calls++
	return &blobs.BlobRef{Type: "blob", Ref: map[string]string{"$link": "bafkreimediawiringupload"}, MimeType: mimeType, Size: len(data)}, nil
}

// The server's upload service must enforce the configured per-DID cap.
func TestBuildApplicationUploadServiceEnforcesConfiguredLimit(t *testing.T) {
	config.ClearEnvForTest(t)
	t.Setenv("IS_DEV_ENV", "true")
	t.Setenv("DATABASE_URL", "postgres://unused.invalid/unused")
	t.Setenv("IMAGE_PROXY_ENABLED", "false")
	t.Setenv("TELEGRAM_ALERTS_ENABLED", "")
	t.Setenv("TELEGRAM_ALERT_REASONS", "")
	t.Setenv("MEDIA_UPLOAD_RATE_PER_10MIN", "2")
	cfg, err := config.Load()
	require.NoError(t, err)

	connector := &unreachableDatabaseConnector{}
	db := sql.OpenDB(connector)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })

	app, err := buildApplication(context.Background(), cfg, db, db, db, credentialciphertest.Fixed())
	require.NoError(t, err)
	t.Cleanup(app.Close)
	require.NotNil(t, app.mediaUploads)

	const did = "did:plc:mediawiringuploader"
	uploader := &mediaWiringUploader{}
	upload := func() error {
		_, err := app.mediaUploads.Upload(context.Background(), did, uploader,
			media.Input{Data: testkit.TestJPEG(8, 8), DeclaredContentType: "image/jpeg"})
		return err
	}
	require.NoError(t, upload(), "first upload")
	require.NoError(t, upload(), "second upload")

	err = upload()
	var mediaErr *media.Error
	require.ErrorAs(t, err, &mediaErr, "third upload with MEDIA_UPLOAD_RATE_PER_10MIN=2")
	assert.Equal(t, media.ErrorCodeUploadRateLimited, mediaErr.Code)
	assert.Equal(t, 2, uploader.calls, "uploads reaching the PDS")
}
