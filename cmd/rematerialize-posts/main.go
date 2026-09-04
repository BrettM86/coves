// Command rematerialize-posts is the operator-invoked cutover tool of
// docs/PRD_AUTHOR_OWNED_POSTS.md §11: it moves every legacy
// social.coves.community.post (written into a COMMUNITY's repo under the
// community's credentials) to an author-owned social.coves.community.postv2 in
// the AUTHOR's repo, plus the community's acceptance that pins it, and then —
// and only then — deletes the old record and directly tombstones its AppView
// index row before marking it done. The firehose no longer ingests legacy post
// deletes, so the tool must converge both stores itself.
//
// # THIS COMMAND DELETES PRODUCTION USER DATA IRREVERSIBLY
//
// It is run by hand, once, during a maintenance window, by someone who has been
// awake too long. Everything in this file that is not wiring exists because of
// that sentence:
//
//   - it prints WHAT IT IS ABOUT TO TOUCH — database host, PDS, instance DID,
//     community count, record count — and refuses to write anything until the
//     operator passes -yes;
//   - -dry-run walks the identical code path with only the mutations replaced,
//     so the rehearsal really resolves credentials, really re-reads records,
//     really fetches blobs, and really reports what it would delete;
//   - it holds a Postgres ADVISORY LOCK, so a second invocation refuses to start
//     rather than racing the first through guarded transitions and reporting
//     "the ledger and the tool have diverged", which reads like corruption;
//   - it logs one line per record and a rolling n/N, so a slow run is
//     distinguishable from a hung one;
//   - it ALWAYS prints the census, including on the error path, because "did it
//     delete 0 records or 4,131?" is the only question that matters after a
//     failure;
//   - SIGINT and SIGTERM cancel the run between records rather than killing it
//     mid-transition, and every outbound call is bounded by a timeout.
//
// It is a THIN WRAPPER otherwise. All of the safety logic lives in
// posts.Rematerializer; this file wires the production seams the state machine
// drives:
//
//   - the ledger (migration 037), so the run is resumable and idempotent;
//   - the author-repo factory, so each postv2 is signed by its own author (an
//     aggregator's stored session for a non-interactive author, never a forged
//     admin signature);
//   - the DIRECT community acceptance writer, so a since-banned author's live
//     post is preserved rather than re-adjudicated;
//   - the community-repo factory, so the acceptance is READ BACK from the
//     community's own repo before anything is deleted;
//   - a LegacySource over the real community repos: listRecords to discover the
//     deprecated posts, getRecord for the pre-delete re-read, and a
//     swap-guarded deleteRecord to remove them once verified.
//   - the AppView post repository, so the old indexed row is tombstoned after
//     the PDS delete and before the ledger reaches done.
package main

import (
	"Coves/internal/crypto/credentialcipher"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"Coves/internal/atproto/oauth"
	"Coves/internal/atproto/pds"
	"Coves/internal/config"
	"Coves/internal/core/aggregators"
	"Coves/internal/core/blobs"
	"Coves/internal/core/communities"
	"Coves/internal/core/posts"
	postgresRepo "Coves/internal/db/postgres"

	_ "github.com/lib/pq"
)

// legacyPostCollection is the deprecated community-repo post collection the tool
// drains — the domain's own constant, not a second spelling of the NSID: the
// tool enumerates exactly the records the READ PATH still serves and
// IsPostCollection still routes, and a private copy here would be free to
// disagree with them.
const legacyPostCollection = posts.LegacyPostCollection

// listPageSize bounds each communities/records page so an instance with a large
// catalogue is enumerated in bounded queries rather than one unbounded read.
const listPageSize = 100

// rematerializeAdvisoryLock is the Postgres advisory-lock key this tool holds
// for the whole run.
//
// TWO CONCURRENT RUNS ARE NOT MERELY WASTEFUL — they interleave on the ledger's
// guarded transitions, so the loser of each race is told "no row in the expected
// prior state (the ledger and the tool have diverged)". That message is correct
// and terrifying, and an operator reading it at 3am has no way to tell it from
// real corruption. Refusing the second run is the only version of this that
// stays legible.
const rematerializeAdvisoryLock int64 = 0x52454d4154 // "REMAT"

// perRecordTimeout bounds everything one record's processing does — several PDS
// round trips and a handful of ledger writes. Without it a single half-open
// socket stalls the entire migration with no output.
const perRecordTimeout = 5 * time.Minute

