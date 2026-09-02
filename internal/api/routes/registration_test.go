package routes

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	indigooauth "github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/go-chi/chi/v5"

	"Coves/internal/api/middleware"
	"Coves/internal/atproto/oauth"
	"Coves/internal/core/userblocks"
)

// The whole HTTP surface of the AppView, declared once and checked against the
// router the seventeen Register* functions actually build.
//
// # WHY A TABLE AND NOT SEVENTEEN FILES
//
// Every one of those functions is a list of `r.With(...).Method(pattern,
// handler)` calls, and the interesting content is not any single line — it is
// the SHAPE of the whole list: which NSIDs exist, which verb each answers, what
// guards each one, and what budget each one gets. Those are properties of the
// set, and three of the four ways they go wrong are invisible from inside one
// file:
//
//   - A route that quietly disappears (a handler renamed, a registration lost in
//     a merge) leaves no compile error and no failing handler test. Nothing but
//     an enumeration notices.
//   - A route that quietly appears — a new endpoint added without anyone
//     deciding whether it should be public — is the same problem pointed the
//     other way, and matters more, because the default for a new `r.Get` is
//     "world-readable".
//   - A guard dropped from ONE route in a file whose other routes still have it
//     is exactly the diff a reviewer's eye slides over.
//
// So the table below is a declaration, maintained by hand, of what this service
// is supposed to expose. The tests compare it to the real router in both
// directions. Changing the product means changing this table, and that is the
// point: the diff that removes RequireAuth from a write endpoint cannot be
// merged without also editing a line that says, in words, that the endpoint is
// authenticated.
//
// # HOW THE GUARD IS OBSERVED
//
// Not by reflection and not by string-matching the response body. chi's
// `Walk` hands back, per route, the inline middleware chain that `With`
// attached (`ChainHandler.Middlewares`), so each middleware can be pulled out
// and interrogated on its own, wrapped around a sentinel handler that records
// whether it was reached:
//
//	anonymous request       -> refused with 401, sentinel not reached  => RequireAuth
//	request with a bad token -> sentinel reached, unsealer consulted    => OptionalAuth
//	repeated requests        -> 429 on the (N+1)th                      => rate limiter, budget N
//	none of the above                                                   => something else (CORS)
//
// This is behavioural rather than structural, which is the property that
// matters: a middleware that has been renamed, rewrapped or replaced still
// classifies correctly, and one that has been neutered does not. Rate limits
// come out as the exact number rather than a yes/no, so raising the signup
// budget from 5 to 500 fails here rather than shipping.
//
// # WHAT NEVER RUNS
//
// No handler. The services below are all nil interfaces on purpose: the
// assertions are about registration, and reaching a handler would prove nothing
// about it while dragging a database into a T0 test. The one exception is
// documented at TestRoutes_AggregatorRegisterBudget, which has to drive the real
// route because its rate limiter is not in the chain at all.
//
// # JURISDICTION: WHAT THIS TABLE DOES NOT COVER
//
// It covers the routes registered by this package's Register* functions, which
// is not quite the same set as the routes the server serves. cmd/server mounts
// one endpoint of its own — social.coves.community.comment.getComments, in
// registerCommentQueryRoute (cmd/server/routes.go:109-120) — because it needs a
// rate limit stricter than the global one plus optional auth, and it is not
// declared below because this package does not register it.
//
// That route is worth knowing about for one reason: it uses `r.Handle`, which
// matches EVERY verb, so a POST to it is accepted where every endpoint in the
// table below answers 405. The refusal comes from the handler instead
// (get_comments.go:51, covered by internal/api/handlers/comments/
// get_comments_test.go:294) — a real 405, one layer lower, and therefore after
// the rate limiter has already spent the caller's budget.
//
// The right long-term answer is to move that registration in here, where the
// table would hold it to the same discipline. Until then this is a stated
// boundary rather than a gap: note that a route added to THIS package with
// `r.Handle` would not escape — chi expands it into every concrete verb, so the
// surplus methods would surface as "registered but NOT declared" below.

// authKind is what a route requires of a caller's credentials.
type authKind int

const (
	// authNone: no authentication middleware. Anyone may call it, and an
	// authenticated caller gets no viewer state because nothing put their DID in
	// the context.
	authNone authKind = iota
	// authOptional: public, but an authenticated caller is recognised. This is
	// the shape every personalised-read endpoint needs — vote state, block
	// filtering, viewer.subscribed.
	authOptional
	// authRequired: refused without a valid session.
	authRequired
)

func (k authKind) String() string {
	switch k {
	case authNone:
		return "no auth"
	case authOptional:
		return "OptionalAuth"
	case authRequired:
		return "RequireAuth"
	}
	return fmt.Sprintf("authKind(%d)", int(k))
}

