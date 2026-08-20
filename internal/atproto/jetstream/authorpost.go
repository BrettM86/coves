package jetstream

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"Coves/internal/atproto/identity"
	"Coves/internal/atproto/oauth"
	"Coves/internal/core/communities"
	"Coves/internal/core/posts"
	"Coves/internal/core/users"

	"github.com/bluesky-social/indigo/atproto/atdata"
	"github.com/bluesky-social/indigo/repo"
)

// Ingesting author-owned posts and the community records that decide about
// them (docs/PRD_AUTHOR_OWNED_POSTS.md §5.3-§5.6).
//
// Three collections land here, and they invert each other:
//
//   - social.coves.community.postv2 arrives from the AUTHOR's repo, so
//     event.Did IS the author and the community is a claim the record makes.
//   - social.coves.community.acceptance and .removal arrive from the
//     COMMUNITY's repo, so event.Did IS the community and the post is a
//     subject the record names.
//
// The old community.post path (still in post_consumer.go) checks that the repo
// DID EQUALS the record's community. Here that check splits in two opposite
// directions, which is why these handlers are their own file rather than more
// branches in the old ones.

// PostV2Collection is the author-repo post record of
// docs/PRD_AUTHOR_OWNED_POSTS.md §3.1 — the §3.0 successor to the deprecated
// social.coves.community.post.
//
// The NSID is new rather than reused, and that is a safety property, not
// bookkeeping: a consumer built against the published community.post derives
// community = repo DID for that collection, so feeding it author-repo records
// under the same name would have it index authors as communities. A new NSID
// makes a stale consumer ignore the records entirely, which is the correct
// failure mode.
//
// It is an alias for the domain's constant rather than a second spelling of the
// string: the read path resolves post URIs against the same name, and two
// literals would let the indexer and the reader drift into disagreeing about
// what a post record is called.
const PostV2Collection = posts.PostV2Collection

// DeletedAccountLookup reports whether a DID names an account this AppView was
// asked to erase (migration 036, PRD rev 2.7).
//
// It exists because "no users row" stopped being an answer. Under author-owned
// posts an unknown author is a NORMAL state that must still index (§5.3), so
// the absence of a profile can no longer stand in for "this account is gone" —
// and without a marker, a redriven post event or a replayed acceptance quietly
// recreates the very rows the deletion swept.
//
// A lookup FAILURE must never be read as "not deleted". Failing open here means
// a database blip re-indexes an erased account's content, which is the one
// outcome a deletion is supposed to make impossible.
type DeletedAccountLookup interface {
	IsAccountDeleted(ctx context.Context, did string) (bool, error)
}

// WithAdmissions installs the per-(community, post) admission store. Without
// it, postv2 and acceptance/removal events have nowhere to record a decision.
func WithAdmissions(admissions posts.AdmissionRepository) PostEventConsumerOption {
	return func(c *PostEventConsumer) { c.admissions = admissions }
}

// WithDeletedAccounts installs the erased-account gate. Without it, no gate
// runs and every event indexes — the pre-036 behaviour.
func WithDeletedAccounts(lookup DeletedAccountLookup) PostEventConsumerOption {
	return func(c *PostEventConsumer) { c.deletedAccounts = lookup }
}

// WithPostRecordFetcher installs the §5.4 direct fetch used when an acceptance
// names a post this AppView has never indexed.
func WithPostRecordFetcher(fetcher PostRecordFetcher) PostEventConsumerOption {
	return func(c *PostEventConsumer) { c.postFetcher = fetcher }
}

// AcceptanceDeleter withdraws a community's acceptance of a post. Satisfied by
// posts.CommunityRecordWriter.
//
// Narrowed to one method because that is all the
// tombstone path needs: the consumer must never write an acceptance, a removal
// or a repin — those are the ENGINE's verdicts, and a consumer holding the full
// writer is one edit away from making one.
type AcceptanceDeleter interface {
	DeleteAcceptance(ctx context.Context, cmd posts.CommunityAcceptanceDeleteCommand) (posts.CommunityWriteResult, error)
}

// WithAcceptanceCleanup installs the host-side sweep that withdraws a
// community's acceptance when the AUTHOR deletes their post (§5.3).
//
// Only the HOST can do this — the acceptance lives in the community's repo and
// needs its keys — so the sweep is silently a no-op for every community this
// AppView does not host, and that is the common case on any instance that is
// not the community's home. nil disables it entirely.
func WithAcceptanceCleanup(deleter AcceptanceDeleter) PostEventConsumerOption {
	return func(c *PostEventConsumer) { c.acceptanceCleanup = deleter }
}

// ---------------------------------------------------------------------------
// §5.4 direct fetch
// ---------------------------------------------------------------------------

// FetchedPost is one author-repo record read directly from its PDS.
//
// It carries the CID separately from the record because the CID is what the
// caller VERIFIES: an acceptance pins a strongRef, and a fetch that returned
// only the record body would leave the consumer indexing whatever the author's
// PDS felt like serving under that rkey.
type FetchedPost struct {
	URI    string
	CID    string
	Record map[string]interface{}
}

// PostRecordFetcher reads an author's post record straight from their PDS.
//
// This is what makes firehose-only ingestion actually CONVERGE (§5.4).
// Acceptance-before-post does not converge by dead-letter redrive alone:
// bounded retries cannot manufacture a post event that a relay-coverage gap
// will never deliver. Redrive stays the backstop for transient failures; this
// is the mechanism.
type PostRecordFetcher interface {
	// FetchPost resolves the repo DID in postURI to a PDS and reads the record.
	// The returned CID is the PDS's, unverified — checking it against the CID an
	// acceptance pinned is the caller's job, because only the caller knows what
	// was pinned.
	FetchPost(ctx context.Context, postURI string) (*FetchedPost, error)
}

// DirectPostFetcher is the production PostRecordFetcher: DID resolution, then
// com.atproto.repo.getRecord over an SSRF-guarded client with a size cap.
type DirectPostFetcher struct {
	resolver identity.Resolver

	// allowPrivateHosts disables the SSRF protection that blocks private and
	// loopback addresses. Reachable only through withPrivateHostsAllowed, which is
	// unexported, so the one production route in is PrivatePostFetcherOptions
	// carrying cmd/server's allowPrivateHosts() — false everywhere but dev. It
	// exists for the same reason blueskypost's allowPrivateHost does: a dev
	// stack's PDS, and this package's own fixtures, listen on loopback, which is
	// exactly what the guard that must stay on in production refuses.
	//
	// The guard is not decorative here. The DID document that names the PDS is
	// attacker-controlled — anyone can publish one — so an unguarded fetcher is
	// a request forger pointed at whatever is reachable from the AppView's
	// network, driven by any stranger who writes an acceptance record.
	allowPrivateHosts bool

	// client is the guarded HTTP client, built once. See httpClient.
	clientOnce sync.Once
	client     *http.Client
}

// fetchTimeout bounds ONE direct fetch, inside the client's own 15-second
// ceiling.
//
// The two are not redundant. The client's timeout is a backstop for a
// connection that hangs; this one is a policy about how long the posts lane may
// wait on a stranger's PDS. The lane is single-threaded and now carries four
// collections, so every second spent here is a second nothing else is indexed —
// and the destination is chosen by whoever wrote the acceptance. Five seconds
// is generous for a repo read and cheap to lose.
const fetchTimeout = 5 * time.Second

// maxFetchedRecordBytes bounds how much of a PDS getRecord response is read.
//
// A post record has a lexicon-bounded size, so a PDS streaming megabytes is
// either broken or hostile — and the host is chosen by a stranger's record, so
// an unbounded read here is a memory-exhaustion primitive handed to the public.
// The cap mirrors users.maxProfileResponseBytes, which bounds the same call for
// the same reason.
const maxFetchedRecordBytes = 1 << 20 // 1 MiB

// maxFetchErrorDetailBytes caps how much of a failing PDS response is echoed
// into the logs, so a hostile host cannot flood them.
const maxFetchErrorDetailBytes = 256

