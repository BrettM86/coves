package main

import (
	"Coves/internal/api/middleware"
	"Coves/internal/atproto/identity"
	"Coves/internal/atproto/jetstream"
	"Coves/internal/atproto/oauth"
	"Coves/internal/atproto/pds"
	"Coves/internal/config"
	"Coves/internal/core/adminreports"
	"Coves/internal/core/aggregators"
	"Coves/internal/core/blobs"
	"Coves/internal/core/blueskypost"
	"Coves/internal/core/comments"
	"Coves/internal/core/communities"
	"Coves/internal/core/communityFeeds"
	"Coves/internal/core/communitysuggestions"
	"Coves/internal/core/discover"
	"Coves/internal/core/imageproxy"
	"Coves/internal/core/posts"
	"Coves/internal/core/timeline"
	"Coves/internal/core/unfurl"
	"Coves/internal/core/userblocks"
	"Coves/internal/core/users"
	"Coves/internal/core/votes"
	"Coves/internal/notify/telegram"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	imageproxyhandlers "Coves/internal/api/handlers/imageproxy"
	postgresRepo "Coves/internal/db/postgres"

	indigoauth "github.com/bluesky-social/indigo/atproto/auth"
	indigoidentity "github.com/bluesky-social/indigo/atproto/identity"
)

const (
	// identityHTTPTimeout bounds DID document fetches made while validating
	// aggregator service JWTs. The middleware passes the request context to
	// the validator, so cancellation is already honoured; this is the safety
	// net for a PLC directory that accepts connections but never answers.
	identityHTTPTimeout = 10 * time.Second

	// (The profile backfill's timeout used to be declared here as well. It now
	// lives with the client that carries it, in users.NewProfileBackfillClient,
	// because that client also has to re-apply it over the shared SSRF client's
	// own ceiling — and a deadline stated in two packages is one that can be
	// changed in one of them.)

	// unfurlTimeout bounds fetching and parsing a link preview target.
	unfurlTimeout = 10 * time.Second

	// unfurlCacheTTL is how long a link preview is reused before refetching.
	unfurlCacheTTL = 24 * time.Hour

	// blueskyFetchTimeout bounds fetching a quoted Bluesky post.
	blueskyFetchTimeout = 10 * time.Second

	// blueskyCacheTTL is deliberately shorter than unfurlCacheTTL: a quoted
	// post's engagement counts go stale faster than a link's title does.
	blueskyCacheTTL = time.Hour

	// voteCacheTTL is how long a user's votes read from their PDS are reused.
	// It papers over PDS eventual consistency between casting a vote and the
	// firehose delivering it back; the cache is also written through on
	// create and delete.
	voteCacheTTL = 10 * time.Minute

	// unfurlUserAgent identifies Coves to the sites it fetches previews from.
	unfurlUserAgent = "CovesBot/1.0 (+https://coves.social)"
)

// application holds every constructed dependency, wired once at startup and
// then read-only. It exists so the wiring can be split across focused
// functions without threading a dozen parameters through each one.
type application struct {
	cfg *config.Config
	db  *sql.DB

	// Identity and authentication
	identityResolver identity.Resolver
	oauthClient      *oauth.OAuthClient
	oauthStore       *oauth.MobileAwareStoreWrapper
	oauthHandler     *oauth.OAuthHandler
	authMiddleware   *middleware.OAuthAuthMiddleware
	dualAuth         *middleware.DualAuthMiddleware

	// Repositories reused outside their own service (Jetstream consumers,
	// route options).
	userRepo      users.UserRepository
	communityRepo communities.Repository
	// postRepo is held as the CONCRETE repository rather than posts.Repository:
	// the comment service's PostReader requires the admission-aware
	// VisibleHeaderView as well, and storing the narrower interface here would
	// erase it before the wiring could hand it over.
	postRepo       *postgresRepo.PostRepository
	voteRepo       votes.Repository
	commentRepo    comments.Repository
	userBlockRepo  userblocks.Repository
	aggregatorRepo aggregators.Repository
	// admissionRepo is shared by the ingestion consumer, which WRITES the
	// per-(community, post) decisions, and the status query, which reads them.
	admissionRepo posts.AdmissionRepository

	// Domain services
	userService      users.UserService
	communityService communities.Service
	postService      posts.Service
	// postStatusService answers post.getStatus. Separate from postService
	// because a status query needs the admissions store and nothing else, and
	// widening the write-path interface to reach it would make every test
	// double of posts.Service carry a method it has no opinion about.
	postStatusService posts.StatusService
	// communityWriter publishes acceptances and removals into the repos of
	// communities this AppView hosts. Shared by the acceptance engine, which
	// writes verdicts, and the post consumer, which withdraws an acceptance
	// when the author deletes the post it covers.
	communityWriter posts.CommunityRecordWriter
	// acceptanceQueue walks the undecided backlog. nil when the driver is
	// disabled (ACCEPTANCE_QUEUE_INTERVAL=0).
	acceptanceQueue            *posts.QueueDriver
	voteService                votes.Service
	commentService             comments.Service
	userBlockService           userblocks.Service
	adminReportService         adminreports.Service
	communitySuggestionService communitysuggestions.Service
	feedService                communityFeeds.Service
	timelineService            timeline.Service
	discoverService            discover.Service
	aggregatorService          aggregators.Service
	apiKeyService              *aggregators.APIKeyService
	blueskyService             blueskypost.Service

	// Jetstream indexing infrastructure
	jetstreamState *jetstream.PostgresStateStore
	revGate        *jetstream.RevGate
	bridgeTrust    *jetstream.BridgeTrust

	// imageProxyHandler is nil when the image proxy is disabled.
	imageProxyHandler *imageproxyhandlers.Handler
	// stopImageProxyCleanup halts the disk cache eviction job. Never nil.
	stopImageProxyCleanup context.CancelFunc
	// closeOnce guards Close, which is reached from both serve and run's
	// deferred cleanup on every shutdown.
	closeOnce sync.Once
}