// declaredRoute is one endpoint as the product intends it.
type declaredRoute struct {
	method string
	path   string
	auth   authKind

	// rateLimit is the per-client-IP request budget of the route's OWN limiter,
	// or 0 when it has none and only the global 100/min limiter in cmd/server
	// applies. Several routes share one limiter instance; that is why every
	// probe below uses a distinct synthetic client address.
	rateLimit int

	// limiterInsideHandler marks the one route whose limiter is not registered
	// as middleware but wrapped around the handler value itself
	// (aggregator.go:53). The chain-based assertions cannot see it, so it is
	// excluded from them and covered by its own test.
	limiterInsideHandler bool
}

// declaredRoutes is the AppView's HTTP surface.
//
// Grouped by the Register* function that owns each block, in the order
// cmd/server/routes.go calls them, so this reads alongside the wiring.
var declaredRoutes = []declaredRoute{
	// RegisterUserRoutesWithOptions — social.coves.actor.*
	{http.MethodGet, "/api/me", authRequired, 0, false},
	{http.MethodGet, "/xrpc/social.coves.actor.getProfile", authOptional, 0, false},
	{http.MethodPost, "/xrpc/social.coves.actor.signup", authNone, 0, false},
	// The bot gate in front of account creation. Its budget is the whole
	// mechanism: a Turnstile token buys one PDS invite code, so the cost of
	// bulk-probing the handshake is whatever this number says it is.
	{http.MethodPost, "/xrpc/social.coves.actor.requestSignupToken", authNone, 5, false},
	{http.MethodPost, "/xrpc/social.coves.actor.deleteAccount", authRequired, 0, false},
	{http.MethodPost, "/xrpc/social.coves.actor.updateProfile", authRequired, 0, false},

	// RegisterCommunityRoutes — social.coves.community.*
	{http.MethodGet, "/xrpc/social.coves.community.get", authOptional, 0, false},
	{http.MethodGet, "/xrpc/social.coves.community.list", authOptional, 0, false},
	// search takes no auth at all, unlike get and list beside it. That
	// asymmetry is deliberate — search returns no viewer state — and declaring
	// it here is what stops it being "fixed" into OptionalAuth by accident, or
	// stops the other two silently decaying into it.
	{http.MethodGet, "/xrpc/social.coves.community.search", authNone, 0, false},
	{http.MethodPost, "/xrpc/social.coves.community.create", authRequired, 0, false},
	{http.MethodPost, "/xrpc/social.coves.community.update", authRequired, 0, false},
	{http.MethodPost, "/xrpc/social.coves.community.subscribe", authRequired, 0, false},
	{http.MethodPost, "/xrpc/social.coves.community.unsubscribe", authRequired, 0, false},
	{http.MethodPost, "/xrpc/social.coves.community.blockCommunity", authRequired, 0, false},
	{http.MethodPost, "/xrpc/social.coves.community.unblockCommunity", authRequired, 0, false},
	// Reading community blocks is authenticated because this list is scoped to
	// the caller's DID, not because the records are secret: each is public in the
	// user's repo, but there is no public form of "my blocks".
	{http.MethodGet, "/xrpc/social.coves.community.getBlockedCommunities", authRequired, 0, false},

	// RegisterPostRoutes — social.coves.community.post.*
	// The three writes take DualAuthMiddleware in production (OAuth users OR
	// aggregator service JWTs); the guard is RequireAuth either way, which is
	// what this table can see. Which principals that RequireAuth accepts is
	// post_aggregator_test.go's subject.
	{http.MethodPost, "/xrpc/social.coves.community.post.create", authRequired, 0, false},
	// update arrived with the write-path flip (PRD §4.2): a post lives in its
	// author's repo now, so editing one is a write this AppView can actually
	// perform. It is declared exactly like create beside it — the same guard,
	// no limiter of its own — because it is the same kind of write by the same
	// principals, and an edit that was cheaper to call than the post it edits
	// would be the obvious way to spend a quota the create path meters.
	{http.MethodPost, "/xrpc/social.coves.community.post.update", authRequired, 0, false},
	{http.MethodPost, "/xrpc/social.coves.community.post.delete", authRequired, 0, false},
	{http.MethodGet, "/xrpc/social.coves.community.post.get", authOptional, 0, false},
	// getStatus takes no auth at all, unlike post.get beside it, and the
	// asymmetry is the decision this line exists to hold. The caller it is for
	// is an author on ANOTHER server asking this host whether it accepted their
	// post (PRD §7): they have no account here, so there is no session to
	// require and no viewer state to personalise. A rejection is AppView-local
	// and writes no community record (§3.3), so this endpoint is the only way
	// that answer is reachable at all. The accepted cost, recorded in PRD rev
	// 2.7: anyone who can name a post URI learns its status in a community.
	// Adding OptionalAuth here would be harmless; adding RequireAuth would make
	// the cross-server case unanswerable, which is why it is declared.
	// getStatus carries its OWN limiter, tighter than the global 100/minute,
	// and it is the only unauthenticated route in the product that does. Two
	// things make it worth the exception: §7's client UX is to POLL it until a
	// post flips to accepted, so the honest traffic shape is repeated requests
	// from one caller; and because it takes no auth, an unauthenticated
	// stranger can ask about any post URI they can name. The budget is what
	// bounds enumeration of a community's rejected posts to something an
	// operator would notice.
	{http.MethodGet, "/xrpc/social.coves.community.post.getStatus", authNone, 120, false},

	// RegisterVoteRoutes — social.coves.feed.vote.*
	{http.MethodPost, "/xrpc/social.coves.feed.vote.create", authRequired, 0, false},
	{http.MethodPost, "/xrpc/social.coves.feed.vote.delete", authRequired, 0, false},

	// RegisterUserBlockRoutes — social.coves.actor.*User
	{http.MethodPost, "/xrpc/social.coves.actor.blockUser", authRequired, 0, false},
	{http.MethodPost, "/xrpc/social.coves.actor.unblockUser", authRequired, 0, false},
	// Reading your own block list is authenticated, not public: the list is
	// itself sensitive, and it is scoped to the caller's DID.
	{http.MethodGet, "/xrpc/social.coves.actor.getBlockedUsers", authRequired, 0, false},

	// RegisterCommentRoutes — social.coves.community.comment.*
	{http.MethodPost, "/xrpc/social.coves.community.comment.create", authRequired, 0, false},
	{http.MethodPost, "/xrpc/social.coves.community.comment.update", authRequired, 0, false},
	{http.MethodPost, "/xrpc/social.coves.community.comment.delete", authRequired, 0, false},

	// RegisterAdminReportRoutes — social.coves.admin.*
	{http.MethodPost, "/xrpc/social.coves.admin.submitReport", authRequired, 10, false},

	// RegisterCommunitySuggestionRoutes — social.coves.community.suggestion.*
	{http.MethodGet, "/xrpc/social.coves.community.suggestion.list", authOptional, 0, false},
	{http.MethodGet, "/xrpc/social.coves.community.suggestion.get", authOptional, 0, false},
	{http.MethodPost, "/xrpc/social.coves.community.suggestion.create", authRequired, 10, false},
	// vote and removeVote share ONE limiter instance, so 30 is the combined
	// budget across both, not 30 each.
	{http.MethodPost, "/xrpc/social.coves.community.suggestion.vote", authRequired, 30, false},
	{http.MethodPost, "/xrpc/social.coves.community.suggestion.removeVote", authRequired, 30, false},
	// updateStatus is admin-only, but the admin check is inside the handler
	// (it compares the authenticated DID against adminDIDs), not a middleware.
	// From the router's point of view it is an ordinary authenticated POST.
	{http.MethodPost, "/xrpc/social.coves.community.suggestion.updateStatus", authRequired, 0, false},

	// RegisterCommunityFeedRoutes / Timeline / Discover / Actor — the read tier.
	{http.MethodGet, "/xrpc/social.coves.communityFeed.getCommunity", authOptional, 0, false},
	// getTimeline is the one feed that is NOT public: it is the personalised
	// fan-out over the caller's subscriptions, so an anonymous caller has no
	// timeline to serve. It is also why the timeline's behaviour is unreachable
	// from the pipeline tier (docs/TEST_ARCHITECTURE.md §3.4b).
	{http.MethodGet, "/xrpc/social.coves.feed.getTimeline", authRequired, 0, false},
	{http.MethodGet, "/xrpc/social.coves.feed.getDiscover", authOptional, 0, false},
	{http.MethodGet, "/xrpc/social.coves.actor.getPosts", authOptional, 0, false},
	{http.MethodGet, "/xrpc/social.coves.actor.getComments", authOptional, 0, false},

	// RegisterAggregatorRoutes — social.coves.aggregator.*
	{http.MethodGet, "/xrpc/social.coves.aggregator.getServices", authNone, 0, false},
	{http.MethodGet, "/xrpc/social.coves.aggregator.getAuthorizations", authNone, 0, false},
	{http.MethodGet, "/xrpc/social.coves.aggregator.listForCommunity", authNone, 0, false},
	// Self-registration for bots, deliberately unauthenticated — an aggregator
	// has no Coves credential until it has registered. The budget is the only
	// thing standing between that and a table full of junk.
	{http.MethodPost, "/xrpc/social.coves.aggregator.register", authNone, 10, true},

	// RegisterAggregatorAPIKeyRoutes
	{http.MethodPost, "/xrpc/social.coves.aggregator.createApiKey", authRequired, 0, false},
	{http.MethodGet, "/xrpc/social.coves.aggregator.getApiKey", authRequired, 0, false},
	{http.MethodPost, "/xrpc/social.coves.aggregator.revokeApiKey", authRequired, 0, false},
	// Deliberately open: operational counters with no per-aggregator detail.
	{http.MethodGet, "/xrpc/social.coves.aggregator.getMetrics", authNone, 0, false},

	// RegisterOAuthRoutes. The three metadata documents are public by
	// specification — an authorization server fetches them unauthenticated.
	{http.MethodGet, "/oauth-client-metadata.json", authNone, 0, false},
	{http.MethodGet, "/oauth-client-keys.json", authNone, 0, false},
	{http.MethodGet, "/.well-known/oauth-protected-resource", authNone, 0, false},
	// login, mobile login, callback and the deep-link fallback SHARE one
	// limiter, so 10 is the combined budget over all four. That is the
	// credential-stuffing bound, and it is why they are declared with the same
	// number rather than 10 each.
	{http.MethodGet, "/oauth/login", authNone, 10, false},
	{http.MethodGet, "/oauth/mobile/login", authNone, 10, false},
	{http.MethodGet, "/oauth/callback", authNone, 10, false},
	{http.MethodGet, "/app/oauth/callback", authNone, 10, false},
	{http.MethodPost, "/oauth/logout", authNone, 10, false},
	{http.MethodPost, "/oauth/refresh", authNone, 20, false},

	// RegisterWellKnownRoutes — RFC 8615 mobile deep-linking manifests. Both
	// MUST be publicly readable at these exact paths or iOS and Android refuse
	// to bind the app to the domain.
	{http.MethodGet, "/.well-known/apple-app-site-association", authNone, 0, false},
	{http.MethodGet, "/.well-known/assetlinks.json", authNone, 0, false},

	// RegisterWebRoutes — the browser-facing pages.
	{http.MethodGet, "/", authNone, 0, false},
	{http.MethodGet, "/delete-account", authNone, 0, false},
	{http.MethodPost, "/delete-account", authNone, 0, false},
	{http.MethodGet, "/delete-account/success", authNone, 0, false},
	{http.MethodGet, "/privacy", authNone, 0, false},
	{http.MethodGet, "/safety/child-safety", authNone, 0, false},
	{http.MethodGet, "/m/turnstile.html", authNone, 0, false},
	{http.MethodGet, "/static/*", authNone, 0, false},

	// RegisterImageProxyRoutes
	{http.MethodGet, "/img/{preset}/plain/{did}/{cid}", authNone, 0, false},
}