func main() {
	communityFilter := flag.String("community", "",
		"restrict the run to a single community DID (a staged rollout); empty means every hosted community")
	dryRun := flag.Bool("dry-run", false,
		"rehearse: walk the identical code path — resolving credentials, re-reading records, fetching blobs — but write and delete nothing")
	confirm := flag.Bool("yes", false,
		"required for a real run: confirm the target printed in the banner before anything is written or deleted")
	acceptFallbacks := flag.Bool("accept-fallbacks", false,
		"proceed even if the credential census leaves posts as legacy (default: stop before mutating anything and report them)")
	reopenFallbacks := flag.Bool("reopen-fallbacks", false,
		"move rows previously left as legacy back to 'discovered' so this run retries them, then exit; use after re-authorizing the affected authors")
	recordDelay := flag.Duration("delay", 0,
		"pause between records, to rate-limit the PDS during the run (e.g. 100ms)")
	runTimeout := flag.Duration("timeout", 6*time.Hour,
		"hard deadline for the whole run; the run cancels cleanly between records when it expires")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("rematerialize-posts: loading config: %v", err)
	}
	if cfg.EncryptionKeyGenerated {
		// A maintenance tool has no use for a per-process key: every community
		// credential it reads was sealed under the server's persistent key, so
		// a generated one turns every row into an authentication failure.
		log.Fatal("rematerialize-posts: ENCRYPTION_KEY is unset; set it to the AppView's key before running")
	}
	credentialCipher, err := credentialcipher.NewFromBase64(cfg.EncryptionKey)
	if err != nil {
		log.Fatalf("rematerialize-posts: initializing credential cipher: %v", err)
	}

	db, err := openDatabase(cfg)
	if err != nil {
		log.Fatalf("rematerialize-posts: %v", err)
	}
	defer func() { _ = db.Close() }()

	// SIGINT/SIGTERM cancel between records rather than killing the process
	// mid-transition; the ledger then holds a coherent checkpoint to resume from.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancelDeadline := context.WithTimeout(ctx, *runTimeout)
	defer cancelDeadline()

	// The advisory lock is taken on a DEDICATED connection held for the whole run:
	// a session-scoped lock released the instant the pool recycles its connection
	// is not a lock.
	unlock, err := takeAdvisoryLock(ctx, db)
	if err != nil {
		log.Fatalf("rematerialize-posts: %v", err)
	}
	defer unlock()

	ledger := postgresRepo.NewRematerializeLedger(db)

	if *reopenFallbacks {
		moved, err := ledger.ReopenFallback(ctx, *communityFilter)
		if err != nil {
			log.Fatalf("rematerialize-posts: reopening fallback rows: %v", err)
		}
		log.Printf("rematerialize-posts: moved %d row(s) out of %s back to %s; re-run the tool to retry them",
			moved, posts.RematerializeFallbackLeftLegacy, posts.RematerializeDiscovered)
		return
	}

	// The OAuth client is the credential seam for BOTH kinds of author: a human's
	// browser session is not available in a batch tool, so the only authors this
	// run can re-materialize are non-interactive ones (aggregators) whose stored
	// session it resumes. Any human-authored post surfaces as a no-creds fallback
	// and is left as legacy — never forged.
	oauthClient, err := buildOAuthClient(cfg, db)
	if err != nil {
		log.Fatalf("rematerialize-posts: %v", err)
	}

	// Same gate as cmd/server: the blob service's fetch and PDS upload are both
	// guarded, and the hatch opens only in dev, where cfg.PDS.URL is a loopback
	// address the guard would otherwise refuse. The gate is allowPrivateHosts —
	// the IS_DEV_ENV environment variable, not one of this tool's own flags — and
	// every other guarded construction here goes through the same accessor,
	// including the OAuth client's AllowPrivateIPs.
	blobService := blobs.NewBlobService(cfg.PDS.URL, blobs.PrivateHostOptions(allowPrivateHosts(cfg))...)

	// The same gate again, on the xrpc calls this package makes to a community's
	// PDS. They used to leave xrpc.Client.Client nil, which makes indigo
	// substitute an unguarded util.RobustHTTPClient().
	communityPDSOptions := communities.PrivateHostOptions(allowPrivateHosts(cfg))
	provisioner := communities.NewPDSAccountProvisioner(
		cfg.Instance.Domain, cfg.PDS.URL, communityPDSOptions...)
	communityService := communities.NewCommunityService(
		postgresRepo.NewCommunityRepository(db, credentialCipher),
		cfg.PDS.URL,
		cfg.Instance.DID,
		cfg.Instance.Domain,
		provisioner,
		oauthClient,
		blobService,
		communityPDSOptions...,
	)

	// The DIRECT acceptance writer over the production community-repo factory —
	// the same credential-presence hosting test the acceptance engine uses. The
	// tool holds this writer and no decider, so it cannot re-run admission. The
	// same factory is handed to the Rematerializer for the acceptance READ-BACK.
	//
	// pdsClientOptions is this tool's SSRF dev gate, resolved once and shared by
	// both PDS-client paths below: every community PDS URL they dial comes from
	// the database, and allowPrivateHosts is what lets a local run reach a
	// loopback PDS without opening the guard anywhere else.
	pdsClientOptions := pds.PrivateHostOptions(allowPrivateHosts(cfg))

	repoFactory := posts.NewCommunityRepoFactory(communityService, pdsClientOptions...)
	writer := posts.NewCommunityRecordWriter(repoFactory, time.Now)

	authorFactory := posts.NewAuthorRepoFactory(oauthClient.ClientApp, aggregators.DefaultSessionID)

	source := &realLegacySource{
		communityFilter: *communityFilter,
		hostedDIDs:      postgresRepo.NewHostedCommunityQuery(db).HostedCommunityDIDs,
		openRepo:        communityRepoOpener(communityService, pdsClientOptions...),
	}

	progress := newProgressLogger()
	tool := &posts.Rematerializer{
		Source:         source,
		Ledger:         ledger,
		AuthorRepos:    authorFactory,
		Acceptances:    writer,
		CommunityRepos: repoFactory,
		Index:          postgresRepo.NewPostRepository(db),
		CommunityScope: *communityFilter,
		// The blob copy's dev gate, decided HERE rather than inside the state
		// machine, exactly as blobs.PrivateHostOptions is decided above.
		//
		// The Rematerializer's own fallback is guarded with no way to open it,
		// because it is what every caller that expressed no opinion degrades to.
		// This is the caller that has an opinion: the host it copies from is the
		// author repo's serviceEndpoint, which in a local stack is a loopback PDS —
		// the address the guard exists to refuse. Passing allowPrivateHosts keeps a
		// dev run working and leaves production on the guarded path, and it puts the
		// decision where an operator reading the wiring can see which one they got.
		Blobs:            posts.DefaultRematerializeBlobClient(allowPrivateHosts(cfg)),
		Progress:         progress.log,
		PerRecordTimeout: perRecordTimeout,
		AbortOnFallback:  !*acceptFallbacks,
	}

	// The banner is printed BEFORE the confirmation and from real queries, so the
	// operator confirms the target they were shown rather than the one they
	// assumed. Enumerating the source here also means a misconfigured run — wrong
	// database, wrong PDS, zero hosted communities — is visible before it writes.
	scope, err := describeTarget(ctx, cfg, source)
	if err != nil {
		log.Fatalf("rematerialize-posts: describing the target: %v", err)
	}
	printBanner(cfg, scope, *dryRun, *communityFilter)

	if *dryRun {
		tool = posts.DryRunOf(tool)
	} else if !*confirm {
		log.Printf("rematerialize-posts: REFUSING TO RUN. This deletes %d legacy record(s) from %d community repo(s) IRREVERSIBLY.",
			scope.records, scope.communities)
		log.Printf("rematerialize-posts: rehearse it with -dry-run, or confirm the target above with -yes.")
		os.Exit(2)
	}

	if *recordDelay > 0 {
		source.delay = *recordDelay
	}

	report, runErr := tool.Run(ctx)

	// THE CENSUS IS ALWAYS PRINTED, including on the error path. A run that fails
	// on record 900 of 4,131 has already deleted 899 records, and an operator who
	// sees only the error has no way to know that.
	logCensus(report, *dryRun)
	if deletes, isDry := posts.DryRunDeletes(tool); isDry {
		tombstones, _ := posts.DryRunTombstones(tool)
		log.Printf("rematerialize-posts: DRY RUN — %d record(s) would have been deleted and %d AppView row(s) tombstoned; nothing was written or removed",
			deletes, tombstones)
	}

	if runErr != nil {
		log.Printf("rematerialize-posts: THE RUN FAILED: %v", runErr)
		log.Printf("rematerialize-posts: the ledger above is the truth about what completed; re-running is safe and resumes from it")
		os.Exit(1)
	}

	// TWO SIGNALS, REPORTED SEPARATELY. A staged -community run finishing its own
	// scope is a success even though the migration as a whole is not done, and
	// collapsing the two taught the operator to ignore a red exit code on every
	// staged run — which would leave §11 step 6 with no machine-checkable gate at
	// all.
	if !report.ScopeComplete {
		log.Printf("rematerialize-posts: SCOPE INCOMPLETE — %d of %d row(s) in scope reached done, %d fallback(s), %d legacy record(s) still standing",
			report.Done, report.Discovered, report.Fallbacks, report.RemainingLegacy)
		os.Exit(1)
	}
	log.Printf("rematerialize-posts: scope complete — every post in %s was re-materialized", scopeName(*communityFilter))

	if !report.Complete {
		log.Printf("rematerialize-posts: THE MIGRATION AS A WHOLE IS NOT COMPLETE — %d of %d ledger row(s) done, %d fallback(s), %d legacy record(s) still standing.",
			report.GlobalDone, report.GlobalDiscovered, report.GlobalFallbacks, report.RemainingLegacy)
		log.Printf("rematerialize-posts: DO NOT run the legacy-removal follow-up (PRD §11 step 6) until this line says complete.")
		return
	}
	log.Printf("rematerialize-posts: MIGRATION COMPLETE — every discovered post was re-materialized and no legacy record remains")
}

