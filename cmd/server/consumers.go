package main

import (
	"Coves/internal/atproto/jetstream"
	postgresRepo "Coves/internal/db/postgres"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
)

// consumerSet is the collection of Jetstream consumers started for this
// process, kept so /health/consumers can report on them and so shutdown can
// drain them.
type consumerSet struct {
	connectors []*jetstream.Connector
}

// feedConsumer pairs a consumer's stable name with its handler. The name keys
// the consumer's persisted cursor and dead letter rows, so it must not change
// between releases.
type feedConsumer struct {
	name    string
	handler jetstream.EventHandler
}

// startConsumers wires every Jetstream consumer, validates the feed topology,
// and starts one connector per (feed, consumer) pair plus the dead letter
// redriver.
//
// All consumers share ctx so a single cancellation drains them: read loops
// unblock, an interrupted in-flight event is abandoned without advancing its
// cursor (it replays idempotently on the next boot), and the final cursor is
// flushed.
func startConsumers(ctx context.Context, wg *sync.WaitGroup, app *application) (*consumerSet, error) {
	feeds, err := jetstream.ParseFeeds(app.cfg.Jetstream.FeedsSpec)
	if err != nil {
		return nil, fmt.Errorf("invalid JETSTREAM_FEEDS: %w", err)
	}
	warnIfNoPrimaryFeed(feeds)

	consumers := app.registerFeedConsumers()

	// FAIL CLOSED: with more than one feed, every consumer must be
	// rev-gated. An ungated consumer would apply the lagging feed's stale
	// copies — zombie deletes, regressed edits — which is silent data
	// corruption, not a degraded mode. A forgotten WithXRevGate option must
	// stop the boot, not ship the bug.
	if len(feeds) > 1 {
		for _, consumer := range consumers {
			gated, ok := consumer.handler.(interface{ RevGated() bool })
			if !ok || !gated.RevGated() {
				return nil, fmt.Errorf("consumer %q is not rev-gated but %d Jetstream feeds are configured; "+
					"multi-feed operation requires every consumer to carry the rev gate (see rev_gate.go)",
					consumer.name, len(feeds))
			}
		}
	}

	set := &consumerSet{}
	handlers := make(map[string]jetstream.EventHandler, len(consumers)*len(feeds))

	// Consumer names on the primary ("bsky") feed stay bare so live cursors
	// carry over from the single-feed era; other feeds get "<consumer>@<feed>"
	// names, which start cursor-less and live-tail. Rev-gating makes the
	// cross-feed overlap safe — expect "rev-gate: skipped stale" lines for the
	// lagging feed's copies. That is the system working, not an error.
	for _, feed := range feeds {
		for _, consumer := range consumers {
			collections, err := jetstream.WantedCollections(consumer.name)
			if err != nil {
				return nil, fmt.Errorf("resolving wantedCollections for consumer %s: %w", consumer.name, err)
			}
			wsURL, err := jetstream.SubscribeURL(feed.BaseURL, collections)
			if err != nil {
				return nil, fmt.Errorf("building Jetstream URL for consumer %s on feed %s: %w",
					consumer.name, feed.Key, err)
			}

			name := jetstream.FeedConsumerName(consumer.name, feed.Key)
			handlers[name] = consumer.handler
			set.start(ctx, wg, app, name, wsURL, consumer.handler)
		}
		slog.Info("started Jetstream consumers on feed",
			"consumers", len(consumers), "feed", feed.Key, "url", feed.BaseURL)
	}

	// The redriver replays events that failed every in-line retry against the
	// same handlers, so a transient failure (a Postgres blip, say) self-heals
	// instead of silently losing the event.
	redriver := jetstream.NewDeadLetterRedriver(app.jetstreamState, handlers,
		jetstream.WithRedriveInterval(app.cfg.Jetstream.RedriveInterval))
	wg.Add(1)
	go func() {
		defer wg.Done()
		redriver.Run(ctx)
	}()
	slog.Info("started Jetstream dead letter redriver")

	return set, nil
}

// start launches one connector with cursor persistence, retry plus dead
// letter, and graceful shutdown.
func (s *consumerSet) start(ctx context.Context, wg *sync.WaitGroup, app *application, name, wsURL string, handler jetstream.EventHandler) {
	connector := jetstream.NewConnector(name, wsURL, handler,
		jetstream.WithCursorStore(app.jetstreamState),
		jetstream.WithDeadLetterWriter(app.jetstreamState),
	)
	s.connectors = append(s.connectors, connector)

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := connector.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("Jetstream consumer stopped", "consumer", name, "error", err)
		}
	}()
}

