package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"

	"Coves/internal/atproto/identity"
	"Coves/internal/atproto/jetstream"
	covesoauth "Coves/internal/atproto/oauth"
	"Coves/internal/core/communities"
	"Coves/internal/core/users"
	"Coves/internal/crypto/credentialcipher"
	"Coves/internal/db/postgres"
)

const (
	kindUser      = "user"
	kindCommunity = "community"
)

// Reason codes printed for a row that still differs.
const (
	reasonResolveError     = "resolve-error"
	reasonUnverified       = "unverified"
	reasonSubjectMismatch  = "subject-mismatch"
	reasonHandleConflict   = "handle-conflict"
	reasonInvalidHandle    = "invalid-handle"
	reasonHostedByMismatch = "hostedby-mismatch"
	reasonLockBusy         = "lock-busy"
	// reasonHostedByPending: the community's hostedBy DID document could not
	// be fetched in this run, even when retried alone after the parallel pass.
	reasonHostedByPending = "hostedby-pending"
	// reasonError is any failure outside the codes above (a database error, a
	// purge failure, a hostedBy document that could not be fetched). reconcile
	// returns an error counting these rows.
	reasonError = "error"
)

// bridgedRow is one selected users or communities row.
type bridgedRow struct {
	kind   string
	did    string
	handle string
}

type rowStatus int

const (
	rowUnchanged rowStatus = iota
	rowUpdated
	rowWouldUpdate
	rowFailed
)

// rowOutcome is what the sweep established for one row.
type rowOutcome struct {
	differing differingRow // set for rowWouldUpdate and rowFailed
	status    rowStatus
}

// handleRefresher runs one DID's locked handle refresh.
type handleRefresher func(ctx context.Context, did string) (jetstream.IdentityHandleRefresh, error)

// reconcile runs one sweep and writes the human report to out.
func reconcile(ctx context.Context, options sweepOptions, dependencies sweepDependencies, out io.Writer) (sweepReport, error) {
	report := sweepReport{ClearedIdentityCacheRows: -1}
	if err := checkSweep(options, dependencies); err != nil {
		return report, err
	}

	var inspect func(context.Context, bridgedRow) rowOutcome
	if options.DryRun {
		inspect = func(ctx context.Context, row bridgedRow) rowOutcome {
			return inspectRow(ctx, dependencies.UncachedResolver, row)
		}
	} else {
		refreshers, err := newHandleRefreshers(dependencies)
		if err != nil {
			return report, err
		}
		inspect = func(ctx context.Context, row bridgedRow) rowOutcome {
			return refreshRow(ctx, refreshers[row.kind], row)
		}
	}

	printer := &reportPrinter{out: out}
	if options.ClearIdentityCache {
		deleted, err := clearIdentityCache(ctx, dependencies.Database)
		if err != nil {
			return report, err
		}
		report.ClearedIdentityCacheRows = deleted
		printer.printf("identity_cache_deleted=%d\n", deleted)
	}

	rows, err := selectBridgedRows(ctx, dependencies.Database, dependencies.Trust)
	if err != nil {
		return report, err
	}
	report.Checked = len(rows)

	outcomes := sweepRows(ctx, rows, options.Concurrency, inspect)
	if err := ctx.Err(); err != nil {
		return report, fmt.Errorf("sweep interrupted before every row was checked: %w", err)
	}

	failedWithError := 0
	for _, outcome := range outcomes {
		switch outcome.status {
		case rowUnchanged:
			report.Unchanged++
		case rowUpdated:
			report.Updated++
		case rowWouldUpdate:
			report.WouldUpdate++
			report.StillDiffering++
			report.Differing = append(report.Differing, outcome.differing)
		case rowFailed:
			report.StillDiffering++
			report.Differing = append(report.Differing, outcome.differing)
			if outcome.differing.Reason == reasonError {
				failedWithError++
			}
		}
	}
	sort.Slice(report.Differing, func(i, j int) bool {
		if report.Differing[i].Kind != report.Differing[j].Kind {
			return report.Differing[i].Kind < report.Differing[j].Kind
		}
		return report.Differing[i].DID < report.Differing[j].DID
	})

	for _, row := range report.Differing {
		printer.printf("differs kind=%s did=%s stored=%s verified=%s reason=%s\n",
			row.Kind, row.DID, dashIfEmpty(row.StoredHandle), dashIfEmpty(row.VerifiedHandle), dashIfEmpty(row.Reason))
	}
	if options.DryRun {
		printer.printf("checked=%d would_update=%d unchanged=%d still_differing=%d\n",
			report.Checked, report.WouldUpdate, report.Unchanged, report.StillDiffering)
	} else {
		printer.printf("checked=%d updated=%d unchanged=%d still_differing=%d\n",
			report.Checked, report.Updated, report.Unchanged, report.StillDiffering)
	}

	var failures []error
	if failedWithError > 0 {
		failures = append(failures, fmt.Errorf("%d row(s) failed with reason %s", failedWithError, reasonError))
	}
	if printer.err != nil {
		failures = append(failures, fmt.Errorf("write report: %w", printer.err))
	}
	return report, errors.Join(failures...)
}

