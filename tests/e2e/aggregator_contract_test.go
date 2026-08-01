//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"Coves/tests/testkit"

	"github.com/stretchr/testify/require"
)

// The aggregator domain's pipeline contracts: the ingestion proofs for
// social.coves.aggregator.service and social.coves.aggregator.authorization,
// plus the client-facing surface of the aggregator endpoints.
//
// # TWO COLLECTIONS, TWO REPOS, AND THAT IS THE WHOLE DOMAIN
//
// An aggregator is a bot with its own atProto identity — an RSS bridge, a
// cross-poster — modelled on Bluesky's feed generators and labelers. The domain
// is two record types that live in two different repositories, and keeping them
// straight is most of understanding it:
//
//   - social.coves.aggregator.service is the aggregator DECLARING ITSELF, in
//     its OWN repo, at the canonical rkey "self" (aggregator_consumer.go:83).
//     It carries the display name, description, source URL, maintainer and a
//     JSON Schema describing what communities may configure.
//   - social.coves.aggregator.authorization is a COMMUNITY GRANTING that
//     aggregator access, in the COMMUNITY's repo, at any rkey
//     (aggregator_consumer.go:98). It names the aggregator, carries the
//     community's config for it, and an enabled flag that doubles as the
//     off switch.
//
// Both consumers refuse a record whose claimed identity disagrees with the repo
// it arrived in — a service record's `did` must equal the repo DID
// (aggregator_consumer.go:133), an authorization's `communityDid` must equal
// the community's (`:222`). That is the domain's whole authorization model at
// the ingestion layer.
//
// Only the SERVICE half of it is proven here. TestAggregatorServiceIngestion's
// spoof step writes a declaration from the wrong repo and shows it changes
// nothing, bounded intra-repo so the negative means something. The
// authorization half has no equivalent step below: its check is covered at T0
// (internal/atproto/jetstream/error_taxonomy_test.go's "authorization
// communityDid mismatch", which also pins that the rejection is PERMANENT), and
// a T2 version would cost a second permanent dead letter per run to re-prove a
// rule this file already demonstrates once. Said plainly rather than left to inference, because "both
// consumers are proven against the firehose" is the kind of claim a header
// makes and a test file quietly does not keep.
//
// # NO MUST-KNOW-FIRST GATE, UNLIKE EVERY OTHER IDENTITY IN THE PRODUCT
//
// contracts_test.go's IndexedAccount explains that a user must sign up before
// the users consumer will index anything of theirs. Aggregators are the
// opposite and it is worth knowing before reading these contracts: the
// aggregator consumer indexes a service declaration from a repo it has never
// heard of. There is no signup, and — verified by spike — no need to call
// social.coves.aggregator.register either: register writes a row in the USERS
// table so that an aggregator can later be a post author, and has nothing to do
// with the aggregators table, which is fed only by the firehose.
//
// So provisionAggregatorRepo is just a PDS account, and the very first thing
// the AppView learns about that DID is the service record arriving over the
// firehose. That makes this the purest ingestion contract in the package: there
// is no synchronous path to be confused with, and no reconciliation code that
// reads the PDS behind the tier's back.
//
// # THE THREE SERVING ENDPOINTS, AND THEIR THREE NOT-FOUND SHAPES
//
// All three reads are public, which is what makes this domain observable here
// at all (contrast block_contract_test.go, whose collections have no
// unauthenticated reader). They disagree about what "unknown" means, and each
// contract below pins the shape it depends on, because the tier's waits treat
// not-found as "not yet" and a shape mix-up turns a real failure into a
// timeout:
//
//	getServices?dids=…      → 200 with an EMPTY views array. Never 404: it is a
//	                          batch endpoint, so an unknown DID is simply
//	                          absent from the answer.
//	getAuthorizations?…     → 404 AggregatorNotFound when the AGGREGATOR is
//	                          unknown; 200 with an empty array when it is known
//	                          and has no authorizations.
//	listForCommunity?…      → 404 when the community identifier does not
//	                          resolve; 200 with an empty array otherwise. It
//	                          accepts a DID or a handle, resolved through the
//	                          community index.
const (
	aggregatorServiceCollection       = "social.coves.aggregator.service"
	aggregatorAuthorizationCollection = "social.coves.aggregator.authorization"
)

// aggregatorView is the slice of the aggregator views these contracts observe.
// getServices, getAuthorizations and listForCommunity each answer with a
// different projection; this models the fields all of them agree on plus the
// stats only the detailed form carries.
type aggregatorView struct {
	DID          string         `json:"did"`
	DisplayName  string         `json:"displayName"`
	Description  string         `json:"description"`
	SourceURL    string         `json:"sourceUrl"`
	Maintainer   string         `json:"maintainer"`
	ConfigSchema map[string]any `json:"configSchema"`
	RecordURI    string         `json:"recordUri"`
	Stats        struct {
		CommunitiesUsing int `json:"communitiesUsing"`
		PostsCreated     int `json:"postsCreated"`
	} `json:"stats"`
}

