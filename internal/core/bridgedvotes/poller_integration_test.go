//go:build integration

package bridgedvotes_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/bridgedvotes"
	"Coves/internal/core/notifications"
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

type notificationPollerFixture struct {
	db        *sql.DB
	ctx       context.Context
	now       time.Time
	recipient string
}

func newNotificationPollerFixture(t *testing.T) notificationPollerFixture {
	t.Helper()
	f := notificationPollerFixture{db: testkit.DB(t), ctx: context.Background()}
	require.NoError(t, f.db.QueryRowContext(f.ctx, `SELECT now()`).Scan(&f.now))
	f.now = f.now.UTC().Truncate(time.Microsecond)
	f.recipient = "did:plc:" + testkit.UniqueID(t) + "pollrecipient"
	_, err := f.db.ExecContext(f.ctx, `INSERT INTO users (did, handle, pds_url, created_at) VALUES ($1, $2, $3, $4)`,
		f.recipient, testkit.UniqueID(t)+".test", testkit.Endpoints().PDS.BaseURL, f.now)
	require.NoError(t, err)
	return f
}

func (f notificationPollerFixture) post(t *testing.T, pdsURL string) string {
	t.Helper()
	id := testkit.UniqueID(t)
	community := "did:plc:" + id + "pollcommunity"
	postURI := "at://" + f.recipient + "/social.coves.community.postv2/" + id
	_, err := f.db.ExecContext(f.ctx, `INSERT INTO communities
		(did, handle, name, owner_did, created_by_did, hosted_by_did, pds_url, federated_from, created_at)
		VALUES ($1, $2, 'poll community', $1, $1, $1, $3, 'lemmy', $4)`,
		community, "!"+id+"@local.test", pdsURL, f.now)
	require.NoError(t, err)
	_, err = f.db.ExecContext(f.ctx, `INSERT INTO posts
		(uri, cid, rkey, author_did, community_did, title, created_at, upvote_count, downvote_count, score)
		VALUES ($1, $2, $3, $4, $5, 'poll post', $6, 0, 0, 0)`,
		postURI, "bafy"+id, id, f.recipient, community, f.now.Add(-time.Hour))
	require.NoError(t, err)
	_, err = f.db.ExecContext(f.ctx, `INSERT INTO community_post_admissions
		(community_did, post_uri, status, acceptance_uri, acceptance_rkey, accepted_cid, evaluated_cid, created_at, updated_at)
		VALUES ($1, $2, 'accepted', $3, $4, $5, $5, $6, $6)`,
		community, postURI, "at://"+community+"/social.coves.community.acceptance/"+id,
		id, "bafy"+id, f.now)
	require.NoError(t, err)
	return postURI
}

func (f notificationPollerFixture) poller(t *testing.T, hosts []string, client *http.Client, repo notifications.Repository) *bridgedvotes.Poller {
	t.Helper()
	store := postgres.NewBridgedVotesRepository(f.db,
		postgres.WithBridgedVoteNotifications(repo, jetstream.NewBridgeTrust(hosts)))
	poller, err := bridgedvotes.NewPoller(store, bridgedvotes.NewClient(client), hosts,
		bridgedvotes.Options{Lookback: 2 * time.Hour, SweepCap: 100})
	require.NoError(t, err)
	return poller
}

func (f notificationPollerFixture) notificationRepo() notifications.Repository {
	return postgres.NewNotificationRepository(f.db, postgres.WithBridgedUpvoteTotals())
}

func (f notificationPollerFixture) groupSort(t *testing.T, postURI string) time.Time {
	t.Helper()
	var sortAt time.Time
	require.NoError(t, f.db.QueryRowContext(f.ctx, `SELECT sort_at FROM notifications
		WHERE reason = 'upvote' AND recipient_did = $1 AND subject_uri = $2`, f.recipient, postURI).Scan(&sortAt))
	return sortAt.UTC().Truncate(time.Microsecond)
}