// checkSweep refuses a sweep its options or dependencies cannot run safely.
func checkSweep(options sweepOptions, dependencies sweepDependencies) error {
	if options.DryRun && options.ClearIdentityCache {
		return errors.New("-dry-run and -clear-identity-cache cannot be combined")
	}
	if options.Concurrency < 1 {
		return fmt.Errorf("concurrency must be at least 1 (got %d)", options.Concurrency)
	}
	if dependencies.Database == nil {
		return errors.New("no database")
	}
	if dependencies.UncachedResolver == nil {
		return errors.New("no uncached identity resolver")
	}
	if dependencies.Trust == nil {
		return errors.New("no trusted bridge hosts")
	}
	if options.DryRun {
		return nil
	}
	if dependencies.LockDatabase == nil {
		return errors.New("no identity refresh lock database")
	}
	if dependencies.CachingResolver == nil {
		return errors.New("no caching identity resolver")
	}
	if dependencies.LockWaitTimeout < 0 {
		return fmt.Errorf("lock wait timeout must not be negative (got %s)", dependencies.LockWaitTimeout)
	}
	return nil
}

// newHandleRefreshers builds the identity-event consumers' own refresh entry
// points, one per row kind, sharing one per-DID lock on the lock pool.
func newHandleRefreshers(dependencies sweepDependencies) (map[string]handleRefresher, error) {
	wait := dependencies.LockWaitTimeout
	if wait == 0 {
		wait = jetstream.IdentityRefreshLockWaitTimeout
	}
	lock := jetstream.NewPostgresIdentityRefreshLock(dependencies.LockDatabase, wait)

	sessionStore := covesoauth.NewMobileAwareStoreWrapper(covesoauth.NewPostgresOAuthStore(dependencies.Database, 0)).
		UnwrapPostgresStore()
	if sessionStore == nil {
		return nil, errors.New("OAuth session store is not PostgreSQL-backed, so session handles cannot be updated")
	}
	userService := users.NewUserService(postgres.NewUserRepository(dependencies.Database), dependencies.CachingResolver,
		"", nil, "", users.WithInstanceDomain(dependencies.InstanceDomain))
	userConsumer := jetstream.NewUserEventConsumer(userService, dependencies.CachingResolver,
		jetstream.WithUserUncachedIdentityResolver(dependencies.UncachedResolver),
		jetstream.WithUserIdentityRefreshLock(lock),
		jetstream.WithSessionHandleUpdater(sessionStore),
	)

	// The community refresh reads the row through the repository, which
	// requires a credential cipher. The sweep never reads community
	// credentials, and bridged communities hold none, so it gets a cipher with
	// a fresh random key: a row that does carry credentials fails to decrypt
	// and is reported, rather than being read with the AppView's key.
	cipherKey := make([]byte, 32)
	if _, err := rand.Read(cipherKey); err != nil {
		return nil, fmt.Errorf("generate community repository cipher key: %w", err)
	}
	cipher, err := credentialcipher.New(cipherKey)
	if err != nil {
		return nil, fmt.Errorf("build community repository cipher: %w", err)
	}
	communityOptions := append([]jetstream.CommunityConsumerOption{
		jetstream.WithCommunityUncachedIdentityResolver(dependencies.UncachedResolver),
		jetstream.WithCommunityIdentityRefreshLock(lock),
	}, jetstream.PrivateHostOptions(dependencies.AllowPrivateHosts)...)
	communityConsumer := jetstream.NewCommunityEventConsumer(
		postgres.NewCommunityRepository(dependencies.Database, cipher), "",
		dependencies.SkipHostedByVerification, dependencies.CachingResolver, communityOptions...)

	return map[string]handleRefresher{
		kindUser:      userConsumer.RefreshUserHandle,
		kindCommunity: communityConsumer.RefreshCommunityHandle,
	}, nil
}