// repoClient is the narrow PDS surface the legacy source needs: enumerate,
// re-read, and delete UNDER A GUARD.
//
// It is declared here rather than taken as pds.Client so that the guarded delete
// is a REQUIREMENT of the type. A transport that cannot express swapRecord fails
// to satisfy this interface at compile time instead of quietly deleting whatever
// stands.
type repoClient interface {
	ListRecords(ctx context.Context, collection string, limit int, cursor string) (*pds.ListRecordsResponse, error)
	GetRecord(ctx context.Context, collection, rkey string) (*pds.RecordResponse, error)
	DeleteRecordWithSwap(ctx context.Context, collection, rkey, swapRecord string) error
}

// realLegacySource enumerates the deprecated community.post records across the
// hosted communities, re-reads one on demand, and deletes them from their
// community repos under a swap guard.
//
// It reaches the PDS through a full client rather than the narrowed CommunityRepo
// the acceptance writer uses, because discovery needs listRecords and deletion
// needs deleteRecord — neither of which the write-narrowed surface carries.
type realLegacySource struct {
	// communityFilter scopes the run. It gates DISCOVERY and, independently, the
	// DELETE: the ledger reconcile pass reaches records discovery never listed, so
	// a filter applied only at discovery would let a staged run for community A
	// delete community B's posts.
	communityFilter string

	// hostedDIDs answers which communities this AppView can sign for — credential
	// presence, never a claimed profile field.
	hostedDIDs func(ctx context.Context) ([]string, error)

	// openRepo opens one community's repo over freshly-renewed stored credentials.
	openRepo func(ctx context.Context, did string) (repoClient, error)

	// delay paces the run, so a migration over thousands of records does not
	// saturate the PDS the rest of the instance is still using.
	delay time.Duration
}

