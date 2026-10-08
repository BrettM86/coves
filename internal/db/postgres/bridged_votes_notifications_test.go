//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"Coves/internal/core/bridgedvotes"
	"Coves/internal/core/notifications"
	"Coves/internal/core/posts"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

type bridgedNotificationHosts struct{}

func (bridgedNotificationHosts) TrustsPDS(url string) bool { return url == bridgeAPDSURL }

type bridgedNotificationFixture struct {
	db        *sql.DB
	ctx       context.Context
	now       time.Time
	community string
	recipient string
	subject   string
	root      string
	table     string
}

// Recipient modes are restricted to the fan-out gates exercised by B6.
func newBridgedNotificationFixture(t *testing.T, kind, recipientMode string) bridgedNotificationFixture {
	t.Helper()
	f := bridgedNotificationFixture{db: testkit.DB(t), ctx: context.Background(), table: "posts"}
	require.NoError(t, f.db.QueryRowContext(f.ctx, `SELECT now()`).Scan(&f.now))
	f.now = f.now.UTC().Truncate(time.Microsecond)
	f.community = visibilityCommunity(t, f.db, testkit.UniqueID(t))
	_, err := f.db.ExecContext(f.ctx, `UPDATE communities SET pds_url = $2 WHERE did = $1`, f.community, bridgeAPDSURL)
	require.NoError(t, err)
	id := testkit.UniqueID(t)
	f.recipient = "did:plc:" + id + "recipient"
	switch recipientMode {
	case "absent":
	case "bridge":
		_, err = f.db.ExecContext(f.ctx, `INSERT INTO users (did, handle, pds_url, created_at) VALUES ($1, $2, $3, $4)`,
			f.recipient, id+"recipient.test", bridgeAPDSURL, f.now)
		require.NoError(t, err)
	default:
		createTestUser(t, f.db, id+"recipient.test", f.recipient)
	}
	if recipientMode == "erased" {
		_, err = f.db.ExecContext(f.ctx, `INSERT INTO deleted_accounts (did) VALUES ($1)`, f.recipient)
		require.NoError(t, err)
		_, err = f.db.ExecContext(f.ctx, `DELETE FROM users WHERE did = $1`, f.recipient)
		require.NoError(t, err)
	}
	if recipientMode == "aggregator" {
		_, err = f.db.ExecContext(f.ctx, `INSERT INTO aggregators (did, display_name, record_uri, record_cid)
			VALUES ($1, 'Bridge fixture', $2, 'bafybridgednotificationaggregator')`, f.recipient,
			"at://"+f.recipient+"/social.coves.aggregator.service/self")
		require.NoError(t, err)
	}
	postAuthor := f.recipient
	if kind == "comment" {
		f.table = "comments"
		postAuthor = "did:plc:" + testkit.UniqueID(t) + "root"
		createTestUser(t, f.db, testkit.UniqueID(t)+"root.test", postAuthor)
	}
	f.root = seedVisibilityPost(t, f.db, f.community, postAuthor, testkit.TID(), "bridged notification root", f.now.Add(-8*24*time.Hour))
	seedVisibilityAdmission(t, f.db, f.community, f.root, posts.AdmissionStatusAccepted, "", "")
	f.subject = f.root
	if kind == "comment" {
		f.subject = seedActorComment(t, f.db, f.recipient, f.root, testkit.TID(), f.now.Add(-8*24*time.Hour))
	}
	// seedVisibilityPost supplies a denormalized native vote for feed tests; these
	// cases start with none and add actual votes explicitly where relevant.
	_, err = f.db.ExecContext(f.ctx, `UPDATE posts SET upvote_count = 0, score = 0 WHERE uri = $1`, f.root)
	require.NoError(t, err)
	return f
}

func (f bridgedNotificationFixture) repository() notifications.Repository {
	return NewNotificationRepository(f.db, WithBridgedUpvoteTotals())
}

func (f bridgedNotificationFixture) store(repo notifications.Repository) *BridgedVotesRepository {
	return NewBridgedVotesRepository(f.db, WithBridgedVoteNotifications(repo, bridgedNotificationHosts{}))
}