// communityAuthView is one entry of getAuthorizations: an authorization seen
// from the AGGREGATOR's side, carrying the aggregator's own view nested inside
// it (which is why the endpoint 404s when the aggregator is unknown — it cannot
// build the nested half).
type communityAuthView struct {
	Enabled    bool           `json:"enabled"`
	Config     map[string]any `json:"config"`
	RecordURI  string         `json:"recordUri"`
	Aggregator aggregatorView `json:"aggregator"`
}

// authorizationView is one entry of listForCommunity: the same row seen from
// the COMMUNITY's side, carrying the moderation audit trail instead of the
// aggregator's profile.
type authorizationView struct {
	AggregatorDID string         `json:"aggregatorDid"`
	CommunityDID  string         `json:"communityDid"`
	Enabled       bool           `json:"enabled"`
	Config        map[string]any `json:"config"`
	CreatedBy     string         `json:"createdBy"`
	DisabledBy    string         `json:"disabledBy"`
	DisabledAt    string         `json:"disabledAt"`
	RecordURI     string         `json:"recordUri"`
}

// Services reads aggregator service declarations by DID. detailed asks for the
// stats-bearing projection, which is where communities_using — a database
// trigger's output, not a stored value any code writes — becomes visible.
func (p *pipeline) Services(ctx context.Context, detailed bool, dids ...string) ([]aggregatorView, error) {
	params := url.Values{"dids": {joinDIDs(dids)}}
	if detailed {
		params.Set("detailed", "true")
	}
	var out struct {
		Views []aggregatorView `json:"views"`
	}
	err := p.AppView.Query(ctx, "social.coves.aggregator.getServices", params, &out)
	return out.Views, err
}

// Service reads exactly one aggregator, and reports absence as (zero, false)
// rather than as an error — getServices answers 200 with an empty array for a
// DID it does not know (see the file's opening note).
func (p *pipeline) Service(ctx context.Context, did string, detailed bool) (aggregatorView, bool, error) {
	views, err := p.Services(ctx, detailed, did)
	if err != nil {
		return aggregatorView{}, false, err
	}
	for _, view := range views {
		if view.DID == did {
			return view, true, nil
		}
	}
	return aggregatorView{}, false, nil
}

// joinDIDs renders the comma-separated form getServices takes. Spelled out
// rather than strings.Join so the endpoint's quirk — one repeated-value
// parameter would NOT work, it splits on commas itself — stays visible.
func joinDIDs(dids []string) string {
	joined := ""
	for i, did := range dids {
		if i > 0 {
			joined += ","
		}
		joined += did
	}
	return joined
}

// Authorizations reads an aggregator's authorizations (the aggregator's view of
// which communities let it in).
func (p *pipeline) Authorizations(ctx context.Context, aggregatorDID string, enabledOnly bool) ([]communityAuthView, error) {
	params := url.Values{"aggregatorDid": {aggregatorDID}}
	if enabledOnly {
		params.Set("enabledOnly", "true")
	}
	var out struct {
		Authorizations []communityAuthView `json:"authorizations"`
	}
	err := p.AppView.Query(ctx, "social.coves.aggregator.getAuthorizations", params, &out)
	return out.Authorizations, err
}

// CommunityAggregators reads a community's authorizations (the community's view
// of which aggregators it has let in). identifier is anything
// ResolveCommunityIdentifier accepts.
func (p *pipeline) CommunityAggregators(ctx context.Context, identifier string, enabledOnly bool) ([]authorizationView, error) {
	params := url.Values{"community": {identifier}}
	if enabledOnly {
		params.Set("enabledOnly", "true")
	}
	var out struct {
		Aggregators []authorizationView `json:"aggregators"`
	}
	err := p.AppView.Query(ctx, "social.coves.aggregator.listForCommunity", params, &out)
	return out.Aggregators, err
}

// provisionAggregatorRepo registers the PDS account an aggregator's service
// record lives in, and returns a session on it.
//
// The aggregator analogue of provisionCommunityRepo, and like it there is no
// signup step: the AppView learns this DID exists when its service declaration
// arrives over the firehose (see the file's opening note). Unlike a community,
// an aggregator has no handle convention to satisfy, so the handle is an
// ordinary one under the PDS' own domain.
func provisionAggregatorRepo(t *testing.T, p *pipeline, prefix string) *testkit.Account {
	t.Helper()

	label := testkit.UniqueIDWithPrefix(t, prefix)
	return p.PDS.CreateAccount(t,
		testkit.WithHandle(p.PDS.Endpoint.Handle(label)),
		testkit.WithEmail(label+"@aggregator.test.coves.dev"))
}

