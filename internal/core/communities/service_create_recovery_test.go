//go:build integration

package communities_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"

	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/blobs"
	"Coves/internal/core/communities"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Creating a community makes a PDS account first and stores its credentials
// last. Anything that fails between the two, or a firehose consumer that indexes
// the profile record before the final save, used to leave an account nobody
// holds the password to and a community row the instance does not host. These
// tests drive each of those failures against the real PDS and Postgres and
// assert the community ends up hosted with working credentials.

const recoveryDisplayName = "Recovered Community"

// recoveryRepository wraps the real repository to inject faults into the two
// calls that persist a created community's credentials.
type recoveryRepository struct {
	communities.Repository
	mu sync.Mutex
	// beforeCreate runs ahead of every Create; a non-nil error is returned
	// instead of delegating.
	beforeCreate func(ctx context.Context, community *communities.Community) error
	// attachFailures is how many AttachHostedCredentials calls fail before the
	// wrapper starts delegating.
	attachFailures int
	// lostCreateAcks is how many Create calls perform the real write and then
	// report a connection error, as a commit whose acknowledgement was lost.
	lostCreateAcks int
}

func (r *recoveryRepository) Create(ctx context.Context, community *communities.Community, options ...communities.CreateOption) (*communities.Community, error) {
	if r.beforeCreate != nil {
		if err := r.beforeCreate(ctx, community); err != nil {
			return nil, err
		}
	}
	created, err := r.Repository.Create(ctx, community, options...)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	lose := r.lostCreateAcks > 0
	if lose {
		r.lostCreateAcks--
	}
	r.mu.Unlock()
	if lose {
		return nil, errors.New("injected: connection reset after commit")
	}
	return created, nil
}

func (r *recoveryRepository) AttachHostedCredentials(ctx context.Context, community *communities.Community) error {
	r.mu.Lock()
	fail := r.attachFailures > 0
	if fail {
		r.attachFailures--
	}
	r.mu.Unlock()
	if fail {
		return errors.New("injected: connection reset during credential attach")
	}
	return r.Repository.AttachHostedCredentials(ctx, community)
}

// failOnce returns a beforeCreate hook that fails its first call only.
func failOnce(err error) func(context.Context, *communities.Community) error {
	var once sync.Once
	return func(context.Context, *communities.Community) error {
		var injected error
		once.Do(func() { injected = err })
		return injected
	}
}

// consumerWinsRace returns a beforeCreate hook that, on its first call, indexes
// the community's just-written profile record through the real community
// consumer, as the firehose would, and checks the row it left carries no
// credentials. Later calls do nothing.
func consumerWinsRace(t *testing.T, db *sql.DB, inner communities.Repository, pdsServer *testkit.PDS) func(context.Context, *communities.Community) error {
	var once sync.Once
	return func(ctx context.Context, community *communities.Community) error {
		var raceErr error
		once.Do(func() {
			record, cid, err := readProfileRecord(ctx, pdsServer, community.DID)
			if err != nil {
				raceErr = err
				return
			}
			consumer := jetstream.NewCommunityEventConsumer(inner, instanceDID, true, nil)
			if err := consumer.HandleEvent(ctx, profileEvent(community.DID, "create", "self", cid, record)); err != nil {
				raceErr = err
				return
			}
			indexed, err := inner.GetByDID(ctx, community.DID)
			if err != nil {
				raceErr = err
				return
			}
			assert.Empty(t, indexed.PDSPassword, "the consumer-indexed row must start without a password")
			assert.Empty(t, indexed.PDSRefreshToken, "the consumer-indexed row must start without tokens")
			assert.NotContains(t, hostedDIDs(t, db), community.DID, "the consumer-indexed row must start unhosted")
		})
		return raceErr
	}
}

func readProfileRecord(ctx context.Context, pdsServer *testkit.PDS, did string) (map[string]any, string, error) {
	var resp struct {
		CID   string         `json:"cid"`
		Value map[string]any `json:"value"`
	}
	err := pdsServer.Anon.Query(ctx, "com.atproto.repo.getRecord", url.Values{
		"repo": {did}, "collection": {"social.coves.community.profile"}, "rkey": {"self"},
	}, &resp)
	return resp.Value, resp.CID, err
}