// buildApplication constructs every repository, service, and middleware the
// server needs, in dependency order.
//
// Ordering here is load-bearing in a few places, each noted at the call site.
//
// On failure it cleans up whatever it already started and returns (nil, err),
// the conventional Go contract. Returning a usable value alongside an error
// and asking the caller to Close it would invert that convention, and would
// become a nil dereference during an already-failing boot the first time
// anyone added a plain `return nil, err` below.
func buildApplication(ctx context.Context, cfg *config.Config, db *sql.DB) (app *application, err error) {
	app = &application{
		cfg:                   cfg,
		db:                    db,
		stopImageProxyCleanup: func() {},
	}

	defer func() {
		if err != nil {
			app.Close()
			app = nil
		}
	}()

	app.buildIdentity()
	if err = app.buildAuth(); err != nil {
		return nil, err
	}
	app.buildRepositories()
	if err = app.buildServices(ctx); err != nil {
		return nil, err
	}
	if err = app.buildImageProxy(); err != nil {
		return nil, err
	}
	app.buildJetstreamInfrastructure()
	return app, nil
}

// Close releases resources owned by the application that are not tied to the
// process lifetime. It is safe to call more than once and from more than one
// goroutine — both serve and run's deferred cleanup reach it.
func (a *application) Close() {
	a.closeOnce.Do(func() {
		a.stopImageProxyCleanup()
	})
}

// allowPrivateHosts is THE ONE CONVERSION from configuration to SSRF policy in
// this binary. Every hatch-bearing call site below calls this rather than
// reading a.cfg.IsDevEnv, so there is exactly one expression whose polarity has
// to be right, and exactly one place a test has to reach.
//
// It exists because the polarity was previously spelled out thirteen times in
// this package and no test anywhere evaluated any of them. `.env.ci:140` sets
// IS_DEV_ENV=true, so `make ci` — the hermetic merge gate — takes the PERMISSIVE
// branch at every one of those sites; inverting any single spelling left the
// whole gate green. Collapsed here, the production branch is evaluated by
// TestApplication_AllowPrivateHosts_IsOffUnderAProductionConfig, which is the
// only place in the repository that ever runs it.
//
// It deliberately does NOT read the environment. cfg.IsDevEnv is parsed once at
// boot, and an ambient read here would open every construction in this process
// at once — including productionPLCResolver, which has to stay guarded in dev.
//
// NOT every a.cfg.IsDevEnv read belongs here. buildAuth's OAuthConfig.DevMode is
// an AUTHENTICATION gate (cookie Secure flags, redirect-URI validation) that
// happens to share the same input, and the two slog severity switches are about
// what an operator is told rather than about what may be dialled. Folding those
// in would make one accessor answer three different questions.
func (a *application) allowPrivateHosts() bool { return a.cfg.IsDevEnv }

func (a *application) buildIdentity() {
	// The hatch, and ONLY here. This resolver follows
	// cfg.Identity.ResolverPLCURL, which in dev is a PLC on the developer's own
	// machine. productionPLCResolver below is built from the same DefaultConfig
	// and must NOT get it — the two resolvers need opposite answers in the same
	// process, which is why this is an argument rather than something the
	// identity package works out for itself.
	identityConfig := identity.DefaultConfig(identity.PrivateHostOptions(a.allowPrivateHosts())...)
	identityConfig.PLCURL = a.cfg.Identity.ResolverPLCURL
	if a.cfg.Identity.CacheTTL > 0 {
		identityConfig.CacheTTL = a.cfg.Identity.CacheTTL
	}
	a.identityResolver = identity.NewResolver(a.db, identityConfig)

	// The same expression the hatch above was derived from, not a second read of
	// the same config field: a log line describing the SSRF gate has to be unable
	// to disagree with the gate itself.
	if a.allowPrivateHosts() {
		slog.Warn("dev mode: identity resolver is using a local PLC directory",
			"plc_url", identityConfig.PLCURL)
	} else {
		slog.Info("identity resolver initialized", "plc_url", identityConfig.PLCURL)
	}
}

