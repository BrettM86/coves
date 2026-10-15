package jetstream

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lib/pq"
)

// lockStatementConnector hands out lockStatementConnections that record every
// statement and answer pg_advisory_lock through onLock.
type lockStatementConnector struct {
	onLock func() error

	mu          sync.Mutex
	statements  []string
	opened      int
	closedConns int
}

func (c *lockStatementConnector) Connect(context.Context) (driver.Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.opened++
	return &lockStatementConnection{connector: c}, nil
}

func (c *lockStatementConnector) Driver() driver.Driver { return lockStatementDriver{} }

func (c *lockStatementConnector) record(statement string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.statements = append(c.statements, statement)
}

func (c *lockStatementConnector) snapshot() (statements []string, opened, closedConns int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.statements...), c.opened, c.closedConns
}

type lockStatementDriver struct{}

func (lockStatementDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("lockStatementDriver is used through its connector")
}

type lockStatementConnection struct {
	connector *lockStatementConnector
}

func (c *lockStatementConnection) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not supported")
}

func (c *lockStatementConnection) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are not supported")
}

func (c *lockStatementConnection) Close() error {
	c.connector.mu.Lock()
	defer c.connector.mu.Unlock()
	c.connector.closedConns++
	return nil
}

func (c *lockStatementConnection) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.connector.record(query)
	if strings.HasPrefix(query, "SELECT pg_advisory_lock(") {
		if err := c.connector.onLock(); err != nil {
			return nil, err
		}
	}
	return driver.RowsAffected(0), nil
}

func (c *lockStatementConnection) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.connector.record(query)
	return &singleBooleanRows{column: "result", value: true}, nil
}

type singleBooleanRows struct {
	column string
	value  bool
	done   bool
}

func (r *singleBooleanRows) Columns() []string { return []string{r.column} }
func (r *singleBooleanRows) Close() error      { return nil }
func (r *singleBooleanRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = r.value
	return nil
}

func containsStatementPrefix(statements []string, prefix string) bool {
	for _, statement := range statements {
		if strings.HasPrefix(statement, prefix) {
			return true
		}
	}
	return false
}

// A cancelled wait can race the grant: lib/pq then closes the connection,
// which releases the lock server-side, but the statement still reports
// success. The callback must not run without the lock.
func TestIdentityRefreshLockGrantLandingAsContextEndsSkipsCallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	connector := &lockStatementConnector{onLock: func() error {
		cancel()
		return nil
	}}
	db := sql.OpenDB(connector)
	defer func() { _ = db.Close() }()
	lock := NewPostgresIdentityRefreshLock(db, 250*time.Millisecond)

	called := false
	err := lock.WithDIDLock(ctx, "did:plc:grantascontextends", func(context.Context) error {
		called = true
		return nil
	})

	statements, _, closedConns := connector.snapshot()
	if called {
		t.Errorf("callback ran although the context ended as the lock was granted")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want one wrapping context.Canceled", err)
	}
	if errors.Is(err, ErrPermanentEvent) {
		t.Errorf("error = %v, want a non-permanent error", err)
	}
	if containsStatementPrefix(statements, "SELECT pg_advisory_unlock(") {
		t.Errorf("statements = %q, want no pg_advisory_unlock on a discarded connection", statements)
	}
	if closedConns != 1 {
		t.Errorf("closed connections = %d, want 1 (the lock connection discarded)", closedConns)
	}
}

// lock_timeout expiring means the lock was never granted: the refresh is
// deferred to the redriver and the connection stays usable.
func TestIdentityRefreshLockBusyIsDeferredAndKeepsConnection(t *testing.T) {
	connector := &lockStatementConnector{onLock: func() error {
		return &pq.Error{Code: "55P03", Message: "canceling statement due to lock timeout"}
	}}
	db := sql.OpenDB(connector)
	defer func() { _ = db.Close() }()
	lock := NewPostgresIdentityRefreshLock(db, 250*time.Millisecond)

	called := false
	err := lock.WithDIDLock(context.Background(), "did:plc:lockbusy", func(context.Context) error {
		called = true
		return nil
	})

	statements, opened, closedConns := connector.snapshot()
	if called {
		t.Errorf("callback ran although the lock wait timed out")
	}
	for _, target := range []error{ErrIdentityRefreshBusy, ErrUnresolvedReference, errRedriveDeferred} {
		if !errors.Is(err, target) {
			t.Errorf("error = %v, want one wrapping %v", err, target)
		}
	}
	if errors.Is(err, ErrPermanentEvent) {
		t.Errorf("error = %v, want a non-permanent error", err)
	}
	if len(statements) == 0 || statements[0] != "SET lock_timeout = 250" {
		t.Errorf("statements = %q, want the first to be %q", statements, "SET lock_timeout = 250")
	}
	if opened != 1 || closedConns != 0 {
		t.Errorf("opened, closed connections = %d, %d; want 1, 0 (the lock connection returned to the pool)", opened, closedConns)
	}
}