func hostedDIDs(t *testing.T, db *sql.DB) []string {
	t.Helper()
	dids, err := postgres.NewHostedCommunityQuery(db).HostedCommunityDIDs(context.Background())
	require.NoError(t, err)
	return dids
}

// failingBlobService refuses every upload.
type failingBlobService struct{ blobs.Service }

func (failingBlobService) UploadBlob(context.Context, blobs.BlobOwner, []byte, string) (*blobs.BlobRef, error) {
	return nil, errors.New("injected: blob upload failed")
}

// faultingPDSProxy fronts the real PDS and fails requests to the given XRPC
// paths with 502. With forwardFirst the request reaches the PDS and only its
// response is lost.
func faultingPDSProxy(t *testing.T, pdsServer *testkit.PDS, forwardFirst bool, paths ...string) string {
	t.Helper()
	target, err := url.Parse(pdsServer.URL())
	require.NoError(t, err)
	forward := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(target) }}
	failing := make(map[string]bool, len(paths))
	for _, path := range paths {
		failing["/xrpc/"+path] = true
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !failing[r.URL.Path] {
			forward.ServeHTTP(w, r)
			return
		}
		if forwardFirst {
			forward.ServeHTTP(httptest.NewRecorder(), r)
		}
		http.Error(w, `{"error":"BadGateway","message":"injected"}`, http.StatusBadGateway)
	}))
	t.Cleanup(proxy.Close)
	return proxy.URL
}

func recoveryRequest(name, creator string) communities.CreateCommunityRequest {
	return communities.CreateCommunityRequest{
		Name:                   name,
		DisplayName:            recoveryDisplayName,
		Description:            "a community whose creation was interrupted",
		Visibility:             "public",
		CreatedByDID:           creator,
		AllowExternalDiscovery: true,
	}
}

func recoveryName(t *testing.T, prefix string) string {
	t.Helper()
	name := testkit.UniqueIDWithPrefix(t, prefix)
	require.LessOrEqualf(t, len("c-"+name), testkit.MaxIDLength,
		"the generated community name %q makes a handle label the PDS will refuse", name)
	return name
}

func resolveCommunityHandle(t *testing.T, pdsServer *testkit.PDS, name string) string {
	t.Helper()
	var resolved struct {
		DID string `json:"did"`
	}
	require.NoError(t, pdsServer.Anon.Query(context.Background(), "com.atproto.identity.resolveHandle",
		url.Values{"handle": {"c-" + name + "." + instanceDomain}}, &resolved),
		"the failed create must have left a PDS account behind for this test to be about recovery")
	return resolved.DID
}

// requireHostedWithWorkingCredentials asserts the community row for did holds
// credentials that open a session on that account, the instance hosts it, and
// the PDS profile record carries the requested display name. It returns the
// stored row.
func requireHostedWithWorkingCredentials(t *testing.T, db *sql.DB, repo communities.Repository, pdsServer *testkit.PDS, did string) *communities.Community {
	t.Helper()
	ctx := context.Background()
	stored, err := repo.GetByDID(ctx, did)
	require.NoError(t, err)
	require.NotEmpty(t, stored.PDSPassword, "the community row holds no password: nobody can ever log into its account")
	session := pdsServer.Login(t, stored.Handle, stored.PDSPassword)
	assert.Equal(t, did, session.DID, "the stored credentials open a different account")
	assert.Contains(t, hostedDIDs(t, db), did, "the community has an account but the instance does not host it")
	_, err = repo.GetProvisioningAccountByHandle(ctx, stored.Handle)
	assert.ErrorIs(t, err, communities.ErrProvisioningAccountNotFound,
		"a finished create must delete its provisioning account, a second sealed copy of the password")
	record, _, err := readProfileRecord(ctx, pdsServer, did)
	require.NoError(t, err, "the community account has no profile record")
	assert.Equal(t, recoveryDisplayName, record["displayName"])
	return stored
}

