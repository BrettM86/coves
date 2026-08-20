// cmd/backfill-profiles/main.go
// One-off reconciliation job for users indexed without profile data.
//
// A social.coves.actor.profile record is written once at rkey "self", so its
// firehose event fires exactly once. Users whose profile event was missed
// (e.g. while bsky.network was throttling bridge PDSs) were indexed with only
// DID/handle/PDS and stay bare forever — nothing replays a lost profile commit.
//
// This job finds every user with a completely empty profile and fetches their
// profile record directly from their own PDS via com.atproto.repo.getRecord.
// It is safe to re-run: only users still lacking all profile fields are touched,
// and users without a profile record are simply skipped.
//
// If DATABASE_URL is unset, the job falls back to the local dev database
// (localhost:5435/coves_dev) and logs a prominent warning — set it explicitly
// when running against any other environment.
//
// Usage:
//
//	DATABASE_URL=postgres://... go run ./cmd/backfill-profiles [-dry-run] [-concurrency N]
package main

import (
	"context"
	"database/sql"
	"flag"
	"log"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	covesoauth "Coves/internal/atproto/oauth"
	"Coves/internal/core/users"
	"Coves/internal/db/postgres"

	_ "github.com/lib/pq"
)

type bareUser struct {
	did    string
	pdsURL string
}

// backfillStats aggregates the concurrent workers' outcome counters.
type backfillStats struct {
	updated   atomic.Int64
	noRecord  atomic.Int64
	failed    atomic.Int64
	processed atomic.Int64
}

func main() {
	dryRun := flag.Bool("dry-run", false, "fetch and report what would be updated without writing to the database")
	concurrency := flag.Int("concurrency", 4, "number of concurrent PDS fetches")
	allowPrivateHosts := flag.Bool("allow-private-hosts", false,
		"DEV ONLY: dial PDS URLs that resolve to private, loopback or link-local addresses (a local dev stack); never pass this against a real database")
	flag.Parse()

	if *concurrency < 1 {
		*concurrency = 1
	}

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://dev_user:dev_password@localhost:5435/coves_dev?sslmode=disable"
		log.Printf("WARNING: DATABASE_URL is not set — falling back to the LOCAL DEV database (localhost:5435/coves_dev). Set DATABASE_URL explicitly to target any other environment.")
	}

	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			log.Printf("Warning: failed to close database: %v", closeErr)
		}
	}()

	ctx := context.Background()
	userRepo := postgres.NewUserRepository(db)

	// Users with a completely empty profile: either their profile event was
	// missed, or they genuinely have no profile record (distinguished below by
	// what their PDS returns).
	rows, err := db.QueryContext(ctx, `
		SELECT did, pds_url
		FROM users
		WHERE COALESCE(display_name, '') = ''
		  AND COALESCE(bio, '') = ''
		  AND COALESCE(avatar_cid, '') = ''
		  AND COALESCE(banner_cid, '') = ''
		ORDER BY did
	`)
	if err != nil {
		log.Fatalf("Failed to query users without profile data: %v", err)
	}

	var candidates []bareUser
	for rows.Next() {
		var u bareUser
		if err := rows.Scan(&u.did, &u.pdsURL); err != nil {
			log.Fatalf("Failed to scan user row: %v", err)
		}
		candidates = append(candidates, u)
	}
	if err := rows.Err(); err != nil {
		log.Fatalf("Failed to iterate user rows: %v", err)
	}
	if closeErr := rows.Close(); closeErr != nil {
		log.Printf("Warning: failed to close rows: %v", closeErr)
	}

	log.Printf("Found %d users without profile data (dry-run=%v, concurrency=%d)",
		len(candidates), *dryRun, *concurrency)
	if len(candidates) == 0 {
		return
	}

	client := newProfileFetchClient(*allowPrivateHosts)

	var stats backfillStats
	var wg sync.WaitGroup
	work := make(chan bareUser)

	for i := 0; i < *concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for u := range work {
				processUser(ctx, client, userRepo, u, *dryRun, &stats)
				if n := stats.processed.Add(1); n%50 == 0 {
					log.Printf("Progress: %d/%d processed", n, len(candidates))
				}
			}
		}()
	}

	for _, u := range candidates {
		work <- u
	}
	close(work)
	wg.Wait()

	log.Printf("Done: %d profiles %s, %d users have no profile record, %d failed (re-run to retry failures)",
		stats.updated.Load(), updatedVerb(*dryRun), stats.noRecord.Load(), stats.failed.Load())
	if stats.failed.Load() > 0 {
		os.Exit(1)
	}
}

