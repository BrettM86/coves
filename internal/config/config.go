package config

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"Coves/internal/core/bridgedvotes"
	"Coves/internal/core/imageproxy"
)

// devCursorSecret is the placeholder HMAC key used for pagination cursors when
// CURSOR_SECRET is unset. It is only ever accepted in dev: in production a
// known cursor secret lets anyone forge a signed cursor, so Validate rejects it.
const devCursorSecret = "dev-cursor-secret-change-in-production"

const (
	// sealSecretBytes is the decoded length oauth.NewOAuthClient requires of
	// OAUTH_SEAL_SECRET.
	sealSecretBytes = 32

	// minSecretLength is the shortest value accepted for a production secret
	// that is used directly as key material rather than decoded.
	minSecretLength = 16

	// defaultRedriveInterval is how long a dead-lettered event waits for its
	// next replay. Five minutes is slow enough that a backlog of events failing
	// for the same reason does not hammer the dependency that is already unwell,
	// and fast enough that MaxRedriveAttempts passes still fit inside an outage
	// a person would call brief.
	defaultRedriveInterval = 5 * time.Minute
)

// placeholderPrefix marks the documented "fill this in" values in
// .env.prod.example. They are published in the repository, so a production
// deployment still carrying one has no secret at all.
const placeholderPrefix = "CHANGE_ME"

// isPlaceholder reports whether value is one of the documented placeholders.
func isPlaceholder(value string) bool {
	return strings.HasPrefix(value, placeholderPrefix)
}

// requirePublicHost rejects a URL that is empty, not absolute, or points at the
// loopback interface, which in production means the dev default was never
// replaced.
//
// Absoluteness is checked explicitly because url.Parse accepts almost anything:
// "img.coves.social" parses without error as a bare *path* — no scheme, no
// host — and Hostname() then returns "", which is not in the loopback list and
// so used to slip through. Every URL this guards is handed to clients verbatim,
// and a URL with no origin is one the mobile app cannot resolve at all, so an
// absolute http(s) URL is the actual requirement rather than merely a
// well-formed string.
func requirePublicHost(name, rawURL string) error {
	if rawURL == "" {
		return fmt.Errorf("%s is required in production", name)
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("%s is not a valid URL: %w", name, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("%s must be an absolute http(s) URL in production (got %q); "+
			"a value with no scheme is parsed as a relative path and reaches clients "+
			"as a URL they cannot resolve", name, rawURL)
	}
	host := parsed.Hostname()
	if host == "" {
		return fmt.Errorf("%s must include a hostname in production (got %q)", name, rawURL)
	}
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return fmt.Errorf("%s must not point at localhost in production (got %q); "+
			"the loopback default is dev-only and leaves this unreachable to clients",
			name, rawURL)
	}
	return nil
}

// legacyJetstreamVars are single-feed environment variables replaced by
// JETSTREAM_FEEDS. They are rejected rather than ignored — silently dropping a
// configured firehose URL would leave the AppView indexing nothing while
// looking healthy.
var legacyJetstreamVars = []string{
	"JETSTREAM_URL",
	"COMMUNITY_JETSTREAM_URL",
	"POST_JETSTREAM_URL",
	"AGGREGATOR_JETSTREAM_URL",
	"VOTE_JETSTREAM_URL",
	"COMMENT_JETSTREAM_URL",
}

// Config is the fully resolved server configuration. Defaults are applied
// during Load; Validate then enforces the constraints that differ between dev
// and production.
type Config struct {
	// IsDevEnv relaxes several production requirements (private-IP OAuth
	// resolution, generated secrets, localhost defaults). It must never be
	// true in a real deployment.
	IsDevEnv bool

	Database  DatabaseConfig
	Server    ServerConfig
	Identity  IdentityConfig
	OAuth     OAuthConfig
	Instance  InstanceConfig
	PDS       PDSConfig
	Jetstream JetstreamConfig
	Signup    SignupConfig
	Media     MediaConfig

	// Submissions bounds what one author may post into one community.
	Submissions SubmissionsConfig

	// CursorSecret is the HMAC key that signs pagination cursors, preventing
	// clients from forging or tampering with them.
	CursorSecret string
}

// DatabaseConfig holds the AppView PostgreSQL connection and pool settings.
//
// The pool bounds matter: database/sql defaults to an unlimited number of open
// connections, so a traffic spike can open connections until PostgreSQL's
// max_connections (100 by default) is exhausted — which locks out every other
// client, including psql and the maintenance commands under cmd/.
type DatabaseConfig struct {
	// URL is the libpq connection string for the AppView database.
	URL string

	// MaxOpenConns caps total connections (in use + idle). Kept well below
	// PostgreSQL's default max_connections of 100 so operators and the
	// backfill/reindex tools can still connect during a spike.
	MaxOpenConns int

	// MaxIdleConns caps connections retained for reuse. The database/sql
	// default is 2, which forces a fresh connection and PostgreSQL startup
	// handshake for nearly every query under concurrency; matching
	// MaxOpenConns avoids that churn.
	MaxIdleConns int

	// ConnMaxLifetime retires connections after this age, so a rolling
	// PostgreSQL restart or failover does not strand the pool on dead
	// connections.
	ConnMaxLifetime time.Duration

	// ConnMaxIdleTime releases connections idle for this long, returning
	// server-side memory after a spike subsides.
	ConnMaxIdleTime time.Duration

	// StatementTimeout bounds how long a single query may run server-side.
	// This is enforced by PostgreSQL rather than by the client, so a runaway
	// query is actually cancelled instead of merely being abandoned while it
	// continues to hold a connection and a backend process. Zero disables it.
	StatementTimeout time.Duration
}

// ServerConfig holds the HTTP listener settings.
//
// The timeouts are required, not optional hardening: with all four at zero
// (the net/http default) a single client that opens a connection and never
// completes its request headers pins a goroutine and a file descriptor
// indefinitely. Enough of them exhaust the process's file-descriptor limit —
// the classic slowloris attack. Validate enforces that none is zero.
type ServerConfig struct {
	// Port is the TCP port to listen on.
	Port string

	// ReadHeaderTimeout bounds the time allowed to send request headers.
	// This is the specific defence against slowloris.
	ReadHeaderTimeout time.Duration

	// ReadTimeout bounds reading the entire request, headers plus body.
	ReadTimeout time.Duration

	// WriteTimeout bounds the time from end-of-headers to end-of-response.
	// It must comfortably exceed the slowest legitimate handler — the image
	// proxy, which may spend IMAGE_PROXY_FETCH_TIMEOUT_SECONDS (30s by
	// default) fetching from a remote PDS before it encodes anything.
	WriteTimeout time.Duration

	// IdleTimeout bounds how long an idle keep-alive connection is kept open.
	IdleTimeout time.Duration

	// ShutdownTimeout bounds graceful shutdown: draining in-flight requests
	// and flushing Jetstream consumer cursors.
	ShutdownTimeout time.Duration
}