// clearIdentityCache deletes every identity_cache row and returns how many.
func clearIdentityCache(ctx context.Context, db *sql.DB) (int64, error) {
	result, err := db.ExecContext(ctx, `DELETE FROM identity_cache`)
	if err != nil {
		return 0, fmt.Errorf("clear identity_cache: %w", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count cleared identity_cache rows: %w", err)
	}
	return deleted, nil
}

// selectBridgedRows returns every users and communities row whose stored
// pds_url is a trusted bridge host.
func selectBridgedRows(ctx context.Context, db *sql.DB, trust *jetstream.BridgeTrust) ([]bridgedRow, error) {
	var selected []bridgedRow
	for _, table := range []struct {
		kind  string
		query string
	}{
		{kindUser, `SELECT did, handle, COALESCE(pds_url, '') FROM users ORDER BY did`},
		{kindCommunity, `SELECT did, handle, COALESCE(pds_url, '') FROM communities ORDER BY did`},
	} {
		rows, err := selectTrustedRows(ctx, db, trust, table.kind, table.query)
		if err != nil {
			return nil, err
		}
		selected = append(selected, rows...)
	}
	return selected, nil
}

func selectTrustedRows(ctx context.Context, db *sql.DB, trust *jetstream.BridgeTrust, kind, query string) ([]bridgedRow, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("select %s rows: %w", kind, err)
	}
	defer func() { _ = rows.Close() }()

	var selected []bridgedRow
	for rows.Next() {
		var did, handle, pdsURL string
		if err := rows.Scan(&did, &handle, &pdsURL); err != nil {
			return nil, fmt.Errorf("read %s row: %w", kind, err)
		}
		if trust.TrustsPDS(pdsURL) {
			selected = append(selected, bridgedRow{kind: kind, did: did, handle: handle})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("select %s rows: %w", kind, err)
	}
	return selected, nil
}

// sweepRows inspects every row, then re-inspects each hostedby-pending row
// alone, in row order. Communities share one hostedBy verification per
// (hostedBy DID, handle domain), so in the parallel pass every refresh but the
// one fetching the DID document is answered "pending"; a row still pending
// after its retry keeps that reason.
func sweepRows(ctx context.Context, rows []bridgedRow, concurrency int,
	inspect func(context.Context, bridgedRow) rowOutcome,
) []rowOutcome {
	outcomes := inspectRows(ctx, rows, concurrency, inspect)
	for index, outcome := range outcomes {
		if ctx.Err() != nil {
			break
		}
		if outcome.differing.Reason == reasonHostedByPending {
			outcomes[index] = inspect(ctx, rows[index])
		}
	}
	return outcomes
}

// inspectRows runs inspect over rows with at most concurrency at once. Rows
// not started before ctx ends keep a zero outcome; the caller checks ctx.
func inspectRows(ctx context.Context, rows []bridgedRow, concurrency int,
	inspect func(context.Context, bridgedRow) rowOutcome,
) []rowOutcome {
	outcomes := make([]rowOutcome, len(rows))
	indexes := make(chan int)
	var workers sync.WaitGroup
	for range min(concurrency, len(rows)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range indexes {
				outcomes[index] = inspect(ctx, rows[index])
			}
		}()
	}
feed:
	for index := range rows {
		select {
		case indexes <- index:
		case <-ctx.Done():
			break feed
		}
	}
	close(indexes)
	workers.Wait()
	return outcomes
}