// registerFeedConsumers builds every consumer, in a fixed order. Each runs
// once per configured feed, with its collection filters appended by
// jetstream.WantedCollections.
func (a *application) registerFeedConsumers() []feedConsumer {
	var consumers []feedConsumer

	// Users: actor profiles and actor blocks.
	//
	// A trusted bridge hosts many virtual repos, and its profile records may
	// be the first time Coves sees those identities. Relay scheduling can
	// deliver profiles and posts in either order, so the user, post, and
	// comment consumers share one provenance gate.
	userOpts := []jetstream.ConsumerOption{
		jetstream.WithUserBridgeTrust(a.bridgeTrust),
		jetstream.WithUserRevGate(a.revGate),
		jetstream.WithUserBlockRepo(a.userBlockRepo),
	}
	// Statically typed rather than asserted at runtime: a type assertion here
	// would silently disable handle sync if the store's method set ever
	// drifted, leaving OAuth sessions pinned to stale handles with no error.
	// The compiler now catches that instead.
	if sessionUpdater := a.oauthStore.UnwrapPostgresStore(); sessionUpdater != nil {
		userOpts = append(userOpts, jetstream.WithSessionHandleUpdater(sessionUpdater))
		slog.Info("OAuth session handle sync enabled for identity changes")
	}
	consumers = append(consumers, feedConsumer{
		name:    jetstream.ConsumerUsers,
		handler: jetstream.NewUserEventConsumer(a.userService, a.identityResolver, userOpts...),
	})

	// Communities: profiles (in the community's own repo), plus subscriptions
	// and community blocks (in the subscribing user's repo). The identity
	// resolver supplies PLC handle resolution, which is the source of truth.
	if a.cfg.Instance.SkipDIDWebVerification {
		slog.Warn("did:web domain verification is DISABLED; this must never be set in production")
	}
	// The SSRF hatch is open only in dev, where a developer's own instance is
	// what the .well-known DID document is fetched from. In production that
	// fetch dials whatever domain a community record published by anyone on the
	// federated network names, from inside this AppView's own network.
	communityOpts := append(
		[]jetstream.CommunityConsumerOption{jetstream.WithCommunityRevGate(a.revGate)},
		jetstream.PrivateHostOptions(a.allowPrivateHosts())...)
	consumers = append(consumers, feedConsumer{
		name: jetstream.ConsumerCommunities,
		handler: jetstream.NewCommunityEventConsumer(
			a.communityRepo, a.cfg.Instance.DID, a.cfg.Instance.SkipDIDWebVerification,
			a.identityResolver, communityOpts...),
	})

	// Posts, in both shapes, plus the community records that decide about
	// them: the deprecated community-repo post, the author-repo postv2, and
	// the acceptance/removal pair. One consumer, because they write the same
	// admission row and an acceptance is meaningless without the post it pins.
	//
	// The direct fetcher is what makes acceptance-before-post converge without
	// full relay coverage (PRD §5.4). It dials a PDS named by a DID document
	// anyone can publish, so its SSRF guard stays on in production and the hatch
	// opens only under IS_DEV_ENV — where the hermetic stack's PDS is a private
	// address the guard would otherwise refuse.
	//
	// The decision goes through jetstream.PrivatePostFetcherOptions rather than
	// the `if` that used to stand here, for the same reason the community
	// consumer's gate does nineteen lines above: `.env.ci:140` sets
	// IS_DEV_ENV=true, so `make ci` takes the permissive branch and an inline
	// conditional in wiring is reachable only by standing up this wiring with a
	// production config, which nothing in this tree does. As a pure function the
	// branch production actually runs is testable in T0.
	//
	// The warning stays here, because it is about this process rather than about
	// the option — a helper that logged would fire once per test that builds a
	// hatched fetcher, and this line has to mean "this server is running
	// unguarded".
	if a.allowPrivateHosts() {
		slog.Warn("direct post fetch has SSRF protection DISABLED (IS_DEV_ENV); this must never be set in production")
	}
	postFetcher := jetstream.NewDirectPostFetcher(a.identityResolver,
		jetstream.PrivatePostFetcherOptions(a.allowPrivateHosts())...)
	consumers = append(consumers, feedConsumer{
		name: jetstream.ConsumerPosts,
		handler: jetstream.NewPostEventConsumer(a.postRepo, a.communityRepo, a.userService, a.db,
			jetstream.WithPostBridgeTrust(a.bridgeTrust),
			jetstream.WithPostIdentityResolver(a.identityResolver),
			jetstream.WithAdmissions(a.admissionRepo),
			jetstream.WithDeletedAccounts(postgresRepo.NewDeletedAccountRepository(a.db)),
			jetstream.WithPostRecordFetcher(postFetcher),
			// The host-side half of an author's own deletion (§5.3): when the
			// author tombstones a post this instance's community accepted, the
			// acceptance in that community's repo is withdrawn. It refuses
			// itself for every community this AppView does not host, which on
			// most instances is all of them.
			jetstream.WithAcceptanceCleanup(a.communityWriter)),
	})

	// Aggregators: service declarations and authorization records, following
	// Bluesky's feed generator and labeler pattern.
	consumers = append(consumers, feedConsumer{
		name: jetstream.ConsumerAggregators,
		handler: jetstream.NewAggregatorEventConsumer(a.aggregatorRepo,
			jetstream.WithAggregatorRevGate(a.revGate)),
	})

	// Votes from user repositories, with atomic post/comment count updates.
	//
	// The same erasure marker the post consumer reads: a vote whose subject was
	// swept by migration 036 has no row to wait for, and without the lookup the
	// ordering gate would redrive it until it dead-letters.
	consumers = append(consumers, feedConsumer{
		name: jetstream.ConsumerVotes,
		handler: jetstream.NewVoteEventConsumer(a.voteRepo, a.userService, a.db,
			jetstream.WithVoteDeletedAccounts(postgresRepo.NewDeletedAccountRepository(a.db))),
	})

	// Comments from user repositories, with atomic parent count updates.
	consumers = append(consumers, feedConsumer{
		name: jetstream.ConsumerComments,
		handler: jetstream.NewCommentEventConsumer(a.commentRepo, a.db,
			jetstream.WithCommentBridgeTrust(a.bridgeTrust)),
	})

	return consumers
}

// warnIfNoPrimaryFeed flags a topology with no primary-key feed. This is
// expected in local dev (a self-only feed), but in production it usually means
// cursor continuity from the single-feed era is being forfeited.
func warnIfNoPrimaryFeed(feeds []jetstream.Feed) {
	for _, feed := range feeds {
		if feed.Key == jetstream.PrimaryFeedKey {
			return
		}
	}
	slog.Warn("no JETSTREAM_FEEDS entry uses the primary feed key: every consumer name will be "+
		"suffixed \"@<feedKey>\", so cursors persisted under the bare legacy names will NOT be used",
		"primary_feed_key", jetstream.PrimaryFeedKey)
}
