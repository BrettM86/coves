// Command rematerialize-posts is the operator-invoked cutover tool of
// docs/PRD_AUTHOR_OWNED_POSTS.md §11: it moves every legacy
// social.coves.community.post (written into a COMMUNITY's repo under the
// community's credentials) to an author-owned social.coves.community.postv2 in
// the AUTHOR's repo, plus the community's acceptance that pins it, and then —
// and only then — deletes the old record.
//
// It is a THIN WRAPPER. All of the safety logic lives in posts.Rematerializer;
// this file only wires the production seams the state machine drives:
//
//   - the ledger (migration 037), so the run is resumable and idempotent;
//   - the author-repo factory, so each postv2 is signed by its own author (an
//     aggregator's stored session for a non-interactive author, never a forged
//     admin signature);
//   - the DIRECT community acceptance writer, so a since-banned author's live
//     post is preserved rather than re-adjudicated;
//   - a LegacySource over the real community repos: listRecords to discover the
//     deprecated posts, deleteRecord to remove them once verified.
//
// It is run by hand during the deploy window (§11 step 4), against a database
// whose migrations are already applied, and it reports a census that refuses to
// declare the migration complete while any post was left as legacy — the gate on
// the separate, irreversible legacy-removal follow-up.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
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
// drains. It is the lexicon NSID, spelled here rather than imported because the
// posts package keeps its copy private; the two must agree, and there is exactly
// one correct string.
const legacyPostCollection = "social.coves.community.post"

// listPageSize bounds each communities/records page so an instance with a large
// catalogue is enumerated in bounded queries rather than one unbounded read.
const listPageSize = 100

