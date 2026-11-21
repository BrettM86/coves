//go:build integration

package communities_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/communities"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// CreateCommunity indexes the creator's owner record locally at the commit rev
// the PDS wrote it in, under the same rev gate the firehose copy of that commit
// meets, so the two together are one roster change.

// pdsRecord is one record as com.atproto.repo.listRecords returns it.
type pdsRecord struct {
	URI   string         `json:"uri"`
	CID   string         `json:"cid"`
	Value map[string]any `json:"value"`
}

// listRoleRecords reads the role records in a community's repo from the PDS.
func listRoleRecords(t *testing.T, pdsServer *testkit.PDS, communityDID string) []pdsRecord {
	t.Helper()
	var resp struct {
		Records []pdsRecord `json:"records"`
	}
	require.NoError(t, pdsServer.Anon.Query(context.Background(), "com.atproto.repo.listRecords", url.Values{
		"repo": {communityDID}, "collection": {communities.RoleCollection},
	}, &resp))
	return resp.Records
}

// latestCommitRev is the rev of the repo's newest commit.
func latestCommitRev(t *testing.T, pdsServer *testkit.PDS, did string) string {
	t.Helper()
	var resp struct {
		Rev string `json:"rev"`
	}
	require.NoError(t, pdsServer.Anon.Query(context.Background(), "com.atproto.sync.getLatestCommit",
		url.Values{"did": {did}}, &resp))
	require.NotEmpty(t, resp.Rev)
	return resp.Rev
}

// recordRev is the rev the gate holds for a record URI; false when no handler
// claimed it.
func recordRev(t *testing.T, db *sql.DB, recordURI string) (string, bool) {
	t.Helper()
	var rev string
	err := db.QueryRow(`SELECT rev FROM jetstream_record_revs WHERE record_uri = $1`, recordURI).Scan(&rev)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false
	}
	require.NoError(t, err)
	return rev, true
}