// IdentityConfig holds atProto identity resolution settings.
type IdentityConfig struct {
	// PLCURL is the PLC directory used to resolve DIDs.
	PLCURL string

	// ResolverPLCURL is the PLC directory used by the identity resolver. In
	// dev this is forced to the local PLC so end-to-end tests never touch
	// plc.directory; in production IDENTITY_PLC_URL may point reads at a
	// separate mirror.
	ResolverPLCURL string

	// CacheTTL overrides the resolver's default cache lifetime. Zero means
	// use the resolver default.
	CacheTTL time.Duration

	// WellKnownHosts redirects the HTTP leg of handle verification, mapping a
	// handle suffix to the host:port that answers /.well-known/atproto-did for
	// it. Read from HANDLE_WELL_KNOWN_HOSTS.
	//
	// DEV AND CI ONLY. See handle_well_known_hosts_config_test.go for the
	// format, the leading-dot rule, and why a value outside dev is a hard
	// error rather than an ignored one.
	WellKnownHosts map[string]string
}

// OAuthConfig holds the atProto OAuth client settings.
type OAuthConfig struct {
	// PublicURL is this AppView's externally reachable base URL. It appears
	// in the OAuth client metadata and the redirect URI, so it must match
	// what the authorization server sees.
	PublicURL string

	// SealSecret encrypts the sealed session tokens handed to clients. It
	// must be stable across restarts: rotating it invalidates every live
	// session, signing out every mobile and web user.
	SealSecret string

	// SealSecretGenerated reports that SealSecret was randomly generated
	// because OAUTH_SEAL_SECRET was unset. Dev-only, and worth warning about
	// loudly — it means every restart signs all users out.
	SealSecretGenerated bool

	// ClientPrivateKeyMultibase and ClientKeyID upgrade this to a
	// confidential OAuth client when both are set, which raises the session
	// lifetime the authorization server will grant.
	ClientPrivateKeyMultibase string
	ClientKeyID               string
}

// InstanceConfig holds this Coves instance's atProto identity.
type InstanceConfig struct {
	// DID identifies this instance and is the audience for aggregator
	// service JWTs.
	DID string

	// Domain suffixes community handles. For did:web instance DIDs it is
	// derived from the DID itself rather than read from the environment:
	// allowing an arbitrary domain would let an instance mint handles like
	// !leagueoflegends@riotgames.com and impersonate another operator.
	Domain string

	// AllowedCommunityCreators restricts community creation to these DIDs.
	// Nil means any authenticated user may create a community.
	AllowedCommunityCreators []string

	// TrustedBridgePDSHosts may assert bridged vote aggregates
	// (bridgedStats) for the repos they host. Every other repo is
	// default-denied so it cannot inflate its own vote counts. Nil means
	// bridgedStats are ignored everywhere.
	TrustedBridgePDSHosts []string

	// SkipDIDWebVerification disables did:web domain verification in the
	// community consumer. Dev-only: it is what stops a community record from
	// claiming to be hosted by a domain it does not control.
	SkipDIDWebVerification bool

	// BridgedVotePollInterval is how often the bridged-vote poller sweeps the
	// trusted bridges' vote-aggregate side channel. It must be positive: unlike
	// its two siblings below, zero does not mean "poller default", and the
	// poller has no disabled state of its own — an instance that should not
	// poll leaves TRUSTED_BRIDGE_PDS_HOSTS unset. Validate enforces this so a
	// typo fails the boot instead of leaving a job that logs "configured" and
	// never runs.
	BridgedVotePollInterval time.Duration

	// BridgedVotePollLookback bounds sweep candidates by created_at. Zero means
	// the poller's own default applies (single source of truth for the value).
	BridgedVotePollLookback time.Duration

	// BridgedVotePollSweepCap bounds subjects polled per sweep. Zero means the
	// poller's own default applies.
	BridgedVotePollSweepCap int
}

// PDSConfig holds the settings for this instance's own PDS account, used to
// write the community records the instance owns.
type PDSConfig struct {
	// URL is the default PDS for this instance.
	URL string

	// InstanceHandle and InstancePassword authenticate the instance's PDS
	// account. When either is empty, community write-forward is disabled.
	InstanceHandle   string
	InstancePassword string

	// AdminPassword mints single-use PDS invite codes after a successful
	// captcha. Empty disables the signup-token endpoint.
	AdminPassword string
}

// HasInstanceCredentials reports whether the instance can authenticate with
// its PDS to write community records.
func (p PDSConfig) HasInstanceCredentials() bool {
	return p.InstanceHandle != "" && p.InstancePassword != ""
}

// JetstreamConfig holds the firehose feed topology.
type JetstreamConfig struct {
	// FeedsSpec is the raw semicolon-separated <feedKey>=<baseURL> list.
	// Parsing into feeds lives in the jetstream package, which owns the
	// primary-feed and cursor-naming semantics.
	FeedsSpec string

	// RedriveInterval is how often the dead letter redriver replays the
	// backlog.
	//
	// It must be STRICTLY POSITIVE, which is the opposite of
	// SubmissionsConfig.AcceptanceQueueInterval, and the difference is worth
	// keeping straight because the two look interchangeable. Zero disables the
	// acceptance driver, and disabling it is a supported deployment: an AppView
	// that hosts no communities has no backlog to walk. The redriver has no
	// such deployment — every consumer dead-letters, so a disabled redriver is
	// a queue that only ever grows — and a zero interval would panic
	// time.NewTicker on the first boot after a typo rather than quietly
	// disabling anything.
	RedriveInterval time.Duration
}

// SignupConfig holds the bot-protected signup settings.
//
// Signup stays gated by the PDS's own PDS_INVITE_REQUIRED, so missing config
// here means signup is closed, never that it is open and unprotected.
type SignupConfig struct {
	// TurnstileSiteKey is the public Cloudflare key embedded in the mobile
	// WebView captcha page. Empty makes that page return 503.
	TurnstileSiteKey string

	// TurnstileSecretKey verifies captcha tokens server-side.
	TurnstileSecretKey string

	// TurnstileSiteverifyURL replaces Cloudflare's siteverify endpoint. Read
	// from TURNSTILE_SITEVERIFY_URL and honoured ONLY when IS_DEV_ENV is true,
	// so a stray env var can never redirect production's captcha check at an
	// endpoint that answers "success" to everything. The hermetic CI stack sets
	// it because its Docker network is egress-blocked; empty everywhere else.
	TurnstileSiteverifyURL string
}

