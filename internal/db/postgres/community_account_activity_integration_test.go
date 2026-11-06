//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"Coves/internal/core/communities"
	"Coves/internal/core/moderation"
	"Coves/internal/core/users"
	"Coves/internal/crypto/credentialcipher/credentialciphertest"
	"Coves/internal/db/postgres"
	"Coves/tests/fixtures"
	"Coves/tests/testkit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newActivityCommunity is a community profile at did with a unique handle.
func newActivityCommunity(t *testing.T, did string) *communities.Community {
	t.Helper()
	label := testkit.UniqueIDWithPrefix(t, "activity")
	return &communities.Community{
		DID:          did,
		Handle:       "!" + label + "@coves.local",
		Name:         label,
		OwnerDID:     did,
		CreatedByDID: did,
		HostedByDID:  fixtures.InstanceDID(),
		Visibility:   "public",
		CreatedAt:    time.Date(2026, time.November, 5, 12, 0, 0, 0, time.UTC),
		UpdatedAt:    time.Date(2026, time.November, 5, 12, 0, 0, 0, time.UTC),
	}
}

func countCommunityRows(t *testing.T, db *sql.DB, did string) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM communities WHERE did = $1`, did).Scan(&count))
	return count
}

func countUserRows(t *testing.T, db *sql.DB, did string) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM users WHERE did = $1`, did).Scan(&count))
	return count
}

// seedActivityUser gives did a users row.
func seedActivityUser(t *testing.T, db *sql.DB, did string) {
	t.Helper()
	fixtures.User(t, db, testkit.UniqueIDWithPrefix(t, "actuser")+".test", did)
}

// seedActivityPost gives did an authored post (posts.author_did) in another community, with no users row.
func seedActivityPost(t *testing.T, db *sql.DB, did string) {
	t.Helper()
	containerName := testkit.UniqueIDWithPrefix(t, "actplace")
	container, err := fixtures.Community(t.Context(), db, containerName, "owner"+containerName)
	require.NoError(t, err)
	indexedModerationPost(t, db, moderation.PostV2Collection, container, did, "an account's post", "")
}

// seedActivityComment gives did a comment (comments.commenter_did), with no users row.
func seedActivityComment(t *testing.T, db *sql.DB, did string) {
	t.Helper()
	rkey := testkit.TID()
	rootURI := "at://" + fixtures.DID("actroot") + "/" + moderation.PostV2Collection + "/" + rkey
	_, err := db.ExecContext(t.Context(), `
		INSERT INTO comments (uri, cid, rkey, commenter_did, root_uri, root_cid, parent_uri, parent_cid, content, created_at)
		VALUES ($1, 'bafyactivitycomment', $2, $3, $4, 'bafyactivityroot', $4, 'bafyactivityroot', 'a comment', NOW())
	`, "at://"+did+"/"+moderation.CommentCollection+"/"+rkey, rkey, did, rootURI)
	require.NoError(t, err)
}

// seedActivityVote gives did a vote (votes.voter_did), with no users row.
func seedActivityVote(t *testing.T, db *sql.DB, did string) {
	t.Helper()
	rkey := testkit.TID()
	_, err := db.ExecContext(t.Context(), `
		INSERT INTO votes (uri, cid, rkey, voter_did, subject_uri, subject_cid, direction, created_at, indexed_at)
		VALUES ($1, 'bafyactivityvote', $2, $3, $4, 'bafyactivitysubject', 'up', NOW(), NOW())
	`, "at://"+did+"/social.coves.feed.vote/"+rkey, rkey, did,
		"at://"+fixtures.DID("actsubject")+"/"+moderation.PostV2Collection+"/"+rkey)
	require.NoError(t, err)
}

