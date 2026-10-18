package jetstream

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"Coves/internal/atproto/identity"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func originProfileEvent(operation string) *JetstreamEvent {
	return &JetstreamEvent{
		Kind: "commit",
		Did:  debouncedUserDID,
		Commit: &CommitEvent{
			Operation: operation, Collection: CovesProfileCollection, RKey: "self",
			Record: map[string]interface{}{
				"displayName": "Bridge Name", "origin": "lemmy.world", "originUsername": "Alice_B",
				"originUrl": "https://lemmy.world/u/Alice_B", "originSoftware": "lemmy",
			},
		},
	}
}

// TestUserConsumer_ProfileOriginTimeoutIsDebouncedAndDeferred drives a profile
// carrying origin fields through identity.NewUncachedResolver against local
// servers whose handle domain hangs. Any account can assert origin fields, so
// each such event forces a fresh resolution; without the debounce shared with
// identity refresh, every event and every redrive would cost the serial user
// lane the whole identityResolveTimeout.
func TestUserConsumer_ProfileOriginTimeoutIsDebouncedAndDeferred(t *testing.T) {
	t.Parallel()

	const handleSuffix = ".originhang.test"
	const handle = "alice" + handleSuffix
	// The handle sits under the bridge PDS host's registrable domain, so the
	// identity passes the document checks and reaches handle verification.
	const bridgePDS = "https://pds.originhang.test"
	deliveries := []struct {
		name      string
		known     bool
		operation string
	}{
		{name: "known bridged user", known: true, operation: "update"},
		{name: "unknown identity discovery", known: false, operation: "create"},
	}
	for _, delivery := range deliveries {
		t.Run(delivery.name, func(t *testing.T) {
			t.Parallel()

			handleDomain := newHangableServer(t, true, nil)
			plc := newHangableServer(t, false, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/"+debouncedUserDID {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"id":%q,"alsoKnownAs":["at://%s"],`+
					`"service":[{"id":"#atproto_pds","type":"AtprotoPersonalDataServer","serviceEndpoint":%q}]}`,
					debouncedUserDID, handle, bridgePDS)
			})
			config := identity.DefaultConfig(identity.WithPrivateHostsAllowed(), identity.WithWellKnownHosts(map[string]string{
				handleSuffix: strings.TrimPrefix(handleDomain.server.URL, "http://"),
			}))
			config.PLCURL = plc.server.URL
			fixture := newDebouncedUserConsumer(identity.NewUncachedResolver(config),
				WithUserBridgeTrust(NewBridgeTrust([]string{bridgePDS})))
			if delivery.known {
				fixture.service.users[debouncedUserDID].PDSURL = bridgePDS
			} else {
				delete(fixture.service.users, debouncedUserDID)
			}

			started := time.Now()
			err := fixture.consumer.HandleEvent(context.Background(), originProfileEvent(delivery.operation))
			require.ErrorIs(t, err, ErrUnresolvedReference, "a timed-out fresh resolution on a trusted host must be redrivable")
			require.NotErrorIs(t, err, errRedriveDeferred,
				"the event that actually ran the resolution did real work and must spend its attempt")
			require.GreaterOrEqual(t, time.Since(started), identityResolveTimeout,
				"the first event must take the timeout path, not a fast failure")
			require.Equal(t, int64(2), plc.requests.Load(),
				"the document is read once for admission and once more by the verifying lookup")
			require.Equal(t, int64(1), handleDomain.requests.Load())

			fixture.advance(time.Second)
			err = fixture.consumer.HandleEvent(context.Background(), originProfileEvent(delivery.operation))
			require.ErrorIs(t, err, ErrUnresolvedReference, "a debounced profile must still be redrivable rather than dropped")
			assert.ErrorIs(t, err, errRedriveDeferred,
				"the timeout must be recorded so the next event within the TTL defers without resolving")
			assert.Equal(t, int64(2), plc.requests.Load(), "a debounced profile must not reach the PLC directory")
			assert.Equal(t, int64(1), handleDomain.requests.Load(), "a debounced profile must not reach the handle domain")
			assert.Empty(t, fixture.service.updatedCalls, "no profile write may happen without a verified resolution")
			if !delivery.known {
				assert.NotContains(t, fixture.service.users, debouncedUserDID, "an unverified identity must not be discovered")
			}
		})
	}
}