func main() {
	communityFilter := flag.String("community", "",
		"restrict the run to a single community DID (a staged rollout); empty means every hosted community")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("rematerialize-posts: loading config: %v", err)
	}

	db, err := openDatabase(cfg)
	if err != nil {
		log.Fatalf("rematerialize-posts: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()

	// The OAuth client is the credential seam for BOTH kinds of author: a human's
	// browser session is not available in a batch tool, so the only authors this
	// run can re-materialize are non-interactive ones (aggregators) whose stored
	// session it resumes. Any human-authored post surfaces as a no-creds fallback
	// and is left as legacy — never forged.
	oauthClient, err := buildOAuthClient(cfg, db)
	if err != nil {
		log.Fatalf("rematerialize-posts: %v", err)
	}

	blobService := blobs.NewBlobService(cfg.PDS.URL)
	provisioner := communities.NewPDSAccountProvisioner(cfg.Instance.Domain, cfg.PDS.URL)
	communityService := communities.NewCommunityService(
		postgresRepo.NewCommunityRepository(db),
		cfg.PDS.URL,
		cfg.Instance.DID,
		cfg.Instance.Domain,
		provisioner,
		oauthClient,
		blobService,
	)

	// The DIRECT acceptance writer over the production community-repo factory —
	// the same credential-presence hosting test the acceptance engine uses. The
	// tool holds this writer and no decider, so it cannot re-run admission.
	repoFactory := posts.NewCommunityRepoFactory(communityService)
	writer := posts.NewCommunityRecordWriter(repoFactory, time.Now)

	authorFactory := posts.NewAuthorRepoFactory(oauthClient.ClientApp, aggregators.DefaultSessionID)

	source := &realLegacySource{
		communities:     communityService,
		creds:           communityService,
		communityFilter: *communityFilter,
	}

	tool := &posts.Rematerializer{
		Source:      source,
		Ledger:      postgresRepo.NewRematerializeLedger(db),
		AuthorRepos: authorFactory,
		Acceptances: writer,
	}

	report, err := tool.Run(ctx)
	if err != nil {
		log.Fatalf("rematerialize-posts: the run failed: %v", err)
	}

	logCensus(report)

	// A surviving fallback means at least one post still lives only as a legacy
	// record, so the operator must NOT proceed to the legacy-removal step. Exiting
	// non-zero makes that a machine-checkable gate rather than a line of output an
	// operator might skim past.
	if !report.Complete {
		log.Printf("rematerialize-posts: INCOMPLETE — %d post(s) left as legacy; do not run the legacy-removal step", report.Fallbacks)
		os.Exit(1)
	}
	log.Printf("rematerialize-posts: complete — every discovered post was re-materialized")
}

// realLegacySource enumerates the deprecated community.post records across the
// hosted communities and deletes them from their community repos.
//
// It reaches the PDS through a full pds.Client rather than the narrowed
// CommunityRepo the acceptance writer uses, because discovery needs listRecords
// and deletion needs deleteRecord — neither of which the write-narrowed surface
// carries. The credentials come from the same source the acceptance writer's
// factory uses, so the two never disagree about which communities are hosted.
type realLegacySource struct {
	communities     communities.Service
	creds           posts.CommunityCredentialSource
	communityFilter string
}

// ListLegacyPosts walks every hosted community and lists its remaining
// social.coves.community.post records as LegacyPosts.
func (s *realLegacySource) ListLegacyPosts(ctx context.Context) ([]posts.LegacyPost, error) {
	dids, err := s.hostedCommunityDIDs(ctx)
	if err != nil {
		return nil, err
	}

	var legacy []posts.LegacyPost
	for _, did := range dids {
		client, err := s.communityClient(ctx, did)
		if err != nil {
			return nil, fmt.Errorf("opening the repo of %s to list legacy posts: %w", did, err)
		}

		cursor := ""
		for {
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

// DeleteLegacyPost removes the old community.post from its community repo. A
// delete of an already-gone record is success — it is the step a crash after the
// migrated checkpoint retries, so idempotence is the contract.
func (s *realLegacySource) DeleteLegacyPost(ctx context.Context, legacy posts.LegacyPost) error {
	client, err := s.communityClient(ctx, legacy.CommunityDID)
	if err != nil {
		return fmt.Errorf("opening the repo of %s to delete %s: %w", legacy.CommunityDID, legacy.URI, err)
	}
	rkey := legacy.URI[lastSlash(legacy.URI)+1:]
	if err := client.DeleteRecord(ctx, legacyPostCollection, rkey); err != nil {
		if errors.Is(err, pds.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("deleting %s: %w", legacy.URI, err)
	}
	return nil
}

// hostedCommunityDIDs returns the DIDs of the communities this AppView can sign
// for — the only ones whose posts it can re-materialize and whose old records it
// can delete. A --community filter narrows the run to one for a staged rollout.
func (s *realLegacySource) hostedCommunityDIDs(ctx context.Context) ([]string, error) {
	if s.communityFilter != "" {
		return []string{s.communityFilter}, nil
	}

	var dids []string
	offset := 0
	for {
		page, err := s.communities.ListCommunities(ctx, communities.ListCommunitiesRequest{
			Limit:  listPageSize,
			Offset: offset,
		})
		if err != nil {
			return nil, fmt.Errorf("listing communities: %w", err)
		}
		for _, community := range page {
			// Hosting is credential presence, never a claimed profile field: only a
			// community whose refresh token this AppView holds can be written to, and
			// the repo factory would refuse the rest with ErrCommunityNotHosted.
			if community.PDSRefreshToken != "" {
				dids = append(dids, community.DID)
			}
		}
		if len(page) < listPageSize {
			break
		}
		offset += listPageSize
	}
	return dids, nil
}

// communityClient opens a full PDS client bound to one community's repo, over
// freshly-renewed stored credentials.
func (s *realLegacySource) communityClient(ctx context.Context, did string) (pds.Client, error) {
	community, err := s.creds.GetByDID(ctx, did)
	if err != nil {
		return nil, fmt.Errorf("reading the credentials of %s: %w", did, err)
	}
	if community == nil {
		return nil, fmt.Errorf("reading the credentials of %s: no such community is indexed", did)
	}
	fresh, err := s.creds.EnsureFreshToken(ctx, community)
	if err != nil {
		return nil, fmt.Errorf("renewing the credentials of %s: %w", did, err)
	}
	if fresh == nil || fresh.PDSAccessToken == "" {
		return nil, fmt.Errorf("renewing the credentials of %s: no access token came back", did)
	}
	return pds.NewFromAccessToken(fresh.PDSURL, fresh.DID, fresh.PDSAccessToken)
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

// logCensus prints the run's per-state tally.
func logCensus(report posts.RematerializeReport) {
	log.Printf("rematerialize-posts: census — discovered=%d done=%d fallbacks=%d complete=%v",
		report.Discovered, report.Done, report.Fallbacks, report.Complete)
	for state, n := range report.ByState {
		log.Printf("  %-22s %d", state, n)
	}
}

func lastSlash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return i
		}
	}
	return -1
}

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
		AllowPrivateIPs:           cfg.IsDevEnv,
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
// session lacks at the first write.
func oauthScopes() []string {
	return []string{
		"atproto",
		"blob:*/*",
		"repo:social.coves.community.post?action=create&action=update&action=delete",
		"repo:social.coves.community.comment?action=create&action=update&action=delete",
		"repo:social.coves.community.profile?action=create&action=update&action=delete",
		"repo:social.coves.community.subscription?action=create&action=update&action=delete",
		"repo:social.coves.actor.profile?action=create&action=update&action=delete",
		"repo:social.coves.feed.vote?action=create&action=delete",
		"repo:social.coves.actor.block?action=create&action=delete",
	}
}