func indexedRoleCount(t *testing.T, db *sql.DB, communityDID string) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM community_roles WHERE community_did = $1`, communityDID).Scan(&count))
	return count
}

func rosterVersion(t *testing.T, db *sql.DB, communityDID string) int64 {
	t.Helper()
	state, err := postgres.NewCommunitySubjectStateReader(db).ReadCommunitySubjectState(context.Background(), communityDID, communityDID, "roster")
	require.NoError(t, err)
	require.NotNil(t, state, "the reader must return the roster state of %s", communityDID)
	return state.Version
}

func recordKeyOf(t *testing.T, recordURI string) string {
	t.Helper()
	index := strings.LastIndex(recordURI, "/")
	require.Positive(t, index, "record URI %q", recordURI)
	return recordURI[index+1:]
}

// B6 (b)
func TestService_CreateIndexesTheOwnerRecordAtItsCommitRev(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service, _, pdsServer, db := newCommunityServiceWithDatabase(t)
	const creator = "did:plc:csownerrevcreator"

	community, err := service.CreateCommunity(ctx, recoveryRequest(recoveryName(t, "orv"), creator))
	require.NoError(t, err)
	// The owner record is the create's last write to the community's repo, so
	// the repo's newest commit right after create is the one that wrote it.
	ownerRev := latestCommitRev(t, pdsServer, community.DID)
	records := listRoleRecords(t, pdsServer, community.DID)
	require.Len(t, records, 1, "the create writes one role record, the creator's owner record")
	owner := records[0]

	stamped, ok := recordRev(t, db, owner.URI)
	require.True(t, ok, "the create must claim the owner record's rev gate")
	assert.Equal(t, ownerRev, stamped, "the gate must hold the owner record's commit rev")

	consumer := jetstream.NewCommunityEventConsumer(postgres.NewCommunityRepository(db, credentialciphertest.Fixed()), instanceDID, true, nil,
		jetstream.WithCommunityRevGate(jetstream.NewRevGate(db)),
		jetstream.WithCommunityRoster(postgres.NewCommunityRoleRepository(db)))
	require.NoError(t, consumer.HandleEvent(ctx, &jetstream.JetstreamEvent{
		Kind: "commit", Did: community.DID, TimeUS: time.Now().UnixMicro(),
		Commit: &jetstream.CommitEvent{
			Rev: ownerRev, Operation: "create", Collection: communities.RoleCollection,
			RKey: recordKeyOf(t, owner.URI), CID: owner.CID, Record: owner.Value,
		},
	}), "the firehose copy of the owner record")

	roles, err := postgres.NewCommunityRoleRepository(db).ListCommunityRoles(ctx, community.DID)
	require.NoError(t, err)
	require.Len(t, roles, 1)
	assert.Equal(t, "did:plc:csownerrevcreator", roles[0].SubjectDID)
	assert.Equal(t, communities.RoleOwner, roles[0].Role)
	assert.Equal(t, owner.CID, roles[0].RecordCID)
	assert.Equal(t, int64(1), rosterVersion(t, db, community.DID),
		"the local index and its firehose copy are one roster change")
}

// ownerRecordNormalizingProxy fronts the real PDS. Once a createdAt is stored
// in the returned value, it rewrites the createdAt of every role record written
// through createRecord or putRecord to it, so a resumed create rewrites the
// owner record with exactly the bytes the interrupted create wrote. The PDS
// itself then answers the identical write.
func ownerRecordNormalizingProxy(t *testing.T, pdsServer *testkit.PDS) (string, *atomic.Value) {
	t.Helper()
	target, err := url.Parse(pdsServer.URL())
	require.NoError(t, err)
	forward := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(target) }}
	createdAt := &atomic.Value{}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		canonical, set := createdAt.Load().(string)
		if !set || (r.URL.Path != "/xrpc/com.atproto.repo.putRecord" && r.URL.Path != "/xrpc/com.atproto.repo.createRecord") {
			forward.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		var payload map[string]any
		if json.Unmarshal(body, &payload) == nil && payload["collection"] == communities.RoleCollection {
			if record, ok := payload["record"].(map[string]any); ok {
				record["createdAt"] = canonical
				if rewritten, err := json.Marshal(payload); err == nil {
					body = rewritten
				}
			}
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		r.Header.Set("Content-Length", strconv.Itoa(len(body)))
		forward.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)
	return proxy.URL, createdAt
}

// B6 (d): a resumed create whose owner record put is identical to the record
// the interrupted create wrote produces no commit, so it has no rev to index
// at. The create succeeds without indexing the owner role locally; the
// interrupted write's firehose event indexes it under the rev gate.
func TestService_ResumedCreateWithAnIdenticalOwnerRecordIndexesNothingLocally(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := testkit.DB(t)
	pdsServer := testkit.NewPDS(t)
	proxyURL, createdAt := ownerRecordNormalizingProxy(t, pdsServer)
	req := recoveryRequest(recoveryName(t, "orr"), "did:plc:csownerresume")

	first, _ := buildCommunityService(t, db, pdsServer, withRecordPDSURL(proxyURL), withProvisionerPDSURL(proxyURL),
		withRepositoryWrapper(func(inner communities.Repository) communities.Repository {
			return &recoveryRepository{Repository: inner, beforeCreate: func(context.Context, *communities.Community) error {
				return errors.New("injected: database unavailable")
			}}
		}))
	_, err := first.CreateCommunity(ctx, req)
	require.Error(t, err, "the injected save failure did not fail the first create")
	communityDID := resolveCommunityHandle(t, pdsServer, req.Name)

	written := listRoleRecords(t, pdsServer, communityDID)
	require.Len(t, written, 1, "fixture: the interrupted create wrote the owner record")
	writtenCreatedAt, ok := written[0].Value["createdAt"].(string)
	require.True(t, ok, "fixture: the owner record carries createdAt")
	createdAt.Store(writtenCreatedAt)
	require.Zero(t, indexedRoleCount(t, db, communityDID), "fixture: the interrupted create indexed no role")

	retry, _ := buildCommunityService(t, db, pdsServer, withRecordPDSURL(proxyURL), withProvisionerPDSURL(proxyURL))
	community, err := retry.CreateCommunity(ctx, req)
	require.NoError(t, err, "a resumed create whose owner record write produced no commit must still succeed")
	assert.Equal(t, communityDID, community.DID)

	rewritten := listRoleRecords(t, pdsServer, communityDID)
	require.Len(t, rewritten, 1)
	require.Equal(t, written[0].CID, rewritten[0].CID, "fixture: the resumed owner record write was identical")

	assert.Zero(t, indexedRoleCount(t, db, communityDID), "the resumed create must not index the owner role without a commit rev")
	_, claimed := recordRev(t, db, written[0].URI)
	assert.False(t, claimed, "the resumed create must not claim the owner record's rev gate without a commit rev")
	assert.Equal(t, int64(0), rosterVersion(t, db, communityDID), "the roster version must be as it was before the resume")
}