func TestService_CreateSurvivesConsumerIndexingFirst(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	pdsServer := testkit.NewPDS(t)
	inner := postgres.NewCommunityRepository(db, credentialciphertest.Fixed())
	service, _ := buildCommunityService(t, db, pdsServer, withRepositoryWrapper(func(communities.Repository) communities.Repository {
		return &recoveryRepository{Repository: inner, beforeCreate: consumerWinsRace(t, db, inner, pdsServer)}
	}))

	community, err := service.CreateCommunity(ctx, recoveryRequest(recoveryName(t, "race"), "did:plc:recoveryrace"))
	require.NoError(t, err, "the consumer indexing the profile first must not fail the create")
	before := requireHostedWithWorkingCredentials(t, db, inner, pdsServer, community.DID)

	record, cid, err := readProfileRecord(ctx, pdsServer, community.DID)
	require.NoError(t, err)
	consumer := jetstream.NewCommunityEventConsumer(inner, instanceDID, true, nil)
	require.NoError(t, consumer.HandleEvent(ctx, profileEvent(community.DID, "create", "self", cid, record)))
	record["description"] = "edited after creation"
	require.NoError(t, consumer.HandleEvent(ctx, profileEvent(community.DID, "update", "self", cid, record)))

	after := requireHostedWithWorkingCredentials(t, db, inner, pdsServer, community.DID)
	assert.Equal(t, before.PDSPassword, after.PDSPassword, "a firehose event replaced the stored password")
	assert.Equal(t, before.PDSAccessToken, after.PDSAccessToken, "a firehose event replaced the stored access token")
	assert.Equal(t, before.PDSRefreshToken, after.PDSRefreshToken, "a firehose event replaced the stored refresh token")
}

// createRecoveryCase is one step at which a first create fails after the PDS
// account exists.
type createRecoveryCase struct {
	name    string
	prepare func(req *communities.CreateCommunityRequest)
	failing func(t *testing.T, pdsServer *testkit.PDS) []communityServiceOption
}

var profileWriteFails = createRecoveryCase{
	name: "profile record write fails",
	failing: func(t *testing.T, pdsServer *testkit.PDS) []communityServiceOption {
		return []communityServiceOption{withRecordPDSURL(faultingPDSProxy(t, pdsServer, false,
			"com.atproto.repo.createRecord", "com.atproto.repo.putRecord"))}
	},
}

// failFirstCreate runs a create that fails at the case's step and returns the
// DID of the PDS account it left behind.
func failFirstCreate(t *testing.T, db *sql.DB, pdsServer *testkit.PDS, tc createRecoveryCase, req communities.CreateCommunityRequest) string {
	t.Helper()
	service, _ := buildCommunityService(t, db, pdsServer, tc.failing(t, pdsServer)...)
	_, err := service.CreateCommunity(context.Background(), req)
	require.Error(t, err, "the injected failure did not fail the first create")
	return resolveCommunityHandle(t, pdsServer, req.Name)
}