// aggregatorServiceRecord builds a social.coves.aggregator.service record in
// the shape the consumer parses (AggregatorServiceRecord,
// aggregator_consumer.go:308) — note `maintainer` and `sourceUrl`, whose JSON
// names disagree with the Go field names they land in, which is exactly the
// kind of mapping a contract should be written against rather than around.
func aggregatorServiceRecord(did, displayName, description string) map[string]any {
	return map[string]any{
		"$type":       aggregatorServiceCollection,
		"did":         did,
		"displayName": displayName,
		"description": description,
		"sourceUrl":   "https://example.invalid/" + displayName,
		"maintainer":  did,
		"configSchema": map[string]any{
			"type":       "object",
			"properties": map[string]any{"feedUrl": map[string]any{"type": "string"}},
		},
		"createdAt": time.Now().UTC().Format(time.RFC3339),
	}
}

// aggregatorAuthorizationRecord builds a social.coves.aggregator.authorization
// record in the shape the consumer parses
// (AggregatorAuthorizationRecord, aggregator_consumer.go:344). createdBy is the
// moderator who granted it: the consumer stores it verbatim, so it is the
// domain's audit trail and not a field the AppView derives.
func aggregatorAuthorizationRecord(aggregatorDID, communityDID, createdBy string, enabled bool) map[string]any {
	return map[string]any{
		"$type":         aggregatorAuthorizationCollection,
		"aggregatorDid": aggregatorDID,
		"communityDid":  communityDID,
		"createdBy":     createdBy,
		"enabled":       enabled,
		"config":        map[string]any{"feedUrl": "https://example.invalid/feed.xml"},
		"createdAt":     time.Now().UTC().Format(time.RFC3339),
	}
}

// indexedAggregator provisions an aggregator's repo, declares its service
// record and waits for the AppView to have learned about it from the firehose.
// The authorization contracts need an aggregator that is INDEXED, not merely
// provisioned: the authorizations table has a foreign key onto it.
func indexedAggregator(t *testing.T, p *pipeline, prefix string) (*testkit.Account, string) {
	t.Helper()

	account := provisionAggregatorRepo(t, p, prefix)
	displayName := "aggregator " + testkit.UniqueID(t)
	account.PutRecord(t, aggregatorServiceCollection, "self",
		aggregatorServiceRecord(account.DID, displayName, "an aggregator to authorize"))

	p.Await(t, "the aggregator declaring these authorizations to be indexed", func() (bool, error) {
		_, found, err := p.Service(context.Background(), account.DID, false)
		return found, err
	})
	return account, displayName
}