// ListLegacyPosts walks every hosted community in scope and lists its remaining
// social.coves.community.post records as LegacyPosts.
func (s *realLegacySource) ListLegacyPosts(ctx context.Context) ([]posts.LegacyPost, error) {
	dids, err := s.hostedCommunityDIDs(ctx)
	if err != nil {
		return nil, err
	}

	var legacy []posts.LegacyPost
	for _, did := range dids {
		client, err := s.openRepo(ctx, did)
		if err != nil {
			return nil, fmt.Errorf("opening the repo of %s to list legacy posts: %w", did, err)
		}

		cursor := ""
		for {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			page, err := client.ListRecords(ctx, legacyPostCollection, listPageSize, cursor)
			if err != nil {
				return nil, fmt.Errorf("listing %s in %s: %w", legacyPostCollection, did, err)
			}
			for _, entry := range page.Records {
				post, err := legacyPostFromEntry(did, entry)
				if err != nil {
					return nil, fmt.Errorf("decoding legacy record %s: %w", entry.URI, err)
				}
				legacy = append(legacy, post)
			}
			if page.Cursor == "" {
				break
			}
			cursor = page.Cursor
		}
	}
	return legacy, nil
}

// ReadLegacyPost re-reads one record as it stands right now — the read the
// pre-delete CID check is made against.
func (s *realLegacySource) ReadLegacyPost(ctx context.Context, uri string) (posts.LegacyPost, bool, error) {
	communityDID, rkey, err := splitLegacyURI(uri)
	if err != nil {
		return posts.LegacyPost{}, false, err
	}
	if err := s.inScope(communityDID, uri); err != nil {
		return posts.LegacyPost{}, false, err
	}

	client, err := s.openRepo(ctx, communityDID)
	if err != nil {
		return posts.LegacyPost{}, false, fmt.Errorf("opening the repo of %s to re-read %s: %w", communityDID, uri, err)
	}
	record, err := client.GetRecord(ctx, legacyPostCollection, rkey)
	if err != nil {
		if errors.Is(err, pds.ErrNotFound) {
			return posts.LegacyPost{}, false, nil
		}
		return posts.LegacyPost{}, false, fmt.Errorf("re-reading %s: %w", uri, err)
	}
	post, err := legacyPostFromEntry(communityDID, pds.RecordEntry{URI: record.URI, CID: record.CID, Value: record.Value})
	if err != nil {
		return posts.LegacyPost{}, false, err
	}
	return post, true, nil
}