// routeKey identifies a route the way both the table and chi's walk see it.
type routeKey struct {
	method string
	path   string
}

func (k routeKey) String() string { return k.method + " " + k.path }

// ---------------------------------------------------------------------------
// Building the router under test
// ---------------------------------------------------------------------------

// probeUnsealer is the middleware's SessionUnsealer, and the only collaborator
// either auth middleware has that a test can watch. It refuses every token: an
// unsealer that succeeded would make RequireAuth and OptionalAuth behave
// identically, which is precisely the distinction being drawn.
//
// Being asked to unseal anything is what proves an OAuth middleware is in a
// chain — nothing else in the codebase calls this interface.
//
// It records the TOKENS it was shown rather than counting calls, and that
// detail is load-bearing. A shared counter read before and after a probe is
// only meaningful if nothing else is probing, and the tests below run in
// parallel over one router; the first version of this file used a counter and
// duly misreported an unguarded OAuth route as OptionalAuth when a sibling
// test's probe landed between the two reads. A token is unique to one probe, so
// "did you see mine" has an answer that does not depend on who else is asking.
type probeUnsealer struct {
	mu   sync.Mutex
	seen map[string]bool
}

func (u *probeUnsealer) UnsealSession(token string) (*oauth.SealedSession, error) {
	u.mu.Lock()
	u.seen[token] = true
	u.mu.Unlock()
	return nil, fmt.Errorf("probeUnsealer refuses every token by design")
}