func TestService_CreateRetryResumesAfterFailureOnceTheAccountExists(t *testing.T) {
	t.Parallel()

	persistentSaveFailure := func(*testing.T, *testkit.PDS) []communityServiceOption {
		return []communityServiceOption{withRepositoryWrapper(func(inner communities.Repository) communities.Repository {
			return &recoveryRepository{Repository: inner, beforeCreate: func(context.Context, *communities.Community) error {
				return errors.New("injected: database unavailable")
			}}
		})}
	}
	failingBlobs := func(*testing.T, *testkit.PDS) []communityServiceOption {
		return []communityServiceOption{withBlobServiceWrapper(func(inner blobs.Service) blobs.Service {
			return failingBlobService{Service: inner}
		})}
	}

	for _, tc := range []createRecoveryCase{
		{
			name: "createAccount response lost",
			failing: func(t *testing.T, pdsServer *testkit.PDS) []communityServiceOption {
				return []communityServiceOption{withProvisionerPDSURL(
					faultingPDSProxy(t, pdsServer, true, "com.atproto.server.createAccount"))}
			},
		},
		{
			name: "avatar upload fails",
			prepare: func(req *communities.CreateCommunityRequest) {
				req.AvatarBlob, req.AvatarMimeType = testkit.TestPNG(32, 32), "image/png"
			},
			failing: failingBlobs,
		},
		{
			name: "banner upload fails",
			prepare: func(req *communities.CreateCommunityRequest) {
				req.BannerBlob, req.BannerMimeType = testkit.TestPNG(64, 32), "image/png"
			},
			failing: failingBlobs,
		},
		profileWriteFails,
		{name: "final save fails persistently", failing: persistentSaveFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testkit.DB(t)
			pdsServer := testkit.NewPDS(t)
			req := recoveryRequest(recoveryName(t, "rsm"), "did:plc:recoveryretry")
			if tc.prepare != nil {
				tc.prepare(&req)
			}
			orphanDID := failFirstCreate(t, db, pdsServer, tc, req)

			retryService, repo := buildCommunityService(t, db, pdsServer)
			community, err := retryService.CreateCommunity(context.Background(), req)
			require.NoError(t, err, "retrying the same create for the same creator must resume the existing account")
			assert.Equal(t, orphanDID, community.DID, "the retry must reuse the account the failed create made")
			requireHostedWithWorkingCredentials(t, db, repo, pdsServer, orphanDID)
		})
	}
}

func TestService_CreateRetriesTransientPersistenceFailures(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		wrap func(t *testing.T, db *sql.DB, inner communities.Repository, pdsServer *testkit.PDS) communities.Repository
	}{
		{
			name: "final save fails once",
			wrap: func(_ *testing.T, _ *sql.DB, inner communities.Repository, _ *testkit.PDS) communities.Repository {
				return &recoveryRepository{Repository: inner, beforeCreate: failOnce(errors.New("injected: connection reset"))}
			},
		},
		{
			name: "final save commits but its acknowledgement is lost",
			wrap: func(_ *testing.T, _ *sql.DB, inner communities.Repository, _ *testkit.PDS) communities.Repository {
				return &recoveryRepository{Repository: inner, lostCreateAcks: 1}
			},
		},
		{
			name: "consumer indexes first and credential attach fails once",
			wrap: func(t *testing.T, db *sql.DB, inner communities.Repository, pdsServer *testkit.PDS) communities.Repository {
				return &recoveryRepository{
					Repository:     inner,
					beforeCreate:   consumerWinsRace(t, db, inner, pdsServer),
					attachFailures: 1,
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testkit.DB(t)
			pdsServer := testkit.NewPDS(t)
			inner := postgres.NewCommunityRepository(db, credentialciphertest.Fixed())
			service, _ := buildCommunityService(t, db, pdsServer, withRepositoryWrapper(func(communities.Repository) communities.Repository {
				return tc.wrap(t, db, inner, pdsServer)
			}))

			community, err := service.CreateCommunity(context.Background(),
				recoveryRequest(recoveryName(t, "tsp"), "did:plc:recoverytransient"))
			require.NoError(t, err, "a single transient persistence error must not fail the create")
			requireHostedWithWorkingCredentials(t, db, inner, pdsServer, community.DID)
		})
	}
}

func TestService_CreateByAnotherCreatorDoesNotTakeAnInterruptedAccount(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	pdsServer := testkit.NewPDS(t)
	name := recoveryName(t, "own")
	creatorRequest := recoveryRequest(name, "did:plc:recoveryownera")
	orphanDID := failFirstCreate(t, db, pdsServer, profileWriteFails, creatorRequest)

	service, repo := buildCommunityService(t, db, pdsServer)
	_, err := service.CreateCommunity(ctx, recoveryRequest(name, "did:plc:recoveryotherb"))
	require.ErrorIs(t, err, communities.ErrHandleTaken,
		"another creator asking for the same name must be told it is taken")

	community, err := service.CreateCommunity(ctx, creatorRequest)
	require.NoError(t, err, "the original creator's retry must still resume the account")
	assert.Equal(t, orphanDID, community.DID)
	requireHostedWithWorkingCredentials(t, db, repo, pdsServer, orphanDID)
}