// communityRepo.Create refuses a DID that already acts as an account, and
// writes no row.
func TestCommunityRepositoryCreate_RefusesDIDWithAccountActivity(t *testing.T) {
	for _, test := range []struct {
		name string
		seed func(t *testing.T, db *sql.DB, did string)
	}{
		{"a users row", seedActivityUser},
		{"an authored post", seedActivityPost},
		{"a comment", seedActivityComment},
		{"a vote", seedActivityVote},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := testkit.DB(t)
			did := fixtures.DID(testkit.UniqueIDWithPrefix(t, "activity"))
			test.seed(t, db, did)

			_, err := postgres.NewCommunityRepository(db, credentialciphertest.Fixed()).
				Create(t.Context(), newActivityCommunity(t, did))
			require.ErrorIs(t, err, communities.ErrDIDHasAccountActivity)
			assert.Zero(t, countCommunityRows(t, db, did), "a refused community writes no row")
		})
	}
}

// The account-activity refusal admits a clean DID and a trusted bridge's
// community, and leaves an existing community's duplicate answer unchanged.
func TestCommunityRepositoryCreate_AccountActivityExceptions(t *testing.T) {
	t.Run("a clean DID is created", func(t *testing.T) {
		db := testkit.DB(t)
		did := fixtures.DID(testkit.UniqueIDWithPrefix(t, "clean"))
		_, err := postgres.NewCommunityRepository(db, credentialciphertest.Fixed()).
			Create(t.Context(), newActivityCommunity(t, did))
		require.NoError(t, err)
		assert.Equal(t, 1, countCommunityRows(t, db, did))
	})
	t.Run("a trusted bridge community with account activity is created", func(t *testing.T) {
		db := testkit.DB(t)
		did := fixtures.DID(testkit.UniqueIDWithPrefix(t, "bridged"))
		seedActivityUser(t, db, did)
		seedActivityPost(t, db, did)
		_, err := postgres.NewCommunityRepository(db, credentialciphertest.Fixed()).
			Create(t.Context(), newActivityCommunity(t, did), communities.AllowAccountActivityForTrustedBridge)
		require.NoError(t, err)
		assert.Equal(t, 1, countCommunityRows(t, db, did))
	})
	t.Run("an existing community is a duplicate, not account activity", func(t *testing.T) {
		db := testkit.DB(t)
		did := fixtures.DID(testkit.UniqueIDWithPrefix(t, "existing"))
		insertCommunityRowForDID(t, db, did)
		seedActivityUser(t, db, did)

		_, err := postgres.NewCommunityRepository(db, credentialciphertest.Fixed()).
			Create(t.Context(), newActivityCommunity(t, did))
		require.ErrorIs(t, err, communities.ErrCommunityAlreadyExists)
		assert.False(t, errors.Is(err, communities.ErrDIDHasAccountActivity))
	})
}

// holdBanSubjectLock opens a transaction holding the ban-subject advisory lock
// on did (communities.BanSubjectLockKeySQL). Cleanup rolls it back if the test
// has not committed it.
func holdBanSubjectLock(t *testing.T, db *sql.DB, did string) *sql.Tx {
	t.Helper()
	connection, err := db.Conn(t.Context())
	require.NoError(t, err)
	holder, err := connection.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		rollbackError := holder.Rollback()
		assert.True(t, rollbackError == nil || errors.Is(rollbackError, sql.ErrTxDone), "rolling back the holder: %v", rollbackError)
		assert.NoError(t, connection.Close())
	})
	_, err = holder.ExecContext(t.Context(), `SELECT pg_advisory_xact_lock(`+communities.BanSubjectLockKeySQL+`)`, did)
	require.NoError(t, err)
	return holder
}