func (u *probeUnsealer) sawToken(token string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.seen[token]
}

// unreachableUserBlockService is a non-nil userblocks.Service with no
// implementation.
//
// userblock.NewBlockHandler refuses a nil service at construction
// (block.go:24), which is the right guard and the reason this exists: the
// embedded interface is nil, so the value satisfies the guard and panics loudly
// on any actual call. That keeps this file's rule intact — no handler runs —
// and makes a violation of it a crash rather than a quiet result.
type unreachableUserBlockService struct{ userblocks.Service }

// builtRouter is the router plus the collaborator the classifier watches.
type builtRouter struct {
	mux      *chi.Mux
	unsealer *probeUnsealer
}

// theRouter builds the AppView's router once for the whole file.
//
// It is built once rather than per test because every Register* call that
// creates a rate limiter also starts a cleanup goroutine that only Stop() ends,
// and the limiter is never returned — so a router is not a disposable object.
// Sharing one is safe here because the probes never share a client address, and
// a rate limiter's state is per address.
var theRouter = sync.OnceValue(func() builtRouter {
	unsealer := &probeUnsealer{seen: map[string]bool{}}
	auth := middleware.NewOAuthAuthMiddleware(unsealer, nil)
	mux := chi.NewRouter()

	// Every service argument below is a nil interface. These tests never reach
	// a handler, and a nil dependency is the honest way to say so: if an
	// assertion ever did reach one, it would panic rather than quietly assert
	// something about a stub.
	//
	// The OAuth client app is the exception, because RegisterUserRoutes takes
	// the no-options path and NewUpdateProfileHandler refuses a nil one at
	// construction (update_profile.go:72) rather than at the first request —
	// the same fail-fast wiring guard RegisterPostRoutes has. Building a real
	// one keeps this on the production branch instead of the
	// PDSClientFactory test seam. Nothing dials: the client app resolves
	// identities only when a request reaches it, and none does.
	clientConfig := indigooauth.NewLocalhostConfig("http://127.0.0.1:0/oauth/callback", []string{"atproto"})
	RegisterUserRoutes(mux, nil, auth, indigooauth.NewClientApp(&clientConfig, nil))
	RegisterCommunityRoutes(mux, nil, nil, auth, nil)
	RegisterPostRoutes(mux, nil, nil, nil, auth, auth)
	RegisterVoteRoutes(mux, nil, auth)
	RegisterUserBlockRoutes(mux, unreachableUserBlockService{}, auth)
	RegisterCommentRoutes(mux, nil, auth)
	RegisterAdminReportRoutes(mux, nil, auth)
	RegisterCommunitySuggestionRoutes(mux, nil, auth, nil)
	RegisterCommunityFeedRoutes(mux, nil, nil, nil, auth)
	RegisterTimelineRoutes(mux, nil, nil, nil, auth)
	RegisterDiscoverRoutes(mux, nil, nil, nil, auth)
	RegisterActorRoutes(mux, nil, nil, nil, nil, nil, auth)
	RegisterAggregatorRoutes(mux, nil, nil, nil, nil)
	RegisterAggregatorAPIKeyRoutes(mux, auth, nil, nil)
	RegisterOAuthRoutes(mux, nil, []string{"https://coves.social"})
	RegisterWellKnownRoutes(mux)
	RegisterWebRoutes(mux, nil, nil, "")
	RegisterImageProxyRoutes(mux, nil)

	return builtRouter{mux: mux, unsealer: unsealer}
})

