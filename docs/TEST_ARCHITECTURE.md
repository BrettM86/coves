# Coves Test Architecture

Read §3.1 and §3.5 before writing a test. §3.6 lists the checks that fail the build.

## 1. Goals

- Green means deployable. A passing `make ci` certifies the whole pipeline: endpoint, PDS, firehose, consumer, Postgres, serving endpoint.
- Red means a regression. A test that fails one run in twenty teaches people to retry instead of investigate.
- A skip is a failure unless it is allowlisted. `cmd/ci-report` is the only skip mechanism.
- Tests never touch public atProto infrastructure (`plc.directory`, Bluesky relays and Jetstreams, public PDSes). The opt-in T3 tier is the one exception.

## 3. The architecture

### 3.1 Four tiers, selected by build tag

| Tier | Tag | Lives in | Talks to | Budget | Parallel |
|---|---|---|---|---|---|
| T0 Unit | none | in-package | nothing out of process | under 60 s | yes |
| T1 Integration | `integration` | in-package, next to the code | Postgres; some packages also the PDS | under 3 min | within a package (§3.3) |
| T2 Pipeline | `e2e` | `tests/e2e/` | the full hermetic stack, including a running AppView | under 10 min | no, serial (§3.4) |
| T3 Live | `live` | `tests/live/` | the public internet | opt-in | n/a |

Rules:

- Build tags are the only tier selector. Directory, filename and `-short` select nothing. `testing.Short()` is banned everywhere, production code included.
- Tags classify files, so a file is single-tier. A file that mixes tiers is split by test function. A mixed package is fine: its infra-gated `TestMain` lives in a tagged file and its shared fakes in an untagged sibling (`tests/testkit/harness_test.go` and `harness_support_test.go`). Tag sets are additive, so a tagged file may use untagged helpers, never the reverse.
- Missing infrastructure is a failure, not a skip. `testkit.Main(m, testkit.RequirePostgres, testkit.RequirePDS, ...)` probes once per package and fails naming the make target that brings the service up. T3 follows the same rule: missing config or no egress fails.
- `t.Skip` is banned in test bodies. The only sanctioned skip is an entry in `tests/ci/allowed_skips.txt`, read by `cmd/ci-report`, and that file is empty.
- `tests/` holds only cross-cutting code: `e2e/`, `testkit/`, `fixtures/`, `live/`, `ci/`, `audit/`, `domaincorpus/` (the shared hostname-validation cases) and the lexicon validation harness. Repo, service and consumer tests live next to the code they test, as T1. There is no `tests/integration` or `tests/unit`.

One more tag exists and is not a tier: `dev` compiles the dev-only OAuth resolvers in `internal/atproto/oauth` (`dev_resolver.go` and `dev_auth_resolver.go`, with `dev_stubs.go` as the `!dev` side). `make ci` vets it and runs that package once with `-tags dev` as its own gate.

### 3.2 What each tier is for

T0 holds behavioural breadth: validation, transforms, cursor encoding, error mapping, service logic against interface fakes, every edge case and error path. It may use loopback `httptest` servers. Transport-error paths use an injected failing transport, not a real remote dial.

T1 covers two seams that end at Postgres. Repo tests run real SQL against the real schema. Consumer tests hand a synthetic `jetstream.JetstreamEvent` to the consumer's `HandleEvent` and assert rows; idempotency, out-of-order delivery, rev-gating and malformed records are tested here. Feed, timeline and discover sort-and-cursor tests are fixture-driven T1 tests. Packages whose `TestMain` adds `RequirePDS` hold the write-forward tests: they assert the shape of the record the AppView writes against a PDS that rejects a malformed one. Authenticated write behaviour is still mostly proven here rather than at T2.

T2 is the pipeline contracts of §3.4. Narrow and deep: one ingestion proof per consumed collection, API contracts per client write surface, one cross-domain saga, and the reliability suite.

