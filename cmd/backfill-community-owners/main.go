// Command backfill-community-owners writes the owner role record into hosted
// communities that have none.
//
// Communities created before community.create wrote an owner record have no
// indexed owner, and the indexed owner is what authorizes community settings.
// For each community this AppView hosts (it stores the community's PDS
// credentials), the command lists the community repo's
// social.coves.community.moderator records on the PDS — not the index — and,
// when none has role "owner", writes one for the profile's createdBy at
// RoleRecordKey(createdBy), signed in as the community.
//
// An owner record counts as established only when the indexer would accept
// it: a did:plc or did:web subject at RoleRecordKey(subject). A repo whose
// owner-role records all fail that rule gets no write — a second owner record
// could later supersede whatever an admin restores — and is counted and logged
// as owner-unindexable, with the record URI, for an instance admin to assign an
// owner. A listRecords cursor that repeats fails the community, since an
// unread page could hold its owner record.
//
// It writes repo records only and no database rows: the firehose indexes the
// records it writes. It signs in with the community's stored handle and
// password rather than refreshing the stored tokens, because a refresh would
// rewrite the community's credential columns. Rerunning writes nothing for a
// community that already holds an owner record.
//
// createdBy becomes the owner only when it is an indexed account that has not
// been erased; any other community is counted as skipped and left for an
// instance admin. A failing community is counted and the run continues; the
// command exits non-zero when any community failed.
//
// Usage (the AppView's environment: DATABASE_URL, ENCRYPTION_KEY, PDS_URL, ...):
//
//	go run ./cmd/backfill-community-owners [-timeout 1h]
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"Coves/internal/atproto/pds"
	"Coves/internal/config"
	"Coves/internal/core/communities"
	"Coves/internal/core/users"
	"Coves/internal/crypto/credentialcipher"
	"Coves/internal/db/postgres"

	"github.com/bluesky-social/indigo/atproto/syntax"
	_ "github.com/lib/pq"
)

// listPageSize is the listRecords page size.
const listPageSize = 100

type ownerRepoClient interface {
	ListRecords(ctx context.Context, collection string, limit int, cursor string) (*pds.ListRecordsResponse, error)
	PutRecord(ctx context.Context, collection, rkey string, record any, swapRecord string) (uri, cid string, err error)
}

type ownerBackfillDeps struct {
	Hosted interface {
		HostedCommunityDIDs(ctx context.Context) ([]string, error)
	}
	Communities interface {
		GetByDID(ctx context.Context, did string) (*communities.Community, error)
	}
	OpenRepo func(ctx context.Context, community *communities.Community) (ownerRepoClient, error)
	Erased   interface {
		IsAccountDeleted(ctx context.Context, did string) (bool, error)
	}
	Users interface {
		GetByDID(ctx context.Context, did string) (*users.User, error)
	}
	Now func() time.Time
}

type ownerBackfillReport struct{ Written, AlreadyOwned, OwnerUnindexable, Skipped, Failed int }

// ownerBackfillOutcome is what the back-fill did with one community.
type ownerBackfillOutcome int

const (
	ownerWritten ownerBackfillOutcome = iota
	ownerAlreadyPresent
	ownerUnindexable
	ownerSkipped
)

