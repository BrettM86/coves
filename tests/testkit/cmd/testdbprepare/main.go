// Command testdbprepare provisions the template database that testkit.DB
// clones, and sweeps the clones and private templates left behind by processes
// that died before their cleanup ran.
//
// It exists as a Go program rather than as SQL in a shell script for one
// reason: the CI runner image has no psql and no goose binary, and adding them
// would mean the provisioning logic lived in two places — here and in
// tests/testkit/db.go — with nothing keeping them in step. This shares the
// harness's own code, so the template the script builds is by construction the
// template the fallback path would have built.
//
// Driven by scripts/test-db-prepare.sh; see that script for the wiring.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"Coves/tests/testkit"
)

func main() {
	force := flag.Bool("force", false,
		"rebuild the template even if its stamp already matches the migrations")
	sweepAge := flag.Duration("sweep-age", time.Hour,
		"drop idle leftover clone and private-template databases older than this (0 disables the sweep)")
	wait := flag.Duration("wait", 60*time.Second,
		"how long to wait for Postgres to accept connections before giving up")
	printFlags := flag.Bool("print-flags", false,
		"print only the safe `go test` concurrency flags for this server, and exit")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *printFlags {
		if err := reportConcurrencyBudget(ctx, *wait); err != nil {
			fmt.Fprintf(os.Stderr, "test-db-prepare: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := run(ctx, *force, *sweepAge, *wait); err != nil {
		fmt.Fprintf(os.Stderr, "test-db-prepare: %v\n", err)
		os.Exit(1)
	}
}

// reportConcurrencyBudget prints the concurrency flags and nothing else, so a
// shell can splice them into a `go test` command line with $(...).
func reportConcurrencyBudget(ctx context.Context, wait time.Duration) error {
	if err := testkit.WaitForPostgres(ctx, wait); err != nil {
		return err
	}
	budget, err := testkit.ConcurrencyBudget(ctx)
	if err != nil {
		return err
	}
	fmt.Println(budget.Flags())
	return nil
}

func run(ctx context.Context, force bool, sweepAge, wait time.Duration) error {
	pg := testkit.Endpoints().Postgres

	fmt.Printf("  server:   %s\n", pg.Redacted(pg.Database))
	fmt.Printf("  template: %s\n", testkit.TemplateName())

	if err := testkit.WaitForPostgres(ctx, wait); err != nil {
		return err
	}

	hash, err := testkit.MigrationsHash()
	if err != nil {
		return err
	}

	// One call, one lock acquisition. Asking whether the template is current,
	// releasing the lock, and then acting on the answer would act on a fact
	// another process may have invalidated in between — and would need a
	// hardcoded force=true to make the second step unconditional, which throws
	// away the re-check the lock exists to protect.
	start := time.Now()
	action, err := testkit.ProvisionTemplate(ctx, force)
	if err != nil {
		return err
	}
	if action == testkit.ProvisionUpToDate {
		fmt.Printf("  up to date (migrations %s)\n", hash[:12])
	} else {
		fmt.Printf("  %s: migrated and stamped %s in %s\n",
			action, hash[:12], time.Since(start).Round(time.Millisecond))
	}

	if sweepAge > 0 {
		result, err := testkit.SweepOrphanClones(ctx, sweepAge)
		if err != nil {
			return fmt.Errorf("sweeping orphaned testkit databases: %w", err)
		}
		if len(result.Dropped) > 0 {
			fmt.Printf("  swept %d orphaned database(s) older than %s\n", len(result.Dropped), sweepAge)
			for _, name := range result.Dropped {
				fmt.Printf("    - %s\n", name)
			}
		}
		// Warnings, not failures. A clone still in use by a concurrent run, or
		// one Postgres declines to drop, is not a reason to fail the gate: the
		// sweep is opportunistic cleanup and the next run retries.
		for _, name := range result.Skipped {
			fmt.Printf("  ! kept %s: still has active sessions\n", name)
		}
		for name, dropErr := range result.Failed {
			fmt.Printf("  ! could not drop %s: %v\n", name, dropErr)
		}
	}

	return nil
}