T3 is reality checks: live handle resolution, real unfurl targets, real PLC. It is never on the merge path. A live test that cannot reach the internet must fail, and its message says what was unreachable, that `make ci` blocks egress so the run did not come from the CI stack, and, for assertions pinned to third-party content, which fixture variable to replace and with what shape. Third-party content goes stale; the red run is how the drift gets noticed.

### 3.3 One harness: `tests/testkit`

**Dependency rule.** testkit imports no `internal/core/*` package, directly or transitively. Its only internal import is `internal/db/migrations`. T1 tests are in-package, so if testkit imported `internal/core/communities`, the communities tests importing testkit would be an import cycle. The transitive part matters: `internal/atproto/pds` imports `internal/core/blobs`, so testkit has its own PDS client. Where testkit duplicates a type for this reason, an external `package testkit_test` file pins the copy against the original; `firehose_pin_test.go` is the example. Domain-specific builders live in the domain's own `_test.go` files or in small leaf `<domain>test` packages.

**Template database.** `go test ./...` runs packages as separate processes, so per-package `TestMain`s racing to create and migrate the template would flake. The harness provisions it before `go test` starts: `make test-integration` and `scripts/ci-runner.sh` both run `scripts/test-db-prepare.sh` (Go code in `tests/testkit/cmd/testdbprepare`). It creates `coves_test_template` (or `POSTGRES_TEST_TEMPLATE`), migrates it, stamps it with a hash of the migrations, rebuilds on a mismatch, and sweeps clones orphaned by killed runs (`--sweep-age`, default one hour). A direct `go test -tags integration ./internal/foo` falls back to a verify-or-provision under a Postgres advisory lock. Concurrent T1 runs in different worktrees share one template, so export a per-run `POSTGRES_TEST_TEMPLATE` there.

`testkit.DB(t)` clones the template into a uniquely named database and drops it `WITH (FORCE)` on cleanup. It is the only way a test gets a database, and tests cannot see each other's rows. The test Postgres, on the host and in CI, runs on tmpfs with `fsync`, `synchronous_commit` and `full_page_writes` off. Every `DROP DATABASE` waits for a checkpoint, and on a disk volume under load those took up to 24 s and overran the 30 s cleanup deadline. Nothing in it outlives the container. In test code goose runs only inside testkit: template provisioning, and `MigrateUp` / `MigrateDownOne` for tests of a migration's Down section, which refuse any database that is not a clone. `credential_reencrypt_migration_test.go` in `internal/db/postgres` is the one test that builds its own goose provider.

**Connection budget.** Every parallel test holds its own clone pool, and `-p` multiplies that by the number of test binaries. `testkit.ConcurrencyBudget` derives `-p` and `-parallel` from the server's `max_connections`; `test-db-prepare.sh --print-flags` prints them for the Makefile and the CI runner. Neither number is hardcoded.

**`-p 1`.** Jetstream `account` and `identity` events bypass the consumers' wanted-collection filter, so parallel package binaries that create accounts flood every subscriber on the stream and starve each other. The constraint is the stream, not the database. `packageParallelism` in `tests/testkit/db.go` is a single constant that carries this reason. See §6 item 4.

**Firehose helper.** `testkit.Firehose` is for T1 and debugging only; T2 never dials a websocket (§3.4 rule 1). It captures the cursor before the write and replays from it, so subscribe-after-write races cannot be expressed. A gorilla connection is corrupt after a read deadline expires, so the helper re-dials on every read error (only an undecodable frame is terminal), replays every dial from the construction cursor less a 250 ms clock-skew allowance, deduplicates by event identity, names any matching event it discarded for predating that bound, and fails on pending-buffer overflow instead of dropping events.

**Waiting.** Three primitives, and nothing else:

```go
testkit.WaitFor(t, timeout, probe)   // eventually true; polls every 100 ms
testkit.Holds(t, window, probe)      // stays true: deletes stay deleted, counts stay stable
fh.Await(t, timeout, match)          // firehose delivery (T1 and debugging only)
```