// profileFetchTimeout bounds one getRecord round trip against a user's PDS.
//
// It is this tool's own value and not the AppView's: processUser wraps each
// fetch in a 20s context, and the client's ceiling has always sat below that so
// a stalled PDS is attributed to the fetch rather than to the context.
const profileFetchTimeout = 15 * time.Second

// newProfileFetchClient builds the client every profile fetch in this job goes
// through, and is the dev gate for this call site.
//
// # WHAT IT IS GUARDING
//
// processUser hands `u.pdsURL` to users.FetchProfileRecord, and that value is
// read straight out of the `users` table — so it is whatever some other
// instance's account data put there, the same attacker-influenced input the
// AppView's own backfill was converted for. Being a CLI makes it worse rather
// than better: it is run by hand against production while an operator reconciles
// an incident, it fetches for every bare user at once, and its failures are one
// log.Printf per user among thousands, so a request to an internal address reads
// as ordinary churn.
//
// # THE FLAG IS THE GATE
//
// DATABASE_URL defaults to the LOCAL DEV database, whose PDS is on loopback, so
// the documented no-environment invocation is exactly the one the guard refuses.
// -allow-private-hosts is what keeps that usage working, and it is the only way
// to open the guard: false yields no options at all, which is
// PrivateAddressOptions' contract.
//
// # THE opts PARAMETER IS THE TEST SEAM
//
// It mirrors users.NewProfileBackfillClient and the other converted sites, and
// exists because a guard test that builds its own client proves only that
// internal/atproto/oauth works. main passes nothing.
//
// # WHY THE CLIENT MOVED OUT OF main()
//
// It was a bare `&http.Client{Timeout: 15 * time.Second}` on a line inside
// main(), which no test can reach — main() opens a database, queries it and
// blocks on a worker pool. This is the smallest extraction that makes the
// construction addressable; main keeps every other line it had.
func newProfileFetchClient(allowPrivateHosts bool, opts ...covesoauth.Option) *http.Client {
	client := covesoauth.NewSSRFSafeHTTPClient(
		append(covesoauth.PrivateAddressOptions(allowPrivateHosts), opts...)...)
	client.Timeout = profileFetchTimeout
	return client
}

func updatedVerb(dryRun bool) string {
	if dryRun {
		return "would be updated"
	}
	return "updated"
}

func processUser(
	ctx context.Context,
	client *http.Client,
	userRepo users.UserRepository,
	u bareUser,
	dryRun bool,
	stats *backfillStats,
) {
	fetchCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	input, err := users.FetchProfileRecord(fetchCtx, client, u.pdsURL, u.did)
	if err != nil {
		log.Printf("FAIL %s (%s): %v", u.did, u.pdsURL, err)
		stats.failed.Add(1)
		return
	}
	if input == nil {
		stats.noRecord.Add(1)
		return
	}

	if dryRun {
		log.Printf("WOULD UPDATE %s: displayName=%v avatar=%v banner=%v bio=%v",
			u.did, input.DisplayName != nil, input.AvatarCID != nil, input.BannerCID != nil, input.Bio != nil)
		stats.updated.Add(1)
		return
	}

	if _, err := userRepo.UpdateProfile(fetchCtx, u.did, *input); err != nil {
		log.Printf("FAIL %s: profile fetched but database update failed: %v", u.did, err)
		stats.failed.Add(1)
		return
	}

	log.Printf("UPDATED %s: displayName=%v avatar=%v banner=%v bio=%v",
		u.did, input.DisplayName != nil, input.AvatarCID != nil, input.BannerCID != nil, input.Bio != nil)
	stats.updated.Add(1)
}