// refreshRow runs the locked refresh for one row and classifies the result.
func refreshRow(ctx context.Context, refresh handleRefresher, row bridgedRow) rowOutcome {
	result, err := refresh(ctx, row.did)
	if err == nil {
		if result.Changed {
			return rowOutcome{status: rowUpdated}
		}
		return rowOutcome{status: rowUnchanged}
	}

	stored := result.StoredHandle
	if stored == "" {
		stored = row.handle
	}
	return rowOutcome{
		status: rowFailed,
		differing: differingRow{
			Kind: row.kind, DID: row.did, StoredHandle: stored,
			VerifiedHandle: result.VerifiedHandle, Reason: refreshFailureReason(err), Cause: err,
		},
	}
}

// refreshFailureReason maps a refresh error to its reason code.
func refreshFailureReason(err error) string {
	switch {
	case errors.Is(err, jetstream.ErrIdentityRefreshBusy):
		return reasonLockBusy
	case errors.Is(err, jetstream.ErrIdentityResolveFailed):
		return reasonResolveError
	case errors.Is(err, identity.ErrIdentitySubjectMismatch):
		return reasonSubjectMismatch
	case errors.Is(err, identity.ErrHandleUnverified):
		return reasonUnverified
	case errors.Is(err, jetstream.ErrVerifiedHandleInvalid):
		return reasonInvalidHandle
	case errors.Is(err, jetstream.ErrHostedByMismatch):
		return reasonHostedByMismatch
	case errors.Is(err, jetstream.ErrHostedByVerificationPending):
		return reasonHostedByPending
	case errors.Is(err, users.ErrHandleAlreadyTaken), errors.Is(err, communities.ErrHandleTaken):
		return reasonHandleConflict
	default:
		return reasonError
	}
}

// inspectRow is the dry run's read-only check: a fresh uncached resolution and
// handle verification compared with the stored handle. No lock, no purge, no
// write.
func inspectRow(ctx context.Context, resolver identity.UncachedResolver, row bridgedRow) rowOutcome {
	failed := func(reason string, cause error) rowOutcome {
		return rowOutcome{status: rowFailed, differing: differingRow{
			Kind: row.kind, DID: row.did, StoredHandle: row.handle, Reason: reason, Cause: cause,
		}}
	}
	resolved, err := resolver.Resolve(ctx, row.did)
	if err != nil {
		return failed(reasonResolveError, fmt.Errorf("resolve identity: %w", err))
	}
	verified, err := identity.VerifiedHandle(resolved, row.did)
	if errors.Is(err, identity.ErrIdentitySubjectMismatch) {
		return failed(reasonSubjectMismatch, fmt.Errorf("verify handle: %w", err))
	}
	if err != nil {
		return failed(reasonUnverified, fmt.Errorf("verify handle: %w", err))
	}
	if verified == row.handle {
		return rowOutcome{status: rowUnchanged}
	}
	return rowOutcome{status: rowWouldUpdate, differing: differingRow{
		Kind: row.kind, DID: row.did, StoredHandle: row.handle, VerifiedHandle: verified,
	}}
}

// reportPrinter writes report lines and keeps the first write error.
type reportPrinter struct {
	out io.Writer
	err error
}

func (p *reportPrinter) printf(format string, args ...any) {
	if p.err != nil {
		return
	}
	_, p.err = fmt.Fprintf(p.out, format, args...)
}

func dashIfEmpty(value string) string {
	if value == "" {
		return "-"
	}
	return value
}