func (a *application) buildAuth() error {
	// The wrapper intercepts SaveAuthRequestInfo to capture mobile CSRF state
	// from the request context, so it must be what everything else holds.
	baseOAuthStore := oauth.NewPostgresOAuthStore(a.db, 0) // 0 = default 7-day TTL
	a.oauthStore = oauth.NewMobileAwareStoreWrapper(baseOAuthStore)

	oauthConfig := &oauth.OAuthConfig{
		PublicURL:  a.cfg.OAuth.PublicURL,
		SealSecret: a.cfg.OAuth.SealSecret,
		Scopes:     oauthScopes(),
		DevMode:    a.cfg.IsDevEnv,
		// Private IPs are only resolvable in dev, where the PDS and PLC run
		// on localhost. In production this must stay off: it is what stops
		// OAuth from being pointed at an internal address.
		AllowPrivateIPs: a.allowPrivateHosts(),
		PLCURL:          a.cfg.Identity.PLCURL,
		PDSURL:          a.cfg.PDS.URL,
		// Setting both upgrades this to a confidential client, lifting the
		// 14-day session cap the authorization server imposes on public
		// clients. Our own defaults then apply — see oauth.NewOAuthClient.
		ClientPrivateKeyMultibase: a.cfg.OAuth.ClientPrivateKeyMultibase,
		ClientKeyID:               a.cfg.OAuth.ClientKeyID,
	}

	client, err := oauth.NewOAuthClient(oauthConfig, a.oauthStore)
	if err != nil {
		return fmt.Errorf("initializing OAuth client: %w", err)
	}
	a.oauthClient = client

	a.authMiddleware = middleware.NewOAuthAuthMiddleware(a.oauthClient, a.oauthStore)
	slog.Info("OAuth auth middleware initialized (sealed session tokens)")
	return nil
}

// oauthScopes lists the atProto OAuth scopes Coves requests. Each collection
// is scoped to the exact actions the AppView performs on the user's behalf.
func oauthScopes() []string {
	return []string{
		"atproto",
		"blob:*/*", // avatar and image uploads
		// The author-owned post collection: CreatePost, UpdatePost (§3.4),
		// post.delete AND the cutover tool all write postv2 through the author's
		// own OAuth session, so a scope-enforcing PDS refuses the entire write
		// path without this grant.
		"repo:social.coves.community.postv2?action=create&action=update&action=delete",
		// The deprecated collection is RETAINED through the drain: the cutover tool
		// deletes legacy community.post records through these same sessions (§11);
		// dropping it would strand every legacy record undeleteable.
		"repo:social.coves.community.post?action=create&action=update&action=delete",
		"repo:social.coves.community.comment?action=create&action=update&action=delete",
		"repo:social.coves.community.profile?action=create&action=update&action=delete",
		"repo:social.coves.community.subscription?action=create&action=update&action=delete",
		"repo:social.coves.actor.profile?action=create&action=update&action=delete",
		"repo:social.coves.feed.vote?action=create&action=delete",
		"repo:social.coves.actor.block?action=create&action=delete",
	}
}

func (a *application) buildRepositories() {
	a.userRepo = postgresRepo.NewUserRepository(a.db)
	a.communityRepo = postgresRepo.NewCommunityRepository(a.db)
	a.postRepo = postgresRepo.NewPostRepository(a.db)
	a.voteRepo = postgresRepo.NewVoteRepository(a.db)
	a.commentRepo = postgresRepo.NewCommentRepository(a.db)
	a.userBlockRepo = postgresRepo.NewUserBlockRepository(a.db)
	a.aggregatorRepo = postgresRepo.NewAggregatorRepository(a.db)
	a.admissionRepo = postgresRepo.NewAdmissionRepository(a.db)
}