// sessionFaultingPDSProxy fronts the real PDS and, while the returned switch is
// on, fails com.atproto.server.createSession with 502 without reaching the PDS.
func sessionFaultingPDSProxy(t *testing.T, pdsServer *testkit.PDS) (string, *atomic.Bool) {
	t.Helper()
	target, err := url.Parse(pdsServer.URL())
	require.NoError(t, err)
	forward := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(target) }}
	failing := &atomic.Bool{}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failing.Load() && r.URL.Path == "/xrpc/com.atproto.server.createSession" {
			http.Error(w, `{"error":"BadGateway","message":"injected"}`, http.StatusBadGateway)
			return
		}
		forward.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)
	return proxy.URL, failing
}

func TestService_CreateRetryKeepsTheAccountWhenTheSessionServerFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	pdsServer := testkit.NewPDS(t)
	req := recoveryRequest(recoveryName(t, "ses"), "did:plc:recoverysession")
	proxyURL, sessionFails := sessionFaultingPDSProxy(t, pdsServer)

	firstOptions := append(profileWriteFails.failing(t, pdsServer), withProvisionerPDSURL(proxyURL))
	firstService, _ := buildCommunityService(t, db, pdsServer, firstOptions...)
	_, err := firstService.CreateCommunity(ctx, req)
	require.Error(t, err, "the injected profile write failure did not fail the first create")
	orphanDID := resolveCommunityHandle(t, pdsServer, req.Name)

	sessionFails.Store(true)
	failingRetry, _ := buildCommunityService(t, db, pdsServer)
	_, err = failingRetry.CreateCommunity(ctx, req)
	require.Error(t, err, "a retry whose session request hit a 502 must fail rather than report success")

	sessionFails.Store(false)
	retryService, repo := buildCommunityService(t, db, pdsServer)
	community, err := retryService.CreateCommunity(ctx, req)
	require.NoError(t, err, "a 502 from createSession must keep the saved credentials so a later retry can still resume")
	assert.Equal(t, orphanDID, community.DID, "the retry must reuse the account the failed create made")
	requireHostedWithWorkingCredentials(t, db, repo, pdsServer, orphanDID)
}

const createAccountPath = "/xrpc/com.atproto.server.createAccount"

// createAccountProxy fronts the real PDS, forwards every request except
// createAccount, and hands each createAccount to handle with its 1-based call
// number and a function that forwards it to the PDS.
func createAccountProxy(t *testing.T, pdsServer *testkit.PDS,
	handle func(call int64, w http.ResponseWriter, r *http.Request, forward func(http.ResponseWriter)),
) string {
	t.Helper()
	target, err := url.Parse(pdsServer.URL())
	require.NoError(t, err)
	reverse := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(target) }}
	var calls atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != createAccountPath {
			reverse.ServeHTTP(w, r)
			return
		}
		handle(calls.Add(1), w, r, func(to http.ResponseWriter) { reverse.ServeHTTP(to, r) })
	}))
	t.Cleanup(proxy.Close)
	return proxy.URL
}

// refusingPDSProxy fronts the real PDS and answers requests to the given XRPC
// paths with 400 without reaching the PDS. A 400 is not retried, so the
// failure is immediate.
func refusingPDSProxy(t *testing.T, pdsServer *testkit.PDS, paths ...string) string {
	t.Helper()
	target, err := url.Parse(pdsServer.URL())
	require.NoError(t, err)
	forward := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(target) }}
	refused := make(map[string]bool, len(paths))
	for _, path := range paths {
		refused["/xrpc/"+path] = true
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if refused[r.URL.Path] {
			http.Error(w, `{"error":"InvalidRequest","message":"injected"}`, http.StatusBadRequest)
			return
		}
		forward.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)
	return proxy.URL
}