// DirectPostFetcherOption configures the §5.4 fetch at construction time.
//
// Construction state, never a read inside the fetch: an environment read at the
// call site would make the guarded branch untestable alongside t.Parallel and
// would hide the most consequential input to a security decision from the place
// that makes it.
type DirectPostFetcherOption func(*DirectPostFetcher)

// NewDirectPostFetcher wires the §5.4 fetch. SSRF protection is ON unless an
// option stands it down, and it takes no boolean: a constructor that accepted
// one is a constructor someone eventually passes true to from production wiring.
func NewDirectPostFetcher(resolver identity.Resolver, opts ...DirectPostFetcherOption) *DirectPostFetcher {
	f := &DirectPostFetcher{resolver: resolver}
	for _, opt := range opts {
		opt(f)
	}
	return f
}

// withPrivateHostsAllowed stands the SSRF guard down on the direct fetch.
//
// UNEXPORTED, AND THAT IS THE WHOLE POINT. It replaced NewDevDirectPostFetcher,
// whose doc comment claimed that "no production wiring can reach it by passing a
// variable that happens to be true" — but that constructor was EXPORTED, so any
// package, cmd/server included, could call it directly and open the hatch. The
// guarantee was prose. This is the type system: outside this package the hatch is
// unreachable, and the only way in is PrivatePostFetcherOptions, whose false
// branch returns nothing.
//
// pds/factory.go's withTransportOptions and this package's own
// withWellKnownHTTPClient are unexported for exactly this hazard. Every
// legitimate caller of the hatch is a fixture in this package, so it costs them
// nothing.
func withPrivateHostsAllowed() DirectPostFetcherOption { // coves:allow-ssrf-hatch: this IS the hatch itself; the name is the contract
	return func(f *DirectPostFetcher) { f.allowPrivateHosts = true }
}

// PrivatePostFetcherOptions returns the options a caller holding an allow-private
// boolean should pass to NewDirectPostFetcher: the hatch when it is set, and
// NOTHING when it is not.
//
// It mirrors PrivateHostOptions above and oauth.PrivateAddressOptions, and it is
// a function rather than an `if` in cmd/server/consumers.go for the reason
// documented there: `.env.ci:140` sets IS_DEV_ENV=true, so `make ci` takes the
// PERMISSIVE branch at every call site holding such a boolean. A unit test
// against this function is the only place in the repository where the branch
// production actually runs is ever evaluated. Do not inline it back.
//
// FALSE RETURNS ZERO OPTIONS, AND THAT IS THE CONTRACT — not "options that are
// safe", but none, so that what production gets is exactly the constructor's own
// defaults.
func PrivatePostFetcherOptions(allowPrivate bool) []DirectPostFetcherOption {
	if !allowPrivate {
		return nil
	}
	return []DirectPostFetcherOption{withPrivateHostsAllowed()} // coves:allow-ssrf-hatch: the gate helper allow-branch; its false branch returns nothing
}

// httpClient returns the guarded client, building it once on first use.
//
// ONE CLIENT, not one per fetch. A fresh http.Client means a fresh
// http.Transport, and a fresh transport means an empty connection pool: every
// fetch would pay a new TCP handshake and a new TLS handshake against a PDS
// this consumer may be about to fetch from a hundred more times, and the
// discarded transports leak idle connections until their finalizers run. The
// guard is a property of the transport rather than of the moment, so nothing
// about correctness needed it rebuilt.
//
// It is built lazily rather than in the constructor so that a fetcher which is
// wired but never used — the common case on an instance hosting no communities
// — costs nothing.
func (f *DirectPostFetcher) httpClient() *http.Client {
	f.clientOnce.Do(func() {
		f.client = oauth.NewSSRFSafeHTTPClient(oauth.PrivateAddressOptions(f.allowPrivateHosts)...)
	})
	return f.client
}

// FetchPost implements PostRecordFetcher.
func (f *DirectPostFetcher) FetchPost(ctx context.Context, postURI string) (*FetchedPost, error) {
	repoDID, collection, rkey, ok := parseRecordURI(postURI)
	if !ok {
		return nil, fmt.Errorf("cannot fetch %q: not an at:// record URI", postURI)
	}
	if f.resolver == nil {
		return nil, fmt.Errorf("cannot fetch %s: no identity resolver is wired", postURI)
	}

	// The PDS is resolved from the DID document rather than taken from anything
	// the acceptance record said. The record names a subject; where that
	// subject's repo lives is a fact about the DID, and letting a record assert
	// it would let the record choose the host this request goes to.
	resolved, err := f.resolver.Resolve(ctx, repoDID)
	if err != nil {
		return nil, fmt.Errorf("resolving the repo of %s: %w", postURI, err)
	}
	if resolved == nil || resolved.PDSURL == "" {
		return nil, fmt.Errorf("resolving the repo of %s: no PDS endpoint in the DID document", postURI)
	}

	// com.atproto.sync.getRecord, NOT repo.getRecord, and the difference is the
	// whole trustworthiness of this path.
	//
	// repo.getRecord answers with JSON — {"uri":…, "cid":…, "value":{…}} — whose
	// `cid` is a CLAIM BY THE SERVER. That server is the author's PDS: named by
	// a DID document, reached because a stranger wrote an acceptance naming this
	// subject. Comparing the pinned CID against that field asks the attacker
	// whether the attacker is lying, and the consequence is the worst one this
	// design has — the AppView indexes whatever `value` holds under a
	// community's SIGNED acceptance of a CID that content does not have. The
	// community attested to one thing and every reader is shown another.
	//
	// sync.getRecord answers with a CAR: the repo's own blocks. The CID is then
	// RECOMPUTED from the bytes rather than read off a label, and no server can
	// lie about the hash of what it just sent.
	endpoint := strings.TrimSuffix(resolved.PDSURL, "/") + "/xrpc/com.atproto.sync.getRecord?did=" +
		url.QueryEscape(repoDID) + "&collection=" + url.QueryEscape(collection) +
		"&rkey=" + url.QueryEscape(rkey)

	fetchCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("building the getRecord request for %s: %w", postURI, err)
	}

	resp, err := f.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s from its PDS: %w", postURI, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// One byte past the cap, so an over-cap body is DETECTED rather than
	// silently truncated and then parsed as if it were whole. A truncated
	// record that happened to parse would be indexed as the author's content.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchedRecordBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading the getRecord response for %s: %w", postURI, err)
	}
	if len(body) > maxFetchedRecordBytes {
		return nil, fmt.Errorf("the PDS serving %s returned more than %d bytes", postURI, maxFetchedRecordBytes)
	}

	if resp.StatusCode != http.StatusOK {
		detail := string(body)
		if len(detail) > maxFetchErrorDetailBytes {
			detail = detail[:maxFetchErrorDetailBytes]
		}
		// THE CLASSIFICATION IS THE EXPENSIVE PART OF THIS FUNCTION. The
		// connector reads the returned error's shape: ErrPermanentEvent is
		// dead-lettered with its redrive budget already spent, and anything
		// else costs three inline retries (~4.2s of a blocked lane that also
		// carries posts) plus ten redrives. So a non-200 has to be sorted, not
		// merely reported.
		//
		// A GENUINE XRPC RecordNotFound is a definite fact about the repo: the
		// PDS was reached, understood the question, and answered that the
		// record is not there. Nothing a retry does changes it — and left
		// transient, any community can mint unlimited lane-blocking by writing
		// acceptances for URIs nobody ever wrote.
		//
		// A BARE 404 is the opposite, and the distinction is not pedantry:
		// with no XRPC envelope the request most likely never reached a PDS at
		// all — a stale pds_url pointing at a reverse proxy or a generic web
		// server, both of which 404 everything. Reading that as proof the
		// record does not exist would permanently discard a real post over a
		// misconfigured hostname. Everything else, 5xx included, is the PDS
		// having a bad time and is transient by definition.
		//
		// users.FetchProfileRecord draws exactly this line for exactly this
		// reason; the predicates are shared with it rather than re-derived.
		if users.IsRecordNotFoundResponse(resp.StatusCode, body) {
			return nil, fmt.Errorf("%w: the PDS serving %s answered getRecord with status %d: %s",
				ErrPermanentEvent, postURI, resp.StatusCode, strconv.Quote(detail))
		}
		// Quoted so control characters and ANSI escapes from a hostile PDS
		// cannot corrupt log output.
		return nil, fmt.Errorf("the PDS serving %s answered getRecord with status %d: %s",
			postURI, resp.StatusCode, strconv.Quote(detail))
	}

	// The CAR carries the record's block plus the blocks proving it belongs to
	// the repo. Reading it can fail on a hostile or broken server, which is a
	// refusal like any other — a response that is not a CAR is not evidence.
	stored, err := repo.ReadRepoFromCar(ctx, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("reading the CAR for %s: %w", postURI, err)
	}

	claimedCID, recordBytes, err := stored.GetRecordBytes(ctx, collection+"/"+rkey)
	if err != nil {
		return nil, fmt.Errorf("reading %s out of the fetched CAR: %w", postURI, err)
	}
	if recordBytes == nil || len(*recordBytes) == 0 {
		return nil, fmt.Errorf("the CAR for %s carried no record bytes", postURI)
	}

	// THE RECOMPUTATION, which is the entire point of taking a CAR at all. The
	// CID that came out of the repo structure is still something the server
	// assembled; hashing the record's own bytes under that CID's codec and
	// multihash is what turns it into a fact. A server that substituted content
	// produces a digest that does not match, whatever it labelled the block.
	computedCID, err := claimedCID.Prefix().Sum(*recordBytes)
	if err != nil {
		return nil, fmt.Errorf("recomputing the CID of %s: %w", postURI, err)
	}
	if !computedCID.Equals(claimedCID) {
		return nil, fmt.Errorf("%w: the CAR for %s labels a block %s whose bytes hash to %s",
			ErrPermanentEvent, postURI, claimedCID, computedCID)
	}

	record, err := atdata.UnmarshalCBOR(*recordBytes)
	if err != nil {
		return nil, fmt.Errorf("decoding the record block of %s: %w", postURI, err)
	}

	// The CID reported back is the COMPUTED one. The caller compares it against
	// what the acceptance pinned, and handing back the label instead would put
	// the server's claim back into the comparison the recomputation just removed
	// it from.
	return &FetchedPost{URI: postURI, CID: computedCID.String(), Record: record}, nil
}