// MediaConfig holds how user media is served to clients.
type MediaConfig struct {
	// ImageProxy configures the resizing proxy at /img/{preset}/plain/{did}/{cid}.
	// It is parsed here rather than at the point of use so Validate can enforce
	// the production invariant below before anything starts.
	ImageProxy imageproxy.Config

	// AllowUnproxiedMedia acknowledges, for a production deployment, that
	// media will be served straight from PDS blob endpoints.
	//
	// coves.social routes media through the image proxy on a single CDN-fronted
	// hostname because that is the only surface an upstream CSAM scanner can
	// see; with the proxy off, the AppView emits com.atproto.sync.getBlob URLs
	// that bypass it entirely (and violate the site CSP). Validate therefore
	// refuses to start a production server with the proxy disabled, and this
	// flag is the deliberate opt-out — a self-hoster who is not fronting media
	// with a scanning CDN sets ALLOW_UNPROXIED_MEDIA=true rather than patching
	// the source. The point is that no one arrives there by forgetting a
	// variable.
	//
	// Taking the opt-out is not complete on its own: the shipped Caddyfile
	// pins img-src to the media hostname, so a deployment serving direct PDS
	// URLs must widen that CSP or its own web client will block every image.
	// Validate says so in the startup error rather than leaving it to be
	// discovered in a browser console.
	AllowUnproxiedMedia bool
}

// SubmissionsConfig bounds what one author may submit to one community
// (docs/PRD_AUTHOR_OWNED_POSTS.md §8).
//
// It mirrors posts.SubmissionLimits field for field rather than embedding it.
// The duplication is deliberate: this package is imported by everything that
// starts a process, and giving it a dependency on a core domain package would
// make the domain's import graph the startup path's problem. The mapping is one
// struct literal at wiring time.
//
// EVERY FIELD IS REQUIRED. There is no "unset means unlimited" reading, which
// is the whole reason these are validated at startup: a quota that evaporates
// when someone forgets an environment variable is indistinguishable, in
// production, from having no quota at all — and it fails open, silently, on the
// one path that exists to bound abuse.
type SubmissionsConfig struct {
	// MaxPerAuthorPerCommunity is how many posts one author may have admitted
	// to one community inside Window.
	MaxPerAuthorPerCommunity int

	// Window is the rolling window the quota is counted over.
	Window time.Duration

	// DedupeWindow scopes how long an identical resubmission is refused as a
	// repeat. It is separate from Window because the two answer different
	// questions: one bounds volume, the other catches retries.
	DedupeWindow time.Duration

	// AcceptanceQueueInterval is how often the acceptance engine's driver walks
	// the undecided backlog (docs/PRD_AUTHOR_OWNED_POSTS.md §5.6).
	//
	// It is the PULL side of admission. The synchronous fast path and the
	// firehose consumer both push work at the engine, and neither can see a
	// subject that was left undecided because a credential expired or a lookup
	// blipped — this pass is what eventually reaches those, so its cadence is
	// the worst-case delay before a stranded post becomes visible.
	//
	// Zero DISABLES the driver, which is a supported deployment rather than a
	// misconfiguration: an AppView that hosts no communities can accept nothing
	// and has no backlog to walk. It is the one submission setting Validate does
	// not require to be positive, for exactly that reason.
	AcceptanceQueueInterval time.Duration

	// AcceptanceQueueBatchSize bounds how many subjects one pass lists. The
	// backlog table grows with every submission the instance has ever seen, so
	// an unbounded pass would hold a transaction open across all of it and then
	// try to settle it inside a single cycle.
	//
	// Unlike the quotas above it is not validated, because it cannot fail open:
	// the backlog query substitutes its own page size for a non-positive value
	// and clamps an over-large one, so a bound exists whatever is set here.
	AcceptanceQueueBatchSize int
}

// TokenEndpointEnabled reports whether the signup-token endpoint can operate.
// It needs both the captcha secret and (from PDSConfig) an admin password to
// mint invite codes, so the caller passes the latter in.
func (s SignupConfig) TokenEndpointEnabled(pdsAdminPassword string) bool {
	return s.TurnstileSecretKey != "" && pdsAdminPassword != ""
}