// A community create and a users insert on DID D wait while another
// transaction holds D's ban-subject lock, and finish once it commits, so the
// account-activity check sees whatever that transaction wrote.
func TestBanSubjectLockSerializesAccountAndCommunityWriters(t *testing.T) {
	for _, test := range []struct {
		name         string
		beforeCommit func(t *testing.T, holder *sql.Tx, did string)
		write        func(ctx context.Context, db *sql.DB, did string, community *communities.Community) error
		check        func(t *testing.T, db *sql.DB, did string, err error)
		// untouchedRelation is a table the waiting writer must not have
		// locked yet: the ban-subject lock comes before its row write.
		untouchedRelation string
	}{
		{
			name: "community create sees a users row the holder commits",
			beforeCommit: func(t *testing.T, holder *sql.Tx, did string) {
				_, err := holder.ExecContext(t.Context(), `INSERT INTO users (did, handle, pds_url) VALUES ($1, $2, $3)`,
					did, testkit.UniqueIDWithPrefix(t, "lockuser")+".test", testkit.Endpoints().PDS.BaseURL)
				require.NoError(t, err)
			},
			write: func(ctx context.Context, db *sql.DB, _ string, community *communities.Community) error {
				_, err := postgres.NewCommunityRepository(db, credentialciphertest.Fixed()).Create(ctx, community)
				return err
			},
			check: func(t *testing.T, db *sql.DB, did string, err error) {
				require.ErrorIs(t, err, communities.ErrDIDHasAccountActivity)
				assert.Zero(t, countCommunityRows(t, db, did))
			},
		},
		{
			name:              "user create",
			untouchedRelation: "users",
			beforeCommit:      func(*testing.T, *sql.Tx, string) {},
			write: func(ctx context.Context, db *sql.DB, did string, _ *communities.Community) error {
				_, err := postgres.NewUserRepository(db).Create(ctx, &users.User{
					DID: did, Handle: "lockcreate-" + did[len("did:plc:"):] + ".test", PDSURL: testkit.Endpoints().PDS.BaseURL,
				})
				return err
			},
			check: func(t *testing.T, db *sql.DB, did string, err error) {
				require.NoError(t, err)
				assert.Equal(t, 1, countUserRows(t, db, did))
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := testkit.DB(t)
			did := fixtures.DID(testkit.UniqueIDWithPrefix(t, "lock"))
			community := newActivityCommunity(t, did)
			holder := holdBanSubjectLock(t, db, did)

			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			results := make(chan error, 1)
			go func() { results <- test.write(ctx, db, did, community) }()
			testkit.WaitFor(t, 5*time.Second, func() (bool, error) {
				advisory, other, err := countLockWaiters(ctx, db)
				return advisory == 1 && other == 0, err
			}, testkit.WithDescription("the writer waits on the ban-subject advisory lock"))
			if test.untouchedRelation != "" {
				assert.Zero(t, waiterRelationLocks(t, db, holder, test.untouchedRelation),
					"the writer waiting on the ban-subject lock holds no lock on %s", test.untouchedRelation)
			}

			test.beforeCommit(t, holder, did)
			require.NoError(t, holder.Commit())
			var err error
			select {
			case err = <-results:
			case <-ctx.Done():
				t.Fatalf("the writer did not finish after the holder committed: %v", ctx.Err())
			}
			test.check(t, db, did, err)
		})
	}
}

// waiterRelationLocks counts the relation locks on relation held by the
// backend blocked on holder's ban-subject advisory lock.
func waiterRelationLocks(t *testing.T, db *sql.DB, holder *sql.Tx, relation string) int {
	t.Helper()
	var holderProcessID int
	require.NoError(t, holder.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&holderProcessID))
	var waiterProcessID int
	require.NoError(t, db.QueryRowContext(t.Context(), `
		SELECT pid FROM pg_stat_activity
		WHERE datname = current_database() AND wait_event_type = 'Lock' AND wait_event = 'advisory'
		  AND $1 = ANY(pg_blocking_pids(pid))`, holderProcessID).Scan(&waiterProcessID),
		"one backend waits on the holder's advisory lock")
	var count int
	require.NoError(t, db.QueryRowContext(t.Context(), `
		SELECT count(*) FROM pg_locks
		WHERE pid = $1 AND locktype = 'relation' AND relation = $2::regclass`, waiterProcessID, relation).Scan(&count))
	return count
}

// activityCommunityPDSURL is the pds_url of the community rows the account
// subject facts tests insert; users rows carry testkit's PDS URL instead.
const activityCommunityPDSURL = "https://pds.activity-community.test"

// activityCommunityHandle is the stored handle of those community rows (one
// per test database).
const activityCommunityHandle = "c-actfacts.coves.social"