func (a *application) buildServices(ctx context.Context) error {
	var turnstileVerifier users.TurnstileVerifier
	if a.cfg.Signup.TurnstileSecretKey != "" {
		turnstileVerifier = users.NewCloudflareTurnstile(
			a.cfg.Signup.TurnstileSecretKey,
			users.WithSiteverifyURL(a.cfg.Signup.TurnstileSiteverifyURL),
		)
	}

	// Profile backfill covers users indexed without profile data — typically
	// because their profile firehose event was missed — by fetching
	// social.coves.actor.profile from their PDS asynchronously while the user
	// is indexed, by either door: the firehose's IndexUser and the OAuth
	// callback's IndexAuthenticatedUser share that tail.
	a.userService = users.NewUserService(
		a.userRepo,
		a.identityResolver,
		a.cfg.PDS.URL,
		turnstileVerifier,
		a.cfg.PDS.AdminPassword,
		// The backfill dials a PDS URL taken from the indexed user's record —
		// firehose-discovered users bring that value from another instance — so
		// the guard stays on in production and the hatch opens only in dev,
		// where every PDS a developer indexes against is on loopback.
		users.WithProfileBackfill(users.NewProfileBackfillClient(a.allowPrivateHosts())),
		users.WithInstanceDomain(a.cfg.Instance.Domain),
	)

	// The OAuth handler indexes users into the AppView after login, so it
	// must be built after userService.
	a.oauthHandler = oauth.NewOAuthHandler(a.oauthClient, a.oauthStore,
		oauth.WithUserIndexer(a.userService))

	// The remote-fetch SSRF guard is ON in production and off only in dev, where
	// the PDS, the PLC and every fixture origin live on loopback — exactly what
	// the guard refuses. Derived from config ONCE, here, rather than read inside
	// the fetch: an environment read at the call site would make the guarded
	// branch untestable alongside t.Parallel and hide the most consequential
	// input to a security decision from the place that makes it.
	//
	// The decision goes through blobs.PrivateHostOptions rather than an inline
	// `if` here, for the reason that helper documents: `.env.ci:140` sets
	// IS_DEV_ENV=true, so `make ci` takes the permissive branch, and an inline
	// conditional in wiring is reachable only by standing up this wiring with a
	// production config — which nothing in this tree does. As a pure function the
	// branch production actually runs becomes testable. The warning stays here,
	// because it is about this process and not about the option — and it reads the
	// SAME accessor the option is derived from, so it cannot drift into describing
	// a gate state that is not the one in force.
	if a.allowPrivateHosts() {
		slog.Warn("dev mode: the blob SSRF guard is disabled on both the remote fetch and the " +
			"PDS upload; remote image URLs and PDS URLs may resolve to private addresses")
	}
	blobService := blobs.NewBlobService(a.cfg.PDS.URL, blobs.PrivateHostOptions(a.allowPrivateHosts())...)

	// V2.0: the PDS generates and manages community DIDs and keys entirely;
	// Coves performs no cryptography of its own here.
	//
	// The same gate as the blob service above, on the xrpc calls this package
	// makes to a community's PDS: account provisioning, token refresh, and the
	// password re-auth that carries the community's cleartext password. All three
	// used to leave xrpc.Client.Client nil, which makes indigo substitute an
	// unguarded util.RobustHTTPClient(). In dev cfg.PDS.URL is a loopback address
	// the guard would otherwise refuse.
	communityPDSOptions := communities.PrivateHostOptions(a.allowPrivateHosts())
	provisioner := communities.NewPDSAccountProvisioner(
		a.cfg.Instance.Domain, a.cfg.PDS.URL, communityPDSOptions...)
	a.communityService = communities.NewCommunityService(
		a.communityRepo,
		a.cfg.PDS.URL,
		a.cfg.Instance.DID,
		a.cfg.Instance.Domain,
		provisioner,
		a.oauthClient,
		blobService,
		communityPDSOptions...,
	)
	a.authenticateInstanceWithPDS(ctx)

	a.aggregatorService = aggregators.NewAggregatorService(a.aggregatorRepo, a.communityService)
	a.apiKeyService = aggregators.NewAPIKeyService(a.aggregatorRepo, a.oauthClient.ClientApp)

	a.buildDualAuth()

	// The SSRF hatch is open only in dev, where the links a developer pastes and
	// the fixtures the test suite serves both live on the developer's own
	// machine. In production this fetch dials whatever address a link pasted into
	// a post resolves to — and unlike every other guarded fetch in this tree, it
	// hands the response's CONTENT back, so an internal endpoint's page title
	// would land in the unfurl cache and then in the post itself.
	unfurlOptions := append([]unfurl.ServiceOption{
		unfurl.WithTimeout(unfurlTimeout),
		unfurl.WithUserAgent(unfurlUserAgent),
		unfurl.WithCacheTTL(unfurlCacheTTL),
	}, unfurl.PrivateHostOptions(a.allowPrivateHosts())...)
	unfurlService := unfurl.NewService(unfurl.NewRepository(a.db), unfurlOptions...)

	// Quoted Bluesky posts reference real handles on the production atProto
	// network, which the dev/test PLC cannot resolve. This resolver is
	// therefore always pointed at plc.directory, in every environment.
	//
	// NO SSRF HATCH, IN DEV EITHER — deliberately, and unlike a.identityResolver
	// above. It is aimed at the public directory, so it has no reason to dial a
	// private address; and dev is precisely the environment where the loopback
	// it would otherwise be allowed to dial is a real PLC, a real Postgres and
	// a real PDS. DefaultConfig() with no options is the guarded construction,
	// which is why this line looks like it is doing nothing.
	//
	// (An earlier comment justified this resolver as "safe to use in dev
	// because identity.Resolver only ever issues HTTP GETs". That reasoning is
	// backwards: only-GETs IS the SSRF primitive. The image-proxy port scanner
	// closed in ff901a5 was GET-only.)
	productionPLCConfig := identity.DefaultConfig()
	productionPLCConfig.PLCURL = "https://plc.directory"
	productionPLCResolver := identity.NewResolver(a.db, productionPLCConfig)

	a.blueskyService = blueskypost.NewService(
		blueskypost.NewRepository(a.db),
		productionPLCResolver,
		blueskypost.WithTimeout(blueskyFetchTimeout),
		blueskypost.WithCacheTTL(blueskyCacheTTL),
	)

	// userBlockRepo backs viewer block enforcement on GetPosts, keeping
	// permalink and cold-load reads consistent with feed/timeline filtering.
	// The admission policy is what makes the ban lookup and the per-author
	// submission quota live (PRD_AUTHOR_OWNED_POSTS §4.1, §8). A post service
	// built without it enforces only what CreatePost enforced before those
	// existed, so this is not an optional enrichment — it is the enforcement.
	// The limits come from config, which refuses to start the process with a
	// non-positive one rather than letting an omission read as "unlimited".
	//
	// The engine is built FIRST because the write path now holds it: §4.2 step
	// 4 has CreatePost settle a local community's admission before it answers,
	// so the author gets the community's decision with their post rather than
	// waiting for their own write to come back around the firehose.
	acceptanceEngine := a.buildAcceptanceEngine()

	a.postService = posts.NewPostService(
		a.postRepo, a.communityService, a.aggregatorService, blobService,
		unfurlService, a.blueskyService, a.cfg.PDS.URL,
		posts.WithBlockChecker(a.userBlockRepo),
		// The SSRF dev gate for the clients this service builds against a
		// COMMUNITY's repo, whose PDS URL is a database column. deleteCommunityPost
		// is the path that needs it here; the withdrawer's factory carries its own,
		// set in buildAcceptanceEngine.
		posts.WithPDSClientOptions(pds.PrivateHostOptions(a.allowPrivateHosts())...),
		// The AUTHOR's own credentials: a browser session when there is one,
		// and an aggregator's stored tokens when there is not (§4.2 step 3).
		posts.WithAuthorRepoFactory(
			posts.NewAuthorRepoFactory(a.oauthClient.ClientApp, aggregators.DefaultSessionID)),
		posts.WithSyncAcceptance(a.admissionRepo, acceptanceEngine),
		// The SAME writer the engine accepts through, so both ends of an
		// acceptance's life — the write the fast path makes and the withdrawal
		// the author's delete makes (§5.3) — go through one configured writer
		// (same repo factory and clock func). The writer is stateless per call,
		// so this is shared configuration, not shared runtime budget.
		// buildAcceptanceEngine above is what populated it.
		posts.WithAcceptanceWithdrawal(a.communityWriter),
		posts.WithAdmissionPolicy(posts.AdmissionPolicy{
			Ledger: postgresRepo.NewSubmissionLedger(a.db),
			Bans:   a.communityService,
			Limits: posts.SubmissionLimits{
				MaxPerAuthorPerCommunity: a.cfg.Submissions.MaxPerAuthorPerCommunity,
				Window:                   a.cfg.Submissions.Window,
				DedupeWindow:             a.cfg.Submissions.DedupeWindow,
			},
			Now: time.Now,
		}),
	)

	// getStatus is how an author on another server learns what happened to
	// their post. It is the ONLY way a rejection is reachable — a submission
	// refused before it was ever accepted writes no community record, so there
	// is nothing on the firehose to read.
	a.postStatusService = posts.NewStatusService(a.admissionRepo)

	// Subject existence is deliberately not validated: the vote is written to
	// the user's own PDS regardless, and the Jetstream consumer only updates
	// counts for subjects that still exist. Checking here would trade a
	// harmless orphan vote for a race against eventual consistency.
	voteCache := votes.NewVoteCache(voteCacheTTL, nil)
	a.voteService = votes.NewService(a.voteRepo, a.oauthClient, a.oauthStore, voteCache, nil)

	a.commentService = comments.NewCommentService(
		a.commentRepo, a.userRepo, a.postRepo, a.communityRepo,
		a.oauthClient, a.oauthStore, nil,
	)
	a.userBlockService = userblocks.NewService(a.userBlockRepo, nil, a.oauthClient, a.oauthStore, nil)
	adminReportOptions, err := adminReportAlertOptions()
	if err != nil {
		return err
	}
	a.adminReportService = adminreports.NewService(
		postgresRepo.NewAdminReportRepository(a.db), adminReportOptions...)
	a.communitySuggestionService = communitysuggestions.NewService(
		postgresRepo.NewCommunitySuggestionRepository(a.db))

	a.feedService = communityFeeds.NewCommunityFeedService(
		postgresRepo.NewCommunityFeedRepository(a.db, a.cfg.CursorSecret), a.communityService)
	a.timelineService = timeline.NewTimelineService(
		postgresRepo.NewTimelineRepository(a.db, a.cfg.CursorSecret))
	a.discoverService = discover.NewDiscoverService(
		postgresRepo.NewDiscoverRepository(a.db, a.cfg.CursorSecret))

	slog.Info("domain services initialized")
	return nil
}