// The HTTP client retries a createAccount whose response was lost; the PDS
// refuses the retry because the first attempt took the handle. That refusal
// must not discard the saved credentials of the account the first attempt made.
func TestService_CreateSucceedsWhenOnlyTheFirstCreateAccountResponseIsLost(t *testing.T) {
	t.Parallel()
	db := testkit.DB(t)
	pdsServer := testkit.NewPDS(t)
	proxyURL := createAccountProxy(t, pdsServer, func(call int64, w http.ResponseWriter, _ *http.Request, forward func(http.ResponseWriter)) {
		if call == 1 {
			forward(httptest.NewRecorder())
			http.Error(w, `{"error":"BadGateway","message":"injected"}`, http.StatusBadGateway)
			return
		}
		forward(w)
	})
	service, repo := buildCommunityService(t, db, pdsServer, withProvisionerPDSURL(proxyURL))
	req := recoveryRequest(recoveryName(t, "lst"), "did:plc:recoverylost")

	community, err := service.CreateCommunity(context.Background(), req)
	require.NoError(t, err, "a createAccount retry refused because the lost first attempt took the handle must resume that account")
	assert.Equal(t, resolveCommunityHandle(t, pdsServer, req.Name), community.DID)
	requireHostedWithWorkingCredentials(t, db, repo, pdsServer, community.DID)
}

// Two creates of the same name by the same creator overlap: the second finds
// the first's provisioning account before the first's createAccount reaches
// the PDS, so it signs in, is refused, and creates the account itself. The
// first's createAccount is then refused because the handle is taken. Neither
// refusal may discard the saved credentials.
func TestService_OverlappingCreatesBySameCreatorKeepTheAccount(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	pdsServer := testkit.NewPDS(t)
	firstArrived := make(chan struct{})
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseFirst) }) }
	proxyURL := createAccountProxy(t, pdsServer, func(call int64, w http.ResponseWriter, _ *http.Request, forward func(http.ResponseWriter)) {
		if call == 1 {
			close(firstArrived)
			<-releaseFirst
		}
		forward(w)
	})
	// Registered after the proxy, so it runs before the proxy's Close, which
	// waits for the held request: a failed assertion must not hang the test.
	t.Cleanup(release)
	recordURL := refusingPDSProxy(t, pdsServer, "com.atproto.repo.createRecord", "com.atproto.repo.putRecord")
	req := recoveryRequest(recoveryName(t, "ovl"), "did:plc:recoveryoverlap")

	first, _ := buildCommunityService(t, db, pdsServer, withProvisionerPDSURL(proxyURL), withRecordPDSURL(recordURL))
	firstErr := make(chan error, 1)
	go func() {
		_, err := first.CreateCommunity(ctx, req)
		firstErr <- err
	}()
	select {
	case <-firstArrived:
	case err := <-firstErr:
		t.Fatalf("the first create ended before its createAccount reached the PDS: %v", err)
	}

	second, _ := buildCommunityService(t, db, pdsServer, withProvisionerPDSURL(proxyURL), withRecordPDSURL(recordURL))
	_, err := second.CreateCommunity(ctx, req)
	require.Error(t, err, "the second create's profile write was refused, so it must fail")
	orphanDID := resolveCommunityHandle(t, pdsServer, req.Name)

	release()
	require.Error(t, <-firstErr, "the first create's profile write was refused, so it must fail")

	retryService, repo := buildCommunityService(t, db, pdsServer)
	community, err := retryService.CreateCommunity(ctx, req)
	require.NoError(t, err, "after overlapping failed creates, a retry by the same creator must resume the account")
	assert.Equal(t, orphanDID, community.DID, "the retry must reuse the account the overlapping creates made")
	requireHostedWithWorkingCredentials(t, db, repo, pdsServer, orphanDID)
}