// ---------------------------------------------------------------------------
// The author-repo post record
// ---------------------------------------------------------------------------

// AuthorPostRecord is a social.coves.community.postv2 record as it arrives from
// Jetstream.
//
// It has NO author field, and that absence is enforced by the type rather than
// by discipline: authorship comes from the repo the record lives in, so a
// struct that could hold an author is a struct someone eventually reads one
// from — which is precisely the impersonation the flip removed. The lexicon has
// no such property either, so a record carrying one is a forger's field and is
// ignored here by construction.
type AuthorPostRecord struct {
	Title        *string                    `json:"title,omitempty"`
	Content      *string                    `json:"content,omitempty"`
	Embed        map[string]interface{}     `json:"embed,omitempty"`
	Labels       *posts.SelfLabels          `json:"labels,omitempty"`
	BridgedStats *BridgedStatsFromJetstream `json:"bridgedStats,omitempty"`
	Type         string                     `json:"$type"`
	Community    string                     `json:"community"`
	CreatedAt    string                     `json:"createdAt"`
	Facets       []interface{}              `json:"facets,omitempty"`
}

// parseAuthorPostRecord converts a raw Jetstream record map into an
// AuthorPostRecord, refusing the shapes that can never become valid.
func parseAuthorPostRecord(record map[string]interface{}) (*AuthorPostRecord, error) {
	recordJSON, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal postv2 record: %w", err)
	}

	var parsed AuthorPostRecord
	if err := json.Unmarshal(recordJSON, &parsed); err != nil {
		// PERMANENT: the record's shape doesn't match the lexicon (wrong field
		// types); replaying the identical bytes can never parse differently.
		return nil, fmt.Errorf("%w: failed to unmarshal postv2 record: %v", ErrPermanentEvent, err)
	}

	// PERMANENT for the same reason: a record missing a required field is
	// structurally invalid forever. `community` is the submission target, so a
	// record without one names no admission subject at all.
	if parsed.Community == "" {
		return nil, fmt.Errorf("%w: postv2 record missing community field", ErrPermanentEvent)
	}
	if parsed.CreatedAt == "" {
		return nil, fmt.Errorf("%w: postv2 record missing createdAt field", ErrPermanentEvent)
	}

	return &parsed, nil
}

// ---------------------------------------------------------------------------
// postv2: the author's own post record
// ---------------------------------------------------------------------------

// handleAuthorPostEvent routes one social.coves.community.postv2 commit.
//
// event.Did is the AUTHOR, unconditionally and for every operation below.
func (c *PostEventConsumer) handleAuthorPostEvent(ctx context.Context, event *JetstreamEvent, commit *CommitEvent) error {
	authorDID := event.Did

	// THE ERASURE GATE, and it runs before anything else touches the database —
	// before parsing, before hydration, before the rev gate. An event from an
	// account this AppView was asked to forget has nothing to do, and the
	// cheapest way to guarantee that is to leave before any code path that
	// could write. Ordering it after a parse would also mean a malformed record
	// from an erased account dead-letters, which is operational noise about
	// content nobody may keep.
	erased, err := c.authorWasErased(ctx, authorDID)
	if err != nil {
		return err
	}
	if erased {
		// Nil, not an error. The connector dead-letters whatever a handler
		// returns, so refusing here would fill the queue with rows that redrive,
		// fail identically and retire — every erased account becoming a
		// permanent stream of noise. This is not a failure; it is an event with
		// nothing to do.
		log.Printf("INFO: dropping %s %s for erased account %s (migration 036 marker)",
			PostV2Collection, commit.Operation, authorDID)
		return nil
	}

	switch commit.Operation {
	case "create", "update":
		return c.upsertAuthorPost(ctx, authorDID, commit, event.TimeUS)
	case "delete":
		return c.tombstoneAuthorPost(ctx, authorDID, commit)
	}
	return nil
}

// tombstoneAuthorPost soft-deletes the author's post and then withdraws any
// acceptance a community this AppView HOSTS still holds for it (§5.3).
//
// THE ORDER IS THE CONTRACT. The tombstone is the local truth and lands first:
// the author asked for their post to be gone, and a community PDS that cannot
// be reached must not keep this AppView serving it. The withdrawal is
// best-effort cleanup of a REMOTE repo, and it is deliberately not allowed to
// hold the deletion hostage.
func (c *PostEventConsumer) tombstoneAuthorPost(ctx context.Context, authorDID string, commit *CommitEvent) error {
	uri := recordURI(authorDID, PostV2Collection, commit.RKey)

	// Read before the tombstone, because the community is what says WHOSE
	// acceptance to withdraw and a delete event carries no record to read it
	// from. The soft delete leaves the row in place, so this could equally run
	// afterwards; doing it first keeps the sweep off the path when the post was
	// never indexed here at all.
	stored, indexed, err := c.loadStoredPost(ctx, uri)
	if err != nil {
		return err
	}

	applied, err := c.tombstoneRecordIfRevWins(ctx, uri, commit.Rev)
	if err != nil {
		return err
	}
	if !indexed {
		return nil
	}

	// THE WITHDRAWAL IS RECONSIDERED ON EVERY DELIVERY, unlike the tombstone.
	// The gate exists to make the local soft-delete happen once; the withdrawal
	// is a write into a REMOTE repo that can fail on its own, and it is
	// idempotent — DeleteAcceptance reports "nothing to withdraw" as a skip.
	//
	// Tying it to the gate is what stranded it: a PDS briefly unreachable on the
	// first delivery meant the acceptance stayed standing, pointing at a record
	// nobody can fetch, permanently — because every redelivery was rejected by
	// the gate before the sweep was reconsidered, and nothing else revisits it.
	// The community's CAR, the thing its portability argument rests on, would
	// keep citing content the author withdrew.
	//
	// The guard is the POST's state rather than this event's: sweep when the row
	// is tombstoned, which is true on the delivery that applied it and on every
	// one after.
	if applied || stored.deletedAt != nil {
		c.withdrawAcceptance(ctx, stored.communityDID, uri)
	}
	return nil
}