// buildAcceptanceEngine wires the §5.6 acceptance engine and the driver that
// feeds it.
//
// EVERYTHING HERE IS ABOUT WRITING INTO A COMMUNITY'S OWN REPO, which only that
// community's host can do — so on an instance that hosts nothing, all of it is
// a no-op that costs one query a minute: the repo factory refuses with
// ErrCommunityNotHosted and the backlog query returns nothing. Both are keyed on
// STORED CREDENTIALS rather than on communities.hosted_by_did, which is copied
// out of a community's own profile record and can therefore be claimed by any
// repo on the network.
// buildDeciderDeps assembles everything the production admission decider reads.
//
// Extracted from buildAcceptanceEngine so the wiring itself is testable: the §8
// firehose quota is only enforced when DeciderDeps.Admissions is wired
// (decider.go applyQuota short-circuits on a nil counter), and a struct literal
// that silently omits the field disables the abuse control in production with no
// error anywhere. wiring_quota_test.go asserts the field is set.
func (a *application) buildDeciderDeps() posts.DeciderDeps {
	return posts.DeciderDeps{
		Posts:       a.postRepo,
		Communities: a.communityService,
		Authorizer:  a.aggregatorService,
		// The §8 firehose quota counter. Without it decider.go's applyQuota
		// short-circuits and admits UNLIMITED posts — the same admissions repo the
		// ingestion consumer writes and the engine settles, so the rows counted as
		// admitted are the rows the quota meters.
		Admissions:  a.admissionRepo,
		Aggregators: a.aggregatorService,
		Policy: posts.AdmissionPolicy{
			Ledger: postgresRepo.NewSubmissionLedger(a.db),
			Bans:   a.communityService,
			Limits: posts.SubmissionLimits{
				MaxPerAuthorPerCommunity: a.cfg.Submissions.MaxPerAuthorPerCommunity,
				Window:                   a.cfg.Submissions.Window,
				DedupeWindow:             a.cfg.Submissions.DedupeWindow,
			},
			Now: time.Now,
		},
		// Resolved ONCE, here, rather than read per decision — and through the
		// same helper the write path uses, so the two cannot drift into
		// disagreeing about who is privileged.
		TrustedAggregatorDIDs: posts.TrustedAggregatorDIDs(),
	}
}

