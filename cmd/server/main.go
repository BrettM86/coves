// Command server runs the Coves AppView: the HTTP/XRPC API, the Jetstream
// consumers that index the atProto firehose into PostgreSQL, and the
// background jobs that keep OAuth sessions and aggregator tokens fresh.
package main

import (
	"Coves/internal/atproto/oauth"
	"Coves/internal/config"
	"Coves/internal/core/users"
	"Coves/internal/observability"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	_ "github.com/lib/pq"
)

// Compile-time interface satisfaction checks
var _ oauth.UserIndexer = (users.UserService)(nil)

func main() {
	if err := run(); err != nil {
		slog.Error("server failed to start", "error", err)
		os.Exit(1)
	}
}

// run performs startup, serves until a shutdown signal arrives, and then
// drains cleanly.
//
// Returning an error rather than calling log.Fatal is what makes the deferred
// cleanup below reachable: os.Exit skips deferred functions, so a fatal call
// partway through startup would abandon the database pool and discard buffered
// OpenTelemetry spans — including the ones describing the failure.
func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logStartupWarnings(cfg)

	// Signal handling is installed first so a Ctrl-C during the slower parts
	// of startup (PDS login, migrations) still shuts down in an orderly way.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := openDatabase(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			slog.Error("failed to close database pool", "error", closeErr)
		}
	}()

	otelProvider, err := startObservability(ctx)
	if err != nil {
		return err
	}
	defer func() {
		// A fresh context: ctx may already be cancelled by the time this
		// runs, and a cancelled context would discard the spans instead of
		// flushing them.
		if shutdownErr := otelProvider.Shutdown(context.Background()); shutdownErr != nil {
			slog.Error("failed to shut down OpenTelemetry", "error", shutdownErr)
		}
	}()

	app, err := buildApplication(ctx, cfg, db)
	if err != nil {
		return err
	}
	defer app.Close()

	// Background work runs on its own context so shutdown can stop producing
	// new work before it starts draining in-flight requests.
	backgroundCtx, stopBackground := context.WithCancel(context.Background())
	defer stopBackground()
	var backgroundWG sync.WaitGroup

	// Resolved here, once, so a store that cannot be unwrapped stops the boot
	// instead of turning the cleanup job into a silent hourly no-op.
	sessionStore := app.oauthStore.UnwrapPostgresStore()
	if sessionStore == nil {
		return errors.New("OAuth store does not expose a PostgreSQL store: " +
			"expired sessions and auth requests could never be cleaned up")
	}
	startOAuthCleanupJob(backgroundCtx, &backgroundWG, sessionStore)
	startAggregatorTokenRefreshJob(backgroundCtx, &backgroundWG, app.apiKeyService)

	consumers, err := startConsumers(backgroundCtx, &backgroundWG, app)
	if err != nil {
		// Some connectors may already be running. Drain them under the same
		// bounded wait the normal shutdown path uses — an unbounded Wait here
		// would hang the boot with no further output if one is wedged.
		drainBackground(cfg.Server.ShutdownTimeout, stopBackground, &backgroundWG)
		return err
	}

	router := newRouter(observability.HTTPMiddleware(otelProvider))
	registerRoutes(router, app, consumers)

	return serve(ctx, cfg, app, router, stopBackground, &backgroundWG)
}

// drainBackground stops background work and waits for it to finish, bounded by
// timeout. It reports whether everything drained in time.
//
// The bound matters on every path: a consumer wedged against a dead database
// must not hang the process, and an unbounded Wait would do exactly that with
// no further log output to explain it.
func drainBackground(timeout time.Duration, stopBackground context.CancelFunc, backgroundWG *sync.WaitGroup) bool {
	// Cancelling unblocks the Jetstream read loops and flushes their cursors,
	// so the next boot resumes from the last processed event (minus a small
	// deliberate replay rewind that idempotent handlers absorb).
	stopBackground()

	drained := make(chan struct{})
	go func() {
		backgroundWG.Wait()
		close(drained)
	}()

	select {
	case <-drained:
		slog.Info("background jobs drained and Jetstream cursors flushed")
		return true
	case <-time.After(timeout):
		slog.Warn("timed out draining background jobs; Jetstream cursors may not have been flushed",
			"timeout", timeout)
		return false
	}
}

