//go:build integration

package bridgedvotes_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"Coves/internal/core/bridgedvotes"
	"Coves/internal/db/postgres"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

const voteAggregatesPath = "/xrpc/social.coves.bridge.getVoteAggregates"

type servedAggregate struct {
	URI       string `json:"uri"`
	Upvotes   int    `json:"upvotes"`
	Downvotes int    `json:"downvotes"`
	UpdatedAt string `json:"updatedAt"`
}

type aggregateServer struct {
	mu         sync.Mutex
	aggregates map[string]servedAggregate
	requested  []string
}

func (s *aggregateServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || r.URL.Path != voteAggregatesPath {
		http.NotFound(w, r)
		return
	}

	var uris []string
	for _, value := range r.URL.Query()["uris"] {
		for _, uri := range strings.Split(value, ",") {
			if uri = strings.TrimSpace(uri); uri != "" {
				uris = append(uris, uri)
			}
		}
	}

	s.mu.Lock()
	s.requested = append(s.requested, uris...)
	aggregates := make([]servedAggregate, 0, len(uris))
	for _, uri := range uris {
		if aggregate, ok := s.aggregates[uri]; ok {
			aggregates = append(aggregates, aggregate)
		}
	}
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(struct {
		Aggregates []servedAggregate `json:"aggregates"`
	}{Aggregates: aggregates}); err != nil {
		panic(fmt.Sprintf("encode aggregate response: %v", err))
	}
}

func (s *aggregateServer) replace(aggregate servedAggregate) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.aggregates[aggregate.URI] = aggregate
}

func (s *aggregateServer) requestedURIs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requested...)
}

type storedVoteStats struct {
	nativeUp    int
	nativeDown  int
	bridgedUp   int
	bridgedDown int
	score       int
	asOf        sql.NullTime
}

type expectedVoteStats struct {
	nativeUp    int
	nativeDown  int
	bridgedUp   int
	bridgedDown int
	score       int
	asOf        *time.Time
}

func TestPollerSweepFoldsBridgeAggregatesIntoNativeContent(t *testing.T) {
	t.Parallel()

	const (
		postURI    = "at://did:plc:nativeauthor/social.coves.community.postv2/native-post"
		commentURI = "at://did:plc:nativecommenter/social.coves.community.comment/native-comment"
		controlURI = "at://did:plc:controlauthor/social.coves.community.postv2/control-post"
		t1Text     = "2026-08-31T02:04:01.080Z"
		t0Text     = "2026-08-30T12:00:00.000Z"
	)

	t1 := mustParseTime(t, t1Text)
	bridge := &aggregateServer{aggregates: map[string]servedAggregate{
		postURI:    {URI: postURI, Upvotes: 5, Downvotes: 2, UpdatedAt: t1Text},
		commentURI: {URI: commentURI, Upvotes: 2, Downvotes: 0, UpdatedAt: t1Text},
	}}
	server := httptest.NewServer(bridge)
	t.Cleanup(server.Close)

	var untrustedRequests atomic.Int64
	untrustedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		untrustedRequests.Add(1)
		http.Error(w, "untrusted community must not be polled", http.StatusInternalServerError)
	}))
	t.Cleanup(untrustedServer.Close)

	db := testkit.DB(t)
	ctx := context.Background()
	seedPollerFixtures(t, ctx, db, server.URL+"/", untrustedServer.URL, postURI, commentURI, controlURI)

	poller, err := bridgedvotes.NewPoller(
		postgres.NewBridgedVotesRepository(db),
		bridgedvotes.NewClient(server.Client()),
		[]string{server.URL},
		bridgedvotes.Options{},
	)
	require.NoError(t, err)
	requireSweep(t, ctx, poller)

	postWant := expectedVoteStats{nativeUp: 3, nativeDown: 1, bridgedUp: 5, bridgedDown: 2, score: 5, asOf: &t1}
	commentWant := expectedVoteStats{nativeUp: 4, nativeDown: 1, bridgedUp: 2, bridgedDown: 0, score: 5, asOf: &t1}
	controlWant := expectedVoteStats{nativeUp: 7, nativeDown: 2, bridgedUp: 0, bridgedDown: 0, score: 5}
	requireVoteStats(t, ctx, db, "posts", postURI, postWant)
	requireVoteStats(t, ctx, db, "comments", commentURI, commentWant)
	requireVoteStats(t, ctx, db, "posts", controlURI, controlWant)
	require.Contains(t, bridge.requestedURIs(), postURI)
	require.Contains(t, bridge.requestedURIs(), commentURI)
	require.NotContains(t, bridge.requestedURIs(), controlURI)
	require.Zero(t, untrustedRequests.Load(), "the untrusted community host must not receive a request")

	requireSweep(t, ctx, poller)
	requireVoteStats(t, ctx, db, "posts", postURI, postWant)
	requireVoteStats(t, ctx, db, "comments", commentURI, commentWant)
	requireVoteStats(t, ctx, db, "posts", controlURI, controlWant)

	bridge.replace(servedAggregate{URI: postURI, Upvotes: 99, Downvotes: 41, UpdatedAt: t0Text})
	requireSweep(t, ctx, poller)
	requireVoteStats(t, ctx, db, "posts", postURI, postWant)
	require.NotContains(t, bridge.requestedURIs(), controlURI)
	require.Zero(t, untrustedRequests.Load(), "the untrusted community host must remain untouched")
}

