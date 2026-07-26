package main

import (
	"Coves/internal/api/middleware"
	"Coves/internal/atproto/identity"
	"Coves/internal/atproto/jetstream"
	"Coves/internal/atproto/oauth"
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
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
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

	// profileBackfillTimeout bounds the best-effort fetch of a user's
	// social.coves.actor.profile from their PDS during indexing.
	profileBackfillTimeout = 10 * time.Second

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
	userRepo       users.UserRepository
	communityRepo  communities.Repository
	postRepo       posts.Repository
	voteRepo       votes.Repository
	commentRepo    comments.Repository
	userBlockRepo  userblocks.Repository
	aggregatorRepo aggregators.Repository

	// Domain services
	userService                users.UserService
	communityService           communities.Service
	postService                posts.Service
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

func (a *application) buildIdentity() {
	identityConfig := identity.DefaultConfig()
	identityConfig.PLCURL = a.cfg.Identity.ResolverPLCURL
	if a.cfg.Identity.CacheTTL > 0 {
		identityConfig.CacheTTL = a.cfg.Identity.CacheTTL
	}
	a.identityResolver = identity.NewResolver(a.db, identityConfig)

	if a.cfg.IsDevEnv {
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
		AllowPrivateIPs: a.cfg.IsDevEnv,
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
}

func (a *application) buildServices(ctx context.Context) error {
	var turnstileVerifier users.TurnstileVerifier
	if a.cfg.Signup.TurnstileSecretKey != "" {
		turnstileVerifier = users.NewCloudflareTurnstile(a.cfg.Signup.TurnstileSecretKey)
	}

	// Profile backfill covers users indexed without profile data — typically
	// because their profile firehose event was missed — by fetching
	// social.coves.actor.profile from their PDS asynchronously during
	// IndexUser.
	a.userService = users.NewUserService(
		a.userRepo,
		a.identityResolver,
		a.cfg.PDS.URL,
		turnstileVerifier,
		a.cfg.PDS.AdminPassword,
		users.WithProfileBackfill(&http.Client{Timeout: profileBackfillTimeout}),
	)

	// The OAuth handler indexes users into the AppView after login, so it
	// must be built after userService.
	a.oauthHandler = oauth.NewOAuthHandler(a.oauthClient, a.oauthStore,
		oauth.WithUserIndexer(a.userService))

	blobService := blobs.NewBlobService(a.cfg.PDS.URL)

	// V2.0: the PDS generates and manages community DIDs and keys entirely;
	// Coves performs no cryptography of its own here.
	provisioner := communities.NewPDSAccountProvisioner(a.cfg.Instance.Domain, a.cfg.PDS.URL)
	a.communityService = communities.NewCommunityService(
		a.communityRepo,
		a.cfg.PDS.URL,
		a.cfg.Instance.DID,
		a.cfg.Instance.Domain,
		provisioner,
		a.oauthClient,
		blobService,
	)
	a.authenticateInstanceWithPDS(ctx)

	a.aggregatorService = aggregators.NewAggregatorService(a.aggregatorRepo, a.communityService)
	a.apiKeyService = aggregators.NewAPIKeyService(a.aggregatorRepo, a.oauthClient.ClientApp)

	a.buildDualAuth()

	unfurlService := unfurl.NewService(
		unfurl.NewRepository(a.db),
		unfurl.WithTimeout(unfurlTimeout),
		unfurl.WithUserAgent(unfurlUserAgent),
		unfurl.WithCacheTTL(unfurlCacheTTL),
	)

	// Quoted Bluesky posts reference real handles on the production atProto
	// network, which the dev/test PLC cannot resolve. This resolver is
	// therefore always pointed at plc.directory — and is safe to use in dev
	// because identity.Resolver only ever issues HTTP GETs.
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
	a.postService = posts.NewPostService(
		a.postRepo, a.communityService, a.aggregatorService, blobService,
		unfurlService, a.blueskyService, a.cfg.PDS.URL,
		posts.WithBlockChecker(a.userBlockRepo),
	)

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
	a.adminReportService = adminreports.NewService(postgresRepo.NewAdminReportRepository(a.db))
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

// buildDualAuth wires the middleware that accepts all three credential types:
// sealed OAuth session tokens (users), PDS-signed service JWTs (aggregators),
// and API keys (aggregator bots).
func (a *application) buildDualAuth() {
	identityDir := &indigoidentity.BaseDirectory{
		PLCURL:     a.cfg.Identity.PLCURL,
		HTTPClient: http.Client{Timeout: identityHTTPTimeout},
	}
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

// buildImageProxy sets up the optional resizing image proxy and publishes the
// URL-generation settings the communities package uses to render avatars.
//
// The URL config is published on every success path — including when the proxy
// is disabled — because communities needs to know whether to emit proxy URLs or
// direct blob URLs.
func (a *application) buildImageProxy() error {
	cfg := imageproxy.ConfigFromEnv()

	// Published on every path, including the disabled one: communities needs
	// to know whether to render proxy URLs or direct blob URLs. Set explicitly
	// at each exit rather than via defer — defer is for cleanup, and using it
	// for control flow hides that this is the function's main effect when the
	// proxy is off.
	publishURLConfig := func() {
		communities.SetImageProxyConfig(blobs.ImageURLConfig{
			ProxyEnabled: cfg.Enabled,
			ProxyBaseURL: cfg.BaseURL,
			CDNURL:       cfg.CDNURL,
		})
	}

	if !cfg.Enabled {
		publishURLConfig()
		slog.Info("image proxy disabled; blob URLs will be served directly")
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
		imageproxy.NewPDSFetcher(cfg.FetchTimeout, cfg.MaxSourceSizeMB),
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