// backfillOwners writes the missing owner record of every hosted community. A
// community that fails is counted and logged, the run moves on, and the
// returned error says how many failed.
func backfillOwners(ctx context.Context, deps ownerBackfillDeps) (ownerBackfillReport, error) {
	var report ownerBackfillReport
	dids, err := deps.Hosted.HostedCommunityDIDs(ctx)
	if err != nil {
		return report, fmt.Errorf("listing hosted communities: %w", err)
	}
	for _, did := range dids {
		if err := ctx.Err(); err != nil {
			return report, fmt.Errorf("back-fill stopped after %d of %d communities: %w",
				report.Written+report.AlreadyOwned+report.OwnerUnindexable+report.Skipped+report.Failed, len(dids), err)
		}
		outcome, err := backfillCommunityOwner(ctx, deps, did)
		if err != nil {
			report.Failed++
			log.Printf("backfill-community-owners: community %s failed: %v", did, err)
			continue
		}
		switch outcome {
		case ownerWritten:
			report.Written++
		case ownerAlreadyPresent:
			report.AlreadyOwned++
		case ownerUnindexable:
			report.OwnerUnindexable++
		case ownerSkipped:
			report.Skipped++
		}
	}
	if report.Failed > 0 {
		return report, fmt.Errorf("%d of %d hosted communities failed", report.Failed, len(dids))
	}
	return report, nil
}

// backfillCommunityOwner writes communityDID's owner record unless its repo
// already holds one or its createdBy cannot be the owner.
func backfillCommunityOwner(ctx context.Context, deps ownerBackfillDeps, communityDID string) (ownerBackfillOutcome, error) {
	community, err := deps.Communities.GetByDID(ctx, communityDID)
	if err != nil {
		return 0, fmt.Errorf("reading the community: %w", err)
	}
	repo, err := deps.OpenRepo(ctx, community)
	if err != nil {
		return 0, fmt.Errorf("opening the community repo: %w", err)
	}
	owned, unindexableOwnerURI, err := repoHoldsOwnerRecord(ctx, repo)
	if err != nil {
		return 0, err
	}
	if owned {
		return ownerAlreadyPresent, nil
	}
	if unindexableOwnerURI != "" {
		log.Printf("backfill-community-owners: community %s owner-unindexable: owner record %s is not indexable "+
			"(its subject is not a did:plc/did:web DID or it is not at its subject's record key); an instance admin must assign an owner",
			communityDID, unindexableOwnerURI)
		return ownerUnindexable, nil
	}
	eligible, err := eligibleOwner(ctx, deps, community.CreatedByDID)
	if err != nil {
		return 0, err
	}
	if !eligible {
		log.Printf("backfill-community-owners: community %s skipped: its createdBy is not an indexed, unerased account", communityDID)
		return ownerSkipped, nil
	}
	record := map[string]any{
		"$type":     communities.RoleCollection,
		"subject":   community.CreatedByDID,
		"role":      communities.RoleOwner,
		"createdAt": deps.Now().UTC().Format(time.RFC3339),
	}
	if _, _, err := repo.PutRecord(ctx, communities.RoleCollection,
		communities.RoleRecordKey(community.CreatedByDID), record, ""); err != nil {
		return 0, fmt.Errorf("writing the owner record: %w", err)
	}
	return ownerWritten, nil
}

// repoHoldsOwnerRecord reads every listRecords page of the repo's role records.
// owned is true when an owner-role record passes the indexer's rule (see
// indexableOwnerRecord). Otherwise unindexableOwnerURI is the URI of the first
// owner-role record that fails it, or "" when the repo has no owner-role
// record. A cursor that repeats is an error: the pages after it are unread.
func repoHoldsOwnerRecord(ctx context.Context, repo ownerRepoClient) (owned bool, unindexableOwnerURI string, err error) {
	cursor := ""
	for {
		page, err := repo.ListRecords(ctx, communities.RoleCollection, listPageSize, cursor)
		if err != nil {
			return false, "", fmt.Errorf("listing the community's role records: %w", err)
		}
		for _, record := range page.Records {
			if role, _ := record.Value["role"].(string); role != communities.RoleOwner {
				continue
			}
			if indexableOwnerRecord(record) {
				return true, "", nil
			}
			if unindexableOwnerURI == "" {
				unindexableOwnerURI = record.URI
			}
		}
		if page.Cursor == "" {
			return false, unindexableOwnerURI, nil
		}
		if page.Cursor == cursor {
			return false, "", fmt.Errorf("listing the community's role records: the PDS repeated cursor %q", cursor)
		}
		cursor = page.Cursor
	}
}