// Load reads the full server configuration from the environment, applies
// defaults, and validates the result. A returned error is fatal: the process
// is misconfigured and should not start.
func Load() (*Config, error) {
	isDevEnv, err := boolVar("IS_DEV_ENV", false)
	if err != nil {
		return nil, err
	}

	cfg := &Config{IsDevEnv: isDevEnv}

	if err := cfg.loadDatabase(); err != nil {
		return nil, err
	}
	if err := cfg.loadServer(); err != nil {
		return nil, err
	}
	if err := cfg.loadIdentity(); err != nil {
		return nil, err
	}
	if err := cfg.loadOAuth(); err != nil {
		return nil, err
	}
	if err := cfg.loadInstance(); err != nil {
		return nil, err
	}
	if err := cfg.loadJetstream(); err != nil {
		return nil, err
	}
	if err := cfg.loadSubmissions(); err != nil {
		return nil, err
	}

	cfg.PDS = PDSConfig{
		URL:              stringVar("PDS_URL", "http://localhost:3001"),
		InstanceHandle:   lookup("PDS_INSTANCE_HANDLE"),
		InstancePassword: lookup("PDS_INSTANCE_PASSWORD"),
		AdminPassword:    lookup("PDS_ADMIN_PASSWORD"),
	}

	cfg.Signup = SignupConfig{
		TurnstileSiteKey:   lookup("TURNSTILE_SITE_KEY"),
		TurnstileSecretKey: lookup("TURNSTILE_SECRET_KEY"),
	}
	if isDevEnv {
		cfg.Signup.TurnstileSiteverifyURL = lookup("TURNSTILE_SITEVERIFY_URL")
	} else if override := lookup("TURNSTILE_SITEVERIFY_URL"); override != "" {
		// Dropping it silently would let an operator believe captcha
		// verification is pointed somewhere it is not. The value is still
		// ignored — that is the fail-closed behaviour — but they hear about it.
		slog.Warn("TURNSTILE_SITEVERIFY_URL ignored in non-dev env; captcha verification uses Cloudflare",
			slog.String("ignored_value", override),
			slog.String("siteverify_url", "https://challenges.cloudflare.com/turnstile/v0/siteverify"),
		)
	}

	allowUnproxiedMedia, err := boolVar("ALLOW_UNPROXIED_MEDIA", false)
	if err != nil {
		return nil, err
	}

	imageProxy := imageproxy.ConfigFromEnv()
	// IMAGE_PROXY_ENABLED gates a security invariant, so it is re-read through
	// boolVar rather than left to ConfigFromEnv's `v == "true" || v == "1"`.
	// That comparison is case-sensitive and untrimmed, so left alone
	// IMAGE_PROXY_ENABLED=TRUE — or a trailing space in a .env file — would mean
	// *disabled*: in production a refused boot whose message says the proxy is
	// off while the operator's .env plainly says it is on, and in dev every
	// image URL silently dropping back to a direct PDS blob URL. Reading it here
	// is what makes those spellings work.
	//
	// The other eight IMAGE_PROXY_* variables are still parsed by ConfigFromEnv
	// on the raw os.Getenv path, so they keep its looser behavior (untrimmed
	// strings, unparseable numerics warning and falling back to defaults).
	imageProxyEnabled, err := boolVar("IMAGE_PROXY_ENABLED", imageProxy.Enabled)
	if err != nil {
		return nil, err
	}
	imageProxy.Enabled = imageProxyEnabled

	cfg.Media = MediaConfig{
		ImageProxy:          imageProxy,
		AllowUnproxiedMedia: allowUnproxiedMedia,
	}

	cfg.CursorSecret = stringVar("CURSOR_SECRET", devCursorSecret)

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) loadDatabase() error {
	maxOpen, err := intVar("DB_MAX_OPEN_CONNS", 25)
	if err != nil {
		return err
	}
	// Default idle to open so a burst of concurrent queries reuses warm
	// connections instead of reconnecting; database/sql's default of 2 makes
	// the pool thrash under exactly the load it exists to absorb.
	maxIdle, err := intVar("DB_MAX_IDLE_CONNS", maxOpen)
	if err != nil {
		return err
	}
	connMaxLifetime, err := durationVar("DB_CONN_MAX_LIFETIME", 30*time.Minute)
	if err != nil {
		return err
	}
	connMaxIdleTime, err := durationVar("DB_CONN_MAX_IDLE_TIME", 5*time.Minute)
	if err != nil {
		return err
	}
	statementTimeout, err := durationVar("DB_STATEMENT_TIMEOUT", 30*time.Second)
	if err != nil {
		return err
	}

	c.Database = DatabaseConfig{
		URL: stringVar("DATABASE_URL",
			"postgres://dev_user:dev_password@localhost:5435/coves_dev?sslmode=disable"),
		MaxOpenConns:     maxOpen,
		MaxIdleConns:     maxIdle,
		ConnMaxLifetime:  connMaxLifetime,
		ConnMaxIdleTime:  connMaxIdleTime,
		StatementTimeout: statementTimeout,
	}
	return nil
}

func (c *Config) loadServer() error {
	readHeaderTimeout, err := durationVar("HTTP_READ_HEADER_TIMEOUT", 10*time.Second)
	if err != nil {
		return err
	}
	readTimeout, err := durationVar("HTTP_READ_TIMEOUT", 30*time.Second)
	if err != nil {
		return err
	}
	// Generous by design: the image proxy may spend its full fetch timeout
	// (30s default) pulling a source image from a remote PDS before writing
	// a single byte. The goal is a bound, not an aggressive one.
	writeTimeout, err := durationVar("HTTP_WRITE_TIMEOUT", 120*time.Second)
	if err != nil {
		return err
	}
	idleTimeout, err := durationVar("HTTP_IDLE_TIMEOUT", 120*time.Second)
	if err != nil {
		return err
	}
	shutdownTimeout, err := durationVar("HTTP_SHUTDOWN_TIMEOUT", 30*time.Second)
	if err != nil {
		return err
	}

	// PORT is what docker-compose sets; APPVIEW_PORT is the legacy name.
	port := stringVar("PORT", stringVar("APPVIEW_PORT", "8080"))

	c.Server = ServerConfig{
		Port:              port,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		ShutdownTimeout:   shutdownTimeout,
	}
	return nil
}

func (c *Config) loadIdentity() error {
	cacheTTL, err := durationVar("IDENTITY_CACHE_TTL", 0)
	if err != nil {
		return err
	}

	plcURL := stringVar("PLC_DIRECTORY_URL", "https://plc.directory")

	// In dev, identity resolution must use the same local PLC that
	// registration writes to, or end-to-end tests resolve DIDs that do not
	// exist yet. In production a separate read mirror may be configured.
	resolverPLCURL := plcURL
	if !c.IsDevEnv {
		resolverPLCURL = stringVar("IDENTITY_PLC_URL", plcURL)
	}

	wellKnownHosts, err := wellKnownHostsVar(c.IsDevEnv)
	if err != nil {
		return err
	}

	c.Identity = IdentityConfig{
		PLCURL:         plcURL,
		ResolverPLCURL: resolverPLCURL,
		CacheTTL:       cacheTTL,
		WellKnownHosts: wellKnownHosts,
	}
	return nil
}