func seedPollerFixtures(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	bridgePDSURL string,
	untrustedPDSURL string,
	postURI string,
	commentURI string,
	controlURI string,
) {
	t.Helper()

	const (
		bridgeCommunityDID  = "did:plc:bridgedcommunity"
		controlCommunityDID = "did:plc:controlcommunity"
	)
	createdAt := time.Now().UTC()

	_, err := db.ExecContext(ctx, `
		INSERT INTO communities
			(did, handle, name, owner_did, created_by_did, hosted_by_did, pds_url, federated_from, created_at)
		VALUES
			($1, '!bridged@local.test', 'bridged', $1, $1, $1, $2, 'lemmy', $3),
			($4, '!control@local.test', 'control', $4, $4, $4, $5, NULL, $3)
	`, bridgeCommunityDID, bridgePDSURL, createdAt, controlCommunityDID, untrustedPDSURL)
	require.NoError(t, err, "seed communities")

	_, err = db.ExecContext(ctx, `
		INSERT INTO posts
			(uri, cid, rkey, author_did, community_did, title, created_at,
			 upvote_count, downvote_count, score, bridged_upvote_count, bridged_downvote_count, bridged_stats_as_of)
		VALUES
			($1, 'bafynativepost', 'native-post', 'did:plc:nativeauthor', $2, 'native post', $3,
			 3, 1, 2, 0, 0, NULL),
			($4, 'bafycontrolpost', 'control-post', 'did:plc:controlauthor', $5, 'control post', $3,
			 7, 2, 5, 0, 0, NULL)
	`, postURI, bridgeCommunityDID, createdAt, controlURI, controlCommunityDID)
	require.NoError(t, err, "seed posts")

	_, err = db.ExecContext(ctx, `
		INSERT INTO comments
			(uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid, content, created_at,
			 upvote_count, downvote_count, score, bridged_upvote_count, bridged_downvote_count, bridged_stats_as_of)
		VALUES
			($1, 'bafynativecomment', 'native-comment', 'did:plc:nativecommenter', $2, 'bafynativepost',
			 $2, 'bafynativepost', 'native comment', $3, 4, 1, 3, 0, 0, NULL)
	`, commentURI, postURI, createdAt)
	require.NoError(t, err, "seed comment")
}

func requireVoteStats(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	table string,
	uri string,
	want expectedVoteStats,
) {
	t.Helper()

	query := `SELECT upvote_count, downvote_count, bridged_upvote_count, bridged_downvote_count, score, bridged_stats_as_of FROM posts WHERE uri = $1`
	if table == "comments" {
		query = `SELECT upvote_count, downvote_count, bridged_upvote_count, bridged_downvote_count, score, bridged_stats_as_of FROM comments WHERE uri = $1`
	}

	var got storedVoteStats
	require.NoError(t, db.QueryRowContext(ctx, query, uri).Scan(
		&got.nativeUp,
		&got.nativeDown,
		&got.bridgedUp,
		&got.bridgedDown,
		&got.score,
		&got.asOf,
	), "read %s vote stats for %s", table, uri)
	require.Equal(t, want.nativeUp, got.nativeUp, "%s native upvotes", uri)
	require.Equal(t, want.nativeDown, got.nativeDown, "%s native downvotes", uri)
	require.Equal(t, want.bridgedUp, got.bridgedUp, "%s bridged upvotes", uri)
	require.Equal(t, want.bridgedDown, got.bridgedDown, "%s bridged downvotes", uri)
	require.Equal(t, want.score, got.score, "%s score", uri)
	if want.asOf == nil {
		require.False(t, got.asOf.Valid, "%s bridged stats as-of must remain NULL", uri)
		return
	}
	require.True(t, got.asOf.Valid, "%s bridged stats as-of must be populated", uri)
	require.True(t, got.asOf.Time.UTC().Equal(want.asOf.UTC()),
		"%s bridged stats as-of: got %s, want %s", uri, got.asOf.Time.UTC(), want.asOf.UTC())
}

func mustParseTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	require.NoError(t, err)
	return parsed
}

func requireSweep(t *testing.T, ctx context.Context, poller *bridgedvotes.Poller) {
	t.Helper()
	_, err := poller.Sweep(ctx)
	require.NoError(t, err)
}