// TestAggregatorServiceIngestion is the pipeline proof for service declarations.
//
// coves:ingestion-contract social.coves.aggregator.service
//
// Every record is written straight into the aggregator's own repo at rkey
// "self", and every observation is made through
// social.coves.aggregator.getServices:
//
//	unknown → 200 with an empty array, which is this endpoint's not-found shape
//	declare → the aggregator appears, carrying the record's own field values
//	update  → the same DID serves the new values at the same record URI
//	spoof   → a DIFFERENT repo declaring itself to be this aggregator changes
//	          nothing, and stays changed-nothing (Holds)
//	delete  → the aggregator is gone, and STAYS gone (Holds, §3.4a)
func TestAggregatorServiceIngestion(t *testing.T) {
	p := newPipeline(t)
	aggregator := provisionAggregatorRepo(t, p, "as")

	ctx := context.Background()

	// ---- the not-found shape, before anything exists -------------------------
	// Asserted rather than assumed, and asserted FIRST: every wait below reads
	// this endpoint, and a wait can only mean "not yet" if absence looks like
	// this. It also proves the DID is genuinely unknown, which is what makes the
	// next step's appearance attributable to the write.
	_, found, err := p.Service(ctx, aggregator.DID, false)
	require.NoError(t, err,
		"social.coves.aggregator.getServices must answer 200 for an unknown DID — it is a batch "+
			"endpoint, so an unknown DID is an absent entry rather than an error")
	require.False(t, found, "a freshly provisioned repo that has declared nothing is not an aggregator")

	// ---- declare -------------------------------------------------------------
	displayName := "declared " + testkit.UniqueID(t)
	description := "written straight into the aggregator's own repo " + testkit.UniqueID(t)
	aggregator.PutRecord(t, aggregatorServiceCollection, "self",
		aggregatorServiceRecord(aggregator.DID, displayName, description))

	view := awaitService(t, p, aggregator.DID, func(v aggregatorView) bool {
		return v.DisplayName == displayName
	}, "the service declaration to reach getServices via the consumers")

	require.Equal(t, description, view.Description)
	require.Equal(t, aggregator.DID, view.Maintainer)
	require.Equal(t, "https://example.invalid/"+displayName, view.SourceURL)
	require.Equal(t, "at://"+aggregator.DID+"/"+aggregatorServiceCollection+"/self", view.RecordURI,
		"the record URI is rebuilt by the consumer from the repo DID and the canonical rkey, so a "+
			"wrong one here means it indexed a record from somewhere other than where it says")
	require.Equal(t, "object", view.ConfigSchema["type"],
		"the config schema is stored as JSONB and served back parsed; a community's configuration "+
			"is validated against it, so it has to survive the round trip intact")

	// The stats are a different mechanism from everything else on this view:
	// communitiesUsing is maintained by a database trigger over the
	// authorizations table (migrations/012, update_aggregator_communities_count)
	// rather than written by the consumer. Zero here is the baseline the
	// authorization contract moves.
	detailed, _, err := p.Service(ctx, aggregator.DID, true)
	require.NoError(t, err)
	require.Equal(t, 0, detailed.Stats.CommunitiesUsing)
	require.Equal(t, 0, detailed.Stats.PostsCreated)

	// ---- update --------------------------------------------------------------
	renamed := "renamed " + testkit.UniqueID(t)
	aggregator.PutRecord(t, aggregatorServiceCollection, "self",
		aggregatorServiceRecord(aggregator.DID, renamed, "the second write"))
	updated := awaitService(t, p, aggregator.DID, func(v aggregatorView) bool {
		return v.DisplayName == renamed
	}, "the updated service declaration to be served")
	require.Equal(t, view.RecordURI, updated.RecordURI,
		"an update writes the same rkey, so the record URI is unchanged — a new URI would mean the "+
			"consumer indexed a second aggregator rather than updating this one")

	// ---- a repo declaring itself to be somebody else -------------------------
	// The domain's authorization model at the ingestion layer, proven against
	// the real firehose: aggregator_consumer.go:133 refuses a service record
	// whose `did` disagrees with the repo it arrived in. Without that check any
	// PDS account could overwrite any aggregator's declaration, because the
	// consumer keys the row on the record's claim rather than on the repo.
	//
	// HOW THE NEGATIVE IS BOUNDED (the rule task 12 established): "the spoof
	// never lands" cannot be proven by waiting, so it is bounded by a later
	// event — the impostor's OWN, honest declaration, written into THE SAME
	// REPO. A repo's commits cannot overtake each other anywhere on the path, so
	// when the honest one is being served the spoof has already been through the
	// same consumer, and only then is its non-effect meaningful. Holds keeps
	// watching in case a redrive changes the answer.
	impostor := provisionAggregatorRepo(t, p, "ai")
	impostor.PutRecord(t, aggregatorServiceCollection, "self",
		aggregatorServiceRecord(aggregator.DID, "SPOOFED "+testkit.UniqueID(t), "written by another repo"))

	impostorName := "impostor " + testkit.UniqueID(t)
	impostor.PutRecord(t, aggregatorServiceCollection, "self",
		aggregatorServiceRecord(impostor.DID, impostorName, "the honest second write that bounds the spoof"))
	awaitService(t, p, impostor.DID, func(v aggregatorView) bool {
		return v.DisplayName == impostorName
	}, "the impostor's honest declaration, which bounds its spoof")

	p.Holds(t, "the spoofed declaration to leave the real aggregator alone", func() (bool, error) {
		current, found, err := p.Service(context.Background(), aggregator.DID, false)
		if err != nil {
			return false, err
		}
		return found && current.DisplayName == renamed, nil
	})

	// ---- delete --------------------------------------------------------------
	// A delete of the service record retires the aggregator: the row goes, and
	// the foreign keys take its authorizations and post records with it
	// (migrations/012, ON DELETE CASCADE).
	aggregator.DeleteExistingRecord(t, aggregatorServiceCollection, "self")
	p.Await(t, "the deleted service declaration to leave getServices", func() (bool, error) {
		_, found, err := p.Service(context.Background(), aggregator.DID, false)
		if err != nil {
			return false, err
		}
		return !found, nil
	})
	p.Holds(t, "the retired aggregator to stay retired", func() (bool, error) {
		_, found, err := p.Service(context.Background(), aggregator.DID, false)
		if err != nil {
			return false, err
		}
		return !found, nil
	})
}

// awaitService waits until getServices serves an aggregator satisfying accept,
// and returns the view it settled on.
func awaitService(t *testing.T, p *pipeline, did string, accept func(aggregatorView) bool, description string) aggregatorView {
	t.Helper()

	var settled aggregatorView
	p.Await(t, description, func() (bool, error) {
		view, found, err := p.Service(context.Background(), did, false)
		if err != nil || !found {
			return false, err
		}
		if !accept(view) {
			return false, nil
		}
		settled = view
		return true, nil
	})
	return settled
}