// wellKnownHostsVar parses HANDLE_WELL_KNOWN_HOSTS: comma-separated
// <suffix>=<host:port> entries naming where handle verification should look for
// each handle suffix when DNS cannot answer.
//
// Returns nil when unset, so "not configured" contributes NOTHING rather than an
// empty map — the same shape identity.PrivateHostOptions(false) returns, and the
// shape production must have.
//
// # WHY A MALFORMED ENTRY IS FATAL RATHER THAN SKIPPED
//
// A partial parse is the dangerous outcome. The operator sees the stack
// half-work, with one namespace verifying and another silently not, and no
// reason to suspect an entry was dropped rather than a PDS being wrong. Every
// malformed shape here is a typo in a file someone edited by hand, and stopping
// the boot is the only report that reaches them.
//
// # WHY THE LEADING DOT IS REQUIRED AND NOT ADDED
//
// The suffix is matched against a hostname, so `pds2.test` also matches
// `evilpds2.test` — a different domain, somebody else's, whose handle
// verification would then be redirected to a host we chose. The dot forces the
// match onto a label boundary; it is the same rule admitCommunityOrigin applies
// to origins. It is refused rather than silently inserted because a config value
// that quietly means something other than what was typed is worse than one that
// stops the boot.
//
// # WHY IT IS AN ERROR OUTSIDE DEV
//
// Unlike TURNSTILE_SITEVERIFY_URL earlier in this file, there is no safe fallback to
// ignore this in favour of. identity.NewResolver PANICS when handed well-known
// hosts without the private-address hatch, and cmd/server passes that hatch only
// when IsDevEnv — so the combination this variable creates in production is a
// panic deep in resolver wiring, with a stack trace where an explanation should
// be. Refusing here turns it into one readable line naming the variable. A
// warning would be worse than either: the operator would be left with a resolver
// that verifies nothing in the namespace they thought they had configured.
func wellKnownHostsVar(isDevEnv bool) (map[string]string, error) {
	const key = "HANDLE_WELL_KNOWN_HOSTS"

	entries := csvVar(key)
	if len(entries) == 0 {
		return nil, nil
	}
	if !isDevEnv {
		return nil, fmt.Errorf("%s: handle verification may only be redirected in dev and CI; "+
			"set IS_DEV_ENV=true or unset %s", key, key)
	}

	hosts := make(map[string]string, len(entries))
	order := make([]string, 0, len(entries))
	for _, entry := range entries {
		rawSuffix, rawHost, found := strings.Cut(entry, "=")
		if !found {
			return nil, fmt.Errorf("%s: %q is not a <suffix>=<host:port> entry", key, entry)
		}
		suffix := strings.TrimSpace(rawSuffix)
		host := strings.TrimSpace(rawHost)
		if suffix == "" {
			return nil, fmt.Errorf("%s: %q has an empty handle suffix, which is a suffix of every "+
				"hostname and would redirect all handle verification", key, entry)
		}
		if host == "" {
			return nil, fmt.Errorf("%s: %q names no host for %q, so verification would be sent to "+
				"http:///.well-known/atproto-did", key, entry, suffix)
		}
		if !strings.HasPrefix(suffix, ".") {
			return nil, fmt.Errorf("%s: suffix %q must start with a dot, or it also matches "+
				"evil%s — a domain we do not own", key, suffix, suffix)
		}

		// The value is DIALLED, not fetched: the rewrite transport puts it
		// straight into URL.Host and supplies the scheme and the path itself. So
		// anything URL-shaped is a misunderstanding of the format that would
		// otherwise surface long after boot, as handles that quietly will not
		// verify.
		if strings.Contains(host, "://") {
			return nil, fmt.Errorf("%s: %q names a URL, but %q is dialled as a host:port — the transport "+
				"supplies the scheme, and this one would end up inside the authority", key, entry, host)
		}
		// Checked SEPARATELY from SplitHostPort, which accepts it: that function
		// splits on the last colon and hands back the port "3011/pds" without an
		// error, so a check that only called it would pass a path straight
		// through.
		if strings.Contains(host, "/") {
			return nil, fmt.Errorf("%s: %q has a path in its host, but %q is dialled as a host:port — the "+
				"transport supplies /.well-known/atproto-did itself", key, entry, host)
		}
		hostname, port, splitErr := net.SplitHostPort(host)
		if splitErr != nil || hostname == "" || port == "" {
			return nil, fmt.Errorf("%s: %q in %q is not a host:port — the transport dials exactly this "+
				"string and there is no default port to fall back on", key, host, entry)
		}
		// SplitHostPort does not look at the port beyond finding it: it returns
		// "notaport" as happily as "3011". So the only thing that would notice a
		// port that cannot be dialled is the dial itself, once per resolution,
		// reported by indigo as handle.invalid — a config typo surfacing as
		// "handles do not verify in this environment".
		//
		// 1-65535 because TCP port numbers are 16-bit and 0 is not a
		// destination: to a listener it means "any free port", and to a dialler
		// it means nothing at all.
		if portNumber, portErr := strconv.Atoi(port); portErr != nil || portNumber < 1 || portNumber > 65535 {
			return nil, fmt.Errorf("%s: %q in %q is not a port number between 1 and 65535 — the transport "+
				"dials this address, and nothing before the dial would notice", key, port, entry)
		}

		// DNS names are case-insensitive, and every consumer of this map already
		// compares against a lowered hostname — the rewrite transport lowers the
		// handle before matching, indigo normalises before checking
		// SkipDNSDomainSuffixes. A key that kept its case would match nothing at
		// all, and would do it silently, with a correct-looking value sitting in
		// the env file.
		suffix = strings.ToLower(suffix)

		if _, duplicate := hosts[suffix]; duplicate {
			return nil, fmt.Errorf("%s: suffix %q appears more than once; map assignment keeps whichever "+
				"entry was parsed last and drops the other without a word", key, suffix)
		}
		// Overlap is checked against the entries ALREADY ACCEPTED, in input
		// order, so the pair named is the same on every run — map iteration is
		// not ordered, and an error message that changes between boots is one an
		// operator cannot search for. Both suffixes are named because an overlap
		// is a relationship: with six entries configured, a message naming one of
		// them has not said which pair to fix.
		for _, seen := range order {
			if strings.HasSuffix(suffix, seen) || strings.HasSuffix(seen, suffix) {
				return nil, fmt.Errorf("%s: suffixes %q and %q overlap — a handle under the longer one "+
					"matches both, so which host answers for it turns on a precedence rule that is not "+
					"visible anywhere in this value", key, seen, suffix)
			}
		}

		hosts[suffix] = host
		order = append(order, suffix)
	}
	return hosts, nil
}

func (c *Config) loadOAuth() error {
	sealSecret := lookup("OAUTH_SEAL_SECRET")
	generated := false
	if sealSecret == "" && c.IsDevEnv {
		// Dev convenience only. Validate rejects an empty secret in
		// production, where a per-boot random key would sign every user out
		// on each deploy.
		randomBytes := make([]byte, 32)
		if _, err := rand.Read(randomBytes); err != nil {
			return fmt.Errorf("generating dev OAuth seal secret: %w", err)
		}
		sealSecret = base64.StdEncoding.EncodeToString(randomBytes)
		generated = true
	}

	c.OAuth = OAuthConfig{
		PublicURL:                 stringVar("APPVIEW_PUBLIC_URL", "http://localhost:8080"),
		SealSecret:                sealSecret,
		SealSecretGenerated:       generated,
		ClientPrivateKeyMultibase: lookup("OAUTH_CLIENT_PRIVATE_KEY"),
		ClientKeyID:               lookup("OAUTH_CLIENT_KEY_ID"),
	}
	return nil
}