// walkRoutes enumerates what the router actually serves, with the inline
// middleware chain each route was registered behind.
func walkRoutes(t *testing.T, mux *chi.Mux) map[routeKey][]func(http.Handler) http.Handler {
	t.Helper()
	found := map[routeKey][]func(http.Handler) http.Handler{}
	err := chi.Walk(mux, func(method, route string, _ http.Handler, mws ...func(http.Handler) http.Handler) error {
		key := routeKey{method: method, path: route}
		if _, duplicate := found[key]; duplicate {
			return fmt.Errorf("chi reported %s twice", key)
		}
		found[key] = mws
		return nil
	})
	if err != nil {
		t.Fatalf("walking the router: %v", err)
	}
	return found
}

// ---------------------------------------------------------------------------
// Classifying a middleware by what it does
// ---------------------------------------------------------------------------

// maxBudgetProbe bounds the hammering that identifies a rate limiter. A
// middleware that has refused nothing after this many requests from one client
// is reported as "not a rate limiter" — so a real limiter set above this
// ceiling would be classified as absent. TestRoutes_BudgetProbeCeilingIsAdequate
// keeps that from happening quietly — and it works: it fired when getStatus's
// budget rose to 120 (above the then-64 ceiling), which is why this is 128.
// The probe is in-process httptest, so the extra requests cost milliseconds;
// never size a production budget down to make this probe cheaper.
const maxBudgetProbe = 128

type mwKind int

const (
	mwOther mwKind = iota
	mwRequireAuth
	mwOptionalAuth
	mwRateLimit
)

func (k mwKind) String() string {
	switch k {
	case mwRequireAuth:
		return "RequireAuth"
	case mwOptionalAuth:
		return "OptionalAuth"
	case mwRateLimit:
		return "rate limiter"
	}
	return "some other middleware"
}

type mwFacts struct {
	kind mwKind
	// budget is the per-client request allowance, for mwRateLimit only.
	budget int
}

// clientAddresses hands out a distinct synthetic client address per probe.
//
// Distinctness is not cosmetic. Four OAuth routes share one limiter and two
// suggestion routes share another, so a probe that reused an address would be
// spending a budget an unrelated route's assertion is about to check. Rate
// limiting is keyed per client, so a fresh client is a fresh budget even on a
// shared limiter. CGNAT space, because these are never real addresses.
var clientAddresses atomic.Int64

func nextClientAddress() string {
	n := clientAddresses.Add(1)
	return fmt.Sprintf("100.64.%d.%d", (n/256)%256, n%256)
}