func (f bridgedNotificationFixture) apply(t *testing.T, repo notifications.Repository, up, down int, asOf time.Time, want bool) {
	t.Helper()
	applied, err := f.store(repo).ApplyAggregate(f.ctx, bridgedvotes.Aggregate{
		URI: f.subject, Upvotes: up, Downvotes: down, AsOf: asOf,
	})
	require.NoError(t, err)
	require.Equal(t, want, applied)
}

func (f bridgedNotificationFixture) groupCount(t *testing.T) int {
	t.Helper()
	var count int
	require.NoError(t, f.db.QueryRowContext(f.ctx, `SELECT count(*) FROM notifications
		WHERE recipient_did = $1 AND subject_uri = $2 AND reason = 'upvote'`, f.recipient, f.subject).Scan(&count))
	return count
}

func (f bridgedNotificationFixture) groupSort(t *testing.T) time.Time {
	t.Helper()
	var at time.Time
	require.NoError(t, f.db.QueryRowContext(f.ctx, `SELECT sort_at FROM notifications
		WHERE recipient_did = $1 AND subject_uri = $2 AND reason = 'upvote'`, f.recipient, f.subject).Scan(&at))
	return at.UTC().Truncate(time.Microsecond)
}

func (f bridgedNotificationFixture) seedGroup(t *testing.T, at time.Time) {
	t.Helper()
	insertDeleteTestGroup(t, upvoteGroupFixture{ctx: f.ctx, db: f.db, rootPostURI: f.root}, f.recipient, f.subject, at)
}

func (f bridgedNotificationFixture) nativeUpvote(t *testing.T) {
	t.Helper()
	voter := "did:plc:" + testkit.UniqueID(t) + "voter"
	qualifyingUpvoteFixture{db: f.db}.insertVote(t, voter, f.subject, "up", f.now, false)
	_, err := f.db.ExecContext(f.ctx, `UPDATE `+f.table+` SET upvote_count = 1, score = score + 1 WHERE uri = $1`, f.subject)
	require.NoError(t, err)
}

func (f bridgedNotificationFixture) unread(t *testing.T, repo notifications.Repository, want int) {
	t.Helper()
	read := repo.(notifications.ReadRepository)
	count, err := read.CountUnread(f.ctx, f.recipient)
	require.NoError(t, err)
	require.Equal(t, want, count)
}

func (f bridgedNotificationFixture) listedUpvotes(t *testing.T, repo notifications.Repository, want int) {
	t.Helper()
	page, err := repo.(notifications.ReadRepository).List(f.ctx, f.recipient, "", 10)
	require.NoError(t, err)
	require.Len(t, page.Notifications, 1)
	require.Equal(t, notifications.ReasonUpvote, page.Notifications[0].Reason)
	require.Equal(t, f.subject, page.Notifications[0].SubjectURI)
	require.Equal(t, want, page.Notifications[0].UpvoteCount)
}

func (f bridgedNotificationFixture) markSeen(t *testing.T, repo notifications.Repository) {
	t.Helper()
	require.NoError(t, repo.(notifications.ReadRepository).UpdateSeen(f.ctx, f.recipient, f.now.Add(time.Hour)))
	f.unread(t, repo, 0)
	var seenAt time.Time
	require.NoError(t, f.db.QueryRowContext(f.ctx, `SELECT seen_at FROM notification_state WHERE did = $1`, f.recipient).Scan(&seenAt))
	f.after(t, seenAt.UTC().Truncate(time.Microsecond))
}

// Wait only for the database clock to pass the prior sort/seen timestamp. This
// prevents a legitimate same-microsecond NOW() from obscuring a subsequent bump.
func (f bridgedNotificationFixture) after(t *testing.T, timestamp time.Time) {
	t.Helper()
	testkit.WaitFor(t, 2*time.Second, func() (bool, error) {
		var now time.Time
		err := f.db.QueryRowContext(f.ctx, `SELECT now()`).Scan(&now)
		return now.UTC().Truncate(time.Microsecond).After(timestamp), err
	}, testkit.WithDescription("database clock after the previous upvote group sort"))
}

type bridgedNotificationRepositoryDecorator struct {
	notifications.Repository
	apply   func(context.Context, *sql.Tx, notifications.UpvoteGroupIntent) error
	lookups func(notifications.Lookups) notifications.Lookups
}