// serve runs the HTTP listener until ctx is cancelled, then shuts everything
// down within cfg.Server.ShutdownTimeout.
func serve(
	ctx context.Context,
	cfg *config.Config,
	app *application,
	handler http.Handler,
	stopBackground context.CancelFunc,
	backgroundWG *sync.WaitGroup,
) error {
	server := newHTTPServer(cfg.Server, handler)

	// Buffered so a listener that fails after shutdown has begun does not
	// leak this goroutine on an unread channel.
	listenErr := make(chan error, 1)
	go func() {
		slog.Info("Coves AppView listening",
			"port", cfg.Server.Port,
			"pds", cfg.PDS.URL,
			"instance_did", cfg.Instance.DID,
		)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			listenErr <- err
			return
		}
		listenErr <- nil
	}()

	var listenFailure error
	select {
	case err := <-listenErr:
		if err != nil {
			// Still fall through to the drain: consumers are already running
			// and hold cursors that must be flushed before run's deferred
			// db.Close fires.
			listenFailure = fmt.Errorf("HTTP server: %w", err)
			slog.Error("HTTP listener failed; draining background work", "error", err)
		}
	case <-ctx.Done():
		slog.Info("shutdown signal received")
	}

	// Drain background work concurrently with the listener drain, not after
	// it. They are independent — background work runs on its own context,
	// while in-flight requests carry contexts owned by the server — so
	// serialising them would make one shared deadline cover two waits, and a
	// slow request (WriteTimeout allows 120s) could consume the entire budget
	// before consumers were even told to stop, leaving cursors unflushed.
	drainResult := make(chan bool, 1)
	go func() {
		drainResult <- drainBackground(cfg.Server.ShutdownTimeout, stopBackground, backgroundWG)
	}()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()

	var shutdownErr error
	if listenFailure == nil {
		if err := server.Shutdown(shutdownCtx); err != nil {
			// Deliberately not returned early: that would abandon the drain,
			// and a lost cursor flush costs more than a few abandoned
			// in-flight requests.
			shutdownErr = fmt.Errorf("HTTP server shutdown: %w", err)
			slog.Error("HTTP server shutdown error", "error", err)
		}
	}

	drained := <-drainResult

	// Stop the image proxy cleanup job here rather than leaving it to run's
	// deferred Close, so it does not outlive the drain. Close is idempotent,
	// so the deferred call is a no-op.
	app.Close()

	if !drained {
		shutdownErr = errors.Join(shutdownErr,
			fmt.Errorf("background jobs did not drain within %s", cfg.Server.ShutdownTimeout))
	}

	// Report shutdown problems through the exit code. Logging them and
	// returning nil made an abandoned drain look like a clean stop to
	// Docker and systemd, which is exactly the signal an operator needs.
	if err := errors.Join(listenFailure, shutdownErr); err != nil {
		return err
	}

	slog.Info("server stopped gracefully")
	return nil
}

// startObservability initializes optional OpenTelemetry tracing. Tracing being
// disabled is the normal case and returns a working no-op provider.
func startObservability(ctx context.Context) (*observability.Provider, error) {
	otelConfig := observability.ConfigFromEnv()
	if err := otelConfig.Validate(); err != nil {
		return nil, fmt.Errorf("invalid OpenTelemetry configuration: %w", err)
	}

	provider, err := observability.NewProvider(ctx, otelConfig)
	if err != nil {
		return nil, fmt.Errorf("initializing OpenTelemetry: %w", err)
	}
	if otelConfig.Enabled {
		slog.Info("OpenTelemetry tracing enabled", "endpoint", otelConfig.Endpoint)
	}
	return provider, nil
}

// logStartupWarnings reports configuration that is valid but degraded, so an
// operator sees why a feature is unavailable instead of discovering it through
// a 503 later.
func logStartupWarnings(cfg *config.Config) {
	if cfg.IsDevEnv {
		slog.Warn("running in DEV mode: production safety checks are relaxed")
	}

	if cfg.OAuth.SealSecretGenerated {
		slog.Warn("OAUTH_SEAL_SECRET is unset: generated a random seal secret, " +
			"so every restart signs out all users")
	}

	// Signup stays gated by the PDS's own PDS_INVITE_REQUIRED, so missing
	// config here closes signup rather than leaving it unprotected.
	if !cfg.Signup.TokenEndpointEnabled(cfg.PDS.AdminPassword) {
		slog.Warn("signup-token endpoint DISABLED: new signups are blocked",
			"turnstile_secret_set", cfg.Signup.TurnstileSecretKey != "",
			"pds_admin_password_set", cfg.PDS.AdminPassword != "",
		)
	}
	if cfg.Signup.TurnstileSiteKey == "" {
		slog.Warn("TURNSTILE_SITE_KEY unset: /m/turnstile.html is disabled and mobile signup " +
			"will fail at the captcha step")
	}

	if len(cfg.Instance.AllowedCommunityCreators) > 0 {
		slog.Info("community creation restricted to an allowlist",
			"allowed_dids", len(cfg.Instance.AllowedCommunityCreators))
	} else {
		slog.Info("community creation is open to all authenticated users")
	}

	slog.Info("instance identity resolved",
		"instance_did", cfg.Instance.DID,
		"instance_domain", cfg.Instance.Domain,
	)
}