// probe sends one request through mw and reports whether the handler behind it
// was reached, and what status came back.
func probe(mw func(http.Handler) http.Handler, clientIP, bearer string) (reached bool, status int) {
	sentinel := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusTeapot)
	})
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("X-Real-IP", clientIP)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	mw(sentinel).ServeHTTP(rec, req)
	return reached, rec.Code
}

// classify decides what a middleware is by watching what it does, never by
// looking at what it is.
//
// The order of the questions matters and is the whole design. RequireAuth is
// identified first, by the one thing only it does: refuse a request that
// carries no credential. OptionalAuth is identified second, by the one thing
// only an OAuth middleware does when a credential IS present: consult the
// unsealer. Rate limiters are identified last, by running out. Anything left is
// something else — in this router, CORS.
func classify(mw func(http.Handler) http.Handler, unsealer *probeUnsealer) mwFacts {
	if reached, status := probe(mw, nextClientAddress(), ""); !reached && status == http.StatusUnauthorized {
		return mwFacts{kind: mwRequireAuth}
	}

	token := fmt.Sprintf("probe-token-%d", clientAddresses.Add(1))
	probe(mw, nextClientAddress(), token)
	if unsealer.sawToken(token) {
		return mwFacts{kind: mwOptionalAuth}
	}

	client := nextClientAddress()
	for sent := 1; sent <= maxBudgetProbe; sent++ {
		if _, status := probe(mw, client, ""); status == http.StatusTooManyRequests {
			return mwFacts{kind: mwRateLimit, budget: sent - 1}
		}
	}
	return mwFacts{kind: mwOther}
}

// chainFacts classifies every middleware guarding one route.
func chainFacts(chain []func(http.Handler) http.Handler, unsealer *probeUnsealer) []mwFacts {
	facts := make([]mwFacts, 0, len(chain))
	for _, mw := range chain {
		facts = append(facts, classify(mw, unsealer))
	}
	return facts
}