// indexableOwnerRecord applies the indexer's (parseRoleRecord) subject,
// record-key and createdAt rules: the subject is a did:plc or did:web DID, the
// record sits at RoleRecordKey(subject), and createdAt is an RFC 3339 string.
// The createdAt rule is the same time.Parse(time.RFC3339, ...) check that
// parseRoleRecord makes inline, repeated here rather than shared.
func indexableOwnerRecord(record pds.RecordEntry) bool {
	createdAt, _ := record.Value["createdAt"].(string)
	if _, err := time.Parse(time.RFC3339, createdAt); err != nil {
		return false
	}
	subject, _ := record.Value["subject"].(string)
	did, err := syntax.ParseDID(subject)
	if err != nil || (did.Method() != "plc" && did.Method() != "web") {
		return false
	}
	uri, err := syntax.ParseATURI(record.URI)
	if err != nil {
		return false
	}
	return uri.RecordKey().String() == communities.RoleRecordKey(subject)
}

// eligibleOwner reports whether createdBy may be given the owner record: a
// non-empty DID of an indexed account that has not been erased.
func eligibleOwner(ctx context.Context, deps ownerBackfillDeps, createdBy string) (bool, error) {
	if createdBy == "" {
		return false, nil
	}
	if _, err := deps.Users.GetByDID(ctx, createdBy); err != nil {
		if errors.Is(err, users.ErrUserNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("reading the creator's account: %w", err)
	}
	erased, err := deps.Erased.IsAccountDeleted(ctx, createdBy)
	if err != nil {
		return false, fmt.Errorf("checking whether the creator's account is erased: %w", err)
	}
	return !erased, nil
}

func main() {
	runTimeout := flag.Duration("timeout", time.Hour, "hard deadline for the whole run")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("backfill-community-owners: loading config: %v", err)
	}
	if cfg.EncryptionKeyGenerated {
		// Every stored community password was sealed under the AppView's
		// persistent key, so a generated one fails every community.
		log.Fatal("backfill-community-owners: ENCRYPTION_KEY is unset; set it to the AppView's key before running")
	}
	cipher, err := credentialcipher.NewFromBase64(cfg.EncryptionKey)
	if err != nil {
		log.Fatalf("backfill-community-owners: initializing credential cipher: %v", err)
	}

	db, err := sql.Open("postgres", cfg.Database.URL)
	if err != nil {
		log.Fatalf("backfill-community-owners: opening the database: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *runTimeout)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		log.Fatalf("backfill-community-owners: pinging the database: %v", err)
	}

	// The SSRF hatch opens only in dev, where community PDS URLs are loopback
	// addresses the guard would otherwise refuse.
	pdsOptions := pds.PrivateHostOptions(cfg.IsDevEnv)
	deps := ownerBackfillDeps{
		Hosted:      postgres.NewHostedCommunityQuery(db),
		Communities: postgres.NewCommunityRepository(db, cipher),
		OpenRepo: func(ctx context.Context, community *communities.Community) (ownerRepoClient, error) {
			// A password session leaves the stored tokens, and every other
			// database row, untouched.
			return pds.NewFromPasswordAuth(ctx, community.PDSURL, community.Handle, community.PDSPassword, pdsOptions...)
		},
		Erased: postgres.NewDeletedAccountRepository(db),
		Users:  postgres.NewUserRepository(db),
		Now:    time.Now,
	}

	report, err := backfillOwners(ctx, deps)
	fmt.Printf("backfill-community-owners: written=%d already-owned=%d owner-unindexable=%d skipped=%d failed=%d\n",
		report.Written, report.AlreadyOwned, report.OwnerUnindexable, report.Skipped, report.Failed)
	if err != nil {
		stop()
		cancel()
		_ = db.Close()
		log.Fatalf("backfill-community-owners: %v", err)
	}
}