// withdrawAcceptance asks the community's host to delete its acceptance of a
// post whose author has just deleted it.
//
// It is a NO-OP unless three things are true, and each exclusion removes a
// large class of pointless work:
//
//   - a sweep is wired at all (nil on any build without the community writer);
//   - THIS AppView holds the community's credentials — the acceptance lives in
//     the community's repo and needs its keys, so on any instance that is not
//     the community's home this is silently not our job, which is the common
//     case;
//   - an acceptance actually stands, per the AppView's own admission row.
//     Consulting it is what keeps this from being a PDS round trip per delete
//     event, since most posts a community sees it never accepted.
//
// A failure is LOGGED AND SWALLOWED. Returning it would dead-letter an event
// whose local half already committed, and the redrive would then be rejected by
// the rev gate — so the retry could never reach this code again anyway. The
// standing acceptance is left pointing at a deleted record until something
// revisits it, which nothing currently does: that gap is real, bounded to
// hosted communities, and named here rather than hidden.
func (c *PostEventConsumer) withdrawAcceptance(ctx context.Context, communityDID, postURI string) {
	if c.acceptanceCleanup == nil || communityDID == "" {
		return
	}

	admission, err := c.admissions.Get(ctx, communityDID, postURI)
	if err != nil {
		if !errors.Is(err, posts.ErrNotFound) {
			log.Printf("[ACCEPTANCE-SWEEP] Warning: could not read the admission of %s in %s: %v",
				postURI, communityDID, err)
		}
		return
	}
	// The URI, not the status. `accepted` and `pending_reacceptance` both have a
	// live acceptance record standing in the community's repo — the second
	// merely pins content the author has since edited — and both must be
	// withdrawn when the subject itself is deleted.
	if admission == nil || admission.AcceptanceURI == nil {
		return
	}

	result, err := c.acceptanceCleanup.DeleteAcceptance(ctx, posts.CommunityAcceptanceDeleteCommand{
		CommunityDID: communityDID,
		PostURI:      postURI,
	})
	switch {
	case errors.Is(err, posts.ErrCommunityNotHosted):
		// Not this instance's community. Expected, and not worth a warning:
		// every AppView sees the deletions of every community it indexes.
		log.Printf("debug: not withdrawing the acceptance of %s — %s is hosted elsewhere", postURI, communityDID)
	case err != nil:
		log.Printf("[ACCEPTANCE-SWEEP] Warning: could not withdraw the acceptance of %s in %s; "+
			"the record now cites a deleted post: %v", postURI, communityDID, err)
	case result.Skipped:
		log.Printf("debug: no acceptance of %s stood in %s to withdraw", postURI, communityDID)
	default:
		log.Printf("✓ Withdrew the acceptance of deleted post %s in %s", postURI, communityDID)
	}
}

// canRecordAdmissions reports whether this consumer has somewhere to put a
// decision.
//
// Every one of the three author-owned collections exists to write an admission
// row, so a consumer built without the store cannot handle any of them — it is
// running in its pre-034 shape. Ignoring the events is the honest answer:
// indexing a postv2 with no admission row would publish a post no community
// ever decided about, which is worse than not indexing it at all. It is logged
// because in production this is always a wiring bug.
func (c *PostEventConsumer) canRecordAdmissions(collection string) bool {
	if c.admissions != nil {
		return true
	}
	log.Printf("WARNING: ignoring %s event - this consumer has no admissions store wired", collection)
	return false
}

// authorWasErased reports whether this DID carries a migration-036 erasure
// marker. A lookup failure is an ERROR, never a false: failing open would
// re-index the content a deletion erased, which is the one outcome the marker
// exists to prevent. With no lookup wired the gate is absent and everything
// indexes, which is the pre-036 behaviour.
func (c *PostEventConsumer) authorWasErased(ctx context.Context, did string) (bool, error) {
	if c.deletedAccounts == nil {
		return false, nil
	}
	erased, err := c.deletedAccounts.IsAccountDeleted(ctx, did)
	if err != nil {
		return false, fmt.Errorf("checking the erasure marker for %s: %w", did, err)
	}
	return erased, nil
}

// upsertAuthorPost indexes an author-repo post and opens (or refreshes) the
// pending admission the community will decide against.
//
// A postv2 event never DECIDES anything. It records content plus the fact that
// the author submitted it; whether the community shows the post lives in
// community_post_admissions and is written only by community events.
func (c *PostEventConsumer) upsertAuthorPost(ctx context.Context, authorDID string, commit *CommitEvent, timeUS int64) error {
	if commit.Record == nil {
		return fmt.Errorf("%w: postv2 %s event missing record data", ErrPermanentEvent, commit.Operation)
	}

	record, err := parseAuthorPostRecord(commit.Record)
	if err != nil {
		return err
	}

	uri := recordURI(authorDID, PostV2Collection, commit.RKey)

	stored, found, err := c.loadStoredPost(ctx, uri)
	if err != nil {
		return err
	}

	// IMMUTABILITY (§3.1) IS CHECKED FIRST, BEFORE THE COMMUNITY IS LOOKED UP,
	// and the order is load-bearing rather than tidy.
	//
	// An update that changes `community` invalidates the WHOLE event — not
	// merely the community field, because applying the content while keeping the
	// old community would leave the first community's admission holding a CID it
	// never evaluated, publishing content nobody judged under a standing
	// acceptance. Retargeting a post means writing a new record.
	//
	// Two rules meet when the retarget names a community nobody has indexed, and
	// only one of them can go first. The unknown-community branch below is
	// TRANSIENT — correctly, since a community's own profile event may simply
	// not have arrived — so checking it first would turn an illegal retarget
	// into a retryable failure that can NEVER succeed: it dead-letters, redrives
	// ten times, blocks ~4.2s inline on each delivery, and is still an illegal
	// retarget once that community exists. An author could mint that load at
	// will by editing one field.
	//
	// A skip, not an error: an invalid record from a stranger's repo is not an
	// infrastructure failure.
	if found && stored.communityDID != record.Community {
		log.Printf("🚨 SECURITY: ignoring the whole %s update for %s - community is immutable (stored %s, incoming %s)",
			PostV2Collection, uri, stored.communityDID, record.Community)
		return nil
	}

	// The community must be one this AppView has indexed, or there is no
	// subject to open an admission against.
	//
	// Deliberately NOT permanent: BigSky preserves order within a repo, not
	// across repos, so a post can genuinely arrive before the community's own
	// profile event. Marking this permanent would discard every post that
	// merely arrived early, with the redrive that would have fixed it already
	// spent.
	if _, err := c.communityRepo.GetByDID(ctx, record.Community); err != nil {
		if communities.IsNotFound(err) {
			log.Printf("Error: cannot index %s before its community %s is indexed", uri, record.Community)
			return fmt.Errorf("community not found: %s - cannot index post before community", record.Community)
		}
		return fmt.Errorf("%w: failed to verify community %s exists: %v", errValidationInfra, record.Community, err)
	}

	// Provenance for bridgedStats is keyed on the AUTHOR's PDS now, because the
	// record lives in the author's repo. The community's host has no say over
	// what an author asserts about their own record any more, so checking the
	// community's PDS (as the community-repo path does) would trust the wrong
	// party entirely. An author this AppView holds no row for — the ordinary
	// unhydrated federated author — has no provenance to prove, so the gate
	// default-denies.
	up, down, asOf := c.trustedBridgedStats(ctx, authorDID, record.BridgedStats, uri)

	facetsJSON, embedJSON, labelsJSON, err := serializePostContent(
		sanitizeFacets(record.Facets, record.Content, uri), record.Embed, record.Labels)
	if err != nil {
		return err
	}

	var applied bool
	if found {
		applied, err = c.applyPostContentUpdate(ctx, postContentUpdate{
			uri: uri, storedID: stored.id, rev: commit.Rev, cid: commit.CID,
			title: record.Title, content: record.Content,
			facets: facetsJSON, embed: embedJSON, labels: labelsJSON,
			bridgedUpvotes: up, bridgedDownvotes: down, bridgedAsOf: asOf,
			storedAsOf: stored.bridgedAsOf, storedDeletedAt: stored.deletedAt,
			storedIndexedAt: stored.indexedAt, timeUS: timeUS,
		})
		if err != nil {
			return err
		}
	} else {
		applied, err = c.insertAuthorPost(ctx, authorPostInsert{
			uri: uri, authorDID: authorDID, record: record, commit: commit, timeUS: timeUS,
			facets: facetsJSON, embed: embedJSON, labels: labelsJSON,
			bridgedUpvotes: up, bridgedDownvotes: down, bridgedAsOf: asOf,
		})
		if err != nil {
			return err
		}
	}

	// THE GATE GUARDS THE POST ROW, NOT THE ADMISSION. The two writes have
	// opposite idempotence: inserting the post must happen exactly once, while
	// UpsertPending is content-addressed and writes nothing when the row already
	// holds this CID. Gating them together is what orphaned admissions — a
	// failed upsert leaves the post indexed, the gate advanced, and every
	// redelivery skipped, so the row is never created and the post is invisible
	// in its community forever with nothing left to retry.
	//
	// A REPLAY IS SAFE, A STALE COPY IS NOT, and the stored rev is what tells
	// them apart. An equal rev is this same commit arriving again — the upsert
	// re-runs harmlessly and repairs the orphan. A strictly greater stored rev
	// means a NEWER event already applied, and re-running the upsert from this
	// older one would drag evaluated_cid backwards onto content the row no
	// longer holds, flipping an accepted post to pending_reacceptance on a
	// duplicate delivery.
	if !applied && !c.revIsCurrent(ctx, uri, commit.Rev) {
		return nil
	}

	// The author is looked up only after the content is safely indexed, and a
	// failure here is never fatal. Under §5.3 an author this AppView has never
	// seen is a normal state that must index anyway, so hydration is an
	// enrichment: getting a profile row for them is nice, and not getting one
	// must not cost the post.
	c.hydrateAuthorOpportunistically(ctx, authorDID)

	// The admission row: PENDING, always. The post claims the community; the
	// community has said nothing. A row opened as anything else would publish
	// speech the community never agreed to carry.
	//
	// It follows the content write rather than sharing its transaction, which
	// leaves one bounded window: a failure between the two indexes the post
	// without opening its admission, and the redrive is then rev-gated away, so
	// the repair needs the record re-emitted. The alternative — opening the
	// admission first — trades that for a phantom moderation-queue entry for a
	// post that was never indexed, which is the worse of the two because it is
	// invisible to the operator rather than visible as a dead letter.
	if _, err := c.admissions.UpsertPending(ctx, posts.UpsertPendingCommand{
		CommunityDID: record.Community,
		PostURI:      uri,
		EvaluatedCID: commit.CID,
	}); err != nil {
		return fmt.Errorf("recording the pending admission for %s in %s: %w", uri, record.Community, err)
	}

	log.Printf("✓ Indexed author post: %s (author: %s, community: %s)", uri, authorDID, record.Community)
	return nil
}

