package imageproxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"Coves/internal/atproto/identity"
	coreimageproxy "Coves/internal/core/imageproxy"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandler_HandleImage_ResolutionDeadlineOnCacheMiss(t *testing.T) {
	for _, tc := range []struct {
		name    string
		context func(*testing.T, context.Context) (context.Context, time.Time)
		check   func(*testing.T, time.Time, bool, error, time.Time)
		// serviceCheck, when set, inspects the deadline of the context the
		// service runs the blob fetch under.
		serviceCheck func(t *testing.T, deadline time.Time, ok bool)
	}{
		{
			name: "no parent deadline",
			context: func(_ *testing.T, parent context.Context) (context.Context, time.Time) {
				return parent, time.Time{}
			},
			check: func(t *testing.T, deadline time.Time, ok bool, err error, _ time.Time) {
				require.True(t, ok, "DID resolution must have a deadline")
				assert.Greater(t, time.Until(deadline), time.Duration(0))
				assert.LessOrEqual(t, time.Until(deadline), 5*time.Second)
			},
			serviceCheck: func(t *testing.T, deadline time.Time, ok bool) {
				if ok {
					assert.Greater(t, time.Until(deadline), identity.ResolveTimeout,
						"the resolution deadline must not bound the blob fetch, decode and resize")
				}
			},
		},
		{
			name: "earlier parent deadline",
			context: func(t *testing.T, parent context.Context) (context.Context, time.Time) {
				ctx, cancel := context.WithTimeout(parent, 1*time.Second)
				t.Cleanup(cancel)
				parentDeadline, _ := ctx.Deadline()
				return ctx, parentDeadline
			},
			check: func(t *testing.T, deadline time.Time, ok bool, err error, parentDeadline time.Time) {
				require.True(t, ok, "DID resolution must retain the parent's deadline")
				assert.False(t, deadline.After(parentDeadline), "resolution must not outlive its parent")
			},
		},
		{
			name: "cancelled parent",
			context: func(_ *testing.T, parent context.Context) (context.Context, time.Time) {
				ctx, cancel := context.WithCancel(parent)
				cancel()
				return ctx, time.Time{}
			},
			check: func(t *testing.T, _ time.Time, _ bool, err error, _ time.Time) {
				require.Error(t, err, "request cancellation must reach DID resolution")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var recordedDeadline time.Time
			var hasDeadline bool
			var recordedError error
			called := false
			resolver := &mockIdentityResolver{
				resolveDIDFunc: func(ctx context.Context, did string) (*identity.DIDDocument, error) {
					called = true
					recordedDeadline, hasDeadline = ctx.Deadline()
					recordedError = ctx.Err()
					return &identity.DIDDocument{DID: did, Service: []identity.Service{{
						ID: "#atproto_pds", Type: "AtprotoPersonalDataServer", ServiceEndpoint: "https://pds.invalid",
					}}}, nil
				},
			}
			var serviceDeadline time.Time
			var serviceHasDeadline bool
			service := &mockService{getImageFunc: func(ctx context.Context, _, _, _, _ string) ([]byte, error) {
				serviceDeadline, serviceHasDeadline = ctx.Deadline()
				return []byte("cached-independent image"), nil
			}}
			handler := NewHandler(service, resolver)
			req := createTestRequest(http.MethodGet, "/img/avatar/plain/"+validTestDID+"/"+validTestCID, map[string]string{
				"preset": "avatar", "did": validTestDID, "cid": validTestCID,
			})
			requestContext, parentDeadline := tc.context(t, req.Context())
			req = req.WithContext(requestContext)
			response := httptest.NewRecorder()
			handler.HandleImage(response, req)

			require.True(t, called, "the miss must invoke DID resolution")
			tc.check(t, recordedDeadline, hasDeadline, recordedError, parentDeadline)
			if tc.serviceCheck != nil {
				tc.serviceCheck(t, serviceDeadline, serviceHasDeadline)
			}
		})
	}
}

func TestHandler_HandleImage_WarmDiskCacheSkipsDIDResolution(t *testing.T) {
	cache, err := coreimageproxy.NewDiskCache(t.TempDir(), 1, 0)
	require.NoError(t, err)
	cachedImage := []byte{0xff, 0xd8, 0xff, 0xe0, 0x01}
	require.NoError(t, cache.Set("avatar", validTestDID, validTestCID, cachedImage))

	processor, err := coreimageproxy.NewProcessor(coreimageproxy.DefaultMaxSourceMegapixels)
	require.NoError(t, err)
	fetcher := &bombFetcher{}
	service, err := coreimageproxy.NewService(cache, processor, fetcher, allowAllBlockChecker{}, allowAllReferences{}, coreimageproxy.DefaultConfig())
	require.NoError(t, err)
	resolverCalls := 0
	resolver := &mockIdentityResolver{resolveDIDFunc: func(context.Context, string) (*identity.DIDDocument, error) {
		resolverCalls++
		return nil, &identity.ErrResolutionFailed{Identifier: validTestDID, Reason: "resolution unavailable"}
	}}
	handler := NewHandler(service, resolver)
	req := createTestRequest(http.MethodGet, "/img/avatar/plain/"+validTestDID+"/"+validTestCID, map[string]string{
		"preset": "avatar", "did": validTestDID, "cid": validTestCID,
	})
	response := httptest.NewRecorder()
	handler.HandleImage(response, req)

	require.Equal(t, http.StatusOK, response.Code)
	assert.Equal(t, cachedImage, response.Body.Bytes())
	assert.Zero(t, resolverCalls, "a disk-cache hit must not resolve the DID")
	assert.Zero(t, fetcher.calls.Load(), "a disk-cache hit must not fetch the blob")
}