func (d bridgedNotificationRepositoryDecorator) ApplyUpvoteGroupTx(ctx context.Context, tx *sql.Tx, intent notifications.UpvoteGroupIntent) error {
	if d.apply != nil {
		return d.apply(ctx, tx, intent)
	}
	return d.Repository.ApplyUpvoteGroupTx(ctx, tx, intent)
}

func (d bridgedNotificationRepositoryDecorator) LookupsTx(tx *sql.Tx) notifications.Lookups {
	lookups := d.Repository.LookupsTx(tx)
	if d.lookups != nil {
		return d.lookups(lookups)
	}
	return lookups
}

type bridgedFailRecipientFacts struct {
	notifications.Lookups
	err error
}

func (d bridgedFailRecipientFacts) RecipientFacts(context.Context, string, []string) (map[string]notifications.RecipientFacts, error) {
	return nil, d.err
}

func TestBridgedVotesNotifications_FirstIncreaseUsesAggregateTransaction(t *testing.T) {
	for _, kind := range []string{"post", "comment"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			f := newBridgedNotificationFixture(t, kind, "native")
			repo := f.repository()
			var transactionNow, clockBeforeWrite, clockAfterWrite time.Time
			var isolation string
			var visibleTotal int
			called := false
			decorated := bridgedNotificationRepositoryDecorator{Repository: repo}
			decorated.apply = func(ctx context.Context, tx *sql.Tx, intent notifications.UpvoteGroupIntent) error {
				called = true
				if err := tx.QueryRowContext(ctx, `SHOW transaction_isolation`).Scan(&isolation); err != nil {
					return err
				}
				if err := tx.QueryRowContext(ctx, `SELECT bridged_upvote_count FROM `+f.table+` WHERE uri = $1`, f.subject).Scan(&visibleTotal); err != nil {
					return err
				}
				if err := tx.QueryRowContext(ctx, `SELECT now(), clock_timestamp()`).Scan(&transactionNow, &clockBeforeWrite); err != nil {
					return err
				}
				if err := repo.ApplyUpvoteGroupTx(ctx, tx, intent); err != nil {
					return err
				}
				return tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&clockAfterWrite)
			}
			f.apply(t, decorated, 3, 1, f.now.Add(time.Minute), true)
			require.True(t, called, "group write must use the aggregate transaction")
			require.Equal(t, "read committed", isolation)
			require.Equal(t, 3, visibleTotal)
			require.Equal(t, 1, f.groupCount(t))
			var recipient, subject, root, reason string
			var actor, record sql.NullString
			var sortAt time.Time
			require.NoError(t, f.db.QueryRowContext(f.ctx, `SELECT recipient_did, subject_uri, root_post_uri,
				reason, actor_did, record_uri, sort_at FROM notifications WHERE reason = 'upvote'`).Scan(
				&recipient, &subject, &root, &reason, &actor, &record, &sortAt))
			require.Equal(t, f.recipient, recipient)
			require.Equal(t, f.subject, subject)
			require.Equal(t, f.root, root)
			require.Equal(t, "upvote", reason)
			require.False(t, actor.Valid)
			require.False(t, record.Valid)
			require.Truef(t, clockBeforeWrite.After(transactionNow),
				"fixture: clock read %s must follow aggregate transaction start %s", clockBeforeWrite, transactionNow)
			requireSortAtWithin(t, sortAt, clockBeforeWrite, clockAfterWrite)
		})
	}
}