// A createAccount that never reached the PDS leaves saved credentials and no
// account. The retry's sign-in is refused, which must lead it to create the
// account with those credentials.
func TestService_CreateRetryCreatesTheAccountWhenTheFirstNeverReachedThePDS(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	pdsServer := testkit.NewPDS(t)
	req := recoveryRequest(recoveryName(t, "nvr"), "did:plc:recoverynever")

	// The provisioning account records the PDS URL it was created against, so
	// the retry goes through this proxy too: it stops failing once the first
	// create has given up.
	var unreachable atomic.Bool
	unreachable.Store(true)
	proxyURL := createAccountProxy(t, pdsServer, func(_ int64, w http.ResponseWriter, _ *http.Request, forward func(http.ResponseWriter)) {
		if unreachable.Load() {
			http.Error(w, `{"error":"BadGateway","message":"injected"}`, http.StatusBadGateway)
			return
		}
		forward(w)
	})

	firstService, repo := buildCommunityService(t, db, pdsServer, withProvisionerPDSURL(proxyURL))
	_, err := firstService.CreateCommunity(ctx, req)
	require.Error(t, err, "a createAccount that never reached the PDS must fail the create")
	_, err = repo.GetProvisioningAccountByHandle(ctx, "c-"+req.Name+"."+instanceDomain)
	require.NoError(t, err, "a 502 says nothing about whether the account exists, so the saved credentials must stay")

	unreachable.Store(false)
	retryService, retryRepo := buildCommunityService(t, db, pdsServer)
	community, err := retryService.CreateCommunity(ctx, req)
	require.NoError(t, err, "the retry must create the account the first create never made")
	assert.Equal(t, resolveCommunityHandle(t, pdsServer, req.Name), community.DID)
	requireHostedWithWorkingCredentials(t, db, retryRepo, pdsServer, community.DID)
}

// A createAccount the PDS refuses, with no account behind the saved
// credentials, frees the name: the provisioning account is deleted and another
// creator can take it.
func TestService_CreateRefusedByThePDSFreesTheName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	pdsServer := testkit.NewPDS(t)
	name := recoveryName(t, "rfs")

	refusedService, repo := buildCommunityService(t, db, pdsServer,
		withProvisionerPDSURL(refusingPDSProxy(t, pdsServer, "com.atproto.server.createAccount")))
	_, err := refusedService.CreateCommunity(ctx, recoveryRequest(name, "did:plc:recoveryrefuseda"))
	require.Error(t, err, "a refused createAccount must fail the create")
	require.NotErrorIs(t, err, communities.ErrHandleTaken,
		"a PDS refusal that is not about the handle must not be reported as a taken name")
	_, err = repo.GetProvisioningAccountByHandle(ctx, "c-"+name+"."+instanceDomain)
	require.ErrorIs(t, err, communities.ErrProvisioningAccountNotFound,
		"a refused createAccount with no account behind it must free the name")

	service, otherRepo := buildCommunityService(t, db, pdsServer)
	community, err := service.CreateCommunity(ctx, recoveryRequest(name, "did:plc:recoveryrefusedb"))
	require.NoError(t, err, "another creator must be able to take a name whose create the PDS refused")
	requireHostedWithWorkingCredentials(t, db, otherRepo, pdsServer, community.DID)
}

// A finished create deletes its provisioning account, so the name is held only
// by the community row. Another creator asking for it must be told the name is
// taken, not fail with an unmapped PDS refusal, and must be refused before any
// createAccount reaches the PDS: if the row's PDS account was deleted or
// renamed, createAccount would succeed and orphan a new account.
func TestService_CreateOfAFinishedCommunityNameByAnotherCreatorIsNameTaken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	pdsServer := testkit.NewPDS(t)
	name := recoveryName(t, "tkn")

	service, repo := buildCommunityService(t, db, pdsServer)
	community, err := service.CreateCommunity(ctx, recoveryRequest(name, "did:plc:recoverytakena"))
	require.NoError(t, err)

	var createAccountCalls atomic.Int64
	proxyURL := createAccountProxy(t, pdsServer, func(_ int64, w http.ResponseWriter, _ *http.Request, forward func(http.ResponseWriter)) {
		createAccountCalls.Add(1)
		forward(w)
	})
	otherService, _ := buildCommunityService(t, db, pdsServer, withProvisionerPDSURL(proxyURL))
	_, err = otherService.CreateCommunity(ctx, recoveryRequest(name, "did:plc:recoverytakenb"))
	require.ErrorIs(t, err, communities.ErrHandleTaken,
		"another creator asking for a finished community's name must be told it is taken")
	assert.Zero(t, createAccountCalls.Load(),
		"a name a community row already holds must be refused before any createAccount reaches the PDS")
	requireHostedWithWorkingCredentials(t, db, repo, pdsServer, community.DID)
}