A `Probe` returns `(done bool, err error)`. A non-nil error is terminal: a 401 or 500 fails at once with the response attached instead of being retried into an opaque timeout. On timeout `WaitFor` reports whatever `WithDiagnostics` renders, so close over the last observation. `testkit.WithConsumerHealth(appview)` adds a snapshot of `/health/consumers` (cursor positions, dead-letter counts) to the failure; the T2 pipeline helpers always pass it.

`time.Sleep` in test code fails the audit (§3.6.3), testkit included. The two that exist are the poll interval inside `WaitFor` and `Holds`, each with a `coves:allow-sleep` marker. Code that depends on time takes an injected clock or sleeper: a rate limiter's window is crossed through an injected clock, a circuit breaker's open window by ageing its stored `lastFailure`, and a slow `httptest` handler blocks on a channel the test releases. Two traps: a probe with a side effect can prevent its own success (polling a TTL cache through a read that touches mtime keeps the entry young forever), and a negative claim needs `Holds`, never `WaitFor`, because an eventually-check passes the moment an asynchronous fix lands and then passes forever.

**Identity.** `testkit.UniqueID(t)` (or `UniqueIDWithPrefix`) is the only handle and ID generator: a random per-process prefix plus an atomic counter, at most `MaxIDLength` (18) characters to fit the PDS handle label cap. Local PDS accounts persist across runs and counters reset across processes, which is why the prefix exists. Kept stacks accumulate accounts; the prefix makes that harmless. Because the prefix is per process, `-count=N` never varies it: assert structure, not substrings.

**Assertions.** testify everywhere: `require` to stop, `assert` to accumulate. No new bare `t.Errorf`.

### 3.4 Pipeline contracts

Several domains index synchronously on the client path. Signup writes the user row directly, and community creation stores the community in the same request. A "write via endpoint, read via endpoint" test therefore passes with the firehose dead. T2 is split into three contract classes so that some of them cannot pass that way.

#### 3.4a Ingestion contracts

One per consumed collection, mandatory:

```
record written directly to the PDS (the AppView never sees the write)
  -> PDS commit -> relay -> Jetstream
  -> the AppView's own consumers (cmd/server wiring, running as a container)
  -> Postgres
  -> XRPC serving endpoint, polled with WaitFor until the record appears
  -> destructive steps also checked with Holds
```

Firehose delivery is the only way the data can appear, so a dead consumer cannot pass. Shape: create, visible; update, visible; delete, gone and stays gone.

A write to the stack's own PDS is honest direct-PDS-path testing, not federation: the record still lives on the one PDS the stack fronts. Federation contracts write to `pds2` instead, a second PDS with its own hostname, volume and PLC registration and no AppView credentials, and Jetstream reads through a hermetic relay as in production. Post, comment, vote and remote blob fetch have federation contracts (the blob is asserted absent locally first). `TestFederatedIdentityIsNotIndexed` pins the limit in §6 item 2.