func (a *application) buildAcceptanceEngine() *posts.AcceptanceEngine {
	repoFactory := posts.NewCommunityRepoFactory(a.communityService,
		pds.PrivateHostOptions(a.allowPrivateHosts())...)
	a.communityWriter = posts.NewCommunityRecordWriter(repoFactory, time.Now)

	decider := posts.NewAdmissionEngineDecider(a.buildDeciderDeps())

	engine := posts.NewAcceptanceEngine(
		a.admissionRepo, decider, a.communityWriter,
		posts.NewCommunityCredentialRefresher(a.communityService))

	// Zero DISABLES the driver, and leaving the field nil is what makes
	// /health/consumers omit the queue block entirely. An all-zero queue and an
	// absent one mean different things: the first reads as a driver that is
	// running and settling nothing, which is the exact failure an operator
	// watches for.
	if a.cfg.Submissions.AcceptanceQueueInterval <= 0 {
		slog.Warn("acceptance queue driver disabled (ACCEPTANCE_QUEUE_INTERVAL=0); " +
			"posts left undecided by the fast path and the firehose will not be revisited")
		return engine
	}

	a.acceptanceQueue = posts.NewQueueDriver(a.admissionRepo, engine, time.Now,
		posts.WithQueueBatchSize(a.cfg.Submissions.AcceptanceQueueBatchSize))

	// RETURNED, NOT ONLY STORED IN THE DRIVER. The write path's fast path and
	// the queue driver settle the same subjects through the same engine on
	// purpose: the deterministic rkeys and swap guards make concurrent passes
	// converge, and a second engine instance would just be a second set of the
	// same collaborators.
	return engine
}

// adminReportAlertOptions builds the operator-alert wiring for admin reports.
//
// Alerting is opt-in (TELEGRAM_ALERTS_ENABLED): most operators running their
// own Coves instance will not want it, and a deployment must not need a
// Telegram account to boot. When it is switched on, however, a misconfiguration
// fails startup rather than degrading to "no alerts" — a silent alerter is
// indistinguishable from a quiet week, which is the fault this exists to fix.
func adminReportAlertOptions() ([]adminreports.ServiceOption, error) {
	cfg, err := telegram.ConfigFromEnv()
	if err != nil {
		return nil, fmt.Errorf("telegram alerts: %w", err)
	}

	if !cfg.Enabled {
		if cfg.CredentialsPresentWhileDisabled {
			// Almost certainly a half-finished setup rather than a deliberate
			// opt-out. Not fatal — disabling a noisy channel in a hurry is
			// legitimate — but it must not pass as quietly as a real opt-out.
			slog.Warn("Telegram credentials are configured but TELEGRAM_ALERTS_ENABLED is not true; "+
				"reports will be stored and NOBODY will be notified",
				"fix", "set TELEGRAM_ALERTS_ENABLED=true")
		} else {
			slog.Info("admin report alerts disabled; reports are stored but nobody is notified",
				"enable_with", "TELEGRAM_ALERTS_ENABLED=true")
		}
		return nil, nil
	}

	reasons, err := adminreports.ParseAlertReasons(cfg.AlertReasons)
	if err != nil {
		return nil, fmt.Errorf("TELEGRAM_ALERT_REASONS: %w", err)
	}

	client, err := telegram.NewClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("telegram alerts: %w", err)
	}

	// Log which reasons alert, never the token or chat ID.
	alerting := "all reasons"
	if len(reasons) > 0 {
		names := make([]string, 0, len(reasons))
		for _, reason := range reasons {
			names = append(names, string(reason))
		}
		alerting = strings.Join(names, ",")
	}
	slog.Info("admin report alerts enabled", "channel", "telegram", "reasons", alerting)

	return []adminreports.ServiceOption{
		adminreports.WithNotifier(adminreports.NewMessageNotifier(client, reasons)),
	}, nil
}