func TestBridgedVotesNotifications_NewHighsBumpOldPost(t *testing.T) {
	t.Parallel()
	f := newBridgedNotificationFixture(t, "post", "native")
	repo := f.repository()
	f.nativeUpvote(t)
	_, err := f.db.ExecContext(f.ctx, `UPDATE notification_activation SET activated_at = $1`, f.now.Add(-time.Hour))
	require.NoError(t, err)
	baseline := f.now.Add(-30 * 24 * time.Hour)
	seedStoredAggregate(t, f.ctx, f.db, "posts", f.subject, 3, 0, baseline)
	f.seedGroup(t, f.now.Add(-time.Minute))
	f.markSeen(t, repo)
	f.unread(t, repo, 0)
	first := f.now.Add(-20 * 24 * time.Hour)
	f.apply(t, repo, 4, 0, first, true)
	requireAggregateStats(t, f.ctx, f.db, "posts", f.subject, expectedAggregateStats{
		nativeUp: 1, bridgedUp: 4, score: 5, asOf: &first,
	})
	previous := f.groupSort(t)
	require.True(t, previous.After(f.now.Add(-time.Minute)))
	f.unread(t, repo, 1)
	f.apply(t, repo, 4, 0, first, true)
	require.True(t, f.groupSort(t).Equal(previous), "equal total and asOf must not bump")
	newer := first.Add(time.Minute)
	f.apply(t, repo, 4, 0, newer, true)
	require.True(t, f.groupSort(t).Equal(previous), "newer asOf with equal upvotes must not bump")
	downOnly := newer.Add(time.Minute)
	f.apply(t, repo, 4, 2, downOnly, true)
	require.True(t, f.groupSort(t).Equal(previous), "downvotes alone must not bump")
	f.after(t, previous)
	fallingScore := downOnly.Add(time.Minute)
	f.apply(t, repo, 5, 5, fallingScore, true)
	requireAggregateStats(t, f.ctx, f.db, "posts", f.subject, expectedAggregateStats{
		nativeUp: 1, bridgedUp: 5, bridgedDown: 5, score: 1, asOf: &fallingScore,
	})
	current := f.groupSort(t)
	require.True(t, current.After(previous), "an increase bumps despite a falling score")
	previous = current
	f.after(t, previous)
	f.apply(t, repo, 6, 5, fallingScore, true)
	current = f.groupSort(t)
	require.True(t, current.After(previous), "equal-asOf increase must bump")
	previous = current
	millisecond := fallingScore.Add(time.Minute).Truncate(time.Millisecond)
	storedSameMillisecond := millisecond.Add(800 * time.Microsecond)
	f.apply(t, repo, 6, 5, storedSameMillisecond, true)
	require.True(t, f.groupSort(t).Equal(previous))
	f.after(t, previous)
	olderSameMillisecond := millisecond.Add(100 * time.Microsecond)
	f.apply(t, repo, 7, 5, olderSameMillisecond, true)
	current = f.groupSort(t)
	require.True(t, current.After(previous), "numerically older asOf in the same millisecond must bump")
	previous = current
	decrease := millisecond.Add(time.Minute)
	f.apply(t, repo, 2, 5, decrease, true)
	require.True(t, f.groupSort(t).Equal(previous), "decrease must not bump")
	f.listedUpvotes(t, repo, 3)
	f.markSeen(t, repo)
	f.unread(t, repo, 0)
	f.after(t, previous)
	rebound := decrease.Add(time.Minute)
	f.apply(t, repo, 4, 5, rebound, true)
	require.True(t, f.groupSort(t).Equal(previous), "rebound below the peak must not bump")
	f.unread(t, repo, 0)
}

func TestBridgedVotesNotifications_DropToZero(t *testing.T) {
	for _, native := range []bool{false, true} {
		name := "bridged only deletes"
		if native {
			name = "qualifying native upvote keeps group"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newBridgedNotificationFixture(t, "post", "native")
			repo := f.repository()
			if native {
				f.nativeUpvote(t)
			}
			seedStoredAggregate(t, f.ctx, f.db, "posts", f.subject, 3, 0, f.now.Add(-time.Hour))
			f.seedGroup(t, f.now.Add(-time.Minute))
			f.apply(t, repo, 0, 0, f.now, true)
			if native {
				require.Equal(t, 1, f.groupCount(t))
				require.True(t, f.groupSort(t).Equal(f.now.Add(-time.Minute)))
			} else {
				require.Equal(t, 0, f.groupCount(t))
			}
		})
	}
}

func TestBridgedVotesNotifications_LaunchBaselineAndStaleAggregate(t *testing.T) {
	t.Parallel()
	f := newBridgedNotificationFixture(t, "post", "native")
	repo := f.repository()
	f.nativeUpvote(t)
	baseline := f.now.Add(-time.Hour)
	seedStoredAggregate(t, f.ctx, f.db, "posts", f.subject, 5, 1, baseline)
	f.apply(t, repo, 5, 1, f.now, true)
	require.Equal(t, 0, f.groupCount(t), "unchanged launch baseline is silent")
	f.apply(t, repo, 9, 4, baseline, false)
	requireAggregateStats(t, f.ctx, f.db, "posts", f.subject, expectedAggregateStats{
		nativeUp: 1, bridgedUp: 5, bridgedDown: 1, score: 5, asOf: &f.now,
	})
	require.Equal(t, 0, f.groupCount(t))
	f.apply(t, repo, 6, 1, f.now.Add(time.Minute), true)
	require.Equal(t, 1, f.groupCount(t))
	f.listedUpvotes(t, repo, 7)
}