func (c *Config) loadInstance() error {
	skipDIDWeb, err := boolVar("SKIP_DID_WEB_VERIFICATION", false)
	if err != nil {
		return err
	}
	bridgedVotePollInterval, err := durationVar("BRIDGED_VOTE_POLL_INTERVAL", 5*time.Minute)
	if err != nil {
		return err
	}
	bridgedVotePollLookback, err := durationVar("BRIDGED_VOTE_POLL_LOOKBACK", 0)
	if err != nil {
		return err
	}
	bridgedVotePollSweepCap, err := intVar("BRIDGED_VOTE_POLL_SWEEP_CAP", 0)
	if err != nil {
		return err
	}

	did := stringVar("INSTANCE_DID", "did:web:coves.social")

	// For did:web the DID *is* the domain claim, so deriving the domain from
	// it keeps the two from drifting apart. Only non-web DIDs (did:plc) need
	// INSTANCE_DOMAIN, and then it is required.
	var domain string
	if suffix, ok := strings.CutPrefix(did, "did:web:"); ok {
		domain = suffix
	} else {
		domain = lookup("INSTANCE_DOMAIN")
	}

	c.Instance = InstanceConfig{
		DID:                      did,
		Domain:                   domain,
		AllowedCommunityCreators: csvVar("COMMUNITY_CREATORS"),
		TrustedBridgePDSHosts:    csvVar("TRUSTED_BRIDGE_PDS_HOSTS"),
		SkipDIDWebVerification:   skipDIDWeb,
		BridgedVotePollInterval:  bridgedVotePollInterval,
		BridgedVotePollLookback:  bridgedVotePollLookback,
		BridgedVotePollSweepCap:  bridgedVotePollSweepCap,
	}
	return nil
}

func (c *Config) loadJetstream() error {
	for _, legacy := range legacyJetstreamVars {
		if lookup(legacy) != "" {
			return fmt.Errorf("%s is no longer supported: configure feeds via JETSTREAM_FEEDS "+
				"(e.g. \"bsky=wss://jetstream2.us-east.bsky.network;self=ws://tidepool-prod-jetstream:8080\") "+
				"and remove the legacy variable", legacy)
		}
	}

	feedsSpec := lookup("JETSTREAM_FEEDS")
	if feedsSpec == "" && c.IsDevEnv {
		// Dev default: the local dev-stack Jetstream only. Production must
		// always be explicit — see Validate.
		feedsSpec = "self=ws://localhost:6008"
	}

	redriveInterval, err := durationVar("REDRIVE_INTERVAL", defaultRedriveInterval)
	if err != nil {
		return err
	}
	// Rejected HERE rather than in Validate — the only part of this rule that is
	// about placement; why zero is rejected at all is on the
	// JetstreamConfig.RedriveInterval field doc. Validate reads a Config that
	// callers also assemble by hand, where an unset field means "this test is not
	// about the redriver"; only a value that came from the environment can be
	// held to this.
	if redriveInterval <= 0 {
		return fmt.Errorf("REDRIVE_INTERVAL must be greater than 0 (got %s); "+
			"the dead letter redriver cannot be disabled", redriveInterval)
	}

	c.Jetstream = JetstreamConfig{
		FeedsSpec:       feedsSpec,
		RedriveInterval: redriveInterval,
	}
	return nil
}

// Default submission quotas. They apply in every environment, dev and
// production alike, because the alternative — requiring the variables in
// production — makes an omission fail closed on the wrong side: the process
// refuses to boot over an abuse limit rather than running with a conservative
// one. The bounds are deliberately generous enough that no ordinary author
// meets them and tight enough to make scripted flooding expensive, and every
// instance can move them.
const (
	defaultMaxSubmissionsPerCommunity = 10
	defaultSubmissionWindow           = time.Hour
	defaultSubmissionDedupeWindow     = time.Hour

	// defaultAcceptanceQueueInterval is the backlog pass's cadence. A minute is
	// the compromise the two failure modes point at from opposite directions:
	// the pass is the only thing that reaches a subject nothing else will
	// retry, so a long interval is a long wait for an author whose post got
	// stuck, while a short one repeatedly scans a backlog that is usually empty.
	defaultAcceptanceQueueInterval = time.Minute

	// defaultAcceptanceQueueBatch is how many subjects one pass takes. Small
	// enough that a pass fits comfortably inside its own interval even when
	// every subject needs a PDS round trip, since the backlog is drained across
	// passes rather than in one.
	defaultAcceptanceQueueBatch = 50
)

func (c *Config) loadSubmissions() error {
	maxPerCommunity, err := intVar("POST_SUBMISSIONS_MAX_PER_COMMUNITY", defaultMaxSubmissionsPerCommunity)
	if err != nil {
		return err
	}
	window, err := durationVar("POST_SUBMISSIONS_WINDOW", defaultSubmissionWindow)
	if err != nil {
		return err
	}
	dedupeWindow, err := durationVar("POST_SUBMISSIONS_DEDUPE_WINDOW", defaultSubmissionDedupeWindow)
	if err != nil {
		return err
	}

	queueInterval, err := durationVar("ACCEPTANCE_QUEUE_INTERVAL", defaultAcceptanceQueueInterval)
	if err != nil {
		return err
	}
	queueBatch, err := intVar("ACCEPTANCE_QUEUE_BATCH_SIZE", defaultAcceptanceQueueBatch)
	if err != nil {
		return err
	}

	c.Submissions = SubmissionsConfig{
		MaxPerAuthorPerCommunity: maxPerCommunity,
		Window:                   window,
		DedupeWindow:             dedupeWindow,
		AcceptanceQueueInterval:  queueInterval,
		AcceptanceQueueBatchSize: queueBatch,
	}
	return nil
}