// insertActivityCommunityRow indexes did as a community whose pds_url is
// activityCommunityPDSURL.
func insertActivityCommunityRow(t *testing.T, db *sql.DB, did string) {
	t.Helper()
	_, err := db.ExecContext(t.Context(), `
		INSERT INTO communities (did, name, owner_did, created_by_did, hosted_by_did, handle, pds_url, created_at)
		VALUES ($1, $2, $1, $1, $3, $4, $5, NOW())
	`, did, "activityfacts", fixtures.InstanceDID(), activityCommunityHandle, activityCommunityPDSURL)
	require.NoError(t, err)
}

// AccountSubjectFacts, read under the subject lock, reports whether the DID
// has account activity (a users row, or an authored post, comment or vote),
// whether it has an indexed community row, and that row's pds_url and stored handle.
func TestModerationTransactionAccountSubjectFacts(t *testing.T) {
	for _, test := range []struct {
		name  string
		seeds []func(t *testing.T, db *sql.DB, did string)
		want  moderation.AccountSubjectFacts
	}{
		{name: "no activity and no community row", want: moderation.AccountSubjectFacts{HasAccountActivity: false, CommunityPDSURL: "", IsCommunity: false}},
		{
			name: "a users row only", seeds: []func(*testing.T, *sql.DB, string){seedActivityUser},
			want: moderation.AccountSubjectFacts{HasAccountActivity: true, CommunityPDSURL: "", IsCommunity: false},
		},
		{
			name: "an authored post only", seeds: []func(*testing.T, *sql.DB, string){seedActivityPost},
			want: moderation.AccountSubjectFacts{HasAccountActivity: true, CommunityPDSURL: "", IsCommunity: false},
		},
		{
			name: "a comment only", seeds: []func(*testing.T, *sql.DB, string){seedActivityComment},
			want: moderation.AccountSubjectFacts{HasAccountActivity: true, CommunityPDSURL: "", IsCommunity: false},
		},
		{
			name: "a vote only", seeds: []func(*testing.T, *sql.DB, string){seedActivityVote},
			want: moderation.AccountSubjectFacts{HasAccountActivity: true, CommunityPDSURL: "", IsCommunity: false},
		},
		{
			name: "a community row with no activity", seeds: []func(*testing.T, *sql.DB, string){insertActivityCommunityRow},
			want: moderation.AccountSubjectFacts{HasAccountActivity: false, CommunityPDSURL: "https://pds.activity-community.test", CommunityHandle: "c-actfacts.coves.social", IsCommunity: true},
		},
		{
			name: "a community row and a vote", seeds: []func(*testing.T, *sql.DB, string){insertActivityCommunityRow, seedActivityVote},
			want: moderation.AccountSubjectFacts{HasAccountActivity: true, CommunityPDSURL: "https://pds.activity-community.test", CommunityHandle: "c-actfacts.coves.social", IsCommunity: true},
		},
		{
			name: "a community row and a users row on another PDS", seeds: []func(*testing.T, *sql.DB, string){insertActivityCommunityRow, seedActivityUser},
			want: moderation.AccountSubjectFacts{HasAccountActivity: true, CommunityPDSURL: "https://pds.activity-community.test", CommunityHandle: "c-actfacts.coves.social", IsCommunity: true},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := testkit.DB(t)
			did := fixtures.DID(testkit.UniqueIDWithPrefix(t, "facts"))
			for _, seed := range test.seeds {
				seed(t, db, did)
			}

			assert.Equal(t, test.want, readAccountSubjectFacts(t, db, did))
		})
	}
}

// readAccountSubjectFacts reads did's AccountSubjectFacts under its subject
// lock in a moderation transaction, as BanAccount does.
func readAccountSubjectFacts(t *testing.T, db *sql.DB, did string) moderation.AccountSubjectFacts {
	t.Helper()
	var facts moderation.AccountSubjectFacts
	require.NoError(t, postgres.NewModerationRepository(db).InTransaction(t.Context(), func(ctx context.Context, tx moderation.Transaction) error {
		if _, err := tx.LockSubject(ctx, "at://"+did); err != nil {
			return err
		}
		var err error
		facts, err = tx.AccountSubjectFacts(ctx, did)
		return err
	}))
	return facts
}

