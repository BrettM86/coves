//go:build integration

package routes_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	moderationhandlers "Coves/internal/api/handlers/moderation"
	"Coves/internal/api/xrpc"
	"Coves/internal/core/moderation"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// modlogRecordingService keeps the error the list call returned, so the test
// can assert the service's classification as well as the handler's answer.
type modlogRecordingService struct {
	moderation.Service
	err error
}

func (service *modlogRecordingService) ListActions(ctx context.Context, params moderation.ListActionsParams) (*moderation.ActionPage, error) {
	page, err := service.Service.ListActions(ctx, params)
	service.err = err
	return page, err
}

func (service *modlogRecordingService) ListAdminActions(ctx context.Context, params moderation.ListAdminActionsParams) (*moderation.AdminActionPage, error) {
	page, err := service.Service.ListAdminActions(ctx, params)
	service.err = err
	return page, err
}

// A client that disconnects while its list query waits in Postgres gets the
// statement cancelled, and lib/pq reports that as SQLSTATE 57014 rather than a
// context error. The request must still end as cancellation, never as an
// outage, and nothing may be logged at Warn or Error. Not parallel: it
// replaces the global logger.
func TestModlogListCancelledWhileBlockedInPostgres(t *testing.T) {
	for _, test := range []struct {
		name   string
		table  string
		params func(*modlogFixture) url.Values
	}{
		{"store", "moderation_actions", func(*modlogFixture) url.Values { return url.Values{} }},
		{"community resolver", "communities", func(f *modlogFixture) url.Values {
			return url.Values{"community": {f.communityHandle}}
		}},
	} {
		for _, admin := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/admin=%t", test.name, admin), func(t *testing.T) {
				f := newModlogFixture(t)
				service := &modlogRecordingService{Service: f.service}
				path, handle := modlogPublicPath, moderationhandlers.NewListActionsHandler(service).HandleListActions
				if admin {
					path, handle = modlogAdminPath, moderationhandlers.NewListAdminActionsHandler(service).HandleListAdminActions
				}

				// The request's cancel is registered before the lock transaction
				// exists, so the rollback registered after it runs first.
				requestContext, cancelRequest := context.WithCancel(context.Background())
				t.Cleanup(cancelRequest)
				lockTransaction, err := f.db.BeginTx(context.Background(), nil)
				require.NoError(t, err)
				t.Cleanup(func() {
					if rollbackErr := lockTransaction.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
						t.Errorf("roll back lock transaction: %v", rollbackErr)
					}
				})
				_, err = lockTransaction.ExecContext(context.Background(), "LOCK TABLE "+test.table+" IN ACCESS EXCLUSIVE MODE")
				require.NoError(t, err)
				var lockHolderPID int
				require.NoError(t, lockTransaction.QueryRowContext(context.Background(), `SELECT pg_backend_pid()`).Scan(&lockHolderPID))

				var logged bytes.Buffer
				previousLogger := slog.Default()
				slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
				t.Cleanup(func() { slog.SetDefault(previousLogger) })

				recorder := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodGet, path+"?"+test.params(f).Encode(), nil).WithContext(requestContext)
				done := make(chan struct{})
				go func() {
					defer close(done)
					handle(recorder, request)
				}()

				// The probe runs on the lock transaction's own connection, so it
				// never competes with the blocked request for the test pool.
				testkit.WaitFor(t, 10*time.Second, func() (bool, error) {
					var waiting bool
					err := lockTransaction.QueryRowContext(context.Background(), `
						SELECT EXISTS (
							SELECT 1
							FROM pg_locks waiter
							INNER JOIN pg_locks holder
								ON holder.locktype = waiter.locktype
								AND holder.database = waiter.database
								AND holder.relation = waiter.relation
							WHERE holder.pid = $1
								AND holder.granted
								AND NOT waiter.granted
								AND waiter.relation = $2::text::regclass
						)
					`, lockHolderPID, test.table).Scan(&waiting)
					return waiting, err
				}, testkit.WithDescription("list request waiting on the %s lock", test.table))

				cancelRequest()
				testkit.WaitFor(t, 10*time.Second, func() (bool, error) {
					select {
					case <-done:
						return true, nil
					default:
						return false, nil
					}
				}, testkit.WithDescription("cancelled list request to return"))
				require.NoError(t, lockTransaction.Rollback())

				require.Error(t, service.err)
				assert.ErrorIs(t, service.err, context.Canceled)
				assert.NotErrorIs(t, service.err, moderation.ErrModerationUnavailable)
				assert.NotErrorIs(t, service.err, moderation.ErrResolverUnavailable)
				assert.Equal(t, http.StatusBadRequest, recorder.Code)
				var body xrpc.Error
				require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
				assert.Equal(t, "RequestCanceled", body.Error)
				assert.NotContains(t, logged.String(), "level=WARN")
				assert.NotContains(t, logged.String(), "level=ERROR")
			})
		}
	}
}
