package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"Coves/internal/api/middleware"
	"Coves/internal/atproto/oauth"
	"Coves/internal/config"
	"Coves/internal/core/blobs"
	"Coves/internal/core/posts"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/tests/testkit"

	indigooauth "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	loopbackUploaderDID   = "did:plc:loopbackuploader"
	loopbackSessionToken  = "loopback-sealed-session"
	loopbackCommunityDID  = "did:plc:loopbackcommunity"
	loopbackPacingTick    = 250 * time.Millisecond
	loopbackClientTimeout = 30 * time.Second
	// loopbackStoreDelay is how long the fake PDS takes to store a blob, so
	// the upload response is written after the upload read deadline.
	loopbackStoreDelay = 3 * time.Second
)

// loopbackAuthorRepo is the caller's repo as the upload route opens it: blobs
// go to the shared uploader after loopbackStoreDelay, and nothing else is
// expected to be called.
type loopbackAuthorRepo struct {
	posts.AuthorRepo
	uploader *mediaWiringUploader
}

func (repo loopbackAuthorRepo) UploadBlob(ctx context.Context, data []byte, mimeType string) (*blobs.BlobRef, error) {
	select {
	case <-time.After(loopbackStoreDelay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return repo.uploader.UploadBlob(ctx, data, mimeType)
}

func (loopbackAuthorRepo) DID() string { return loopbackUploaderDID }

// loopbackPostService records the post.create requests that reach it.
type loopbackPostService struct {
	posts.Service
	mu       sync.Mutex
	requests []posts.CreatePostRequest
}

func (service *loopbackPostService) CreatePost(_ context.Context, _ *indigooauth.ClientSessionData, req posts.CreatePostRequest) (*posts.CreatePostResponse, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.requests = append(service.requests, req)
	return &posts.CreatePostResponse{
		URI: "at://" + loopbackUploaderDID + "/social.coves.community.post/loopback",
		CID: "bafyreiloopbackpost",
	}, nil
}

func (service *loopbackPostService) received() []posts.CreatePostRequest {
	service.mu.Lock()
	defer service.mu.Unlock()
	return append([]posts.CreatePostRequest(nil), service.requests...)
}

type loopbackServer struct {
	baseURL     string
	uploader    *mediaWiringUploader
	postService *loopbackPostService
}

// startLoopbackServer serves the production XRPC route table with the
// production HTTP server timeouts on a loopback listener. Only the caller's
// session store, repo factory, and post service are fakes.
//
// Timeouts: the whole-request read deadline is 1s and image uploads get 3s.
// HTTP_WRITE_TIMEOUT is 4s, deliberately:
//   - The slow upload below finishes its body at about 2.5s and its 200 is
//     written at about 5.5s, after the fake PDS's loopbackStoreDelay. The
//     route must set the write deadline to 3s + 4s = 7s. A write deadline of
//     3s (the upload budget alone) or 4s (the one set after the headers, or
//     the upload budget plus the 1s read timeout) would cut the response off;
//     each is at least 1.5s from the 5.5s write.
//   - An unauthenticated chunked upload is refused with 401 only after
//     net/http drains the unread body, which ends when the 1s read deadline
//     fires. A write deadline at that same instant would race the 401 onto
//     the wire; 4s leaves the refusal ample time to be written, so a lost 401
//     cannot be blamed on the fixture.
func startLoopbackServer(t *testing.T) *loopbackServer {
	t.Helper()
	config.ClearEnvForTest(t)
	t.Setenv("IS_DEV_ENV", "true")
	t.Setenv("DATABASE_URL", "postgres://unused.invalid/unused")
	t.Setenv("IMAGE_PROXY_ENABLED", "false")
	t.Setenv("TELEGRAM_ALERTS_ENABLED", "")
	t.Setenv("TELEGRAM_ALERT_REASONS", "")
	t.Setenv("HTTP_READ_HEADER_TIMEOUT", "1s")
	t.Setenv("HTTP_READ_TIMEOUT", "1s")
	t.Setenv("HTTP_WRITE_TIMEOUT", "4s")
	t.Setenv("MEDIA_UPLOAD_READ_TIMEOUT", "3s")
	cfg, err := config.Load()
	require.NoError(t, err)

	connector := &unreachableDatabaseConnector{}
	db := sql.OpenDB(connector)
	t.Cleanup(func() { assert.NoError(t, db.Close()) })

	app, err := buildApplication(context.Background(), cfg, db, db, db, credentialciphertest.Fixed())
	require.NoError(t, err)
	t.Cleanup(app.Close)

	unsealer := wiringSessionUnsealer{loopbackSessionToken: &oauth.SealedSession{
		DID: loopbackUploaderDID, SessionID: "browser", ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}}
	store := &wiringOAuthStore{sessions: map[string]*indigooauth.ClientSessionData{
		loopbackUploaderDID + ":browser": {
			AccountDID: syntax.DID(loopbackUploaderDID),
			SessionID:  "browser",
			Scopes:     []string{"atproto", "blob:*/*"},
		},
	}}
	app.dualAuth = middleware.NewDualAuthMiddleware(unsealer, store, nil, nil)

	server := &loopbackServer{uploader: &mediaWiringUploader{}, postService: &loopbackPostService{}}
	app.authorRepoFactory = func(context.Context, string, *indigooauth.ClientSessionData) (posts.AuthorRepo, error) {
		return loopbackAuthorRepo{uploader: server.uploader}, nil
	}
	app.postService = server.postService

	router := newRouter(nil)
	registerXRPCRoutes(router, app)
	require.Zero(t, connector.attempts.Load(), "registering routes must not connect to the database")

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	httpServer := newHTTPServer(cfg.Server, router)
	served := make(chan error, 1)
	go func() { served <- httpServer.Serve(listener) }()
	t.Cleanup(func() {
		assert.NoError(t, httpServer.Close())
		assert.ErrorIs(t, <-served, http.ErrServerClosed)
	})

	server.baseURL = "http://" + listener.Addr().String()
	return server
}

type loopbackExchange struct {
	status  int
	body    []byte
	err     error
	elapsed time.Duration
}

// post sends body in chunks, one every loopbackPacingTick, with no declared
// length, reading the response while the body is still being written. Each
// call gets its own Transport, so its own connection; keep-alive stays on, as
// real clients send it, because "Connection: close" makes net/http skip
// draining an unread body before a refusal (see startLoopbackServer).
// chunks == 1 sends the whole body at once.
func (server *loopbackServer) post(t *testing.T, path, token, contentType string, body []byte, chunks int) loopbackExchange {
	t.Helper()
	require.GreaterOrEqual(t, len(body), chunks, "body too short to split into %d chunks", chunks)

	bodyReader, bodyWriter := io.Pipe()
	request, err := http.NewRequest(http.MethodPost, server.baseURL+path, bodyReader)
	require.NoError(t, err)
	request.Header.Set("Content-Type", contentType)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		ticker := time.NewTicker(loopbackPacingTick)
		defer ticker.Stop()
		size := (len(body) + chunks - 1) / chunks
		for offset := 0; offset < len(body); offset += size {
			if offset > 0 {
				<-ticker.C
			}
			if _, err := bodyWriter.Write(body[offset:min(offset+size, len(body))]); err != nil {
				return // the exchange ended before the body did
			}
		}
		_ = bodyWriter.Close()
	}()

	client := &http.Client{
		Timeout:   loopbackClientTimeout,
		Transport: &http.Transport{},
	}
	defer client.CloseIdleConnections()

	exchange := loopbackExchange{}
	start := time.Now()
	response, err := client.Do(request)
	if err == nil {
		exchange.status = response.StatusCode
		exchange.body, err = io.ReadAll(response.Body)
		_ = response.Body.Close()
	}
	exchange.elapsed = time.Since(start)
	exchange.err = err
	// Unblock a writer still pacing a body the server no longer reads.
	_ = bodyReader.CloseWithError(errors.New("exchange finished"))
	<-writerDone
	return exchange
}

func TestUploadReadDeadlineOverLoopback(t *testing.T) {
	server := startLoopbackServer(t)

	t.Run("authenticated slow upload outlasts the server read and write timeouts", func(t *testing.T) {
		image := testkit.TestJPEG(64, 64)
		// 11 chunks, 250ms apart: the last arrives about 2.5s in, past the 1s
		// read timeout, inside the 3s upload budget. The response follows
		// loopbackStoreDelay later, at about 5.5s: past the upload read
		// deadline and the 4s write timeout, inside the 7s write deadline.
		exchange := server.post(t, "/xrpc/social.coves.media.uploadImage", loopbackSessionToken, "image/jpeg", image, 11)
		t.Logf("slow upload ended after %s: status %d, err %v", exchange.elapsed, exchange.status, exchange.err)
		require.NoError(t, exchange.err, "after %s", exchange.elapsed)
		require.Equal(t, http.StatusOK, exchange.status, "after %s: %s", exchange.elapsed, exchange.body)
		assert.GreaterOrEqual(t, exchange.elapsed, 2400*time.Millisecond+loopbackStoreDelay, "the body and the store must actually have been slow")

		var uploaded struct {
			Blob struct {
				Ref struct {
					Link string `json:"$link"`
				} `json:"ref"`
			} `json:"blob"`
			MimeType string `json:"mimeType"`
		}
		require.NoError(t, json.Unmarshal(exchange.body, &uploaded), "%s", exchange.body)
		assert.Equal(t, "bafkreimediawiringupload", uploaded.Blob.Ref.Link)
		assert.Equal(t, "image/jpeg", uploaded.MimeType)

		server.uploader.mu.Lock()
		defer server.uploader.mu.Unlock()
		assert.Equal(t, 1, server.uploader.calls, "blobs stored in the caller's repo")
		assert.True(t, bytes.HasPrefix(server.uploader.stored, []byte{0xFF, 0xD8, 0xFF}),
			"the stored blob is not a JPEG: % x", server.uploader.stored[:min(8, len(server.uploader.stored))])
	})

	t.Run("slow body to post.create still fails at the server read timeout", func(t *testing.T) {
		postBody := []byte(`{"community":"` + loopbackCommunityDID + `","title":"Loopback pacing","content":"A route-valid post body sent slowly."}`)

		// Control: the same body sent at once reaches the post service.
		fast := server.post(t, "/xrpc/social.coves.community.post.create", loopbackSessionToken, "application/json", postBody, 1)
		require.NoError(t, fast.err)
		require.Equal(t, http.StatusOK, fast.status, "%s", fast.body)
		require.Len(t, server.postService.received(), 1, "the control request must reach the post service")
		assert.Equal(t, loopbackCommunityDID, server.postService.received()[0].Community)

		// 13 chunks over about 3s: the body would complete at 3s, so an
		// exchange ending near 1s ended at the server's read deadline.
		slow := server.post(t, "/xrpc/social.coves.community.post.create", loopbackSessionToken, "application/json", postBody, 13)
		t.Logf("slow post.create ended after %s: status %d, err %v", slow.elapsed, slow.status, slow.err)
		if slow.err == nil {
			assert.False(t, slow.status >= 200 && slow.status < 300,
				"a slow post.create body must not succeed: %d %s", slow.status, slow.body)
		}
		assert.GreaterOrEqual(t, slow.elapsed, 900*time.Millisecond, "ended before the 1s read timeout (status %d, err %v)", slow.status, slow.err)
		assert.Less(t, slow.elapsed, 2500*time.Millisecond, "did not end at the 1s read timeout (status %d, err %v)", slow.status, slow.err)
		assert.Len(t, server.postService.received(), 1, "the slow request must not reach the post service")
	})

	t.Run("unauthenticated slow upload is refused within the server read timeout", func(t *testing.T) {
		// 25 chunks over about 6s, longer than the 3s upload budget, so a
		// refusal held open by the upload deadline would show.
		body := testkit.TestJPEG(64, 64)
		exchange := server.post(t, "/xrpc/social.coves.media.uploadImage", "", "image/jpeg", body, 25)
		require.NoError(t, exchange.err, "after %s", exchange.elapsed)
		assert.Equal(t, http.StatusUnauthorized, exchange.status, "%s", exchange.body)
		assert.Less(t, exchange.elapsed, 2500*time.Millisecond, "the refusal must end at about the 1s read timeout")
		t.Logf("unauthenticated refusal after %s", exchange.elapsed)
	})
}