// TestAggregatorAuthorizationIngestion is the pipeline proof for the records a
// community writes to let an aggregator in.
//
// coves:ingestion-contract social.coves.aggregator.authorization
//
// Every record is written straight into the COMMUNITY's own repo, and every
// observation is made through the two endpoints that serve the same row from
// opposite sides, plus the trigger-maintained counter on a third:
//
//	authorize → both sides serve it, and getServices' communitiesUsing reaches 1
//	disable   → both sides show it disabled, enabledOnly hides it, and the
//	            counter falls back to 0
//	delete    → both sides lose it, and STAY without it (Holds, §3.4a)
//
// # WHY BOTH SIDES, EVERY TIME
//
// Same reasoning as the subscription contract's two counts. getAuthorizations
// and listForCommunity are different queries over one table with different
// projections, and a row that appears on one and not the other is a real bug
// class — an inverted join, a filter applied to one side only. Checking them
// together costs one request and is the cheapest detector for it.
//
// The counter is a third mechanism again: communities_using is maintained by a
// Postgres TRIGGER on the authorizations table (migrations/012), not by any Go
// code, so it is the one assertion here that would survive the entire service
// layer being rewritten and would break if a migration dropped the trigger.
func TestAggregatorAuthorizationIngestion(t *testing.T) {
	p := newPipeline(t)

	moderator := p.IndexedAccount(t, "am")
	community := indexedCommunity(t, p, "au", moderator.DID)
	aggregator, aggregatorName := indexedAggregator(t, p, "ag")

	ctx := context.Background()

	// ---- the not-found shapes, before anything exists ------------------------
	// The two endpoints disagree about absence, and both shapes are load-bearing
	// for the waits below.
	authorizations, err := p.Authorizations(ctx, aggregator.DID, false)
	require.NoError(t, err,
		"getAuthorizations must answer 200 for a KNOWN aggregator with no authorizations; it 404s "+
			"only when the aggregator itself is unknown")
	require.Empty(t, authorizations)

	granted, err := p.CommunityAggregators(ctx, community.DID, false)
	require.NoError(t, err)
	require.Empty(t, granted, "a freshly indexed community has authorized nobody")

	// ---- authorize -----------------------------------------------------------
	rkey := testkit.TID()
	community.PutRecord(t, aggregatorAuthorizationCollection, rkey,
		aggregatorAuthorizationRecord(aggregator.DID, community.DID, moderator.DID, true))
	recordURI := "at://" + community.DID + "/" + aggregatorAuthorizationCollection + "/" + rkey

	p.Await(t, "the authorization to reach the community's side of the index", func() (bool, error) {
		granted, err := p.CommunityAggregators(context.Background(), community.DID, false)
		if err != nil {
			return false, err
		}
		return len(granted) == 1, nil
	})

	granted, err = p.CommunityAggregators(ctx, community.DID, false)
	require.NoError(t, err)
	require.Len(t, granted, 1)
	require.Equal(t, aggregator.DID, granted[0].AggregatorDID)
	require.Equal(t, community.DID, granted[0].CommunityDID)
	require.True(t, granted[0].Enabled)
	require.Equal(t, moderator.DID, granted[0].CreatedBy,
		"createdBy is the audit trail: the consumer stores what the record says, so a wrong value "+
			"here means the moderation log cannot be trusted")
	require.Equal(t, recordURI, granted[0].RecordURI)
	require.Equal(t, "https://example.invalid/feed.xml", granted[0].Config["feedUrl"],
		"the per-community config is JSONB and is what the aggregator is actually told to do")

	p.Await(t, "the authorization to reach the aggregator's side of the index", func() (bool, error) {
		authorizations, err := p.Authorizations(context.Background(), aggregator.DID, false)
		if err != nil {
			return false, err
		}
		return len(authorizations) == 1, nil
	})
	authorizations, err = p.Authorizations(ctx, aggregator.DID, false)
	require.NoError(t, err)
	require.Len(t, authorizations, 1)
	require.True(t, authorizations[0].Enabled)
	require.Equal(t, recordURI, authorizations[0].RecordURI)
	require.Equal(t, aggregatorName, authorizations[0].Aggregator.DisplayName,
		"this endpoint nests the aggregator's own view inside each authorization, and that nested "+
			"half comes from the OTHER collection — so it is also a check that the two indexes agree")

	// The community identifier is resolved, not matched: a client holding a
	// handle must reach the same row as one holding a DID.
	byHandle, err := p.CommunityAggregators(ctx, community.Handle, false)
	require.NoError(t, err, "listForCommunity rejected the community's handle")
	require.Len(t, byHandle, 1)
	require.Equal(t, recordURI, byHandle[0].RecordURI)

	// The trigger's counter.
	p.Await(t, "the trigger-maintained communitiesUsing counter to reach one", func() (bool, error) {
		view, found, err := p.Service(context.Background(), aggregator.DID, true)
		if err != nil || !found {
			return false, err
		}
		return view.Stats.CommunitiesUsing == 1, nil
	})

	// ---- disable -------------------------------------------------------------
	// The off switch is a record UPDATE, not a delete: the row stays, carrying
	// who turned it off and when, and the community can turn it back on. The
	// enable/disable/updateConfig XRPC procedures do not exist yet
	// (internal/api/routes/aggregator.go:54-58 are TODOs, and the service
	// methods return ErrNotImplemented), so this record path is the ONLY way an
	// authorization is disabled today — which makes it the whole feature rather
	// than an alternative to it.
	disabledAt := time.Now().UTC().Format(time.RFC3339)
	disabled := aggregatorAuthorizationRecord(aggregator.DID, community.DID, moderator.DID, false)
	disabled["disabledBy"] = moderator.DID
	disabled["disabledAt"] = disabledAt
	community.PutRecord(t, aggregatorAuthorizationCollection, rkey, disabled)

	p.Await(t, "the disabled authorization to be served as disabled", func() (bool, error) {
		granted, err := p.CommunityAggregators(context.Background(), community.DID, false)
		if err != nil || len(granted) != 1 {
			return false, err
		}
		return !granted[0].Enabled, nil
	})

	granted, err = p.CommunityAggregators(ctx, community.DID, false)
	require.NoError(t, err)
	require.Len(t, granted, 1, "disabling keeps the row: it is an audit trail, not a deletion")
	require.Equal(t, moderator.DID, granted[0].DisabledBy)
	require.NotEmpty(t, granted[0].DisabledAt)

	filtered, err := p.CommunityAggregators(ctx, community.DID, true)
	require.NoError(t, err)
	require.Empty(t, filtered,
		"enabledOnly=true must hide a disabled authorization — this is the filter every caller "+
			"deciding whether an aggregator may post relies on")

	// The same two questions from the aggregator's side. Asserted separately
	// rather than assumed to follow from the community's side, because
	// enabledOnly is implemented once per query (GetAuthorizationsForAggregator
	// and ListAggregatorsForCommunity build their own WHERE clauses), so a
	// filter dropped from one of them is invisible from the other — and this is
	// the direction an aggregator's own tooling reads.
	authorizations, err = p.Authorizations(ctx, aggregator.DID, false)
	require.NoError(t, err)
	require.Len(t, authorizations, 1,
		"unfiltered, the aggregator's side must still show the authorization it lost")
	require.False(t, authorizations[0].Enabled,
		"the aggregator's side must report the SAME state as the community's: a row that reads "+
			"enabled here and disabled there would have an aggregator believing it may still post")

	enabledOnly, err := p.Authorizations(ctx, aggregator.DID, true)
	require.NoError(t, err)
	require.Empty(t, enabledOnly,
		"enabledOnly=true must hide the disabled authorization on the aggregator's side too")

	p.Await(t, "the counter to fall back to zero when the authorization is disabled", func() (bool, error) {
		view, found, err := p.Service(context.Background(), aggregator.DID, true)
		if err != nil || !found {
			return false, err
		}
		return view.Stats.CommunitiesUsing == 0, nil
	})

	// ---- delete --------------------------------------------------------------
	community.DeleteExistingRecord(t, aggregatorAuthorizationCollection, rkey)
	p.Await(t, "the deleted authorization to leave both sides of the index", func() (bool, error) {
		granted, err := p.CommunityAggregators(context.Background(), community.DID, false)
		if err != nil {
			return false, err
		}
		if len(granted) != 0 {
			return false, nil
		}
		authorizations, err := p.Authorizations(context.Background(), aggregator.DID, false)
		if err != nil {
			return false, err
		}
		return len(authorizations) == 0, nil
	})
	p.Holds(t, "the revoked authorization to stay revoked on both sides", func() (bool, error) {
		granted, err := p.CommunityAggregators(context.Background(), community.DID, false)
		if err != nil {
			return false, err
		}
		if len(granted) != 0 {
			return false, nil
		}
		authorizations, err := p.Authorizations(context.Background(), aggregator.DID, false)
		if err != nil {
			return false, err
		}
		return len(authorizations) == 0, nil
	})
}