// revIsCurrent reports whether the gate's stored rev for this record is exactly
// the one this event carries — i.e. whether the event is the newest the AppView
// has seen rather than an older copy.
//
// It exists so a rev-gated SKIP can still be distinguished into its two very
// different causes. A redelivery of the newest commit is safe to act on again;
// a stale cross-feed copy of an older one is not. Anything unreadable answers
// false, which declines to act — the conservative direction, since acting on a
// stale event corrupts state while declining merely waits for the next
// delivery.
func (c *PostEventConsumer) revIsCurrent(ctx context.Context, uri, rev string) bool {
	if rev == "" {
		// A rev-less event bypasses the gate entirely, so there is no stored rev
		// to be current with. Only synthetic events reach this.
		return true
	}
	var stored string
	if err := c.db.QueryRowContext(ctx,
		`SELECT rev FROM jetstream_record_revs WHERE record_uri = $1`, uri,
	).Scan(&stored); err != nil {
		return false
	}
	return stored == rev
}

// hydrateAuthorOpportunistically indexes a minimal profile for an author this
// AppView has not seen, so their posts are not permanently authorless.
//
// Bounded and non-fatal, both deliberately. Bounded because it is an outbound
// resolution on the hot firehose path driven by an identifier a stranger chose;
// non-fatal because §5.3 makes indexing the post the obligation and the profile
// merely an enrichment — refusing the event over a slow PLC lookup would
// reinstate exactly the refusal the flip removed.
func (c *PostEventConsumer) hydrateAuthorOpportunistically(ctx context.Context, authorDID string) {
	if c.identityResolver == nil {
		return
	}

	if _, err := c.userService.GetUserByDID(ctx, authorDID); err == nil {
		return // already indexed
	} else if !errors.Is(err, users.ErrUserNotFound) {
		log.Printf("debug: skipping author hydration for %s (lookup failed: %v)", authorDID, err)
		return
	}

	hydrateCtx, cancel := context.WithTimeout(ctx, authorHydrationTimeout)
	defer cancel()

	resolved, err := c.identityResolver.Resolve(hydrateCtx, authorDID)
	if err != nil || resolved == nil {
		log.Printf("debug: could not resolve post author %s for hydration: %v", authorDID, err)
		return
	}
	// The resolver returns a bidirectionally verified handle, or the reserved
	// "handle.invalid" when verification failed. Indexing the latter would
	// write a placeholder into a column with a uniqueness constraint, so the
	// second unverifiable author would collide with the first.
	if resolved.DID != authorDID || resolved.Handle == "" || resolved.Handle == invalidHandle {
		log.Printf("debug: not hydrating post author %s (unverified identity)", authorDID)
		return
	}

	if err := c.userService.IndexUser(hydrateCtx, resolved.DID, resolved.Handle, resolved.PDSURL); err != nil {
		log.Printf("debug: could not hydrate post author %s: %v", authorDID, err)
	}
}

// invalidHandle is the reserved handle atProto identity resolution reports when
// a DID's handle cannot be bidirectionally verified.
const invalidHandle = "handle.invalid"

// authorHydrationTimeout bounds the opportunistic identity resolution above.
// Short on purpose: it is an enrichment on the firehose path, so it must never
// become the reason events back up.
const authorHydrationTimeout = 5 * time.Second

// trustedBridgedStats returns the bridged aggregate to apply for an author-repo
// post, or a nil asOf meaning "leave the stored bridged columns alone".
//
// Default-deny at every step: no aggregate, no users row for the author, a PDS
// outside the trusted bridge set, or an aggregate failing input hygiene all
// return nothing to apply.
func (c *PostEventConsumer) trustedBridgedStats(ctx context.Context, authorDID string, stats *BridgedStatsFromJetstream, uri string) (int, int, *time.Time) {
	if stats == nil {
		return 0, 0, nil
	}

	author, err := c.userService.GetUserByDID(ctx, authorDID)
	if err != nil || author == nil {
		log.Printf("debug: ignoring bridgedStats on %s (no indexed author %s to prove provenance)", uri, authorDID)
		return 0, 0, nil
	}
	if !c.bridgeTrust.TrustsPDS(author.PDSURL) {
		log.Printf("debug: ignoring bridgedStats on %s from untrusted author repo %s", uri, authorDID)
		return 0, 0, nil
	}
	up, down, asOf, ok := validatedBridgedStats(stats, uri)
	if !ok {
		return 0, 0, nil
	}
	return up, down, &asOf
}

// authorPostInsert is everything the first indexing of an author-repo post
// needs, gathered so the insert reads as one decision rather than a dozen
// positional arguments.
type authorPostInsert struct {
	uri              string
	authorDID        string
	record           *AuthorPostRecord
	commit           *CommitEvent
	timeUS           int64
	facets           sql.NullString
	embed            sql.NullString
	labels           sql.NullString
	bridgedUpvotes   int
	bridgedDownvotes int
	bridgedAsOf      *time.Time
}

