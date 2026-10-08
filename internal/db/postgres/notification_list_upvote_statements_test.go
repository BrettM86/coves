//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"strconv"
	"sync"
	"testing"
	"time"

	"Coves/internal/core/notifications"
	"Coves/internal/core/posts"
	"Coves/tests/testkit"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

type listStatementCounter struct {
	mu     sync.Mutex
	count  int
	hookAt int
	hook   func()
}

func (c *listStatementCounter) beforeStatement() {
	c.mu.Lock()
	c.count++
	number, hook := c.count, c.hook
	c.mu.Unlock()
	if number == c.hookAt && hook != nil {
		hook()
	}
}

func (c *listStatementCounter) reset(hookAt int, hook func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.count, c.hookAt, c.hook = 0, hookAt, hook
}

func (c *listStatementCounter) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count
}

type listCountingConnector struct {
	underlying driver.Connector
	counter    *listStatementCounter
}

func (c *listCountingConnector) Connect(ctx context.Context) (driver.Conn, error) {
	connection, err := c.underlying.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &listCountingConn{Conn: connection, counter: c.counter}, nil
}

func (c *listCountingConnector) Driver() driver.Driver { return c.underlying.Driver() }

type listCountingConn struct {
	driver.Conn
	counter *listStatementCounter
}

func (c *listCountingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.counter.beforeStatement()
	if queryer, ok := c.Conn.(driver.QueryerContext); ok {
		return queryer.QueryContext(ctx, query, args)
	}
	values := make([]driver.Value, len(args))
	for index, arg := range args {
		values[index] = arg.Value
	}
	return c.Conn.(driver.Queryer).Query(query, values)
}

func (c *listCountingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.counter.beforeStatement()
	if executor, ok := c.Conn.(driver.ExecerContext); ok {
		return executor.ExecContext(ctx, query, args)
	}
	values := make([]driver.Value, len(args))
	for index, arg := range args {
		values[index] = arg.Value
	}
	return c.Conn.(driver.Execer).Exec(query, values)
}

func (c *listCountingConn) Prepare(query string) (driver.Stmt, error) {
	c.counter.beforeStatement()
	return c.Conn.Prepare(query)
}

func (c *listCountingConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	c.counter.beforeStatement()
	if preparer, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return preparer.PrepareContext(ctx, query)
	}
	return c.Conn.Prepare(query)
}

func (c *listCountingConn) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	if beginner, ok := c.Conn.(driver.ConnBeginTx); ok {
		return beginner.BeginTx(ctx, options)
	}
	return c.Conn.Begin()
}

func listCountingRepository(t *testing.T, source *sql.DB) (notifications.ReadRepository, *listStatementCounter) {
	t.Helper()
	var database string
	require.NoError(t, source.QueryRowContext(context.Background(), `SELECT current_database()`).Scan(&database))
	connector, err := pq.NewConnector(testkit.Endpoints().Postgres.URL(database))
	require.NoError(t, err)
	counter := &listStatementCounter{}
	db := sql.OpenDB(&listCountingConnector{underlying: connector, counter: counter})
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return NewNotificationRepository(db).(notifications.ReadRepository), counter
}

func TestNotificationList_UpvoteAggregatesUseBoundedStatements(t *testing.T) {
	t.Parallel()
	var statementTotals []int
	for _, size := range []int{50, 8} {
		t.Run(strconv.Itoa(size)+" mixed rows", func(t *testing.T) {
			f := newUnreadCountFixture(t)
			retentionSeenAt(t, f.db, f.recipient, f.sortAt.Add(-time.Second))
			voters := map[string]string{}
			for i := 0; i < size; i++ {
				at := f.sortAt.Add(time.Duration(i+1) * time.Second)
				switch i % 4 {
				case 0:
					post := f.post(t, posts.AdmissionStatusAccepted, false)
					voter := "did:plc:listvoter" + testkit.UniqueID(t)
					f.listVoteAt(t, voter, post, "up", at)
					f.listGroupAt(t, post, post, at)
					voters[post] = voter
				case 1:
					f.insertListedReply(t, f.recipient, "postReply", f.root, at, f.sortAt)
				case 2:
					f.insertListedReply(t, f.recipient, "commentReply", f.comment(t, f.root), at, f.sortAt)
				case 3:
					record := f.comment(t, f.root)
					f.listNotificationAt(t, f.recipient, f.actor, "mention", record, "", f.root, at)
				}
			}
			repository, counter := listCountingRepository(t, f.db)
			page, err := repository.List(context.Background(), f.recipient, "", size)
			require.NoError(t, err)
			require.Equal(t, size, len(page.Notifications), "all reasons must fill the mixed page")
			require.Len(t, voters, (size+3)/4)
			for _, row := range page.Notifications {
				if row.Reason != notifications.ReasonUpvote {
					continue
				}
				require.Equal(t, 1, row.UpvoteCount, "group %s", row.SubjectURI)
				require.Equal(t, []string{voters[row.SubjectURI]}, row.RecentUpvoterDIDs)
			}
			require.LessOrEqual(t, counter.total(), 3, "seen state, page and one aggregate statement")
			statementTotals = append(statementTotals, counter.total())
		})
	}
	if len(statementTotals) == 2 {
		require.Equal(t, statementTotals[0], statementTotals[1], "page size must not change the statement count")
	}
}

func TestNotificationList_UpvoteAggregateSharesPageSnapshot(t *testing.T) {
	t.Parallel()
	f := newUnreadCountFixture(t)
	retentionSeenAt(t, f.db, f.recipient, f.sortAt.Add(-time.Second))
	voters := []string{
		"did:plc:listvoter" + testkit.UniqueID(t),
		"did:plc:listvoter" + testkit.UniqueID(t),
		"did:plc:listvoter" + testkit.UniqueID(t),
	}
	var newestVote string
	for index, voter := range voters {
		newestVote = f.listVoteAt(t, voter, f.root, "up", f.sortAt.Add(time.Duration(index)*time.Second))
	}
	f.listGroupAt(t, f.root, f.root, f.sortAt.Add(4*time.Second))
	repository, counter := listCountingRepository(t, f.db)
	mutated := false
	counter.reset(3, func() {
		result, err := f.db.ExecContext(context.Background(), `UPDATE votes SET deleted_at = $2 WHERE uri = $1`, newestVote, f.sortAt.Add(5*time.Second))
		require.NoError(t, err)
		rows, err := result.RowsAffected()
		require.NoError(t, err)
		require.EqualValues(t, 1, rows)
		mutated = true
	})
	first, err := repository.List(context.Background(), f.recipient, "", 10)
	require.NoError(t, err)
	require.Len(t, first.Notifications, 1, "the qualifying group must be listed")
	require.Equal(t, f.root, first.Notifications[0].SubjectURI)
	require.True(t, mutated, "retraction must commit before aggregate statement")
	require.Equal(t, 3, first.Notifications[0].UpvoteCount)
	require.Equal(t, []string{voters[2], voters[1], voters[0]}, first.Notifications[0].RecentUpvoterDIDs)
	counter.reset(0, nil)
	second, err := repository.List(context.Background(), f.recipient, "", 10)
	require.NoError(t, err)
	require.Len(t, second.Notifications, 1)
	require.Equal(t, 2, second.Notifications[0].UpvoteCount)
	require.Equal(t, []string{voters[1], voters[0]}, second.Notifications[0].RecentUpvoterDIDs)
}