// A users row written by post author hydration is not account activity; one
// written by an account-indexing entry point is, and upgrades a hydrated row.
// Profile and handle updates never change a row's source. Both the community
// create refusal and the ban facts read the source the same way.
func TestUserIndexSourceDecidesAccountActivity(t *testing.T) {
	pdsURL := testkit.Endpoints().PDS.BaseURL
	for _, test := range []struct {
		name         string
		index        func(t *testing.T, service users.UserService, did, handle string)
		wantSource   users.IndexSource
		wantActivity bool
	}{
		{
			name: "author hydration only",
			index: func(t *testing.T, service users.UserService, did, handle string) {
				require.NoError(t, service.IndexHydratedAuthor(t.Context(), did, handle, pdsURL))
			},
			wantSource: users.IndexSourceAuthorHydration, wantActivity: false,
		},
		{
			name: "author hydration then an authenticated login",
			index: func(t *testing.T, service users.UserService, did, handle string) {
				require.NoError(t, service.IndexHydratedAuthor(t.Context(), did, handle, pdsURL))
				require.NoError(t, service.IndexAuthenticatedUser(t.Context(), did, handle, pdsURL))
			},
			wantSource: users.IndexSourceAccount, wantActivity: true,
		},
		{
			name: "author hydration then firehose account indexing",
			index: func(t *testing.T, service users.UserService, did, handle string) {
				require.NoError(t, service.IndexHydratedAuthor(t.Context(), did, handle, pdsURL))
				require.NoError(t, service.IndexUser(t.Context(), did, handle, pdsURL))
			},
			wantSource: users.IndexSourceAccount, wantActivity: true,
		},
		{
			name: "account indexing then author hydration",
			index: func(t *testing.T, service users.UserService, did, handle string) {
				require.NoError(t, service.IndexUser(t.Context(), did, handle, pdsURL))
				require.NoError(t, service.IndexHydratedAuthor(t.Context(), did, handle, pdsURL))
			},
			wantSource: users.IndexSourceAccount, wantActivity: true,
		},
		{
			name: "author hydration then a profile update and a handle update",
			index: func(t *testing.T, service users.UserService, did, handle string) {
				require.NoError(t, service.IndexHydratedAuthor(t.Context(), did, handle, pdsURL))
				displayName := "Renamed"
				_, err := service.UpdateProfile(t.Context(), did, users.UpdateProfileInput{DisplayName: &displayName})
				require.NoError(t, err)
				_, err = service.UpdateHandle(t.Context(), did, "renamed-"+handle)
				require.NoError(t, err)
			},
			wantSource: users.IndexSourceAuthorHydration, wantActivity: false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := testkit.DB(t)
			did := fixtures.DID(testkit.UniqueIDWithPrefix(t, "idxsource"))
			handle := testkit.UniqueIDWithPrefix(t, "idxsource") + ".test"
			userRepository := postgres.NewUserRepository(db)
			test.index(t, users.NewUserService(userRepository, nil, pdsURL, nil, ""), did, handle)

			stored, err := userRepository.GetByDID(t.Context(), did)
			require.NoError(t, err, "the users row exists")
			assert.Equal(t, test.wantSource, stored.IndexSource)
			assert.Equal(t, test.wantActivity, readAccountSubjectFacts(t, db, did).HasAccountActivity)

			_, err = postgres.NewCommunityRepository(db, credentialciphertest.Fixed()).
				Create(t.Context(), newActivityCommunity(t, did))
			if test.wantActivity {
				require.ErrorIs(t, err, communities.ErrDIDHasAccountActivity)
				assert.Zero(t, countCommunityRows(t, db, did))
				return
			}
			require.NoError(t, err, "a hydrated author's users row does not refuse the community")
			assert.Equal(t, 1, countCommunityRows(t, db, did))
		})
	}
}