// insertAuthorPost indexes a post the AppView has never held. It reports
// whether the write applied — false means the rev gate refused the event.
func (c *PostEventConsumer) insertAuthorPost(ctx context.Context, in authorPostInsert) (bool, error) {
	createdAt := parseRecordCreatedAt(in.record.CreatedAt, in.uri)

	post := &posts.Post{
		URI:  in.uri,
		CID:  in.commit.CID,
		RKey: in.commit.RKey,
		// THE AUTHOR IS THE REPO. Not a field, not a lookup — deriving it from
		// anywhere else is what would let any repo claim any author.
		AuthorDID: in.authorDID,
		// The community is a CLAIM the record makes: the author's submission
		// target, which the community has not yet agreed to.
		CommunityDID:  in.record.Community,
		Title:         in.record.Title,
		Content:       in.record.Content,
		ContentFacets: nullableString(in.facets),
		Embed:         nullableString(in.embed),
		ContentLabels: nullableString(in.labels),
		CreatedAt:     createdAt,
		IndexedAt:     indexedAtForEvent(in.timeUS),
	}
	if in.bridgedAsOf != nil {
		post.BridgedUpvoteCount = in.bridgedUpvotes
		post.BridgedDownvoteCount = in.bridgedDownvotes
		post.BridgedStatsAsOf = in.bridgedAsOf
		post.Score = in.bridgedUpvotes - in.bridgedDownvotes
	}

	// The rev gate decides inside this transaction, so a refusal and the writes
	// it refuses can never half-apply. A gate skip surfaces as applied=false.
	applied, err := c.indexPostIfRevWins(ctx, post, in.commit.Rev)
	if err != nil {
		return false, fmt.Errorf("failed to index author post %s: %w", in.uri, err)
	}
	return applied, nil
}

// ---------------------------------------------------------------------------
// acceptance and removal: the community's decision records
// ---------------------------------------------------------------------------

// communityDecisionRecord is an acceptance or a removal as it arrives from
// Jetstream. Both name their subject by strongRef; only a removal carries a
// code.
type communityDecisionRecord struct {
	Type    string `json:"$type"`
	Subject struct {
		URI string `json:"uri"`
		CID string `json:"cid"`
	} `json:"subject"`
	Code      string `json:"code,omitempty"`
	Reason    string `json:"reason,omitempty"`
	CreatedAt string `json:"createdAt"`
}

// parseCommunityDecision converts a raw acceptance/removal record, refusing the
// shapes that can never become valid.
func parseCommunityDecision(record map[string]interface{}, collection string) (*communityDecisionRecord, error) {
	recordJSON, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal %s record: %w", collection, err)
	}

	var parsed communityDecisionRecord
	if err := json.Unmarshal(recordJSON, &parsed); err != nil {
		return nil, fmt.Errorf("%w: failed to unmarshal %s record: %v", ErrPermanentEvent, collection, err)
	}
	if parsed.Subject.URI == "" {
		return nil, fmt.Errorf("%w: %s record names no subject", ErrPermanentEvent, collection)
	}
	// The pinned CID is half the decision, not decoration: agreeing to a URI is
	// not agreeing to whatever that URI holds later. A record without one gives
	// the consumer nothing to compare against the indexed content.
	if parsed.Subject.CID == "" {
		return nil, fmt.Errorf("%w: %s record for %s pins no CID", ErrPermanentEvent, collection, parsed.Subject.URI)
	}
	if collection == posts.RemovalCollection && parsed.Code == "" {
		return nil, fmt.Errorf("%w: removal record for %s carries no code", ErrPermanentEvent, parsed.Subject.URI)
	}
	return &parsed, nil
}

// handleCommunityDecisionEvent routes one acceptance or removal commit.
//
// event.Did is the COMMUNITY, and that is the only thing in the event that says
// which community decided — the record names a post, not a decider. So the repo
// has to BE an indexed community: taking an arbitrary repo at its word would let
// anyone with a PDS publish into any feed by writing a record about someone
// else's post.
func (c *PostEventConsumer) handleCommunityDecisionEvent(ctx context.Context, event *JetstreamEvent, commit *CommitEvent) error {
	communityDID := event.Did

	if _, err := c.communityRepo.GetByDID(ctx, communityDID); err != nil {
		if communities.IsNotFound(err) {
			// Transient, and the reason is delivery order rather than leniency:
			// a community's first acceptance can genuinely outrun its own
			// profile event, and marking this permanent would spend the redrive
			// budget that resolves the race and discard a real decision.
			log.Printf("🚨 SECURITY: refusing %s from %s - not an indexed community repo",
				commit.Collection, communityDID)
			return fmt.Errorf("community not found: %s - cannot apply a %s from a repo that is not an indexed community",
				communityDID, commit.Collection)
		}
		return fmt.Errorf("%w: failed to verify community %s exists: %v", errValidationInfra, communityDID, err)
	}

	if commit.Operation == "delete" {
		return c.applyCommunityDecisionDelete(ctx, communityDID, commit)
	}

	if commit.Record == nil {
		return fmt.Errorf("%w: %s %s event missing record data", ErrPermanentEvent, commit.Collection, commit.Operation)
	}
	decision, err := parseCommunityDecision(commit.Record, commit.Collection)
	if err != nil {
		return err
	}

	// The subject's author, and therefore the erasure gate. An admission row
	// for an erased account's post is exactly the row migration 036 exists to
	// stop being recreated, and an acceptance replayed months later is one of
	// the two ways it comes back.
	subjectAuthor, subjectCollection, _, ok := parseRecordURI(decision.Subject.URI)
	if !ok {
		return fmt.Errorf("%w: %s names subject %q, which is not an at:// record URI",
			ErrPermanentEvent, commit.Collection, decision.Subject.URI)
	}
	erased, err := c.authorWasErased(ctx, subjectAuthor)
	if err != nil {
		return err
	}
	if erased {
		log.Printf("INFO: dropping %s %s about %s - its author was erased",
			commit.Collection, commit.Operation, decision.Subject.URI)
		return nil
	}

	watermark := posts.CommunityWatermark{Rev: commit.Rev}

	switch commit.Collection {
	case posts.AcceptanceCollection:
		return c.applyAcceptance(ctx, communityDID, commit, decision, subjectCollection, watermark)
	case posts.RemovalCollection:
		return c.applyRemoval(ctx, communityDID, decision, watermark)
	}
	return nil
}

// applyAcceptance records a community's agreement to exactly one version of one
// post, converging on the subject first when the AppView has never seen it.
func (c *PostEventConsumer) applyAcceptance(
	ctx context.Context,
	communityDID string,
	commit *CommitEvent,
	decision *communityDecisionRecord,
	subjectCollection string,
	watermark posts.CommunityWatermark,
) error {
	stored, indexed, err := c.loadStoredPost(ctx, decision.Subject.URI)
	if err != nil {
		return err
	}

	// A TOMBSTONED SUBJECT IS NOT ACCEPTABLE, and the arrival is legitimate
	// rather than hostile: the community decided before it saw the tombstone,
	// and the two events are in different repos with no ordering between them.
	// Applying it would have getStatus report `accepted` for content no read
	// path will ever serve — and the host-side sweep already ran with the
	// tombstone and will not run again, so the community's repo would keep an
	// acceptance citing a record nobody can fetch.
	//
	// A SKIP, not a refusal. Returning an error would dead-letter an event that
	// is replayed constantly and would be refused identically every time.
	if indexed && stored.deletedAt != nil {
		log.Printf("INFO: not applying the acceptance of %s in %s: its author deleted the post",
			decision.Subject.URI, communityDID)
		return nil
	}

	indexedCommunity := stored.communityDID
	switch {
	case !indexed:
		if err := c.convergeOnAcceptedSubject(ctx, communityDID, decision, subjectCollection); err != nil {
			return err
		}
	case indexedCommunity != communityDID:
		// The same refusal the fetch path makes, on the path where the post is
		// already indexed. Both are the §10.2 rule: a community accepting a
		// post that names a DIFFERENT community is the fork/import flow, which
		// the data model supports and nothing is built for — so today it is a
		// community pulling another community's content into its feed on its
		// own say-so. Enforcing it in only one of the two places would leave
		// the whole check bypassable by getting the post indexed first, which
		// an attacker controls: they simply post before they accept.
		//
		// PERMANENT: a post's community is immutable across updates (§3.1), so
		// no retry makes this valid.
		return fmt.Errorf("%w: %s was submitted to community %s, but the acceptance came from %s (the fork/import flow is not built)",
			ErrPermanentEvent, decision.Subject.URI, indexedCommunity, communityDID)
	}

	result, err := c.admissions.ApplyAcceptance(ctx, posts.ApplyAcceptanceCommand{
		CommunityDID:   communityDID,
		PostURI:        decision.Subject.URI,
		AcceptanceURI:  recordURI(communityDID, posts.AcceptanceCollection, commit.RKey),
		AcceptanceRkey: commit.RKey,
		PinnedCID:      decision.Subject.CID,
		Watermark:      watermark,
	})
	if err != nil {
		return fmt.Errorf("applying the acceptance of %s in %s: %w", decision.Subject.URI, communityDID, err)
	}
	logAdmissionOutcome(posts.AcceptanceCollection, communityDID, decision.Subject.URI, result.Outcome)
	return nil
}

