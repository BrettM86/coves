package votes_test

import (
	"bytes"
	"context"
	"github.com/bluesky-social/indigo/atproto/atclient"
	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"log/slog"
	"testing"
	"testing/synctest"
	"time"

	"Coves/internal/atproto/pds"
	"Coves/internal/core/votes"
	"github.com/stretchr/testify/require"
)

func TestCreateVote_DefinitiveReplacementRefusalSkipsReconciliation(t *testing.T) {
	t.Parallel()
	for _, refusal := range []error{pds.ErrUnauthorized, pds.ErrForbidden, pds.ErrSessionExpired, pds.ErrBadRequest, pds.ErrRateLimited, pds.ErrSwapConflict, pds.ErrConflict, pds.ErrPayloadTooLarge, pds.ErrNotFound, &atclient.APIError{StatusCode: 422, Name: "UnprocessableContent"}} {
		t.Run(refusal.Error(), func(t *testing.T) {
			t.Parallel()
			fake := newFakePDS(t, testVoterDID)
			cache := votes.NewVoteCache(longTTL, nil)
			service := newService(t, fake, cache)
			up, err := service.CreateVote(context.Background(), voter(t, testVoterDID), votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
			require.NoError(t, err)
			fake.batchErr = refusal
			_, err = service.CreateVote(context.Background(), voter(t, testVoterDID), votes.CreateVoteRequest{Subject: subject(), Direction: "down"})
			require.ErrorIs(t, err, refusal)
			if pds.IsAuthError(refusal) {
				require.ErrorIs(t, err, votes.ErrNotAuthorized)
			}
			require.Zero(t, fake.getCalls, "a refused transaction cannot have committed")
			if refusal == pds.ErrNotFound || refusal == pds.ErrConflict || refusal == pds.ErrSwapConflict {
				require.False(t, cache.IsCached(testVoterDID), "state refusal requires fresh state on the next tap")
			} else {
				require.True(t, cache.IsCached(testVoterDID))
				require.Equal(t, up.URI, cache.GetVote(testVoterDID, testSubject).URI)
			}
			require.Equal(t, 1, fake.batchCalls)
		})
	}
}

func TestCreateVote_ReplacementPollsForDelayedCommit(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fake := newFakePDS(t, testVoterDID)
		cache := votes.NewVoteCache(longTTL, nil)
		service := newService(t, fake, cache)
		_, err := service.CreateVote(context.Background(), voter(t, testVoterDID), votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
		require.NoError(t, err)
		<-time.After(time.Microsecond) // Advance the virtual clock before minting another vote TID.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		fake.batchErr = context.DeadlineExceeded
		fake.commitBeforeError = true
		fake.afterBatch = cancel
		var firstKey string
		fake.getHook = func(ctx context.Context, key string) error {
			deadline, ok := ctx.Deadline()
			require.True(t, ok)
			require.LessOrEqual(t, time.Until(deadline), 5*time.Second)
			if fake.getCalls == 1 {
				firstKey = key
				return pds.ErrNotFound
			}
			require.Equal(t, firstKey, key, "poll only the attempted replacement, never re-toggle")
			return nil
		}
		started := time.Now()
		down, err := service.CreateVote(ctx, voter(t, testVoterDID), votes.CreateVoteRequest{Subject: subject(), Direction: "down"})
		require.NoError(t, err, "not-found may precede a late commit")
		require.Greater(t, fake.getCalls, 1)
		require.Greater(t, time.Since(started), time.Duration(0), "polls must wait between reads")
		require.Less(t, time.Since(started), 5*time.Second)
		require.Equal(t, down.URI, cache.GetVote(testVoterDID, testSubject).URI)
		require.Equal(t, 1, fake.batchCalls)
		require.Len(t, fake.records, 1)
	})
}

func TestCreateVote_ReplacementPollingStopsOnDefinitiveReadFailure(t *testing.T) {
	t.Parallel()
	for _, refusal := range []error{pds.ErrUnauthorized, pds.ErrForbidden, pds.ErrSessionExpired, pds.ErrBadRequest, pds.ErrRateLimited} {
		t.Run(refusal.Error(), func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				fake := newFakePDS(t, testVoterDID)
				cache := votes.NewVoteCache(longTTL, nil)
				service := newService(t, fake, cache)
				_, err := service.CreateVote(context.Background(), voter(t, testVoterDID), votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
				require.NoError(t, err)
				<-time.After(time.Microsecond) // Advance the virtual clock before minting another vote TID.
				fake.batchErr = context.DeadlineExceeded
				fake.getHook = func(context.Context, string) error {
					if fake.getCalls == 1 {
						return pds.ErrNotFound
					}
					return refusal
				}
				started := time.Now()
				_, err = service.CreateVote(context.Background(), voter(t, testVoterDID), votes.CreateVoteRequest{Subject: subject(), Direction: "down"})
				require.ErrorIs(t, err, context.DeadlineExceeded, "a failed read cannot establish whether the write committed")
				require.Equal(t, 2, fake.getCalls)
				require.Less(t, time.Since(started), 5*time.Second)
				require.False(t, cache.IsCached(testVoterDID))
				require.Equal(t, 1, fake.batchCalls)
			})
		})
	}
}