// TestUserConsumer_ProfileOriginOutsideBridgeTrustNeverReachesHandleDomain
// drives profiles carrying origin fields from identities that can never be
// admitted through identity.NewUncachedResolver against a local PLC and a handle
// domain that hangs. Indigo's DID lookup resolves the declared handle before it
// returns and reports a hung well-known as handle.invalid with a nil error, so a
// lookup that checks bridge trust only afterwards costs the serial user lane the
// whole identityResolveTimeout on every such event and records nothing. The DID
// document alone settles both checks, so no event may reach the handle domain,
// each must be dropped without a redrive, and none may store origin fields.
func TestUserConsumer_ProfileOriginOutsideBridgeTrustNeverReachesHandleDomain(t *testing.T) {
	t.Parallel()

	const handleSuffix = ".originforeign.test"
	const handle = "alice" + handleSuffix
	const bridgePDS = "https://pds.bridge.test"
	identities := []struct {
		name        string
		declaredPDS string
	}{
		{name: "PDS outside bridge trust", declaredPDS: "https://pds.elsewhere.test"},
		{name: "declared handle outside the bridge domain", declaredPDS: bridgePDS},
	}
	deliveries := []struct {
		name      string
		known     bool
		operation string
	}{
		{name: "known bridged user", known: true, operation: "update"},
		{name: "unknown identity discovery", known: false, operation: "create"},
	}
	for _, declared := range identities {
		for _, delivery := range deliveries {
			t.Run(declared.name+"/"+delivery.name, func(t *testing.T) {
				t.Parallel()

				handleDomain := newHangableServer(t, true, nil)
				plc := newHangableServer(t, false, func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/"+debouncedUserDID {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprintf(w, `{"id":%q,"alsoKnownAs":["at://%s"],`+
						`"service":[{"id":"#atproto_pds","type":"AtprotoPersonalDataServer","serviceEndpoint":%q}]}`,
						debouncedUserDID, handle, declared.declaredPDS)
				})
				config := identity.DefaultConfig(identity.WithPrivateHostsAllowed(), identity.WithWellKnownHosts(map[string]string{
					handleSuffix: strings.TrimPrefix(handleDomain.server.URL, "http://"),
				}))
				config.PLCURL = plc.server.URL
				fixture := newDebouncedUserConsumer(identity.NewUncachedResolver(config),
					WithUserBridgeTrust(NewBridgeTrust([]string{bridgePDS})))
				if delivery.known {
					fixture.service.users[debouncedUserDID].PDSURL = bridgePDS
				} else {
					delete(fixture.service.users, debouncedUserDID)
				}

				const repeats = 3
				for attempt := 1; attempt <= repeats; attempt++ {
					started := time.Now()
					err := fixture.consumer.HandleEvent(context.Background(), originProfileEvent(delivery.operation))
					require.NoError(t, err, "attempt %d: an identity that cannot be admitted is dropped, never redriven", attempt)
					require.Less(t, time.Since(started), identityResolveTimeout/2,
						"attempt %d: the DID document settles admission without waiting on the handle domain", attempt)
					fixture.advance(time.Second)
				}
				assert.Equal(t, int64(0), handleDomain.requests.Load(),
					"an identity outside bridge trust must never reach its handle domain")
				assert.Equal(t, int64(repeats), plc.requests.Load(), "each event reads the DID document once")
				if delivery.known {
					require.Len(t, fixture.service.updatedCalls, repeats, "the rest of the profile still applies")
					for _, input := range fixture.service.updatedCalls {
						require.NotNil(t, input.DisplayName)
						assert.Equal(t, "Bridge Name", *input.DisplayName)
						for _, field := range []*string{input.Origin, input.OriginUsername, input.OriginURL, input.OriginSoftware} {
							require.NotNil(t, field, "the origin fields must be cleared, not left as they were")
							assert.Empty(t, *field)
						}
					}
				} else {
					assert.NotContains(t, fixture.service.users, debouncedUserDID, "an inadmissible identity must not be discovered")
					assert.Empty(t, fixture.service.updatedCalls)
				}
			})
		}
	}
}