// convergeOnAcceptedSubject reads an accepted post straight from its author's
// PDS and indexes it (§5.4).
//
// Redrive alone cannot solve acceptance-before-post: bounded retries cannot
// manufacture an event that a relay-coverage gap will never deliver. This is
// the mechanism that makes convergence a guarantee rather than a bet on full
// relay coverage — and because it is an outbound request whose destination is
// chosen by a stranger's record, most of what follows is refusals.
func (c *PostEventConsumer) convergeOnAcceptedSubject(
	ctx context.Context,
	communityDID string,
	decision *communityDecisionRecord,
	subjectCollection string,
) error {
	if subjectCollection != PostV2Collection {
		// An acceptance is about an author-repo post. A subject in any other
		// collection is not a thing this community can accept, and no retry
		// changes which collection a URI names.
		return fmt.Errorf("%w: acceptance names subject %s, which is not a %s record",
			ErrPermanentEvent, decision.Subject.URI, PostV2Collection)
	}
	if c.postFetcher == nil {
		// Without the fetch the only convergence mechanism left is redrive, so
		// the event must stay retryable rather than be dropped.
		return fmt.Errorf("acceptance for unindexed post %s: no direct fetcher is wired, so only redrive can converge",
			decision.Subject.URI)
	}

	fetched, err := c.postFetcher.FetchPost(ctx, decision.Subject.URI)
	if err != nil {
		// Transient: a PDS that is down, slow, or briefly unreachable is the
		// ordinary case, and the redrive is what it is for.
		return fmt.Errorf("fetching the accepted post %s directly: %w", decision.Subject.URI, err)
	}

	// THE CID CHECK IS WHAT MAKES THE FETCH TRUSTWORTHY AT ALL. Without it the
	// AppView indexes whatever the author's PDS chooses to serve under that
	// rkey — the author (or whoever holds their keys) picks the content, and
	// the community's signed acceptance is made to cover it retroactively.
	//
	// PERMANENT: the pinned version is gone from the repo and no retry brings
	// it back, so re-fetching the same mismatch ten times is pure noise.
	if fetched.CID != decision.Subject.CID {
		return fmt.Errorf("%w: the PDS serving %s returned CID %s, but the acceptance pinned %s",
			ErrPermanentEvent, decision.Subject.URI, fetched.CID, decision.Subject.CID)
	}

	record, err := parseAuthorPostRecord(fetched.Record)
	if err != nil {
		return err
	}

	// Cross-community acceptance is the privileged fork/import flow, and §10.2
	// is explicit that it is deliberately NOT built. Until it exists, a
	// community accepting a post that names someone else is a community pulling
	// another community's content into its feed on its own say-so.
	//
	// PERMANENT: the record's community field is immutable across updates
	// (§3.1), so this can never become valid.
	if record.Community != communityDID {
		return fmt.Errorf("%w: %s names community %s, but the acceptance came from %s (the fork/import flow is not built)",
			ErrPermanentEvent, decision.Subject.URI, record.Community, communityDID)
	}

	authorDID, _, rkey, _ := parseRecordURI(decision.Subject.URI)

	facetsJSON, embedJSON, labelsJSON, err := serializePostContent(
		sanitizeFacets(record.Facets, record.Content, decision.Subject.URI), record.Embed, record.Labels)
	if err != nil {
		return err
	}
	up, down, asOf := c.trustedBridgedStats(ctx, authorDID, record.BridgedStats, decision.Subject.URI)

	// The fetch is not a firehose event, so it carries no rev to gate on and no
	// event time to stamp: an empty rev bypasses the gate (which is correct — a
	// later real event for this record still wins on its own rev) and the
	// watermark falls back to wall clock.
	applied, err := c.insertAuthorPost(ctx, authorPostInsert{
		uri:       decision.Subject.URI,
		authorDID: authorDID,
		record:    record,
		commit: &CommitEvent{
			Operation: "create", Collection: PostV2Collection,
			RKey: rkey, CID: fetched.CID,
		},
		facets: facetsJSON, embed: embedJSON, labels: labelsJSON,
		bridgedUpvotes: up, bridgedDownvotes: down, bridgedAsOf: asOf,
	})
	if err != nil {
		return err
	}

	// THE FETCH IS A CATCH-UP, NOT AN AUTHORITY, and applied=false is how it
	// learns it lost the race. The acceptance and the post live in different
	// repos, so Jetstream parallelises them and the post's own event can land
	// while this fetch is in flight — which is not a rare interleaving but the
	// normal one, since the fetch exists precisely because the event had not
	// arrived. The insert then conflicts and writes nothing.
	//
	// UpsertPending is last-write-wins, so running it anyway would stamp the
	// FETCHED CID over the newer one the real event just recorded. evaluated_cid
	// is what the next decision judges, so the engine would evaluate content the
	// author has already replaced, and an acceptance written from that verdict
	// would pin a version the AppView is no longer serving.
	//
	// Skipping it costs nothing: ApplyAcceptance below classifies against
	// whatever evaluated_cid the row actually holds, which is exactly the
	// comparison that turns a stale pin into pending_reacceptance.
	if applied {
		// The pending admission comes with it. ApplyAcceptance would create a row
		// on its own, but one with no evaluated content: recording what was indexed
		// is what lets the next author edit be recognised as an edit.
		if _, err := c.admissions.UpsertPending(ctx, posts.UpsertPendingCommand{
			CommunityDID: communityDID,
			PostURI:      decision.Subject.URI,
			EvaluatedCID: fetched.CID,
		}); err != nil {
			return fmt.Errorf("recording the pending admission for fetched post %s: %w", decision.Subject.URI, err)
		}
	}

	c.hydrateAuthorOpportunistically(ctx, authorDID)
	log.Printf("✓ Converged on accepted post %s by direct fetch (author: %s, community: %s)",
		decision.Subject.URI, authorDID, communityDID)
	return nil
}

// applyRemoval records a community's moderation decision about a post.
//
// A removal with no prior acceptance is VALID: a community that has decided in
// advance about a post — an author it is about to ban, content it has already
// seen elsewhere — must be able to say so, and requiring an acceptance first
// would drop exactly the decisions a community most wants to make early. No
// direct fetch either: a removed post is not rendered, so there is nothing to
// converge on, and fetching content in order to hide it would hand a moderation
// record the power to make the AppView dial an arbitrary host.
func (c *PostEventConsumer) applyRemoval(
	ctx context.Context,
	communityDID string,
	decision *communityDecisionRecord,
	watermark posts.CommunityWatermark,
) error {
	result, err := c.admissions.ApplyRemoval(ctx, posts.ApplyRemovalCommand{
		CommunityDID: communityDID,
		PostURI:      decision.Subject.URI,
		DecisionCode: decision.Code,
		Watermark:    watermark,
	})
	if err != nil {
		return fmt.Errorf("applying the removal of %s in %s: %w", decision.Subject.URI, communityDID, err)
	}
	logAdmissionOutcome(posts.RemovalCollection, communityDID, decision.Subject.URI, result.Outcome)
	return nil
}