func TestCreateVote_ReplacementAbsenceExhaustsBoundWithoutRetryingWrite(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fake := newFakePDS(t, testVoterDID)
		cache := votes.NewVoteCache(longTTL, nil)
		service := newService(t, fake, cache)
		_, err := service.CreateVote(context.Background(), voter(t, testVoterDID), votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
		require.NoError(t, err)
		<-time.After(time.Microsecond) // Advance the virtual clock before minting another vote TID.
		fake.batchErr = context.DeadlineExceeded
		started := time.Now()
		_, err = service.CreateVote(context.Background(), voter(t, testVoterDID), votes.CreateVoteRequest{Subject: subject(), Direction: "down"})
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Equal(t, 5*time.Second, time.Since(started), "absence needs the complete reconciliation window")
		require.Greater(t, fake.getCalls, 1)
		require.False(t, cache.IsCached(testVoterDID))
		require.Equal(t, 1, fake.batchCalls)
		require.Len(t, fake.creates(), 1, "uncertainty must never trigger another write")
	})
}

func TestCreateVote_MalformedReplacementResultReconciles(t *testing.T) {
	t.Parallel()
	for _, malformed := range []string{"nil", "short", "empty uri", "empty cid", "foreign uri"} {
		for _, committed := range []bool{false, true} {
			name := malformed + "/missing"
			if committed {
				name = malformed + "/committed"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				synctest.Test(t, func(t *testing.T) {
					fake := newFakePDS(t, testVoterDID)
					cache := votes.NewVoteCache(longTTL, nil)
					service := newService(t, fake, cache)
					_, err := service.CreateVote(context.Background(), voter(t, testVoterDID), votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
					require.NoError(t, err)
					<-time.After(time.Microsecond) // Advance the virtual clock before minting another vote TID.
					fake.batchResult = func(result *pds.ApplyWritesResult) *pds.ApplyWritesResult {
						if !committed {
							clear(fake.records)
						}
						switch malformed {
						case "nil":
							return nil
						case "short":
							result.Results = result.Results[:1]
						case "foreign uri":
							result.Results[1].URI = "at://did:plc:other/social.coves.feed.vote/other"
						case "empty uri":
							result.Results[1].URI = ""
						case "empty cid":
							result.Results[1].CID = ""
						}
						return result
					}
					var response *votes.CreateVoteResponse
					require.NotPanics(t, func() {
						response, err = service.CreateVote(context.Background(), voter(t, testVoterDID), votes.CreateVoteRequest{Subject: subject(), Direction: "down"})
					})
					if committed {
						require.NoError(t, err)
						require.NotEmpty(t, response.URI)
						require.NotEmpty(t, response.CID)
						require.Equal(t, response.URI, cache.GetVote(testVoterDID, testSubject).URI)
					} else {
						require.Error(t, err, "malformed reply cannot establish success")
						require.False(t, cache.IsCached(testVoterDID))
					}
					require.Positive(t, fake.getCalls)
					require.Equal(t, 1, fake.batchCalls)
				})
			})
		}
	}
}

func TestCreateVote_ReplacementReconciliationDiagnostics(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"recovered", "mismatch", "read refused"} {
		t.Run(outcome, func(t *testing.T) {
			t.Parallel()
			fake := newFakePDS(t, testVoterDID)
			cache := votes.NewVoteCache(longTTL, nil)
			var logs bytes.Buffer
			service := votes.NewServiceWithPDSFactory(nil, cache, slog.New(slog.NewJSONHandler(&logs, nil)), func(context.Context, *oauth.ClientSessionData) (votes.PDSClient, error) { return fake, nil })
			_, err := service.CreateVote(context.Background(), voter(t, testVoterDID), votes.CreateVoteRequest{Subject: subject(), Direction: "up"})
			require.NoError(t, err)
			logs.Reset()
			fake.batchErr = context.DeadlineExceeded
			fake.commitBeforeError = true
			if outcome == "mismatch" {
				fake.getTransform = func(record *pds.RecordResponse) { record.Value["direction"] = "up" }
			}
			if outcome == "read refused" {
				fake.getErr = pds.ErrForbidden
			}
			_, err = service.CreateVote(context.Background(), voter(t, testVoterDID), votes.CreateVoteRequest{Subject: subject(), Direction: "down"})
			if outcome == "recovered" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			text := logs.String()
			require.Contains(t, text, `"level":"WARN"`)
			require.Contains(t, text, `"old_rkey":"`+fake.creates()[0]+`"`)
			require.Contains(t, text, `"new_rkey":"`+fake.creates()[1]+`"`)
			require.Contains(t, text, `"batch_error":"context deadline exceeded"`)
			if outcome == "mismatch" {
				require.Contains(t, text, `"reason":`)
			}
			require.NotContains(t, text, "test-access-token")
		})
	}
}