// TestUserConsumer_ProfileOriginDocumentSwapOnVerifyingReadIsDebounced drives a
// profile carrying origin fields through identity.NewUncachedResolver against a
// local PLC that answers the admission read with a trusted bridge document and
// the verifying read with a document on an untrusted PDS whose foreign handle
// domain hangs. Indigo spends the whole identityResolveTimeout on that handle
// and reports handle.invalid with a nil error, so the verifying answer is
// rejected as not admitted. That rejection must still record the timeout:
// otherwise every origin-carrying event from the DID stalls the serial user
// lane for the full timeout.
func TestUserConsumer_ProfileOriginDocumentSwapOnVerifyingReadIsDebounced(t *testing.T) {
	t.Parallel()

	const bridgeSuffix = ".originswap.test"
	const bridgeHandle = "alice" + bridgeSuffix
	const bridgePDS = "https://pds.originswap.test"
	const foreignSuffix = ".originswapforeign.test"
	const foreignHandle = "alice" + foreignSuffix
	const foreignPDS = "https://pds.originswapforeign.test"
	deliveries := []struct {
		name      string
		known     bool
		operation string
	}{
		{name: "known bridged user", known: true, operation: "update"},
		{name: "unknown identity discovery", known: false, operation: "create"},
	}
	for _, delivery := range deliveries {
		t.Run(delivery.name, func(t *testing.T) {
			t.Parallel()

			bridgeHandleDomain := newHangableServer(t, false, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNotFound)
			})
			foreignHandleDomain := newHangableServer(t, true, nil)
			var documentReads atomic.Int64
			plc := newHangableServer(t, false, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/"+debouncedUserDID {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				handle, pds := bridgeHandle, bridgePDS
				if documentReads.Add(1)%2 == 0 {
					handle, pds = foreignHandle, foreignPDS
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"id":%q,"alsoKnownAs":["at://%s"],`+
					`"service":[{"id":"#atproto_pds","type":"AtprotoPersonalDataServer","serviceEndpoint":%q}]}`,
					debouncedUserDID, handle, pds)
			})
			config := identity.DefaultConfig(identity.WithPrivateHostsAllowed(), identity.WithWellKnownHosts(map[string]string{
				bridgeSuffix:  strings.TrimPrefix(bridgeHandleDomain.server.URL, "http://"),
				foreignSuffix: strings.TrimPrefix(foreignHandleDomain.server.URL, "http://"),
			}))
			config.PLCURL = plc.server.URL
			fixture := newDebouncedUserConsumer(identity.NewUncachedResolver(config),
				WithUserBridgeTrust(NewBridgeTrust([]string{bridgePDS})))
			if delivery.known {
				fixture.service.users[debouncedUserDID].PDSURL = bridgePDS
			} else {
				delete(fixture.service.users, debouncedUserDID)
			}

			started := time.Now()
			err := fixture.consumer.HandleEvent(context.Background(), originProfileEvent(delivery.operation))
			require.NoError(t, err, "a verifying read that lands outside bridge trust is not admitted and never redriven")
			require.GreaterOrEqual(t, time.Since(started), identityResolveTimeout,
				"the first event must spend the timeout on the swapped document's handle domain")
			require.Equal(t, int64(2), plc.requests.Load(), "the document is read for admission and again for verification")
			require.Equal(t, int64(1), foreignHandleDomain.requests.Load())
			require.Equal(t, int64(0), bridgeHandleDomain.requests.Load())
			require.True(t, fixture.consumer.identityRefreshFailures.recentlyFailed(debouncedUserDID),
				"the timeout must be recorded even though the verifying answer was not admitted")
			if delivery.known {
				require.Len(t, fixture.service.updatedCalls, 1, "the rest of the profile still applies")
				for _, field := range []*string{
					fixture.service.updatedCalls[0].Origin, fixture.service.updatedCalls[0].OriginUsername,
					fixture.service.updatedCalls[0].OriginURL, fixture.service.updatedCalls[0].OriginSoftware,
				} {
					require.NotNil(t, field, "the origin fields must be cleared, not left as they were")
					assert.Empty(t, *field)
				}
			} else {
				assert.NotContains(t, fixture.service.users, debouncedUserDID, "an inadmissible identity must not be discovered")
				assert.Empty(t, fixture.service.updatedCalls)
			}

			fixture.advance(time.Second)
			started = time.Now()
			err = fixture.consumer.HandleEvent(context.Background(), originProfileEvent(delivery.operation))
			assert.Less(t, time.Since(started), identityResolveTimeout/2, "a debounced event must not stall the lane")
			require.ErrorIs(t, err, ErrUnresolvedReference, "a debounced profile must still be redrivable rather than dropped")
			assert.ErrorIs(t, err, errRedriveDeferred, "the event within the TTL defers without resolving")
			assert.Equal(t, int64(2), plc.requests.Load(), "a debounced profile must not reach the PLC directory")
			assert.Equal(t, int64(1), foreignHandleDomain.requests.Load(), "a debounced profile must not reach the handle domain")
			assert.Equal(t, int64(0), bridgeHandleDomain.requests.Load())
		})
	}
}