// applyCommunityDecisionDelete applies the withdrawal of an acceptance or a
// removal.
//
// A delete event carries NO record, so the subject cannot be read from it — and
// the rkey is a SHA-256 digest of the subject URI (§3.2), which is one-way. The
// subject is therefore recoverable only from state the AppView already holds:
// acceptance_rkey, which is stored exactly while an acceptance stands, i.e.
// exactly when an acceptance deletion has something to withdraw.
//
// A removal deletion has no such column and needs none. Every moderation commit
// is a PAIR (§3.3) — the removal commit is {acceptance-delete, removal-put} and
// the restore commit is {removal-delete, acceptance-put} — and the put half
// carries the subject in-record and outranks its paired delete under the §5.2
// tuple. So the put alone converges the row whichever half arrives first, and
// an unresolvable delete is a no-op rather than a lost transition. The lone
// acceptance deletion, which the host writes when an author deletes their post
// (§5.3), is the one delete that arrives unpaired — and it is the one this
// lookup resolves.
func (c *PostEventConsumer) applyCommunityDecisionDelete(ctx context.Context, communityDID string, commit *CommitEvent) error {
	postURI, found, err := c.subjectOfDeletedRecord(ctx, communityDID, commit)
	if err != nil {
		return err
	}
	if !found {
		// Nothing here matches that record key: the paired write of the same
		// commit already superseded it, or this AppView never saw the record
		// being deleted. Either way there is nothing to withdraw.
		log.Printf("INFO: %s deletion %s/%s matches no standing record; nothing to withdraw",
			commit.Collection, communityDID, commit.RKey)
		return nil
	}

	cmd := posts.CommunityDeleteCommand{
		CommunityDID: communityDID,
		PostURI:      postURI,
		Watermark:    posts.CommunityWatermark{Rev: commit.Rev},
	}

	var result posts.AdmissionResult
	if commit.Collection == posts.RemovalCollection {
		result, err = c.admissions.ApplyRemovalDelete(ctx, cmd)
	} else {
		result, err = c.admissions.ApplyAcceptanceDelete(ctx, cmd)
	}
	if err != nil {
		return fmt.Errorf("withdrawing the %s of %s in %s: %w", commit.Collection, postURI, communityDID, err)
	}
	logAdmissionOutcome(commit.Collection+"#delete", communityDID, postURI, result.Outcome)
	return nil
}

// subjectOfDeletedRecord recovers which post a deleted acceptance or removal was
// about.
//
// A delete event carries NO record, and the rkey is a SHA-256 digest of the
// subject URI (§3.2), which is one-way — so the subject can only come from state
// the AppView already holds. The two collections need different lookups because
// only one of them has a column:
//
//   - An ACCEPTANCE stores its rkey on the admission row, so the reverse lookup
//     is an exact match.
//   - A REMOVAL stores none, so its subject is found by recomputing the digest
//     over the rows that could be its subject. The candidate set is bounded to
//     this community's `removed` rows — moderation-sized, and served by the
//     (community_did, status, created_at) index migration 034 already carries.
//
// WHY THE REMOVAL CASE CANNOT STAY A NO-OP, which is what it was. The paired
// commits do converge without it: a removal commit is {acceptance-delete,
// removal-put} and a restore is {removal-delete, acceptance-put}, and in both
// the put carries the subject in-record and outranks its delete under the §5.2
// tuple. But a moderator can also simply WITHDRAW a removal — deleting the
// record and writing nothing — and that commit carries only this event. Ignored,
// the post stays `removed` forever while the community's own repo no longer says
// so: the signed record and the AppView disagree, and only the AppView is
// consulted when the post is served.
func (c *PostEventConsumer) subjectOfDeletedRecord(
	ctx context.Context, communityDID string, commit *CommitEvent,
) (string, bool, error) {
	if commit.Collection == posts.AcceptanceCollection {
		var postURI string
		err := c.db.QueryRowContext(ctx,
			`SELECT post_uri FROM community_post_admissions
			 WHERE community_did = $1 AND acceptance_rkey = $2`,
			communityDID, commit.RKey,
		).Scan(&postURI)
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		if err != nil {
			return "", false, fmt.Errorf("resolving the subject of acceptance deletion %s/%s: %w",
				communityDID, commit.RKey, err)
		}
		return postURI, true, nil
	}

	rows, err := c.db.QueryContext(ctx,
		`SELECT post_uri FROM community_post_admissions
		 WHERE community_did = $1 AND status = 'removed'`, communityDID)
	if err != nil {
		return "", false, fmt.Errorf("resolving the subject of removal deletion %s/%s: %w",
			communityDID, commit.RKey, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var postURI string
		if err := rows.Scan(&postURI); err != nil {
			return "", false, fmt.Errorf("scanning a removed subject of %s: %w", communityDID, err)
		}
		if posts.SubjectRkey(postURI) == commit.RKey {
			return postURI, true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return "", false, fmt.Errorf("reading the removed subjects of %s: %w", communityDID, err)
	}
	return "", false, nil
}

// logAdmissionOutcome records what a community event DID, including the skips.
//
// A skip is the ordering gate working — a multi-feed duplicate, a dead-letter
// redrive, an event superseded by its own commit's other half, or this
// AppView's own write coming back to it — so it is logged rather than returned
// as an error, which would bury healthy skips in the dead-letter queue among
// genuine failures.
//
// THE LAST OF THOSE IS THE COMMON ONE ON A HOSTING INSTANCE and is easy to
// misread in the logs. When this AppView hosts the community, the engine writes
// the acceptance into the repo and stamps the row optimistically; the firehose
// then delivers that same commit back, and the watermark CAS answers
// skipped_stale because the row already carries that exact rev. Nothing is
// wrong — the write landed twice by design, once locally and once as its own
// echo — and the engine's own doc calls that echo a success.
func logAdmissionOutcome(collection, communityDID, postURI string, outcome posts.AdmissionOutcome) {
	if outcome == posts.AdmissionApplied {
		log.Printf("✓ Applied %s for %s in %s", collection, postURI, communityDID)
		return
	}
	log.Printf("admission: %s for %s in %s was %s (an outcome, not a failure)",
		collection, postURI, communityDID, outcome)
}

// ---------------------------------------------------------------------------
// small shared helpers
// ---------------------------------------------------------------------------

// recordURI builds the AT-URI of a record from its repo, collection and rkey.
func recordURI(repoDID, collection, rkey string) string {
	return fmt.Sprintf("at://%s/%s/%s", repoDID, collection, rkey)
}

// parseRecordURI splits at://<repo>/<collection>/<rkey>. It reports ok=false
// for anything else, including URIs with extra path segments — a subject the
// AppView cannot address is a subject it must refuse rather than guess at.
func parseRecordURI(uri string) (repoDID, collection, rkey string, ok bool) {
	rest, found := strings.CutPrefix(uri, "at://")
	if !found {
		return "", "", "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

// indexedPostCommunity returns the community an indexed post was submitted to.
//
// Soft-deleted rows COUNT as indexed: a tombstoned post has been seen, and
// treating it as absent would send the direct fetch to resurrect content its
// author deleted.
func (c *PostEventConsumer) indexedPostCommunity(ctx context.Context, uri string) (string, bool, error) {
	var communityDID string
	err := c.db.QueryRowContext(ctx, `SELECT community_did FROM posts WHERE uri = $1`, uri).Scan(&communityDID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading the indexed community of %s: %w", uri, err)
	}
	return communityDID, true, nil
}

// nullableString converts a serialized JSON column back to the pointer shape
// posts.Post uses, where nil means "the record carried none".
func nullableString(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	s := v.String
	return &s
}