func (f notificationPollerFixture) groupCount(t *testing.T, postURI string) int {
	t.Helper()
	var count int
	require.NoError(t, f.db.QueryRowContext(f.ctx, `SELECT count(*) FROM notifications
		WHERE reason = 'upvote' AND recipient_did = $1 AND subject_uri = $2`, f.recipient, postURI).Scan(&count))
	return count
}

func (f notificationPollerFixture) bridgedTotal(t *testing.T, postURI string) int {
	t.Helper()
	var total int
	require.NoError(t, f.db.QueryRowContext(f.ctx, `SELECT bridged_upvote_count FROM posts WHERE uri = $1`, postURI).Scan(&total))
	return total
}

func (f notificationPollerFixture) watermark(t *testing.T, postURI string) sql.NullTime {
	t.Helper()
	var at sql.NullTime
	require.NoError(t, f.db.QueryRowContext(f.ctx, `SELECT bridged_polled_at FROM posts WHERE uri = $1`, postURI).Scan(&at))
	return at
}

func (f notificationPollerFixture) unread(t *testing.T, repo notifications.Repository, want int) {
	t.Helper()
	got, err := repo.(notifications.ReadRepository).CountUnread(f.ctx, f.recipient)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestPollerSweepTrustedNativeIncreaseCreatesUpvoteGroup(t *testing.T) {
	t.Parallel()
	f := newNotificationPollerFixture(t)
	bridge := &aggregateServer{aggregates: map[string]servedAggregate{}}
	server := httptest.NewServer(bridge)
	t.Cleanup(server.Close)
	postURI := f.post(t, server.URL)
	bridge.replace(servedAggregate{URI: postURI, Upvotes: 3, UpdatedAt: f.now.Format(time.RFC3339Nano)})
	repo := f.notificationRepo()
	requireSweep(t, f.ctx, f.poller(t, []string{server.URL}, server.Client(), repo))
	require.Contains(t, bridge.requestedURIs(), postURI)
	require.Equal(t, 3, f.bridgedTotal(t, postURI))
	require.Equal(t, 1, f.groupCount(t, postURI))
}

func TestPollerSweepDoesNotRequestUntrustedCommunity(t *testing.T) {
	t.Parallel()
	f := newNotificationPollerFixture(t)
	trusted := &aggregateServer{aggregates: map[string]servedAggregate{}}
	trustedServer := httptest.NewServer(trusted)
	t.Cleanup(trustedServer.Close)
	untrusted := &aggregateServer{aggregates: map[string]servedAggregate{}}
	untrustedServer := httptest.NewServer(untrusted)
	t.Cleanup(untrustedServer.Close)
	postURI := f.post(t, untrustedServer.URL)
	untrusted.replace(servedAggregate{URI: postURI, Upvotes: 4, UpdatedAt: f.now.Format(time.RFC3339Nano)})
	requireSweep(t, f.ctx, f.poller(t, []string{trustedServer.URL}, trustedServer.Client(), f.notificationRepo()))
	require.NotContains(t, untrusted.requestedURIs(), postURI)
	require.Equal(t, 0, f.groupCount(t, postURI))
}

func TestPollerSweepHostRemovalPreservesStoredGroupsAndTotals(t *testing.T) {
	t.Parallel()
	f := newNotificationPollerFixture(t)
	first := &aggregateServer{aggregates: map[string]servedAggregate{}}
	firstServer := httptest.NewServer(first)
	t.Cleanup(firstServer.Close)
	second := &aggregateServer{aggregates: map[string]servedAggregate{}}
	secondServer := httptest.NewServer(second)
	t.Cleanup(secondServer.Close)
	firstPost := f.post(t, firstServer.URL)
	secondPost := f.post(t, secondServer.URL)
	first.replace(servedAggregate{URI: firstPost, Upvotes: 3, UpdatedAt: f.now.Add(-time.Minute).Format(time.RFC3339Nano)})
	second.replace(servedAggregate{URI: secondPost, Upvotes: 5, UpdatedAt: f.now.Add(-time.Minute).Format(time.RFC3339Nano)})
	initialRepo := f.notificationRepo()
	require.NoError(t, initialRepo.(notifications.ReadRepository).UpdateSeen(f.ctx, f.recipient, f.now.Add(-time.Hour)))
	requireSweep(t, f.ctx, f.poller(t, []string{firstServer.URL, secondServer.URL}, firstServer.Client(), initialRepo))
	require.Equal(t, 3, f.bridgedTotal(t, firstPost))
	require.Equal(t, 5, f.bridgedTotal(t, secondPost))
	require.Equal(t, 1, f.groupCount(t, firstPost))
	require.Equal(t, 1, f.groupCount(t, secondPost))
	f.unread(t, initialRepo, 2)
	firstSort := f.groupSort(t, firstPost)
	secondSort := f.groupSort(t, secondPost)

	firstRequests := len(first.requestedURIs())
	secondRequests := len(second.requestedURIs())
	first.replace(servedAggregate{URI: firstPost, Upvotes: 9, UpdatedAt: f.now.Format(time.RFC3339Nano)})
	second.replace(servedAggregate{URI: secondPost, Upvotes: 6, UpdatedAt: f.now.Format(time.RFC3339Nano)})
	restartedRepo := f.notificationRepo()
	requireSweep(t, f.ctx, f.poller(t, []string{secondServer.URL}, secondServer.Client(), restartedRepo))
	require.Len(t, first.requestedURIs(), firstRequests)
	require.Greater(t, len(second.requestedURIs()), secondRequests)
	require.Equal(t, 3, f.bridgedTotal(t, firstPost))
	require.True(t, f.groupSort(t, firstPost).Equal(firstSort))
	require.Equal(t, 6, f.bridgedTotal(t, secondPost))
	require.True(t, f.groupSort(t, secondPost).After(secondSort))
	page, err := restartedRepo.(notifications.ReadRepository).List(f.ctx, f.recipient, "", 10)
	require.NoError(t, err)
	require.Len(t, page.Notifications, 2)
	listed := make(map[string]notifications.ListedNotification)
	for _, notification := range page.Notifications {
		listed[notification.SubjectURI] = notification
	}
	require.Equal(t, 3, listed[firstPost].UpvoteCount)
	require.False(t, listed[firstPost].IsRead)
	require.Equal(t, 6, listed[secondPost].UpvoteCount)
	require.False(t, listed[secondPost].IsRead)
	f.unread(t, restartedRepo, 2)
}

type pollerFailingNotificationRepo struct {
	notifications.Repository
	err error
}

func (r pollerFailingNotificationRepo) ApplyUpvoteGroupTx(ctx context.Context, tx *sql.Tx, intent notifications.UpvoteGroupIntent) error {
	if err := r.Repository.ApplyUpvoteGroupTx(ctx, tx, intent); err != nil {
		return err
	}
	return r.err
}

func TestPollerSweepNotificationFailureLeavesBatchUnpolled(t *testing.T) {
	t.Parallel()
	f := newNotificationPollerFixture(t)
	bridge := &aggregateServer{aggregates: map[string]servedAggregate{}}
	server := httptest.NewServer(bridge)
	t.Cleanup(server.Close)
	postURI := f.post(t, server.URL)
	bridge.replace(servedAggregate{URI: postURI, Upvotes: 3, UpdatedAt: f.now.Format(time.RFC3339Nano)})
	_, err := f.db.ExecContext(f.ctx, `UPDATE posts SET bridged_polled_at = $2 WHERE uri = $1`, postURI, f.now.Add(-time.Minute))
	require.NoError(t, err)
	before := f.watermark(t, postURI)
	require.True(t, before.Valid)
	sentinel := errors.New("upvote notification write failed")
	repo := pollerFailingNotificationRepo{Repository: f.notificationRepo(), err: sentinel}
	poller := f.poller(t, []string{server.URL}, server.Client(), repo)
	_, err = poller.Sweep(f.ctx)
	require.ErrorIs(t, err, sentinel)
	after := f.watermark(t, postURI)
	require.True(t, after.Valid)
	require.True(t, after.Time.UTC().Truncate(time.Microsecond).Equal(before.Time.UTC().Truncate(time.Microsecond)))
}