The contract inventory is generated. `jetstream.ConsumedCollections()` reads the consumer table in `internal/atproto/jetstream/feeds.go`, and `cmd/contract-manifest` fails if a consumed collection has no `// coves:ingestion-contract <collection>` marker in `tests/e2e/`, or a marker names a collection nobody consumes. Markers count only in files that build under `-tags e2e` (checked with `go/build`'s `MatchFile`). A file carrying a marker may not import `gorilla/websocket` or `internal/atproto/jetstream`, or call `testkit.DB`; this is an AST check. `tests/ci/pending_contracts.txt` is the burn-down for deferring a contract. It is empty, and `scripts/lib/runner-ready.sh` passes `-allow-pending=false`, so a line there fails both `make ci` and `make test-e2e`.

#### 3.4b API contracts and sign-in

One per client write surface, driving the AppView's XRPC write endpoints exactly as the mobile app calls them, and asserting the response and the synchronous effects: session issued, record URI returned, synchronously indexed rows present, blob accepted. Avatar and blob uploads are steps of the user and community contracts. Auth-boundary contracts check that every write NSID answers 401 to a client with no session, which also catches a route registered without the middleware.

`OAuthAuthMiddleware.RequireAuth` and `OptionalAuth` accept one credential: a sealed session token naming a row in the OAuth session store, minted only by `/oauth/callback`. `DualAuthMiddleware.RequireAuth` also accepts aggregator API keys and service JWTs; see [SECURITY_MODEL.md](SECURITY_MODEL.md). The `accessJwt` from `social.coves.actor.signup` is rejected. `(*AppView).SignIn(t, account)` in `tests/testkit/signin.go` gets a real session the way a browser does: it calls `/oauth/login`, signs in and consents through the PDS OAuth provider's sign-in API with the account's password, and hands the code to `/oauth/callback`. The returned token is the `coves_session` cookie, backed by a real DPoP-bound session, so forwarded writes work. There is no test-only mint in the AppView. A password- or DID-trusting mint could only store a non-DPoP token, which cannot forward a write and which `ResumeSession` deletes as corrupt on first use (`internal/atproto/oauth/session_operation.go`). `session_contract_test.go` pins the helper.

Use it as `viewer := p.AppView.As(p.AppView.SignIn(t, account))`, once per account and outside any `Await` loop. Login and callback share a limiter of 10 requests per minute per client IP, and each sign-in spends two, or three when a dev-mode AppView canonicalizes the host (`HandleLogin` in `internal/atproto/oauth/handlers.go`). Sign-in is proven only for accounts on the primary PDS.

Viewer-scoped behaviour uses the same helper; `timeline_contract_test.go` proves subscription fan-out end to end. Block enforcement is observed through consumer-health measurement windows: snapshot the counters, write the block, bound on a visible event from the same repo, re-read. Moderation contracts act as the bootstrap admins (`testkit.ModerationAdmin`), which `scripts/ci-bootstrap.sh` provisions only in the hermetic stack.

#### 3.4c Pipeline reliability and stack control

The reliability suite covers machinery that steady-state contracts never reach. Its scenarios: restart the AppView mid-stream and verify cursor resume; take it down and up and verify the cursor rewind replays without double-indexing, counted in rows rather than counters; verify a stale rev cannot resurrect a deleted record through a dead-letter redrive; recover a dead letter through the boot-time redrive pass with no test-only knob; and run an overlapping two-feed configuration, asserting connector names, because CI's single-feed topology hides the rev-gating overlap path production uses.

Tests run inside the stack with no Docker socket, so they ask the host to act. The channel is a project-scoped file path watched by a host process that `scripts/lib/ci-stack.sh` starts. The verbs are a closed set with no arguments: `ping`, `appview-stop`, `appview-start`, `appview-two-feed`, `appview-single-feed`. The watcher is not in the production image. `requireSingleFeedTopology` in the e2e `TestMain` refuses to run against a stack a previous run left in the two-feed configuration, because that would invalidate every measurement window in the tier.

Rules for all three classes:

1. T2 tests never instantiate consumers and never dial websockets. The AppView container consumes, as deployed; tests observe through serving endpoints and the consumer-health endpoint.
2. T2 runs serially (`-p 1 -parallel 1`, written once, in `run_pipeline_tier` in `scripts/lib/runner-ready.sh`). Contracts share one AppView, one database, one PLC, one cursor stream and both PDSes. If the tier outgrows its budget, shard stacks; do not interleave writers. Identities come from `UniqueID`, and each contract gets a synthetic client IP (`testkit.SyntheticClientIP`) because the AppView's rate limiter keeps one bucket per IP and outlives the run on a kept stack.
3. Behavioural breadth belongs at T0 and T1. When a contract fails, the answer to "which layer broke?" should be "the pipeline".
4. Sequential steps inside a contract are legitimate here and only here: eventual consistency is what is under test.
5. One cross-domain saga exists: `TestUserJourney` in `journey_test.go` (signup, community, post, comment, vote, timeline).
6. Negative bounds must be in one repo. Jetstream parallelises across repos, so "the bad record did not appear" bounded by a good record in another repo is luck. Write both to the same repo and bound on something the same subscription delivers, never on a counter that `identity` or `account` events also advance.
7. A pin is not a statement of intent. When a contract documents a known defect, the assertion message names the issue and says a failure means the defect is fixed, the wrong-but-current value is pinned with `Holds` as well as `WaitFor`, and the production code next to the defect carries a KNOWN DEFECT comment.

### 3.5 Command surface

```
make test              # T0. No Docker, no database, no public network. Loopback httptest is fine.
make test-integration  # T0+T1. Starts postgres-test, provisions the template, computes -p/-parallel.
make test-e2e          # T2, inside the hermetic stack via the compose runner.
make test-e2e-dev      # T2 against the dev stack. Debugging only.
make test-live         # T3. Opt-in, hits the internet. Missing config fails.
make ci                # The merge gate: hermetic stack, T0+T1+T2, egress blocked, ci-report verdict.
make test-audit        # The violation audit alone (§3.6.3). -v for file:line.
make test-db-prepare   # Create or refresh the template database.
make ci-clean          # Drop the CI module and build caches; the next make ci is cold.
```

- `make test-e2e` cannot run from the host: the hermetic stack publishes no host ports (§3.7). It brings the stack up, or reuses one kept with `COVES_CI_KEEP_STACK`, and runs the tier in the runner's network namespace. `run_pipeline_tier` in `scripts/lib/runner-ready.sh` is the one definition of the T2 invocation, shared by `make ci`, `make test-e2e` and `make test-e2e-dev`.
- `make test-e2e-dev` excludes by name (`-skip "^TestReliability|Federat|^TestModeration"`): the reliability suite needs the AppView container, federation needs `pds2` and the relay, and moderation needs the bootstrap admins. All three exist only in the hermetic stack.
- `make ci` runs, in order: build from the tree, staged compose up, bootstrap, contract manifest, `go vet` per tag set, `test-audit.sh` and `ssrf-audit.sh`, template provisioning, T0+T1 with `-tags integration`, the `-tags dev` OAuth run, T2, then `ci-report`.
- `go test`'s exit code is not the verdict; `ci-report` is, but a mismatch between them fails the gate. Two rules in `scripts/ci-runner.sh`: a `go test` status above 1 (a crashed or OOM-killed run) fails regardless of the report, and a nonzero status with an "ok" report means a truncated capture and fails. These rules are shell and have no automated test of their own.
- One stack per worktree. `scripts/lib/ci-stack.sh` derives the Compose project from the checkout directory (`coves` gives `coves-ci`, `coves-<suffix>` gives `coves-ci-<suffix>`), and `COVES_CI_PROJECT` overrides it. Only the Go module and build cache volumes are shared. Worktrees branched before 2026-09-01 still default to `coves-ci` and collide; pass `COVES_CI_PROJECT` there.
- `make test-integration` fails without the dev stack instead of skipping. Each package's `TestMain` declares what it needs, so a single `go test -tags integration ./internal/foo` needs only that.

### 3.6 Enforcement

All four are hard gates inside `make ci`.

1. `cmd/ci-report`, skip inversion. A skip fails unless `tests/ci/allowed_skips.txt` lists it with a reason. A listed entry that did not skip fails as stale, unless it is marked conditional with a leading `~`. The file is empty; its header asks the next person to add a line to check first whether the answer is to fix the test.
2. `cmd/contract-manifest`, the contract manifest. Every collection in `jetstream.ConsumedCollections()` has a live ingestion-contract marker in `tests/e2e/`, and no marker is stale (§3.4a).
3. `scripts/test-audit.sh`, the violation audit. In test code: `time.Sleep` (testkit included), `t.Skip` outside testkit, client-side websocket dials (`DefaultDialer`, `websocket.Dialer`, `NewClient`; the server-side `Upgrader` used by fake Jetstream servers is allowed), `localhost`/`127.0.0.1` host:port literals outside testkit, and public atProto hostnames in URLs outside `tests/live/`. Everywhere, production included: `testing.Short()`. In production code: raw request-body reads outside `internal/api/reqbody`, and an `xrpc.DecodeJSON` call whose result is discarded. Legitimate exceptions carry a reason marker on the same line (`coves:allow-sleep`, `coves:allow-host-literal`, `coves:allow-public-host`, `coves:allow-raw-body-decode`), or the `-file:` form for a whole file, which prints its reason on every run. There is no marker for `t.Skip`. These greps are tripwires, not proof: concatenation or an IP literal gets past them. The guarantee is §3.7 layer 3.
4. `go vet` per tag set: untagged, `-tags dev`, `-tags integration`, `-tags e2e ./tests/e2e/...`, `-tags live ./tests/live/...`. Tagged files that do not compile are otherwise invisible, and the `live` pass matters most because that tier never runs on the merge path.

`scripts/ssrf-audit.sh` runs beside the audit. It scans production code for unguarded outbound clients, and `tests/audit` checks its rules against planted violations. It is needed because `.env.ci` sets `IS_DEV_ENV=true`, so every SSRF guard in the suite takes its permissive branch. See [SSRF_SECURITY.md](SSRF_SECURITY.md).

### 3.7 Network isolation

1. All endpoints come from testkit, and all defaults point at the Docker stack. `testkit.Endpoints()` reads `POSTGRES_TEST_*`, `PDS_URL`, `PDS2_URL`, `PDS_SERVICE_HANDLE_DOMAINS`, `PDS2_SERVICE_HANDLE_DOMAINS`, `PLC_DIRECTORY_URL`, `JETSTREAM_TEST_URL` and `APPVIEW_URL` (falling back to `APPVIEW_PUBLIC_URL`) once per process. No other test code builds a base URL. The Jetstream variable is not the server's `JETSTREAM_FEEDS`, so the tests' view of the stack is not coupled to the AppView's consumer config. `PDS2_URL` has no default because `pds2` exists only in the CI stack: `RequireFederatedPDS` passes when it is unset, and `NewFederatedPDS` fails by name if a federation contract is reached without it.
2. Lint tripwire (§3.6.3). Public atProto hostnames in test code outside `tests/live/` fail the build. Only hostnames inside a URL with a scheme count, because the target is endpoint construction, not fixture data such as a handle in a table-driven test. What remains and is legitimate (a URL parser's inputs, a config test asserting the production default) carries a marker.
3. Egress-blocked CI network, the real guarantee. The hermetic stack's network is `internal: true`, so any public call fails at connect time. The runner downloads Go modules at run time, so `scripts/lib/ci-stack.sh` prefetches into the module-cache volume before the runner joins the internal network, and `GOPROXY=off` is set on the runner service only. A cold-cache, egress-blocked `make ci` is a supported run.

Egress failures are fast: DNS returns nothing in milliseconds. A test that tolerates a fallback does not hang or skip; it takes the fallback and passes.

Tests that hit real PLC or real handles live in T3, where the `live` tag is the opt-in.

## 5. Decisions

- Build tags over directories: naming conventions did not survive. Tags are compile-time and greppable. The corollary is single-tier files.
- T1 next to the code: repo tests beside the repo get maintained when the repo changes. A catch-all `tests/integration` did not.
- Template clone per test over a shared database with cleanup: cleanup discipline is what failed. Isolation by construction survives tests written at speed.
- Direct-PDS ingestion contracts over client-path E2E: synchronous indexing means endpoint-in, endpoint-out tests cannot prove the pipeline.
- Serial T2: one shared namespace makes interleaved eventually-consistent writers a flake source.
- Black-box T2 over test-instantiated consumers: it tests the wiring that ships.
- Generated contract manifest over a reviewed list: the hand-written list was wrong on day one, missing both block collections and merging the two aggregator types.

## 6. Known limitations

1. T2 sign-in depends on the PDS OAuth provider's sign-in page API (§3.4b). `AppView.SignIn` speaks the JSON API behind `@atproto/oauth-provider`'s sign-in and consent pages (`~api/sign-in`, `~api/consent`, their CSRF, `Sec-Fetch-*` and `Referer` rules). That is an internal UI API, not a protocol. The PDS image is pinned by digest in `docker-compose.ci.yml`; a PDS bump that changes the API fails every `SignIn` loudly, naming the step. Re-check `signin.go` on every PDS image bump.

2. Federated authors cannot be indexed in CI (§3.4a). The users consumer default-denies identities from untrusted hosts, so a record authored on `pds2` indexes (comments, votes and blobs have no author foreign key) but its author never gets a row. The earlier blockers are fixed (`HANDLE_WELL_KNOWN_HOSTS` lets `pds2` handles verify in-stack; the identity cache no longer stores unverified rows). What remains is that `TRUSTED_BRIDGE_PDS_HOSTS` is unset in `.env.ci`. The unlock is to set it for `pds2` and write the contract.

3. `hostedBy` DID verification is off in CI (`SKIP_DID_WEB_VERIFICATION=true` in `.env.ci`). Community contracts prove the field is transported, not verified, and say so. The unlock is an injectable HTTP client on the community service.

4. `-p 1` is still required (§3.3). Jetstream `account` and `identity` events bypass the wanted-collection filter, so parallel package binaries starve each other's subscribers. The unlock is a change to what the consumers subscribe to; the suite side is ready (`packageParallelism` is one constant and the per-package budget is computed).

5. `tests/fixtures` is a leaf by convention only. testkit's dependency rule (§3.3) is compiler-enforced because a violation is an import cycle. `tests/fixtures` imports `internal/core` packages on purpose, so it must stay a package that only tests import, and a test that imports it must be in an external test package (`package foo_test`). Nothing checks the first rule; the compiler catches the second only where the import forms a cycle.

6. The audit's greps are tripwires, not proofs (§3.6.3). A URL built by concatenation, an IP literal or a sleep behind a helper is invisible to them. A green audit means no obvious drift.

7. One known flake vector. A Jetstream reconnect within the 5-second cursor rewind can double-count a permanent dead letter: the row dedupes on conflict but the in-memory counter increments anyway. The message in `requireRejected` (`block_contract_test.go`, also used by the federation contracts) says so.

8. Identity events are not ordered across feeds, and identity refreshes are serialized per DID only in the user and community consumers and the handle sweep. Identity events carry no `rev`, so the rev gate that orders commits across overlapping feeds cannot see them, and cross-feed ordering is still not enforced. `handleIdentityEvent` in `internal/atproto/jetstream/user_consumer.go` ignores the event's handle and writes the handle from a fresh, verified resolution of the DID, so a stale or reordered event cannot revert a handle. `RefreshIdentityHandle` (`internal/atproto/jetstream/identity_refresh.go`) runs everything from the DID purge to the handle write inside a session-scoped PostgreSQL advisory lock keyed by the DID (`IdentityRefreshLock`, `NewPostgresIdentityRefreshLock`), held on a small dedicated pool (`openIdentityRefreshLockDatabase` in `cmd/server/database.go`). Refreshes of one DID therefore run one after another across every consumer feed, the redriver and every process that shares the database, and the stored handle is re-read inside the lock. The wait for the lock is bounded; a refresh that cannot get it in time defers the event to the redriver. `handleIdentityEvent` in `user_consumer.go` and `handleIdentityEvent` in `community_consumer.go` both take the lock. The handle sweep (`cmd/reconcile-handles`) takes it too, through `RefreshUserHandle` and `RefreshCommunityHandle`, on its own small pool (`MaxOpenConns` = `-concurrency`), and reports a DID whose lock it cannot get in time as `lock-busy`. The same unfiltered stream is what keeps `-p 1` (item 4).