// DeleteLegacyPost removes the old community.post from its community repo,
// GUARDED by swapCID.
//
// The guard is what makes this safe rather than merely careful: the tool checks
// the record's CID before deleting, but the check and the delete are two
// moments, and only the PDS can evaluate them as one. A delete of an
// already-gone record is success — it is the step a crash after the migrated
// checkpoint retries, so idempotence is the contract — but a LOST SWAP is not:
// it means the record changed under us, which is exactly what the guard exists
// to catch.
func (s *realLegacySource) DeleteLegacyPost(ctx context.Context, legacy posts.LegacyPost, swapCID string) error {
	if swapCID == "" {
		return fmt.Errorf(
			"refusing to delete %s: no source CID to guard the delete with. An unguarded delete removes whatever stands, including an edit "+
				"that landed after the postv2 was built", legacy.URI)
	}
	if err := s.inScope(legacy.CommunityDID, legacy.URI); err != nil {
		return err
	}

	client, err := s.openRepo(ctx, legacy.CommunityDID)
	if err != nil {
		return fmt.Errorf("opening the repo of %s to delete %s: %w", legacy.CommunityDID, legacy.URI, err)
	}
	_, rkey, err := splitLegacyURI(legacy.URI)
	if err != nil {
		return err
	}
	if err := client.DeleteRecordWithSwap(ctx, legacyPostCollection, rkey, swapCID); err != nil {
		if errors.Is(err, pds.ErrNotFound) {
			return nil
		}
		if errors.Is(err, pds.ErrSwapConflict) {
			return fmt.Errorf(
				"refusing to delete %s: the record changed since the postv2 was built from CID %s, so the PDS rejected the guarded delete. "+
					"Something is still writing to this repo; stop the writer and re-run: %w", legacy.URI, swapCID, err)
		}
		return fmt.Errorf("deleting %s: %w", legacy.URI, err)
	}
	if s.delay > 0 {
		// Pacing the destructive step is the one place a deliberate pause belongs:
		// it keeps a multi-thousand-record drain from saturating the PDS the rest of
		// the instance is still serving from.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.delay):
		}
	}
	return nil
}

// inScope refuses any operation on a community outside a staged run's filter.
func (s *realLegacySource) inScope(communityDID, uri string) error {
	if s.communityFilter != "" && communityDID != s.communityFilter {
		return fmt.Errorf(
			"refusing to touch %s: it belongs to %s but this run is scoped to %s",
			uri, communityDID, s.communityFilter)
	}
	return nil
}

// hostedCommunityDIDs returns the DIDs of the communities this AppView can sign
// for — the only ones whose posts it can re-materialize and whose old records it
// can delete. A -community filter narrows the run to one for a staged rollout.
//
// HOSTING IS CREDENTIAL PRESENCE, asked for directly. The obvious version of
// this — walk the community listing and test the refresh-token field — is
// silently empty, because the listing does not select the credential columns:
// every community is filtered out and the run migrates nothing while reporting
// success. See postgres.HostedCommunitySource.
func (s *realLegacySource) hostedCommunityDIDs(ctx context.Context) ([]string, error) {
	hosted, err := s.hostedDIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing the communities this AppView can sign for: %w", err)
	}

	if s.communityFilter != "" {
		for _, did := range hosted {
			if did == s.communityFilter {
				return []string{did}, nil
			}
		}
		return nil, fmt.Errorf(
			"the run is scoped to %s, but this AppView holds no PDS credentials for it: nothing could be written to that repo and nothing may be deleted from it",
			s.communityFilter)
	}

	if len(hosted) == 0 {
		return nil, errors.New(
			"this AppView hosts no communities (no stored PDS refresh tokens), so there is nothing this tool could migrate. " +
				"Check the database the tool is pointed at before assuming the migration is done")
	}
	return hosted, nil
}

