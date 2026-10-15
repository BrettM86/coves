// Command reconcile-handles re-resolves the handles of bridged users and
// communities (rows whose stored pds_url is on a trusted bridge host), updates
// stored handles through the per-DID locked identity refresh, and reports the
// rows whose stored handle still differs from a verified one.
//
// It is the Coves half of the bridge handle-rename rollout: step 6 runs it
// after the rename job finishes, and step 6a reads its report as the gate (exit
// status 0 only when every bridged row matches its verified handle).
//
// A normal run takes, per DID, the same locked refresh the identity-event
// consumers run: purge the cached DID answer, resolve the DID fresh through the
// uncached resolver, verify the handle, re-read the stored handle inside the
// lock, purge the old handle and write the new one. Users also get their OAuth
// session handles updated; communities pass ValidateHandle and the hostedBy
// verification first. A dry run only resolves and verifies: it takes no lock,
// purges nothing and writes nothing.
//
// It loads the AppView's own configuration (internal/config), so run it with
// the AppView's environment: it resolves through the same PLC directory,
// well-known overrides and private-host rules as cmd/server.
//
// Usage:
//
//	DATABASE_URL=postgres://... reconcile-handles [-dry-run] [-clear-identity-cache]
//	    [-concurrency N] [-trusted-hosts https://bridge.example,...]
//
// Flags:
//
//	-dry-run               report what would change; write nothing
//	-clear-identity-cache  delete every identity_cache row before the sweep
//	                       (refused together with -dry-run)
//	-concurrency N         DIDs refreshed at once (default 4)
//	-trusted-hosts LIST    comma-separated bridge PDS URLs; replaces
//	                       TRUSTED_BRIDGE_PDS_HOSTS
//
// Output, one item per line: identity_cache_deleted=<n> when the cache was
// cleared; "differs kind=<user|community> did=<did> stored=<handle>
// verified=<handle|-> reason=<code|->" per row that still differs; and a
// totals line, "checked=<n> updated=<n> unchanged=<n> still_differing=<n>"
// (a dry run prints would_update instead of updated). Reason codes:
// resolve-error, unverified, subject-mismatch, handle-conflict,
// invalid-handle, hostedby-mismatch, hostedby-pending (the hostedBy DID
// document could not be fetched in this run, and a transient failure stays
// cached for the run; re-run), lock-busy (another refresh held the DID's lock;
// re-run), and error (any other failure).
//
// On stderr it prints "reconcile-handles: plc_url=<url>" once, naming the PLC
// directory the resolvers use, and one "reconcile-handles: cause
// kind=<user|community> did=<did> reason=<code>: <error>" line per failed row,
// in the order of the differs lines.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"Coves/internal/atproto/identity"
	"Coves/internal/atproto/jetstream"
	"Coves/internal/core/bridgedvotes"
)

// trustedHostsVariable is the environment variable -trusted-hosts replaces.
const trustedHostsVariable = "TRUSTED_BRIDGE_PDS_HOSTS"

type sweepOptions struct {
	DryRun             bool
	ClearIdentityCache bool
	Concurrency        int
}

type sweepDependencies struct {
	Database                 *sql.DB // selection, writes, identity_cache clear, OAuth session updates
	LockDatabase             *sql.DB // dedicated pool for the per-DID advisory lock only
	CachingResolver          identity.Resolver
	UncachedResolver         identity.UncachedResolver
	Trust                    *jetstream.BridgeTrust
	SkipHostedByVerification bool          // true skips the whole hostedBy check, domain match included
	AllowPrivateHosts        bool          // IS_DEV_ENV: opens the hostedBy fetch's private-address hatch, as cmd/server does
	LockWaitTimeout          time.Duration // 0 = jetstream.IdentityRefreshLockWaitTimeout
	InstanceDomain           string        // users.WithInstanceDomain, as cmd/server sets it; "" skips that check
	PLCURL                   string        // the PLC directory the resolvers use; printed once on stderr
}

type sweepReport struct {
	ClearedIdentityCacheRows int64 // -1 when the cache was not cleared
	Checked                  int
	Updated                  int // normal run only
	WouldUpdate              int // dry run only
	Unchanged                int
	StillDiffering           int
	Differing                []differingRow
}