func TestBridgedVotesNotifications_RecipientAndWithdrawalGates(t *testing.T) {
	for _, mode := range []string{"absent", "erased", "aggregator", "bridge", "removed post", "deleted comment root"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			kind, recipientMode := "post", mode
			if mode == "removed post" {
				recipientMode = "native"
			}
			if mode == "deleted comment root" {
				kind, recipientMode = "comment", "native"
			}
			f := newBridgedNotificationFixture(t, kind, recipientMode)
			repo := f.repository()
			oldSort := f.now.Add(-time.Minute)
			withdrawn := mode == "removed post" || mode == "deleted comment root"
			if withdrawn {
				f.seedGroup(t, oldSort)
				f.apply(t, repo, 1, 0, f.now, true)
				bumped := f.groupSort(t)
				require.True(t, bumped.After(oldSort), "eligible content must bump before withdrawal")
				oldSort = bumped
			}
			if mode == "removed post" {
				seedVisibilityAdmission(t, f.db, f.community, f.root, posts.AdmissionStatusRemoved, "", "")
			}
			if mode == "deleted comment root" {
				_, err := f.db.ExecContext(f.ctx, `UPDATE posts SET deleted_at = $2 WHERE uri = $1`, f.root, f.now)
				require.NoError(t, err)
			}
			f.apply(t, repo, 3, 0, f.now.Add(time.Minute), true)
			var total int
			require.NoError(t, f.db.QueryRowContext(f.ctx, `SELECT bridged_upvote_count FROM `+f.table+` WHERE uri = $1`, f.subject).Scan(&total))
			require.Equal(t, 3, total)
			if withdrawn {
				require.Equal(t, 1, f.groupCount(t))
				require.True(t, f.groupSort(t).Equal(oldSort), "withdrawn content must not bump")
			} else {
				require.Equal(t, 0, f.groupCount(t))
			}
		})
	}
}

func TestBridgedVotesNotifications_NotificationErrorsRollBackAggregate(t *testing.T) {
	for _, failure := range []string{"recipient facts", "group write after insert"} {
		t.Run(failure, func(t *testing.T) {
			t.Parallel()
			f := newBridgedNotificationFixture(t, "post", "native")
			repo := f.repository()
			baseline := f.now.Add(-time.Hour)
			seedStoredAggregate(t, f.ctx, f.db, "posts", f.subject, 2, 1, baseline)
			sentinel := errors.New("notification failure")
			decorated := bridgedNotificationRepositoryDecorator{Repository: repo}
			if failure == "recipient facts" {
				decorated.lookups = func(lookups notifications.Lookups) notifications.Lookups {
					return bridgedFailRecipientFacts{Lookups: lookups, err: sentinel}
				}
			} else {
				decorated.apply = func(ctx context.Context, tx *sql.Tx, intent notifications.UpvoteGroupIntent) error {
					require.Equal(t, notifications.UpvoteGroupBump, intent.Action)
					require.NoError(t, repo.ApplyUpvoteGroupTx(ctx, tx, intent))
					var count int
					require.NoError(t, tx.QueryRowContext(ctx, `SELECT count(*) FROM notifications WHERE recipient_did = $1 AND subject_uri = $2`, f.recipient, f.subject).Scan(&count))
					require.Equal(t, 1, count, "the group write must have occurred before the forced error")
					return sentinel
				}
			}
			applied, err := f.store(decorated).ApplyAggregate(f.ctx, bridgedvotes.Aggregate{
				URI: f.subject, Upvotes: 4, Downvotes: 2, AsOf: f.now,
			})
			require.ErrorIs(t, err, sentinel)
			require.False(t, applied)
			requireAggregateStats(t, f.ctx, f.db, "posts", f.subject, expectedAggregateStats{
				bridgedUp: 2, bridgedDown: 1, score: 1, asOf: &baseline,
			})
			require.Equal(t, 0, f.groupCount(t), "failed notification rolls back the inserted group")
		})
	}
}