// Validate enforces the constraints that Load's defaults cannot express,
// notably the ones that differ between dev and production. It returns every
// problem at once so a misconfigured deployment can be fixed in a single pass
// instead of one restart per mistake.
func (c *Config) Validate() error {
	var problems []string

	if c.Database.URL == "" {
		problems = append(problems, "DATABASE_URL is required")
	}
	if c.Database.MaxOpenConns == 0 {
		problems = append(problems, "DB_MAX_OPEN_CONNS must be greater than 0 "+
			"(an unbounded pool can exhaust PostgreSQL's max_connections)")
	}
	if c.Database.MaxIdleConns > c.Database.MaxOpenConns {
		problems = append(problems, fmt.Sprintf(
			"DB_MAX_IDLE_CONNS (%d) must not exceed DB_MAX_OPEN_CONNS (%d)",
			c.Database.MaxIdleConns, c.Database.MaxOpenConns))
	}
	if c.Server.Port == "" {
		problems = append(problems, "PORT must not be empty")
	}

	// Every listener timeout must be positive. net/http reads zero as "no
	// deadline", so an explicit HTTP_READ_TIMEOUT=0s silently restores the
	// unbounded-connection exposure these settings exist to close — and
	// HTTP_SHUTDOWN_TIMEOUT=0s yields an already-expired context, so nothing
	// ever drains. Checking only ReadHeaderTimeout left four ways back in.
	for _, timeout := range []struct {
		name  string
		value time.Duration
		why   string
	}{
		{
			"HTTP_READ_HEADER_TIMEOUT", c.Server.ReadHeaderTimeout,
			"zero leaves the server open to slowloris connections",
		},
		{
			"HTTP_READ_TIMEOUT", c.Server.ReadTimeout,
			"zero lets a client hold a connection open indefinitely while sending a body",
		},
		{
			"HTTP_WRITE_TIMEOUT", c.Server.WriteTimeout,
			"zero lets a slow consumer hold a response open indefinitely",
		},
		{
			"HTTP_IDLE_TIMEOUT", c.Server.IdleTimeout,
			"zero lets idle keep-alive connections accumulate without bound",
		},
		{
			"HTTP_SHUTDOWN_TIMEOUT", c.Server.ShutdownTimeout,
			"zero expires the shutdown deadline immediately, so nothing drains and " +
				"Jetstream cursors are never flushed",
		},
	} {
		if timeout.value <= 0 {
			problems = append(problems, fmt.Sprintf("%s must be greater than 0 (%s)",
				timeout.name, timeout.why))
		}
	}

	// A ReadTimeout below ReadHeaderTimeout makes the latter unreachable: the
	// whole-request deadline fires first, so the slowloris guard never applies.
	if c.Server.ReadTimeout > 0 && c.Server.ReadHeaderTimeout > c.Server.ReadTimeout {
		problems = append(problems, fmt.Sprintf(
			"HTTP_READ_HEADER_TIMEOUT (%s) must not exceed HTTP_READ_TIMEOUT (%s)",
			c.Server.ReadHeaderTimeout, c.Server.ReadTimeout))
	}

	if c.Instance.Domain == "" {
		problems = append(problems,
			"INSTANCE_DOMAIN is required when INSTANCE_DID is not a did:web DID")
	}
	if !strings.HasPrefix(c.Instance.DID, "did:") {
		// This becomes the audience every aggregator service JWT is validated
		// against, so a non-DID value fails every aggregator request at
		// runtime rather than at startup.
		problems = append(problems, fmt.Sprintf(
			"INSTANCE_DID must be a DID (got %q)", c.Instance.DID))
	}

	// The submission quotas, in every environment. §8's limits exist because
	// anyone can write unlimited records naming any community, so "unset" must
	// not be readable as "unlimited" — and a zero is worse than unlimited: a
	// limit check written as `count > limit` refuses every post, one written the
	// other way admits every post, and either way the behaviour was decided by
	// an omission. Load supplies defaults, so reaching any of these means an
	// operator set the variable to something that is not a quota.
	if c.Submissions.MaxPerAuthorPerCommunity <= 0 {
		problems = append(problems, fmt.Sprintf(
			"POST_SUBMISSIONS_MAX_PER_COMMUNITY must be greater than 0 (got %d); "+
				"a non-positive per-author quota disables or inverts the abuse limit rather than relaxing it",
			c.Submissions.MaxPerAuthorPerCommunity))
	}
	if c.Submissions.Window <= 0 {
		problems = append(problems, fmt.Sprintf(
			"POST_SUBMISSIONS_WINDOW must be greater than 0 (got %s); "+
				"the quota is counted over a rolling window, and a zero-width one counts nothing",
			c.Submissions.Window))
	}
	if c.Submissions.DedupeWindow <= 0 {
		problems = append(problems, fmt.Sprintf(
			"POST_SUBMISSIONS_DEDUPE_WINDOW must be greater than 0 (got %s); "+
				"it scopes the ledger's uniqueness bucket, and without a width every repost collides with the original forever",
			c.Submissions.DedupeWindow))
	}
	// The interval is checked for being NEGATIVE rather than non-positive,
	// unlike the three above: zero is the documented way to disable the driver
	// on an instance that hosts no communities, while a negative one is a
	// time.Ticker panic waiting for the first boot after a typo.
	if c.Submissions.AcceptanceQueueInterval < 0 {
		problems = append(problems, fmt.Sprintf(
			"ACCEPTANCE_QUEUE_INTERVAL cannot be negative (got %s); use 0 to disable the acceptance queue driver",
			c.Submissions.AcceptanceQueueInterval))
	}
	// The bridged-vote poller. Trusted hosts are validated whether or not the
	// poller will run, because the same list is BridgeTrust's provenance gate
	// for record-stamped bridgedStats: a value BridgeTrust would tolerate but
	// the poller would refuse must fail here, where it reads as a config
	// error, not inside wiring where it reads as a crash.
	for _, host := range c.Instance.TrustedBridgePDSHosts {
		if _, err := bridgedvotes.ParseTrustedHost(host); err != nil {
			problems = append(problems, fmt.Sprintf(
				"TRUSTED_BRIDGE_PDS_HOSTS: %v (scheme + host only, e.g. https://tdpl.io)", err))
		}
	}
	// The interval is held to its rule only when the poller will actually run.
	// A hand-assembled Config with no trust list has no poller to misconfigure,
	// and Load always supplies the 5-minute default, so reaching this with a
	// trust list set means an operator wrote a value that is not an interval.
	if len(c.Instance.TrustedBridgePDSHosts) > 0 && c.Instance.BridgedVotePollInterval <= 0 {
		problems = append(problems, fmt.Sprintf(
			"BRIDGED_VOTE_POLL_INTERVAL must be greater than 0 (got %s); "+
				"the poller has no disabled state — leave TRUSTED_BRIDGE_PDS_HOSTS unset to run without it",
			c.Instance.BridgedVotePollInterval))
	}
	if c.Instance.BridgedVotePollLookback < 0 {
		problems = append(problems, fmt.Sprintf(
			"BRIDGED_VOTE_POLL_LOOKBACK cannot be negative (got %s); use 0 for the poller's default",
			c.Instance.BridgedVotePollLookback))
	}
	if c.Instance.BridgedVotePollSweepCap < 0 {
		problems = append(problems, fmt.Sprintf(
			"BRIDGED_VOTE_POLL_SWEEP_CAP cannot be negative (got %d); use 0 for the poller's default",
			c.Instance.BridgedVotePollSweepCap))
	}
	// REDRIVE_INTERVAL is checked in loadJetstream instead of here; see the note
	// there for why the environment's value and a hand-assembled Config cannot be
	// held to the same rule.
	// The batch size is deliberately NOT validated, unlike every quota above.
	// The quotas fail open when unset — an absent limit is no limit — so an
	// omission there has to stop the boot. A batch size does not: the backlog
	// query substitutes its own page for a non-positive limit and clamps an
	// over-large one, so the bound exists whatever this value is, and the worst
	// an omission costs is a different page size.

	if !c.IsDevEnv {
		switch {
		case c.OAuth.SealSecret == "":
			problems = append(problems, "OAUTH_SEAL_SECRET is required in production")
		case isPlaceholder(c.OAuth.SealSecret):
			problems = append(problems, "OAUTH_SEAL_SECRET is still set to a documented "+
				"placeholder value; generate one with: openssl rand -base64 32")
		default:
			// Checked here rather than left to oauth.NewOAuthClient, which
			// runs after schema migrations have already been applied. A
			// config error should stop the process before it changes
			// anything.
			decoded, err := base64.StdEncoding.DecodeString(c.OAuth.SealSecret)
			if err != nil {
				problems = append(problems, "OAUTH_SEAL_SECRET must be base64: "+err.Error())
			} else if len(decoded) != sealSecretBytes {
				problems = append(problems, fmt.Sprintf(
					"OAUTH_SEAL_SECRET must decode to %d bytes, got %d; "+
						"generate one with: openssl rand -base64 %d",
					sealSecretBytes, len(decoded), sealSecretBytes))
			}
		}

		switch {
		case c.CursorSecret == devCursorSecret:
			problems = append(problems, "CURSOR_SECRET is required in production "+
				"(the dev placeholder is public, so anyone could forge pagination cursors)")
		case isPlaceholder(c.CursorSecret):
			// The shipped .env.prod.example carries CHANGE_ME_CURSOR_SECRET,
			// which is every bit as public as the dev constant. Rejecting
			// only the latter left the documented placeholder usable.
			problems = append(problems, "CURSOR_SECRET is still set to a documented "+
				"placeholder value; generate one with: openssl rand -base64 32")
		case len(c.CursorSecret) < minSecretLength:
			problems = append(problems, fmt.Sprintf(
				"CURSOR_SECRET must be at least %d characters to be a usable HMAC key",
				minSecretLength))
		}

		if c.Jetstream.FeedsSpec == "" {
			problems = append(problems, "JETSTREAM_FEEDS is required in production "+
				"(the localhost default is dev-only): set semicolon-separated <feedKey>=<baseURL> "+
				"entries, e.g. \"bsky=wss://jetstream2.us-east.bsky.network;self=ws://tidepool-prod-jetstream:8080\"")
		}
		if c.Instance.SkipDIDWebVerification {
			problems = append(problems, "SKIP_DID_WEB_VERIFICATION must not be enabled in production "+
				"(it lets a community claim a hostedBy domain it does not control)")
		}

		// The localhost defaults exist for dev. Reaching production with one
		// still in place used to boot cleanly and then fail at first use, in
		// somebody else's logs: an unset APPVIEW_PUBLIC_URL puts
		// http://localhost:8080 into the OAuth client metadata and redirect
		// URI, so every login is rejected by the authorization server.
		if err := requirePublicHost("APPVIEW_PUBLIC_URL", c.OAuth.PublicURL); err != nil {
			problems = append(problems, err.Error())
		}
		if err := requirePublicHost("PDS_URL", c.PDS.URL); err != nil {
			problems = append(problems, err.Error())
		}

		problems = append(problems, c.mediaProblems()...)
	}

	if len(problems) == 0 {
		return nil
	}
	return errors.New("invalid configuration:\n  - " + strings.Join(problems, "\n  - "))
}