// A PDS account nobody here holds credentials for already has the handle. The
// PDS refusal must reach the caller as a taken name.
func TestService_CreateOfAHandleTakenOnThePDSIsNameTaken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	pdsServer := testkit.NewPDS(t)
	name := recoveryName(t, "pdt")
	handle := "c-" + name + "." + instanceDomain
	pdsServer.CreateAccount(t, testkit.WithHandle(handle))

	service, repo := buildCommunityService(t, db, pdsServer)
	_, err := service.CreateCommunity(ctx, recoveryRequest(name, "did:plc:recoverypdstaken"))
	require.ErrorIs(t, err, communities.ErrHandleTaken,
		"a createAccount refused because another account holds the handle must report the name as taken")
	_, err = repo.GetProvisioningAccountByHandle(ctx, handle)
	assert.ErrorIs(t, err, communities.ErrProvisioningAccountNotFound,
		"credentials for an account that can never exist must not be kept")
}

// Two creates of the same name by the same creator overlap: the second finds
// the first's provisioning account while the first's createAccount is held,
// signs in and is refused, and its own createAccount is rate limited. A 429 is
// a refusal a retry could change, so it must not discard the saved
// credentials the first create is still using.
func TestService_RateLimitedOverlappingCreateKeepsTheCredentials(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	pdsServer := testkit.NewPDS(t)
	firstArrived := make(chan struct{})
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseFirst) }) }
	proxyURL := createAccountProxy(t, pdsServer, func(call int64, w http.ResponseWriter, _ *http.Request, forward func(http.ResponseWriter)) {
		switch call {
		case 1:
			close(firstArrived)
			<-releaseFirst
		case 2:
			http.Error(w, `{"error":"RateLimitExceeded","message":"injected"}`, http.StatusTooManyRequests)
			return
		}
		forward(w)
	})
	// Registered after the proxy, so it runs before the proxy's Close, which
	// waits for the held request: a failed assertion must not hang the test.
	t.Cleanup(release)
	recordURL := refusingPDSProxy(t, pdsServer, "com.atproto.repo.createRecord", "com.atproto.repo.putRecord")
	req := recoveryRequest(recoveryName(t, "rtl"), "did:plc:recoveryratelimit")

	first, _ := buildCommunityService(t, db, pdsServer, withProvisionerPDSURL(proxyURL), withRecordPDSURL(recordURL))
	firstErr := make(chan error, 1)
	go func() {
		_, err := first.CreateCommunity(ctx, req)
		firstErr <- err
	}()
	select {
	case <-firstArrived:
	case err := <-firstErr:
		t.Fatalf("the first create ended before its createAccount reached the PDS: %v", err)
	}

	second, _ := buildCommunityService(t, db, pdsServer, withProvisionerPDSURL(proxyURL), withRecordPDSURL(recordURL))
	_, err := second.CreateCommunity(ctx, req)
	require.Error(t, err, "the second create's createAccount was rate limited, so it must fail")

	release()
	require.Error(t, <-firstErr, "the first create's profile write was refused, so it must fail")
	orphanDID := resolveCommunityHandle(t, pdsServer, req.Name)

	retryService, repo := buildCommunityService(t, db, pdsServer)
	community, err := retryService.CreateCommunity(ctx, req)
	require.NoError(t, err, "a rate-limited overlapping create must keep the credentials, so a retry resumes the account")
	assert.Equal(t, orphanDID, community.DID, "the retry must reuse the account the first create made")
	requireHostedWithWorkingCredentials(t, db, repo, pdsServer, orphanDID)
}