// serviceJWTIdentityDirectory builds the identity directory that service-JWT
// validation resolves an inbound `iss` DID through.
//
// # THE MOST CREDENTIAL-FREE FETCH IN THE PROCESS
//
// indigo's ServiceAuthValidator resolves a JWT's `iss` DID in order to find the
// key that would verify its signature — so the resolution happens BEFORE the
// credential is trusted, and it has to: there is nothing to verify against
// until the DID document is in hand. An attacker sends a syntactically valid
// JWT claiming any `iss` they like and the AppView fetches whatever that DID
// document points at. No token needs to be valid and no account needs to exist.
//
// # WHY A FUNCTION AND NOT AN `if` IN buildDualAuth
//
// buildDualAuth is a method on *application, so reaching it means standing up
// wiring with a config, a database and an OAuth client, which nothing in this
// tree does. `.env.ci:140` sets IS_DEV_ENV=true, so `make ci` would take the
// permissive branch even if it could. Extracted, the decision is testable in T0
// without any of that — and the gate must stay an ARGUMENT rather than an
// ambient read of the environment, because this same process builds
// productionPLCResolver, which has to stay guarded in dev. Do not inline it
// back.
//
// # ORDERING
//
// BaseDirectory.HTTPClient is a VALUE, so the shared constructor's pointer is
// dereferenced here — and the timeout has to be applied BEFORE the copy is
// taken, since anything set afterwards is set on a client nobody holds.
//
// The copy itself is safe, though not for the reason it is tempting to give:
// http.Client carries NO LOCK of its own — its four fields are a Transport
// interface, two funcs and a Duration — so `go vet`'s copylocks has nothing to
// say here and would not have caught a type that did. What makes it safe is
// that the copy shares rather than duplicates: both values hold the same
// *ssrfSafeTransport pointer, so the guard, the resolver and the connection
// pool behind it are one instance. A transport copied BY VALUE would be the
// bug, and it is not what this line does.
//
// identityHTTPTimeout (10s) survives the shared client's own
// 15s ceiling because it bounds a fetch an unauthenticated caller can trigger,
// which makes it a denial-of-service bound and not just a latency one.
func serviceJWTIdentityDirectory(plcURL string, allowPrivateHosts bool) *indigoidentity.BaseDirectory {
	client := oauth.NewSSRFSafeHTTPClient(oauth.PrivateAddressOptions(allowPrivateHosts)...)
	client.Timeout = identityHTTPTimeout
	return &indigoidentity.BaseDirectory{
		PLCURL:     plcURL,
		HTTPClient: *client,
	}
}

// buildDualAuth wires the middleware that accepts all three credential types:
// sealed OAuth session tokens (users), PDS-signed service JWTs (aggregators),
// and API keys (aggregator bots).
func (a *application) buildDualAuth() {
	identityDir := serviceJWTIdentityDirectory(a.cfg.Identity.PLCURL, a.allowPrivateHosts())
	serviceValidator := &indigoauth.ServiceAuthValidator{
		// The instance DID is the audience aggregator JWTs must be issued for.
		Audience:        a.cfg.Instance.DID,
		Dir:             identityDir,
		TimestampLeeway: 30 * time.Second,
	}

	a.dualAuth = middleware.NewDualAuthMiddleware(
		a.oauthClient,    // SessionUnsealer for OAuth
		a.oauthStore,     // ClientAuthStore for OAuth sessions
		serviceValidator, // service JWT validation
		a.aggregatorRepo, // AggregatorChecker
	).WithAPIKeyValidator(middleware.NewAPIKeyValidatorAdapter(a.apiKeyService))

	slog.Info("dual auth middleware initialized (OAuth + service JWT + API keys)",
		"service_jwt_audience", a.cfg.Instance.DID)
}

// authenticateInstanceWithPDS logs the instance into its own PDS account and
// hands the resulting access token to the community service.
//
// CAVEAT, and the reason none of the messages below claim a capability: the
// token this obtains is currently inert. communityService stores it in
// pdsAccessToken, and nothing reads that field — community creation actually
// authenticates with the per-community PDS accounts issued by
// PDSAccountProvisioner, each carrying its own token and refresh path. This
// call is therefore a credential check, not a prerequisite for anything.
//
// It is kept rather than deleted because the credentials are still worth
// validating at boot, and instance-owned writes are a plausible future need.
// If that need does not materialise, delete this along with authenticateWithPDS,
// SetPDSAccessToken, and the pdsAccessToken field. What must not happen is the
// previous state: log lines confidently reporting that community creation is
// disabled, or will fail, or is now enabled — none of which were true, and each
// of which sent an operator chasing a phantom.
//
// Failure is non-fatal for the same reason: nothing downstream depends on it.
func (a *application) authenticateInstanceWithPDS(ctx context.Context) {
	if !a.cfg.PDS.HasInstanceCredentials() {
		slog.Info("PDS_INSTANCE_HANDLE / PDS_INSTANCE_PASSWORD not set; " +
			"skipping the instance PDS credential check")
		return
	}

	accessToken, err := authenticateWithPDS(ctx, a.cfg.PDS.URL,
		a.cfg.PDS.InstanceHandle, a.cfg.PDS.InstancePassword)
	if err != nil {
		slog.Warn("instance PDS credential check failed; no current feature depends on it",
			"instance_did", a.cfg.Instance.DID,
			"pds_url", a.cfg.PDS.URL,
			"error", err,
		)
		return
	}

	setter, ok := a.communityService.(interface{ SetPDSAccessToken(string) })
	if !ok {
		slog.Warn("community service does not accept a PDS access token")
		return
	}
	setter.SetPDSAccessToken(accessToken)
	slog.Info("instance authenticated with PDS", "instance_did", a.cfg.Instance.DID)
}