// mediaProblems enforces the production media-serving invariant: every image
// URL the AppView hands a client must point at the image proxy.
//
// The proxy is the choke point that makes upstream CSAM scanning possible —
// one hostname, CDN-fronted, through which all media flows. Disabling it does
// not break anything visibly; URL generation quietly falls back to direct
// com.atproto.sync.getBlob URLs and images keep rendering, so the failure mode
// is a production deployment that looks healthy while serving unscanned media
// past its own CSP. That is worth refusing to start over.
//
// Only called for non-dev configurations. Operators who genuinely intend to
// serve media straight from PDS blob endpoints set ALLOW_UNPROXIED_MEDIA=true.
func (c *Config) mediaProblems() []string {
	var problems []string

	if !c.Media.ImageProxy.Enabled {
		if !c.Media.AllowUnproxiedMedia {
			problems = append(problems, "IMAGE_PROXY_ENABLED must be true in production: "+
				"with the proxy off the AppView emits direct PDS com.atproto.sync.getBlob URLs, "+
				"which bypass the CDN that scans served media and are blocked by the site CSP. "+
				"To serve media directly on purpose (self-hosting without a scanning CDN), "+
				"set ALLOW_UNPROXIED_MEDIA=true — and widen the Caddyfile img-src, which the "+
				"shipped policy pins to the media hostname")
		} else {
			// Not a problem — the operator asked for this — but the CSP half of
			// it is easy to miss, and the symptom (every image blocked, only in
			// the browser console) points nowhere near this setting.
			slog.Warn("[MEDIA] serving unproxied media: image URLs will address PDS blob endpoints directly",
				"reason", "ALLOW_UNPROXIED_MEDIA=true",
				"action_required", "widen the Caddyfile Content-Security-Policy img-src to cover your PDS hosts, "+
					"or the web client will block every image",
			)
		}
		return problems
	}

	// With the proxy on, a relative URL ("/img/...") is what an empty base URL
	// produces. That resolves correctly for a browser on the AppView origin and
	// not at all for the mobile app or any other cross-origin consumer, both of
	// which receive these URLs verbatim.
	baseURL := c.Media.ImageProxy.CDNURL
	name := "IMAGE_PROXY_CDN_URL"
	if baseURL == "" {
		baseURL = c.Media.ImageProxy.BaseURL
		name = "IMAGE_PROXY_BASE_URL"
	}
	if baseURL == "" {
		problems = append(problems, "IMAGE_PROXY_BASE_URL (or IMAGE_PROXY_CDN_URL) is required "+
			"in production: without one the AppView emits relative image URLs, which "+
			"non-browser clients cannot resolve")
		return problems
	}
	if err := requirePublicHost(name, baseURL); err != nil {
		problems = append(problems, err.Error())
	}

	if err := c.Media.ImageProxy.Validate(); err != nil {
		problems = append(problems, "image proxy configuration: "+err.Error())
	}

	return problems
}