// TestAggregatorAuthorizationArrivingBeforeItsAggregator pins what happens when
// the two halves of this domain arrive in the wrong order.
//
// It carries NO ingestion marker: the collection is proven by
// TestAggregatorAuthorizationIngestion, and this is a separate claim about
// ordering.
//
// # WHY THE ORDER CAN BE WRONG AT ALL
//
// The two records live in two different repos, and Jetstream parallelises
// across repos: the guarantee it offers is per-repo commit order and nothing
// more. So a community can authorize an aggregator whose declaration has not
// been indexed yet — a moderator acting on a brand-new bot, or any replay after
// an outage — and the authorization's foreign key onto the aggregators table
// (migrations/012:75) has nothing to point at.
//
// # WHAT THE CODE DOES, AND WHY IT IS RIGHT
//
// The insert fails, and the consumer wraps the error WITHOUT ErrPermanentEvent
// (aggregator_consumer.go:277), so the taxonomy classifies it transient: the
// connector retries in line, then writes the event to the dead letter queue,
// where the redriver replays it every five minutes until it succeeds
// (redrive.go:71). Verified by spike: an authorization written before its
// aggregator existed was absent for a minute, then appeared on its own once the
// aggregator declared itself.
//
// That is the OPPOSITE of the vote consumer, whose out-of-order events are
// silently lost (see the issue named in vote_contract_test.go), and the
// difference is worth a test of its own — "the event was captured, not dropped"
// is the property, and the dead-letter counter is where it is visible.
//
// The recovery itself is NOT asserted here: the redrive interval is five
// minutes, twice the whole tier's wall clock. It belongs to §3.4c's reliability
// suite, which owns dead letters and can drive a redrive directly.
func TestAggregatorAuthorizationArrivingBeforeItsAggregator(t *testing.T) {
	p := newPipeline(t)

	moderator := p.IndexedAccount(t, "om")
	community := indexedCommunity(t, p, "oc", moderator.DID)
	known, _ := indexedAggregator(t, p, "ok")

	// A real DID with a real repo, which has simply never declared itself. Using
	// a live account rather than a synthetic DID keeps the record honest: the
	// only thing wrong with it is the ordering.
	undeclared := provisionAggregatorRepo(t, p, "ou")

	before := p.counters(t, "aggregators")

	// The orphan, then — INTO THE SAME REPO, which is what bounds the negative
	// (see TestAggregatorServiceIngestion's spoof step for the full reasoning) —
	// an authorization that can succeed.
	community.PutRecord(t, aggregatorAuthorizationCollection, testkit.TID(),
		aggregatorAuthorizationRecord(undeclared.DID, community.DID, moderator.DID, true))
	community.PutRecord(t, aggregatorAuthorizationCollection, testkit.TID(),
		aggregatorAuthorizationRecord(known.DID, community.DID, moderator.DID, true))

	p.Await(t, "the authorization for the DECLARED aggregator, which bounds the orphan", func() (bool, error) {
		granted, err := p.CommunityAggregators(context.Background(), community.DID, false)
		if err != nil {
			return false, err
		}
		for _, g := range granted {
			if g.AggregatorDID == known.DID {
				return true, nil
			}
		}
		return false, nil
	})

	granted, err := p.CommunityAggregators(context.Background(), community.DID, false)
	require.NoError(t, err)
	require.Len(t, granted, 1,
		"the authorization naming an aggregator that has not declared itself must not be indexed: "+
			"its foreign key has nothing to point at, and a row that skipped the constraint would "+
			"mean the authorization check can name an aggregator nobody can look up")
	require.Equal(t, known.DID, granted[0].AggregatorDID)

	after := p.counters(t, "aggregators")
	require.Equalf(t, uint64(1), after.DeadLettered-before.DeadLettered,
		"the orphaned authorization must be CAPTURED, not dropped: the aggregators consumer should "+
			"have dead-lettered exactly one event in this window (delta was %d). A delta of zero "+
			"means the event vanished — the failure mode votes have — and the community's grant "+
			"would never take effect no matter how long anyone waited",
		after.DeadLettered-before.DeadLettered)
}