// buildImageProxy sets up the resizing image proxy and publishes the settings
// every view builder uses to render media URLs.
//
// The URL config is published on every success path — including when the proxy
// is disabled — because the view builders need to know whether to emit proxy
// URLs or direct blob URLs. In production, config.Validate has already refused
// the disabled path unless the operator opted into it explicitly.
func (a *application) buildImageProxy() error {
	cfg := a.cfg.Media.ImageProxy

	// Published on every path, including the disabled one. Set explicitly at
	// each exit rather than via defer — defer is for cleanup, and using it for
	// control flow hides that this is the function's main effect when the proxy
	// is off.
	publishURLConfig := func() {
		blobs.SetImageURLConfig(blobs.ImageURLConfig{
			ProxyEnabled: cfg.Enabled,
			ProxyBaseURL: cfg.BaseURL,
			CDNURL:       cfg.CDNURL,
		})
	}

	if !cfg.Enabled {
		publishURLConfig()
		// Warn, not Info. With the proxy off, every image URL this server hands
		// a client addresses a PDS blob endpoint directly — media served around
		// whatever CDN is scanning it, and blocked by the shipped CSP. Nothing
		// downstream logs that: URL generation succeeds, the embeds projection
		// succeeds, and the responses look entirely normal. This line is the
		// only signal that a whole deployment is in that state, so it needs to
		// be findable in the logs rather than buried at Info alongside routine
		// startup chatter. Production additionally refuses to boot here unless
		// the operator set ALLOW_UNPROXIED_MEDIA (see config.mediaProblems).
		slog.Warn("[IMAGE-PROXY] disabled: image URLs will address PDS blob endpoints directly",
			"consequence", "media bypasses any scanning CDN and is blocked by the default Content-Security-Policy",
		)
		return nil
	}

	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("image proxy configuration: %w", err)
	}

	cache, err := imageproxy.NewDiskCache(cfg.CachePath, cfg.CacheMaxGB, cfg.CacheTTLDays)
	if err != nil {
		return fmt.Errorf("creating image proxy cache: %w", err)
	}
	a.stopImageProxyCleanup = cache.StartCleanupJob(cfg.CleanupInterval)

	service, err := imageproxy.NewService(
		cache,
		imageproxy.NewProcessor(),
		// The SSRF hatch is open only in dev, where the PDS runs on the
		// developer's own machine. In production this fetch dials whatever
		// address a DID document's serviceEndpoint names, over a public route
		// that carries no credential.
		imageproxy.NewPDSFetcher(cfg.FetchTimeout, cfg.MaxSourceSizeMB,
			imageproxy.PrivateHostOptions(a.allowPrivateHosts())...),
		cfg,
	)
	if err != nil {
		return fmt.Errorf("creating image proxy service: %w", err)
	}

	a.imageProxyHandler = imageproxyhandlers.NewHandler(service, a.identityResolver)
	publishURLConfig()

	slog.Info("image proxy enabled",
		"base_url", cfg.BaseURL,
		"cdn_url", cfg.CDNURL,
		"cache_path", cfg.CachePath,
		"cache_max_gb", cfg.CacheMaxGB,
		"cache_ttl_days", cfg.CacheTTLDays,
		"cleanup_interval", cfg.CleanupInterval,
		"fetch_timeout", cfg.FetchTimeout,
		"max_source_size_mb", cfg.MaxSourceSizeMB,
	)
	return nil
}

func (a *application) buildJetstreamInfrastructure() {
	// Cursors let each consumer resume from its last processed event after a
	// restart instead of silently losing the gap; the dead letter queue
	// captures events that fail every in-line retry so the redriver can
	// replay them once the underlying failure clears.
	a.jetstreamState = jetstream.NewPostgresStateStore(a.db)

	// The rev gate is the per-record ordering guard that makes it safe to run
	// every consumer against multiple Jetstream feeds carrying the same repos
	// (see rev_gate.go and migration 033).
	a.revGate = jetstream.NewRevGate(a.db)

	// Provenance gate for bridge-asserted vote aggregates. Only repos hosted
	// on a trusted bridge PDS may inflate their displayed counts via
	// bridgedStats; every native repo is default-denied so it cannot
	// self-assert them. Empty means bridgedStats are ignored everywhere,
	// which is the right default for a deployment with no bridge.
	a.bridgeTrust = jetstream.NewBridgeTrust(a.cfg.Instance.TrustedBridgePDSHosts)
	if len(a.cfg.Instance.TrustedBridgePDSHosts) > 0 {
		slog.Info("bridgedStats provenance configured",
			"trusted_bridge_hosts", len(a.cfg.Instance.TrustedBridgePDSHosts))
	} else {
		slog.Info("no trusted bridge PDS hosts configured; bridgedStats will be ignored")
	}
}