// communityRepoOpener builds the production repo opener: a full PDS client bound
// to one community's repo, over freshly-renewed stored credentials.
//
// The options carry the SSRF dev gate, and passing none leaves the client
// guarded: fresh.PDSURL is a per-community database column, so this tool dials
// an address that is data. A local dev run against a loopback PDS is what the
// caller's pds.PrivateHostOptions(allowPrivateHosts(cfg)) opens.
func communityRepoOpener(creds posts.CommunityCredentialSource, opts ...pds.ClientOption) func(context.Context, string) (repoClient, error) {
	return func(ctx context.Context, did string) (repoClient, error) {
		community, err := creds.GetByDID(ctx, did)
		if err != nil {
			return nil, fmt.Errorf("reading the credentials of %s: %w", did, err)
		}
		if community == nil {
			return nil, fmt.Errorf("reading the credentials of %s: no such community is indexed", did)
		}
		fresh, err := creds.EnsureFreshToken(ctx, community)
		if err != nil {
			return nil, fmt.Errorf("renewing the credentials of %s: %w", did, err)
		}
		if fresh == nil || fresh.PDSAccessToken == "" {
			return nil, fmt.Errorf("renewing the credentials of %s: no access token came back", did)
		}
		client, err := pds.NewFromAccessToken(fresh.PDSURL, fresh.DID, fresh.PDSAccessToken, opts...)
		if err != nil {
			return nil, err
		}
		guarded, ok := client.(repoClient)
		if !ok {
			return nil, fmt.Errorf(
				"the PDS client for %s does not support the swap-guarded delete; an unguarded delete would remove whatever stands, so the run stops here", did)
		}
		return guarded, nil
	}
}

// legacyPostFromEntry decodes one listRecords entry into a LegacyPost.
//
// The author DID is read out of the decoded body, but the LOSSLESS conversion
// runs off RawRecord — entry.Value verbatim — so every published field
// (langs/tags/crosspostOf/crosspostChain/bridgedStats and the rest) is carried
// through to the postv2 rather than dropped by the lossy PostRecord shape (P5).
func legacyPostFromEntry(communityDID string, entry pds.RecordEntry) (posts.LegacyPost, error) {
	raw, err := json.Marshal(entry.Value)
	if err != nil {
		return posts.LegacyPost{}, fmt.Errorf("re-encoding record value: %w", err)
	}
	var record posts.PostRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return posts.LegacyPost{}, fmt.Errorf("decoding record value: %w", err)
	}
	if record.Author == "" {
		return posts.LegacyPost{}, fmt.Errorf("record %s carries no author field to re-author under", entry.URI)
	}
	if entry.CID == "" {
		return posts.LegacyPost{}, fmt.Errorf(
			"record %s was listed without a CID; with no CID there is nothing to guard its eventual delete on", entry.URI)
	}
	return posts.LegacyPost{
		URI:          entry.URI,
		CID:          entry.CID,
		CommunityDID: communityDID,
		AuthorDID:    record.Author,
		Record:       record,
		// The lossless source the postv2 is built from (P5): the raw PDS record.
		RawRecord: entry.Value,
	}, nil
}

// splitLegacyURI pulls the repo authority and the record key out of an at:// URI.
func splitLegacyURI(uri string) (repoDID, rkey string, err error) {
	const scheme = "at://"
	if !strings.HasPrefix(uri, scheme) {
		return "", "", fmt.Errorf("%q is not an at:// URI", uri)
	}
	parts := strings.Split(strings.TrimPrefix(uri, scheme), "/")
	if len(parts) != 3 || parts[0] == "" || parts[2] == "" {
		return "", "", fmt.Errorf("%q is not a <did>/<collection>/<rkey> record URI", uri)
	}
	return parts[0], parts[2], nil
}

// targetScope is what the banner reports: the size of what is about to be
// touched, measured rather than assumed.
type targetScope struct {
	communities int
	records     int
}

// describeTarget enumerates the source so the banner states facts.
func describeTarget(ctx context.Context, cfg *config.Config, source *realLegacySource) (targetScope, error) {
	_ = cfg
	dids, err := source.hostedCommunityDIDs(ctx)
	if err != nil {
		return targetScope{}, err
	}
	legacy, err := source.ListLegacyPosts(ctx)
	if err != nil {
		return targetScope{}, err
	}
	return targetScope{communities: len(dids), records: len(legacy)}, nil
}