// TestAggregatorAPIContract covers the client-facing surface of the aggregator
// endpoints. It carries NO ingestion marker — markers are for pipeline proofs
// (§3.4a).
//
// The domain is unusual in having a PUBLIC write endpoint
// (social.coves.aggregator.register, which is how a bot introduces itself
// before it has any credential at all) and a set of key-management endpoints
// behind RequireAuth. What is asserted here is what only the running router can
// answer: that the NSIDs are routed, that the guarded ones are guarded, and
// that registration really does verify domain ownership before writing
// anything.
//
// Registration's SUCCESS path is not reachable here and that is by design, not
// omission: verifyDomainOwnership fetches https://{domain}/.well-known/atproto-did,
// and §3.7's egress-blocked network exists precisely so that nothing in this
// tier can reach a domain on the internet. The success path is proven at T0 in
// internal/api/handlers/aggregator against a local TLS stub.
func TestAggregatorAPIContract(t *testing.T) {
	p := newPipeline(t)

	ctx, cancel := context.WithTimeout(context.Background(), contractBudget)
	defer cancel()

	t.Run("registration validates before it verifies", func(t *testing.T) {
		for _, bad := range []struct {
			name  string
			input map[string]any
		}{
			{"no did", map[string]any{"domain": "example.invalid"}},
			{"not a did", map[string]any{"did": "not-a-did", "domain": "example.invalid"}},
			{"an unsupported did method", map[string]any{"did": "did:key:z6Mk", "domain": "example.invalid"}},
			{"no domain", map[string]any{"did": "did:plc:aaaaaaaaaneveraggregator"}},
			{"an http domain", map[string]any{"did": "did:plc:aaaaaaaaaneveraggregator", "domain": "http://example.invalid"}},
		} {
			err := p.AppView.Procedure(ctx, "social.coves.aggregator.register", bad.input, nil)
			require.Truef(t, testkit.IsStatus(err, http.StatusBadRequest),
				"registering with %s must answer 400 before any network call, answered: %v", bad.name, err)
		}
	})

	t.Run("registration refuses a domain it cannot verify", func(t *testing.T) {
		// The security property of the whole endpoint: anyone may call it, so
		// the only thing standing between a caller and an aggregator identity is
		// .well-known/atproto-did serving their DID over HTTPS.
		//
		// The domain here ends in .invalid, which RFC 2606 reserves precisely so
		// that it can never be registered or resolved — by ANY resolver, on any
		// network. That is the mechanism, not §3.7's egress block: the two would
		// look identical in this run and only one of them survives Phase 5, when
		// the topology grows a second PDS and a relay and the stack's networking
		// changes underneath this tier. Written down because "it fails because
		// we blocked egress" is the plausible-sounding explanation, and acting
		// on it would mean chasing this assertion the first time the network
		// changes.
		err := p.AppView.Procedure(ctx, "social.coves.aggregator.register", map[string]any{
			"did":    "did:plc:aaaaaaaaaneveraggregator",
			"domain": "unreachable." + testkit.UniqueID(t) + ".invalid",
		}, nil)
		require.Truef(t, testkit.IsStatus(err, http.StatusUnauthorized),
			"registering an unverifiable domain must answer 401, answered: %v", err)
	})

	t.Run("the query endpoints reject a request they cannot answer", func(t *testing.T) {
		err := p.AppView.Query(ctx, "social.coves.aggregator.getServices", nil, nil)
		require.Truef(t, testkit.IsStatus(err, http.StatusBadRequest),
			"getServices without dids must answer 400, answered: %v", err)

		// A DID literal at the full 24 base32 characters of a real did:plc,
		// rather than a generated one, for the reason TestPostAPIContract gives:
		// UniqueID does not promise that alphabet, and a malformed identifier
		// would take a validation path instead of the lookup path under test.
		_, err = p.Authorizations(ctx, "did:plc:aaaaaaaaaneveraggregator", false)
		require.Truef(t, testkit.IsStatus(err, http.StatusNotFound),
			"getAuthorizations for an unknown aggregator must answer 404 — its response nests the "+
				"aggregator's own view, so it has nothing to answer with. Answered: %v", err)

		_, err = p.CommunityAggregators(ctx, "did:plc:aaaaaaaaaanevercommunity", false)
		require.Truef(t, testkit.IsStatus(err, http.StatusNotFound),
			"listForCommunity for an unresolvable community must answer 404, answered: %v", err)
	})

	t.Run("the key-management endpoints refuse an unauthenticated client", func(t *testing.T) {
		// Every NSID RegisterAggregatorAPIKeyRoutes puts behind RequireAuth. An
		// API key is a bearer credential that lets a bot post into every
		// community that authorized it, so a route registered without its
		// middleware here is a key-minting endpoint open to the world.
		err := p.AppView.Procedure(ctx, "social.coves.aggregator.createApiKey", map[string]any{}, nil)
		require.Truef(t, testkit.IsStatus(err, http.StatusUnauthorized),
			"social.coves.aggregator.createApiKey must answer 401 to a client with no session, answered: %v", err)

		err = p.AppView.Query(ctx, "social.coves.aggregator.getApiKey", nil, nil)
		require.Truef(t, testkit.IsStatus(err, http.StatusUnauthorized),
			"social.coves.aggregator.getApiKey must answer 401 to a client with no session, answered: %v", err)

		err = p.AppView.Procedure(ctx, "social.coves.aggregator.revokeApiKey", map[string]any{}, nil)
		require.Truef(t, testkit.IsStatus(err, http.StatusUnauthorized),
			"social.coves.aggregator.revokeApiKey must answer 401 to a client with no session, answered: %v", err)
	})
}