func countKind(facts []mwFacts, kind mwKind) int {
	n := 0
	for _, f := range facts {
		if f.kind == kind {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// The assertions
// ---------------------------------------------------------------------------

// TestRoutes_TheSurfaceIsExactlyWhatIsDeclared compares the declared table to
// the router in both directions.
//
// The "unexpected" half is the one that earns its keep. A missing route breaks
// a client and gets reported within the hour; an ADDED route does not break
// anything, which is why an endpoint can sit exposed for months. Adding a route
// to this service now requires adding a line above that says who may call it.
func TestRoutes_TheSurfaceIsExactlyWhatIsDeclared(t *testing.T) {
	t.Parallel()
	router := theRouter()
	actual := walkRoutes(t, router.mux)

	declared := map[routeKey]declaredRoute{}
	for _, route := range declaredRoutes {
		key := routeKey{method: route.method, path: route.path}
		if _, dup := declared[key]; dup {
			t.Fatalf("the declared table lists %s twice", key)
		}
		declared[key] = route
	}

	var missing, unexpected []string
	for key := range declared {
		if _, ok := actual[key]; !ok {
			missing = append(missing, key.String())
		}
	}
	for key := range actual {
		if _, ok := declared[key]; !ok {
			unexpected = append(unexpected, key.String())
		}
	}
	sort.Strings(missing)
	sort.Strings(unexpected)

	if len(missing) > 0 {
		t.Errorf("declared but NOT registered — these endpoints have disappeared from the "+
			"service and every client calling them now gets a 404:\n  %s",
			strings.Join(missing, "\n  "))
	}
	if len(unexpected) > 0 {
		t.Errorf("registered but NOT declared — the service exposes endpoints nobody wrote down. "+
			"Add each to declaredRoutes together with the access it should have; if one of these "+
			"should not be public, that decision has already shipped:\n  %s",
			strings.Join(unexpected, "\n  "))
	}
}

// TestRoutes_GuardsMatchTheDeclaration is the 401 matrix, mechanically, for
// every route at once.
//
// The T2 tier proves a handful of these against the running server, which is
// the only place a real sealed session exists. It cannot prove them all — each
// one costs a round trip — and it does not notice the routes it was never told
// about. This does both, in milliseconds, from the registration itself.
func TestRoutes_GuardsMatchTheDeclaration(t *testing.T) {
	t.Parallel()
	router := theRouter()
	actual := walkRoutes(t, router.mux)

	for _, route := range declaredRoutes {
		key := routeKey{method: route.method, path: route.path}
		chain, registered := actual[key]
		if !registered {
			continue // reported by TestRoutes_TheSurfaceIsExactlyWhatIsDeclared
		}

		facts := chainFacts(chain, router.unsealer)
		required := countKind(facts, mwRequireAuth)
		optional := countKind(facts, mwOptionalAuth)

		switch route.auth {
		case authRequired:
			if required != 1 || optional != 0 {
				t.Errorf("%s is declared %s but its chain has %d RequireAuth and %d OptionalAuth: "+
					"an anonymous caller reaches this endpoint's handler",
					key, route.auth, required, optional)
			}
		case authOptional:
			if optional != 1 || required != 0 {
				t.Errorf("%s is declared %s but its chain has %d OptionalAuth and %d RequireAuth. "+
					"With zero, an authenticated caller gets no viewer state on a public read "+
					"(no vote state, no block filtering, no viewer.subscribed) because nothing put "+
					"their DID in the context; with RequireAuth instead, the endpoint has stopped "+
					"being public",
					key, route.auth, optional, required)
			}
		case authNone:
			if required != 0 || optional != 0 {
				t.Errorf("%s is declared %s but its chain has %d RequireAuth and %d OptionalAuth",
					key, route.auth, required, optional)
			}
		}
	}
}

// TestRoutes_BudgetsMatchTheDeclaration checks the exact per-route request
// budget, not merely that a limiter is present.
//
// A limiter whose number has drifted is worse than no limiter, because the
// route looks protected. The two numbers that matter most are the smallest:
// requestSignupToken at 5, which is the cost of probing the bot gate, and the
// OAuth login family at 10, which is the cost of a credential-stuffing attempt.
func TestRoutes_BudgetsMatchTheDeclaration(t *testing.T) {
	t.Parallel()
	router := theRouter()
	actual := walkRoutes(t, router.mux)

	for _, route := range declaredRoutes {
		if route.limiterInsideHandler {
			continue // TestRoutes_AggregatorRegisterBudget
		}
		key := routeKey{method: route.method, path: route.path}
		chain, registered := actual[key]
		if !registered {
			continue
		}

		facts := chainFacts(chain, router.unsealer)
		var budgets []int
		for _, f := range facts {
			if f.kind == mwRateLimit {
				budgets = append(budgets, f.budget)
			}
		}

		if route.rateLimit == 0 {
			if len(budgets) > 0 {
				t.Errorf("%s is declared with no limiter of its own but is rate limited at %v", key, budgets)
			}
			continue
		}
		if len(budgets) != 1 {
			t.Errorf("%s is declared with a budget of %d but its chain has %d rate limiters: "+
				"the route is now bounded only by the global 100/min limiter",
				key, route.rateLimit, len(budgets))
			continue
		}
		if budgets[0] != route.rateLimit {
			t.Errorf("%s allows %d requests per client before refusing, but is declared at %d",
				key, budgets[0], route.rateLimit)
		}
	}
}

// TestRoutes_BudgetProbeCeilingIsAdequate guards the classifier's own blind
// spot.
//
// classify reports "not a rate limiter" when a middleware has refused nothing
// after maxBudgetProbe requests. That makes the ceiling load-bearing in two
// directions, and both failures are the misleading kind — a PASS that means
// nothing:
//
//   - A route declared with no limiter that gains one set above the ceiling
//     would be classified as unlimited, and TestRoutes_BudgetsMatchTheDeclaration
//     would agree with the declaration and go green.
//   - A route declared at or above the ceiling could never be confirmed at all;
//     the classifier would call it absent and the budget assertion would fail
//     with a confusing "0 rate limiters" rather than "the probe is too short".
//
// One arithmetic check on the table removes both. It costs no requests.
func TestRoutes_BudgetProbeCeilingIsAdequate(t *testing.T) {
	t.Parallel()

	for _, route := range declaredRoutes {
		if route.rateLimit >= maxBudgetProbe {
			t.Errorf("%s %s is declared with a budget of %d, which the classifier cannot observe: it "+
				"gives up after %d requests and reports the limiter as absent. Raise maxBudgetProbe "+
				"above the largest declared budget",
				route.method, route.path, route.rateLimit, maxBudgetProbe)
		}
	}
}

// TestRoutes_AggregatorRegisterBudget covers the one route the chain-based
// assertions cannot see.
//
// aggregator.go:53 wraps the limiter around the HANDLER
// (`limiter.Middleware(http.HandlerFunc(h)).ServeHTTP`) instead of registering
// it with `With`, so chi never records it as middleware and it is invisible to
// Walk. The limit is real, so it is checked here by driving the route.
//
// This is the only test in the file that reaches a handler. It can, because
// HandleRegister decodes the body first and answers 400 on an empty one, long
// before it touches the nil user service or resolves anything over the network.
func TestRoutes_AggregatorRegisterBudget(t *testing.T) {
	t.Parallel()
	router := theRouter()

	const path = "/xrpc/social.coves.aggregator.register"
	const declaredBudget = 10
	client := nextClientAddress()

	send := func() int {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(""))
		req.Header.Set("X-Real-IP", client)
		rec := httptest.NewRecorder()
		router.mux.ServeHTTP(rec, req)
		return rec.Code
	}

	for sent := 1; sent <= declaredBudget; sent++ {
		if status := send(); status == http.StatusTooManyRequests {
			t.Fatalf("request %d of %d was refused with 429: the registration budget is smaller than "+
				"the declared %d", sent, declaredBudget, declaredBudget)
		}
	}
	if status := send(); status != http.StatusTooManyRequests {
		t.Errorf("request %d answered %d, want 429: unauthenticated self-registration is not bounded "+
			"at the declared %d per client, so the aggregators table can be filled by anyone",
			declaredBudget+1, status, declaredBudget)
	}
}

// TestRoutes_VerbsAreNotInterchangeable proves each endpoint answers its own
// verb only.
//
// Queries and procedures are different things in XRPC, and a query that also
// accepts POST is a query that can be driven by a cross-site form submission.
// chi answers the wrong verb with 405 before any middleware or handler runs, so
// this is a statement about the routing table and nothing else.
func TestRoutes_VerbsAreNotInterchangeable(t *testing.T) {
	t.Parallel()
	router := theRouter()

	// /delete-account answers both verbs on purpose (the form and its
	// submission), so it has no "other" verb to refuse.
	bothVerbs := map[string]bool{}
	seen := map[string]int{}
	for _, route := range declaredRoutes {
		seen[route.path]++
		if seen[route.path] > 1 {
			bothVerbs[route.path] = true
		}
	}

	for _, route := range declaredRoutes {
		if bothVerbs[route.path] {
			continue
		}
		other := http.MethodPost
		if route.method == http.MethodPost {
			other = http.MethodGet
		}

		req := httptest.NewRequest(other, requestablePath(route.path), nil)
		rec := httptest.NewRecorder()
		router.mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s answered %d, want 405: this endpoint is declared as a %s-only route",
				other, route.path, rec.Code, route.method)
		}
	}
}

