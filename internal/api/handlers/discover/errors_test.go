package discover

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"Coves/internal/api/xrpc"
	coreDiscover "Coves/internal/core/discover"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type errorDiscoverService struct {
	err error
}

func (f *errorDiscoverService) GetDiscover(context.Context, coreDiscover.GetDiscoverRequest) (*coreDiscover.DiscoverResponse, error) {
	return nil, f.err
}

func TestGetDiscover_MapsCursorAndCapacityErrors(t *testing.T) {
	t.Parallel()

	const internalDetail = "snapshot reservation query failed"
	tests := []struct {
		name           string
		err            error
		wantStatus     int
		wantCode       string
		wantRetryAfter string
	}{
		{
			name:           "capacity unavailable",
			err:            fmt.Errorf("%s: %w", internalDetail, coreDiscover.ErrDiscoverUnavailable),
			wantStatus:     http.StatusServiceUnavailable,
			wantCode:       "DiscoverUnavailable",
			wantRetryAfter: "30",
		},
		{
			name:       "invalid cursor",
			err:        fmt.Errorf("expired snapshot: %w", coreDiscover.ErrInvalidCursor),
			wantStatus: http.StatusBadRequest,
			wantCode:   "InvalidCursor",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/xrpc/social.coves.feed.getDiscover", nil)
			NewGetDiscoverHandler(&errorDiscoverService{err: test.err}, nil, nil).HandleGetDiscover(rec, req)

			var body xrpc.Error
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Equal(t, test.wantStatus, rec.Code)
			assert.Equal(t, test.wantCode, body.Error)
			assert.NotContains(t, rec.Body.String(), internalDetail)
			assert.Equal(t, test.wantRetryAfter, rec.Header().Get("Retry-After"))
		})
	}
}

var _ coreDiscover.Service = (*errorDiscoverService)(nil)