type differingRow struct {
	Kind           string // "user" | "community"
	DID            string
	StoredHandle   string
	VerifiedHandle string // empty when no handle verified
	Reason         string // "" on a dry-run would-update row; otherwise a reason code
	Cause          error  // the failure behind Reason; nil on a dry-run would-update row
}

// exitStatus is 0 only when err is nil and no row still differs.
func exitStatus(report sweepReport, err error) int {
	if err != nil || report.StillDiffering > 0 {
		return 1
	}
	return 0
}

// openFunc opens the database pools and resolvers. run calls it only after
// every flag/env validation passed. run itself sets dependencies.Trust from the
// effective host list (-trusted-hosts replaces TRUSTED_BRIDGE_PDS_HOSTS; both are
// comma-separated; empty entries are dropped), overwriting whatever open returned.
type openFunc func(ctx context.Context, getenv func(string) string, concurrency int) (sweepDependencies, func(), error)

// run parses flags (-dry-run, -clear-identity-cache, -concurrency (default 4),
// -trusted-hosts) and env, validates, calls open, then reconcile, then prints
// errors to stderr. Returns the process exit status (open error → non-zero).
func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer, open openFunc) int {
	flags := flag.NewFlagSet("reconcile-handles", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dryRun := flags.Bool("dry-run", false,
		"resolve and verify every bridged DID and report what would change; take no lock, purge nothing, write nothing")
	clearIdentityCache := flags.Bool("clear-identity-cache", false,
		"delete every identity_cache row before the sweep (not allowed with -dry-run)")
	concurrency := flags.Int("concurrency", 4, "number of DIDs refreshed at once")
	trustedHosts := flags.String("trusted-hosts", "",
		"comma-separated trusted bridge PDS URLs (scheme and host); replaces "+trustedHostsVariable)
	complain := func(format string, args ...any) {
		_, _ = fmt.Fprintf(stderr, "reconcile-handles: "+format+"\n", args...)
	}
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		complain("unexpected arguments: %s", strings.Join(flags.Args(), " "))
		return 2
	}
	if *dryRun && *clearIdentityCache {
		complain("-dry-run and -clear-identity-cache cannot be combined: a dry run must not change identity_cache")
		return 2
	}
	if *concurrency < 1 {
		complain("-concurrency must be at least 1 (got %d)", *concurrency)
		return 2
	}

	rawHosts, hostsSource := getenv(trustedHostsVariable), trustedHostsVariable
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "trusted-hosts" {
			rawHosts, hostsSource = *trustedHosts, "-trusted-hosts"
		}
	})
	hosts := splitHostList(rawHosts)
	if len(hosts) == 0 {
		complain("no trusted bridge hosts: set %s or pass -trusted-hosts", trustedHostsVariable)
		return 2
	}
	for _, host := range hosts {
		if _, err := bridgedvotes.ParseTrustedHost(host); err != nil {
			complain("%s: %v (scheme and host only, e.g. https://tdpl.io)", hostsSource, err)
			return 2
		}
	}

	dependencies, closeDependencies, err := open(ctx, getenv, *concurrency)
	if err != nil {
		complain("%v", err)
		return 1
	}
	if closeDependencies != nil {
		defer closeDependencies()
	}
	dependencies.Trust = jetstream.NewBridgeTrust(hosts)
	complain("plc_url=%s", dashIfEmpty(dependencies.PLCURL))

	options := sweepOptions{DryRun: *dryRun, ClearIdentityCache: *clearIdentityCache, Concurrency: *concurrency}
	report, err := reconcile(ctx, options, dependencies, stdout)
	for _, row := range report.Differing {
		if row.Cause != nil {
			complain("cause kind=%s did=%s reason=%s: %v", row.Kind, row.DID, row.Reason, row.Cause)
		}
	}
	if err != nil {
		complain("%v", err)
	}
	return exitStatus(report, err)
}

// splitHostList splits a comma-separated host list, trimming each entry and
// dropping empty ones.
func splitHostList(raw string) []string {
	var hosts []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			hosts = append(hosts, part)
		}
	}
	return hosts
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	status := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr, openProduction)
	stop()
	os.Exit(status)
}