// printBanner states what this invocation is pointed at, before it is allowed to
// touch any of it.
//
// The database URL is printed WITHOUT its credentials: an operator needs to see
// which host and database they are about to migrate, and nobody needs the
// password in a terminal scrollback that will be pasted into an incident channel.
func printBanner(cfg *config.Config, scope targetScope, dryRun bool, communityFilter string) {
	mode := "LIVE RUN — WRITES AND IRREVERSIBLE DELETES"
	if dryRun {
		mode = "DRY RUN — nothing will be written or deleted"
	}
	log.Printf("──────────────────────────────────────────────────────────────")
	log.Printf(" rematerialize-posts   %s", mode)
	log.Printf("   database    %s", redactDatabaseURL(cfg.Database.URL))
	log.Printf("   PDS         %s", cfg.PDS.URL)
	log.Printf("   instance    %s (%s)", cfg.Instance.DID, cfg.Instance.Domain)
	log.Printf("   scope       %s", scopeName(communityFilter))
	log.Printf("   communities %d", scope.communities)
	log.Printf("   legacy %s  %d", legacyPostCollection, scope.records)
	log.Printf("──────────────────────────────────────────────────────────────")
}

func scopeName(communityFilter string) string {
	if communityFilter == "" {
		return "every hosted community"
	}
	return communityFilter
}

// redactDatabaseURL renders a Postgres URL as host/database only.
func redactDatabaseURL(raw string) string {
	if at := strings.LastIndex(raw, "@"); at != -1 {
		if scheme := strings.Index(raw, "://"); scheme != -1 && scheme+3 < at {
			return raw[:scheme+3] + "***@" + raw[at+1:]
		}
	}
	return raw
}

// progressLogger turns the Rematerializer's transitions into one line each, plus
// a rolling n/N — so the operator can see a slow run is still moving, and so a
// crash leaves a record of exactly how far it got.
type progressLogger struct {
	lastIndex int
	lastTotal int
}

func newProgressLogger() *progressLogger { return &progressLogger{} }

func (p *progressLogger) log(event posts.RematerializeProgress) {
	position := ""
	if event.Total > 0 {
		p.lastIndex, p.lastTotal = event.Index, event.Total
		position = fmt.Sprintf(" [%d/%d]", event.Index, event.Total)
	}
	switch {
	case event.Note != "":
		log.Printf("  %s%s %s → %s: %s", event.OldURI, position, orDash(event.From), orDash(event.To), event.Note)
	default:
		log.Printf("  %s%s %s → %s", event.OldURI, position, orDash(event.From), orDash(event.To))
	}
}

func orDash(state posts.RematerializeState) string {
	if state == "" {
		return "-"
	}
	return string(state)
}

// logCensus prints the run's per-state tally, for the run's scope and for the
// migration as a whole.
func logCensus(report posts.RematerializeReport, dryRun bool) {
	prefix := "census"
	if dryRun {
		prefix = "census (dry run — no state was persisted)"
	}
	log.Printf("rematerialize-posts: %s — scope=%s discovered=%d done=%d fallbacks=%d remaining-legacy=%d scope-complete=%v",
		prefix, scopeName(report.CommunityScope), report.Discovered, report.Done, report.Fallbacks,
		report.RemainingLegacy, report.ScopeComplete)
	for _, state := range censusOrder {
		if n, ok := report.ByState[state]; ok {
			log.Printf("    %-22s %d", state, n)
		}
	}
	if report.CommunityScope != "" {
		log.Printf("rematerialize-posts: whole migration — discovered=%d done=%d fallbacks=%d complete=%v",
			report.GlobalDiscovered, report.GlobalDone, report.GlobalFallbacks, report.Complete)
	}
}

// censusOrder prints the states in machine order rather than map order, so two
// runs' output can be diffed.
var censusOrder = []posts.RematerializeState{
	posts.RematerializeDiscovered,
	posts.RematerializePostV2Written,
	posts.RematerializeVerified,
	posts.RematerializeMigrated,
	posts.RematerializeDone,
	posts.RematerializeFallbackLeftLegacy,
}