// TestRoutes_UnknownNSIDIs404 anchors the negative side of the enumeration: the
// router does not serve a path merely because it is XRPC-shaped. Without this,
// a 405 in the test above could not be distinguished from chi answering
// everything.
func TestRoutes_UnknownNSIDIs404(t *testing.T) {
	t.Parallel()
	router := theRouter()

	for _, path := range []string{
		"/xrpc/social.coves.actor.notAnEndpoint",
		"/xrpc/app.bsky.feed.getTimeline",
		"/definitely/not/a/route",
	} {
		rec := httptest.NewRecorder()
		router.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s answered %d, want 404", path, rec.Code)
		}
	}
}

// TestRoutes_OAuthCallbackHasNoPreflight records a real consequence of how the
// CORS middleware is attached, so that it is a decision rather than a surprise.
//
// RegisterOAuthRoutes wraps /oauth/callback in `cors.Handler` with
// AllowedMethods GET, POST and OPTIONS — but it attaches it with `With` on a
// GET registration, and chi resolves the verb against the routing tree BEFORE
// any middleware runs. An OPTIONS request therefore never reaches the CORS
// middleware and is answered 405, so the preflight the middleware is configured
// to handle cannot happen.
//
// This is harmless as the flow is actually used: the PDS returns the user to
// /oauth/callback by top-level browser navigation, which is not a cross-origin
// XHR and issues no preflight. It stops being harmless the day a browser client
// tries to complete the flow with fetch(), which would fail at the preflight
// with a 405 and no CORS headers — a confusing failure that this test names in
// advance. If preflight is ever wanted, the route needs its own
// `r.Options(...)` registration or router-level `Use`.
func TestRoutes_OAuthCallbackHasNoPreflight(t *testing.T) {
	t.Parallel()
	router := theRouter()

	req := httptest.NewRequest(http.MethodOptions, "/oauth/callback", nil)
	req.Header.Set("Origin", "https://coves.social")
	req.Header.Set("Access-Control-Request-Method", http.MethodGet)
	rec := httptest.NewRecorder()
	router.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("OPTIONS /oauth/callback answered %d, want 405. IF THIS FAILED, preflight now "+
			"reaches the CORS middleware — the route gained an OPTIONS registration or a "+
			"router-level CORS Use, and this test should be replaced by one asserting the "+
			"Access-Control-Allow-Origin response", rec.Code)
	}
	if origin := rec.Header().Get("Access-Control-Allow-Origin"); origin != "" {
		t.Errorf("the 405 carried Access-Control-Allow-Origin: %q — the CORS middleware did run "+
			"after all, and the reasoning in this test's comment is wrong", origin)
	}
}

// requestablePath turns a chi pattern into a concrete path a request can be
// made to. Only two patterns in the table have parameters.
func requestablePath(pattern string) string {
	path := strings.ReplaceAll(pattern, "/*", "/anything")
	if !strings.Contains(path, "{") {
		return path
	}
	replacer := strings.NewReplacer(
		"{preset}", "avatar",
		"{did}", "did:plc:someone",
		"{cid}", "bafysomecid",
	)
	return replacer.Replace(path)
}