// takeAdvisoryLock holds a session-scoped Postgres advisory lock for the whole
// run, on a DEDICATED connection.
//
// It must be a dedicated connection: a session lock taken through the pool is
// released the moment that connection is recycled, which is a lock that protects
// nothing and reports that it does. pg_try_advisory_lock rather than the
// blocking form, because "another run is in progress" is information the
// operator needs immediately, not after an unbounded wait.
func takeAdvisoryLock(ctx context.Context, db *sql.DB) (release func(), err error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("reserving a connection for the run lock: %w", err)
	}
	var acquired bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, rematerializeAdvisoryLock).Scan(&acquired); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("taking the run lock: %w", err)
	}
	if !acquired {
		_ = conn.Close()
		return nil, errors.New(
			"another rematerialize-posts run is already in progress against this database. " +
				"Two concurrent runs interleave on the ledger's guarded transitions and report 'the ledger and the tool have diverged', " +
				"which is indistinguishable from corruption. Wait for the other run to finish")
	}
	return func() {
		// Best-effort: the lock is session-scoped, so closing the connection
		// releases it even if the explicit unlock cannot run.
		_, _ = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, rematerializeAdvisoryLock)
		_ = conn.Close()
	}, nil
}

// allowPrivateHosts is THE ONE CONVERSION from configuration to SSRF policy in
// this binary, the counterpart of cmd/server's method of the same name.
//
// Every hatch-bearing call site in this tool calls it rather than reading
// cfg.IsDevEnv, so there is exactly one expression whose polarity has to be
// right and exactly one place a test has to reach. The five spellings it
// replaced were evaluated by no test at all, and `.env.ci:140` sets
// IS_DEV_ENV=true, so the merge gate ran the permissive branch at every one of
// them — inverting any single site left `make ci` green.
//
// It takes cfg rather than reading the environment for the reason blobs and
// pds both document: an ambient read makes the guarded branch untestable
// alongside t.Parallel, and it opens every construction in the process at once.
//
// buildOAuthClient's DevMode is deliberately NOT routed through here. It is an
// AUTHENTICATION gate over the same input, and one accessor answering two
// different questions is one that gets changed for the wrong reason.
func allowPrivateHosts(cfg *config.Config) bool { return cfg.IsDevEnv }

// openDatabase opens the AppView Postgres the ledger and community catalogue
// live in, with the pool bounds from config.
func openDatabase(cfg *config.Config) (*sql.DB, error) {
	db, err := sql.Open("postgres", cfg.Database.URL)
	if err != nil {
		return nil, fmt.Errorf("opening the database: %w", err)
	}
	db.SetMaxOpenConns(cfg.Database.MaxOpenConns)
	db.SetMaxIdleConns(cfg.Database.MaxIdleConns)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("pinging the database: %w", err)
	}
	return db, nil
}

// buildOAuthClient assembles the OAuth client the author-repo factory resumes
// aggregator sessions through — the same wiring cmd/server uses, so a session
// this tool resumes is byte-identical to one the write path would.
func buildOAuthClient(cfg *config.Config, db *sql.DB) (*oauth.OAuthClient, error) {
	store := oauth.NewMobileAwareStoreWrapper(oauth.NewPostgresOAuthStore(db, 0))
	client, err := oauth.NewOAuthClient(&oauth.OAuthConfig{
		PublicURL:                 cfg.OAuth.PublicURL,
		SealSecret:                cfg.OAuth.SealSecret,
		Scopes:                    oauthScopes(),
		DevMode:                   cfg.IsDevEnv,
		AllowPrivateIPs:           allowPrivateHosts(cfg),
		PLCURL:                    cfg.Identity.PLCURL,
		PDSURL:                    cfg.PDS.URL,
		ClientPrivateKeyMultibase: cfg.OAuth.ClientPrivateKeyMultibase,
		ClientKeyID:               cfg.OAuth.ClientKeyID,
	}, store)
	if err != nil {
		return nil, fmt.Errorf("initializing the OAuth client: %w", err)
	}
	return client, nil
}

// oauthScopes is the granted scope set an aggregator's resumed session must
// carry to write a postv2. It mirrors cmd/server's list, which is the authority;
// the two must agree, so a divergence here shows up as a scope the resumed
// session lacks at the first write. main_test.go asserts the two lists are
// equal, because cmd/server's own test cannot — it is a different package.
func oauthScopes() []string {
	return []string{
		"atproto",
		"blob:*/*",
		"repo:social.coves.community.postv2?action=create&action=update&action=delete",
		"repo:social.coves.community.post?action=create&action=update&action=delete",
		"repo:social.coves.community.comment?action=create&action=update&action=delete",
		"repo:social.coves.community.profile?action=create&action=update&action=delete",
		"repo:social.coves.community.subscription?action=create&action=update&action=delete",
		"repo:social.coves.actor.profile?action=create&action=update&action=delete",
		"repo:social.coves.feed.vote?action=create&action=delete",
		"repo:social.coves.actor.block?action=create&action=delete",
		communities.CommunityBlockOAuthScope,
	}
}
