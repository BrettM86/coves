package notifications

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"Coves/internal/core/posts"
	"Coves/internal/core/richtext"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

// commentCollection is the comment record NSID. The jetstream package exports
// it, but importing jetstream here would be an import cycle.
const commentCollection = "social.coves.community.comment"

// freshnessWindow is how far before the index transaction's time a record's
// createdAt may be and still notify. See docs/PRD_NOTIFICATIONS.md, Architecture,
// "Freshness gate".
const freshnessWindow = 7 * 24 * time.Hour

// errMissingLegacyPost marks a direct reply to a legacy post whose posts row is
// missing. Such an event produces no notification at all.
var errMissingLegacyPost = errors.New("legacy post row is missing")

// FanoutCommentCreate computes reply and mention notifications for a newly indexed
// comment. An invalid thread URI has no reply recipient. Mentions require a root
// URI that parses and names a post collection, because the row's root_post_uri
// must name a post. A reply to a legacy post whose posts row is missing produces
// no notification at all, mentions included (docs/PRD_NOTIFICATIONS.md, "Post
// author resolution"). A withdrawn or unindexed comment, root or parent also
// suppresses fan-out.
// richtext.MaxFacets bounds parsed mentions; at most
// MaxMentionsPerRecord surviving mention rows may belong to the record across
// creates and edits. Excess mentions are dropped and logged. Payload defects
// do not stop comment indexing.
// A nil bridgeHosts, including a nil pointer stored in the interface, trusts no
// host (see BridgeHostChecker).
func FanoutCommentCreate(ctx context.Context, lookups Lookups, bridgeHosts BridgeHostChecker, comment CommentRecord) ([]Intent, error) {
	base := Intent{
		ActorDID:        comment.AuthorDID,
		RecordURI:       comment.URI,
		RecordCID:       comment.CID,
		RootPostURI:     comment.RootURI,
		RecordCreatedAt: comment.CreatedAt,
	}
	root, rootErr := syntax.ParseATURI(comment.RootURI)
	// The row's root_post_uri must name a post.
	rootIsPost := rootErr == nil && posts.IsPostCollection(root.Collection().String())
	if !rootIsPost {
		return nil, nil
	}
	withdrawn, err := anyWithdrawn(ctx, lookups, comment.URI, comment.RootURI, comment.ParentURI)
	if err != nil || withdrawn {
		return nil, err
	}
	reply, err := resolveCommentReply(ctx, lookups, comment, base)
	if errors.Is(err, errMissingLegacyPost) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var mentions []string
	mentionedDIDs, dropped := richtext.MentionedDIDs(comment.FacetsJSON)
	if dropped > 0 {
		slog.InfoContext(ctx, "comment mentions past the facet parse bound were dropped",
			"record_uri", comment.URI, "dropped_mentions", dropped, "parse_bound", richtext.MaxFacets)
	}
	for _, did := range mentionedDIDs {
		if did != comment.AuthorDID && did != reply.RecipientDID {
			mentions = append(mentions, did)
		}
	}
	replyRecipient := reply.RecipientDID
	if replyRecipient == comment.AuthorDID {
		replyRecipient = ""
	}
	if replyRecipient == "" && len(mentions) == 0 {
		return nil, nil
	}
	allowed, err := notificationRecordAllowed(ctx, lookups, comment.CreatedAt)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, nil
	}
	mentions, allowedRecipients, err := notificationCappedMentions(ctx, lookups, bridgeHosts, comment.AuthorDID, comment.URI, mentions, replyRecipient)
	if err != nil {
		return nil, err
	}
	var intents []Intent
	if _, ok := allowedRecipients[reply.RecipientDID]; ok {
		intents = append(intents, reply)
	}
	for _, did := range mentions {
		if facts, ok := allowedRecipients[did]; ok && !facts.Community {
			mention := base
			mention.Reason = ReasonMention
			mention.RecipientDID = did
			intents = append(intents, mention)
		}
	}
	return intents, nil
}

// FanoutPostCreate computes mention notifications for a newly indexed postv2
// post. Unparsable, non-postv2, or withdrawn posts produce no notifications.
// richtext.MaxFacets bounds parsed mentions; MaxMentionsPerRecord limits the
// record's surviving mention rows across creates and edits. Excess mentions
// are dropped and logged. A nil bridgeHosts trusts no host (see BridgeHostChecker).
func FanoutPostCreate(ctx context.Context, lookups Lookups, bridgeHosts BridgeHostChecker, post PostRecord) ([]Intent, error) {
	uri, err := syntax.ParseATURI(post.URI)
	if err != nil || uri.Collection().String() != posts.PostV2Collection {
		return nil, nil
	}
	mentionedDIDs, dropped := richtext.MentionedDIDs(post.FacetsJSON)
	if dropped > 0 {
		slog.InfoContext(ctx, "post mentions past the facet parse bound were dropped",
			"record_uri", post.URI, "dropped_mentions", dropped, "parse_bound", richtext.MaxFacets)
	}
	var candidates []string
	for _, did := range mentionedDIDs {
		if did != post.AuthorDID {
			candidates = append(candidates, did)
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	withdrawn, err := anyWithdrawn(ctx, lookups, post.URI)
	if err != nil || withdrawn {
		return nil, err
	}
	allowed, err := notificationRecordAllowed(ctx, lookups, post.CreatedAt)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, nil
	}
	candidates, _, err = notificationCappedMentions(ctx, lookups, bridgeHosts, post.AuthorDID, post.URI, candidates, "")
	if err != nil {
		return nil, err
	}
	var intents []Intent
	for _, did := range candidates {
		intents = append(intents, Intent{
			Reason: ReasonMention, RecipientDID: did, ActorDID: post.AuthorDID,
			RecordURI: post.URI, RecordCID: post.CID, RootPostURI: post.URI,
			RecordCreatedAt: post.CreatedAt,
		})
	}
	return intents, nil
}

// FanoutPostEdit notifies newly added, non-author mentions on a postv2 edit.
// CreatedAt is the stored post creation time, while CID and FacetsJSON belong
// to the edit. Mentions already in the previous facets are not notified again;
// the remaining per-record mention budget also excludes existing recipients.
// Withdrawn posts are suppressed; the stored creation time must pass activation,
// and a known edit event time must be within the freshness window measured from
// index time.
func FanoutPostEdit(ctx context.Context, lookups Lookups, bridgeHosts BridgeHostChecker, post PostRecord, previousFacetsJSON string) ([]Intent, error) {
	uri, err := syntax.ParseATURI(post.URI)
	if err != nil || uri.Collection().String() != posts.PostV2Collection {
		return nil, nil
	}
	added, dropped := addedMentionDIDs(post.FacetsJSON, previousFacetsJSON, post.AuthorDID)
	if dropped > 0 {
		slog.InfoContext(ctx, "post mentions past the facet parse bound were dropped",
			"record_uri", post.URI, "dropped_mentions", dropped, "parse_bound", richtext.MaxFacets)
	}
	if len(added) == 0 {
		return nil, nil
	}
	withdrawn, err := anyWithdrawn(ctx, lookups, post.URI)
	if err != nil || withdrawn {
		return nil, err
	}
	allowed, err := editRecordAllowed(ctx, lookups, post.URI, post.CreatedAt, post.EditEventTime)
	if err != nil || !allowed {
		return nil, err
	}
	mentions, _, err := notificationCappedMentions(ctx, lookups, bridgeHosts, post.AuthorDID, post.URI, added, "")
	if err != nil {
		return nil, err
	}
	var intents []Intent
	for _, did := range mentions {
		intents = append(intents, Intent{
			Reason: ReasonMention, RecipientDID: did, ActorDID: post.AuthorDID,
			RecordURI: post.URI, RecordCID: post.CID, RootPostURI: post.URI,
			RecordCreatedAt: post.CreatedAt,
		})
	}
	return intents, nil
}

// FanoutCommentEdit notifies mentions added relative to the previous facets,
// excluding the reply recipient. In comment, CID and FacetsJSON come from the
// edit, while ParentURI, RootURI and CreatedAt come from the row read under lock.
// A withdrawn or unindexed comment, root or parent suppresses it. The stored
// createdAt must pass activation; a known edit event time must be within the freshness window
// measured from index time. richtext.MaxFacets bounds parsed mentions;
// MaxMentionsPerRecord limits surviving mention rows across
// creates and edits, with excess mentions dropped and logged.
func FanoutCommentEdit(ctx context.Context, lookups Lookups, bridgeHosts BridgeHostChecker, comment CommentRecord, previousFacetsJSON string) ([]Intent, error) {
	root, err := syntax.ParseATURI(comment.RootURI)
	if err != nil || !posts.IsPostCollection(root.Collection().String()) {
		return nil, nil
	}
	added, dropped := addedMentionDIDs(comment.FacetsJSON, previousFacetsJSON, comment.AuthorDID)
	if dropped > 0 {
		slog.InfoContext(ctx, "comment mentions past the facet parse bound were dropped",
			"record_uri", comment.URI, "dropped_mentions", dropped, "parse_bound", richtext.MaxFacets)
	}
	if len(added) == 0 {
		return nil, nil
	}
	withdrawn, err := anyWithdrawn(ctx, lookups, comment.URI, comment.RootURI, comment.ParentURI)
	if err != nil || withdrawn {
		return nil, err
	}
	base := Intent{
		ActorDID:        comment.AuthorDID,
		RecordURI:       comment.URI,
		RecordCID:       comment.CID,
		RootPostURI:     comment.RootURI,
		RecordCreatedAt: comment.CreatedAt,
	}
	reply, err := resolveCommentReply(ctx, lookups, comment, base)
	if errors.Is(err, errMissingLegacyPost) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	mentions := added[:0]
	for _, did := range added {
		if did != reply.RecipientDID {
			mentions = append(mentions, did)
		}
	}
	if len(mentions) == 0 {
		return nil, nil
	}
	allowed, err := editRecordAllowed(ctx, lookups, comment.URI, comment.CreatedAt, comment.EditEventTime)
	if err != nil || !allowed {
		return nil, err
	}
	mentions, _, err = notificationCappedMentions(ctx, lookups, bridgeHosts, comment.AuthorDID, comment.URI, mentions, "")
	if err != nil {
		return nil, err
	}
	var intents []Intent
	for _, did := range mentions {
		mention := base
		mention.Reason = ReasonMention
		mention.RecipientDID = did
		intents = append(intents, mention)
	}
	return intents, nil
}

// addedMentionDIDs returns, in facet order, the non-author DIDs mentioned in
// newFacetsJSON but not in previousFacetsJSON, and how many mentions in
// newFacetsJSON were dropped past richtext.MaxFacets. DIDs past the previous
// parse bound were never notified, so they intentionally count as added.
func addedMentionDIDs(newFacetsJSON, previousFacetsJSON, authorDID string) ([]string, int) {
	newDIDs, dropped := richtext.MentionedDIDs(newFacetsJSON)
	previousDIDs, _ := richtext.MentionedDIDs(previousFacetsJSON)
	previouslyMentioned := make(map[string]bool, len(previousDIDs))
	for _, did := range previousDIDs {
		previouslyMentioned[did] = true
	}
	var added []string
	for _, did := range newDIDs {
		if did != authorDID && !previouslyMentioned[did] {
			added = append(added, did)
		}
	}
	return added, dropped
}

// notificationCappedMentions selects eligible mentions in facet order using
// the remaining budget, while looking up a reply recipient without charging it
// a mention slot. The caller has already checked record gates and excluded
// self and reply-recipient mentions.
func notificationCappedMentions(ctx context.Context, lookups Lookups, bridgeHosts BridgeHostChecker, actorDID, recordURI string, mentions []string, replyRecipient string) ([]string, map[string]RecipientFacts, error) {
	budget := 0
	if len(mentions) > 0 {
		existing, err := lookups.ExistingMentionRecipients(ctx, recordURI)
		if err != nil {
			return nil, nil, fmt.Errorf("look up notification existing mention recipients: %w", err)
		}
		budget = MaxMentionsPerRecord - len(existing)
		existingRecipients := make(map[string]bool, len(existing))
		for _, did := range existing {
			existingRecipients[did] = true
		}
		remaining := mentions[:0]
		for _, did := range mentions {
			if !existingRecipients[did] {
				remaining = append(remaining, did)
			}
		}
		mentions = remaining
	}
	candidates := make([]string, 0, len(mentions)+1)
	if replyRecipient != "" {
		candidates = append(candidates, replyRecipient)
	}
	if budget > 0 {
		candidates = append(candidates, mentions...)
	}
	var allowedRecipients map[string]RecipientFacts
	if len(candidates) > 0 {
		var err error
		allowedRecipients, err = notificationAllowedRecipients(ctx, lookups, bridgeHosts, actorDID, candidates)
		if err != nil {
			return nil, nil, err
		}
	}
	var selected []string
	dropped := 0
	if budget <= 0 {
		// At a full budget, no recipient facts are needed to cut all
		// remaining candidates. Already-notified recipients were removed.
		dropped = len(mentions)
	} else {
		for _, did := range mentions {
			if facts, ok := allowedRecipients[did]; ok && !facts.Community {
				if len(selected) < budget {
					selected = append(selected, did)
				} else {
					dropped++
				}
			}
		}
	}
	if dropped > 0 {
		slog.InfoContext(ctx, "mentions over the per-record notification cap were dropped",
			"record_uri", recordURI, "dropped_mentions", dropped, "mention_cap", MaxMentionsPerRecord)
	}
	return selected, allowedRecipients, nil
}

// CommentReplySubject resolves the reply subject from the comment's threading
// using the same rules as create fan-out. Missing legacy posts and unsupported
// threading have no subject and return ""; lookup failures propagate.
func CommentReplySubject(ctx context.Context, lookups Lookups, comment CommentRecord) (string, error) {
	reply, err := resolveCommentReply(ctx, lookups, comment, Intent{})
	if errors.Is(err, errMissingLegacyPost) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return reply.SubjectURI, nil
}

// resolveCommentReply sets the reply reason, recipient and subject on reply when
// the comment has a reply recipient, and returns reply unchanged otherwise. It
// returns errMissingLegacyPost only for a direct reply to a legacy post whose
// posts row has disappeared.
func resolveCommentReply(ctx context.Context, lookups Lookups, comment CommentRecord, reply Intent) (Intent, error) {
	root, rootErr := syntax.ParseATURI(comment.RootURI)
	rootIsPost := rootErr == nil && posts.IsPostCollection(root.Collection().String())
	if rootErr == nil && comment.ParentURI == comment.RootURI {
		switch root.Collection().String() {
		case posts.PostV2Collection:
			reply.Reason = ReasonPostReply
			reply.RecipientDID = root.Authority().String()
			reply.SubjectURI = comment.RootURI
		case posts.LegacyPostCollection:
			authorDID, found, err := lookups.LegacyPostAuthor(ctx, comment.RootURI)
			if err != nil {
				return Intent{}, fmt.Errorf("look up notification legacy post author: %w", err)
			}
			if !found {
				return Intent{}, errMissingLegacyPost
			}
			reply.Reason = ReasonPostReply
			reply.RecipientDID = authorDID
			reply.SubjectURI = comment.RootURI
		}
	} else if rootIsPost {
		parent, err := syntax.ParseATURI(comment.ParentURI)
		if err == nil && parent.Collection().String() == commentCollection {
			reply.Reason = ReasonCommentReply
			reply.RecipientDID = parent.Authority().String()
			reply.SubjectURI = comment.ParentURI
		}
	}
	return reply, nil
}

// notificationRecordAllowed checks the activation and freshness gates once for
// a record, before reading any recipient facts.
func notificationRecordAllowed(ctx context.Context, lookups Lookups, createdAt time.Time) (bool, error) {
	activatedAt, err := lookups.ActivatedAt(ctx)
	if err != nil {
		return false, fmt.Errorf("look up notification activation time: %w", err)
	}
	if createdAt.Before(activatedAt) {
		return false, nil
	}
	indexTime, err := lookups.IndexTime(ctx)
	if err != nil {
		return false, fmt.Errorf("look up notification index time: %w", err)
	}
	if createdAt.Before(indexTime.Add(-freshnessWindow)) {
		return false, nil
	}
	return true, nil
}

// anyWithdrawn checks the distinct reference URIs together before recipient,
// activation, or freshness reads. Any state but ReferenceLive withdraws,
// including ReferenceUnindexed. Comment fan-out passes the comment's own URI as
// well, since a removal can predate indexing or outlive a re-create at that URI.
func anyWithdrawn(ctx context.Context, lookups Lookups, uris ...string) (bool, error) {
	distinct := make([]string, 0, len(uris))
	for _, uri := range uris {
		found := false
		for _, previous := range distinct {
			if previous == uri {
				found = true
				break
			}
		}
		if !found {
			distinct = append(distinct, uri)
		}
	}
	states, err := lookups.ReferenceStates(ctx, distinct)
	if err != nil {
		return false, fmt.Errorf("look up notification reference states: %w", err)
	}
	for _, uri := range distinct {
		if states[uri] != ReferenceLive {
			return true, nil
		}
	}
	return false, nil
}

// editRecordAllowed is the gate shared by post and comment edit mentions: the
// stored createdAt must pass activation, and a known edit event time must be
// within the freshness window measured from index time. A zero editEventTime
// means the event has no Jetstream timestamp, so freshness cannot suppress it.
func editRecordAllowed(ctx context.Context, lookups Lookups, recordURI string, createdAt, editEventTime time.Time) (bool, error) {
	activatedAt, err := lookups.ActivatedAt(ctx)
	if err != nil {
		return false, fmt.Errorf("look up notification activation time: %w", err)
	}
	if createdAt.Before(activatedAt) {
		slog.DebugContext(ctx, "edit mentions suppressed: record predates notification activation",
			"record_uri", recordURI)
		return false, nil
	}
	if editEventTime.IsZero() {
		return true, nil
	}
	indexTime, err := lookups.IndexTime(ctx)
	if err != nil {
		return false, fmt.Errorf("look up notification index time: %w", err)
	}
	if editEventTime.Before(indexTime.Add(-freshnessWindow)) {
		slog.DebugContext(ctx, "edit mentions suppressed: edit event is outside the freshness window",
			"record_uri", recordURI)
		return false, nil
	}
	return true, nil
}

// notificationAllowedRecipients reads all candidate facts at once and retains
// indexed recipients eligible for notification. A nil bridgeHosts trusts no host.
func notificationAllowedRecipients(ctx context.Context, lookups Lookups, bridgeHosts BridgeHostChecker, actorDID string, candidates []string) (map[string]RecipientFacts, error) {
	facts, err := lookups.RecipientFacts(ctx, actorDID, candidates)
	if err != nil {
		return nil, fmt.Errorf("look up notification recipient facts: %w", err)
	}
	allowed := make(map[string]RecipientFacts, len(facts))
	for _, did := range candidates {
		fact, found := facts[did]
		if !found || fact.Erased || fact.Aggregator || (bridgeHosts != nil && bridgeHosts.TrustsPDS(fact.PDSURL)) || fact.BlockedWithActor {
			continue
		}
		allowed[did] = fact
	}
	return allowed, nil
}

// FanoutVoteCreate bumps the group only for a voter's first qualifying upvote
// on a subject, or asks the repository to delete it if empty when a resolved
// vote does not qualify. After resolving the subject, it checks direction,
// comment root, self-vote, withdrawn references, activation and freshness,
// voter eligibility, and recipient eligibility before checking for another
// upvote by this voter.
// The earlier-upvote lookup is last: only a vote that would otherwise bump
// needs the index probe, while repeat voters pass through the other lookups.
// Unresolvable subjects give no intent and no error; lookup errors propagate.
func FanoutVoteCreate(ctx context.Context, lookups Lookups, bridgeHosts BridgeHostChecker, vote VoteRecord) (UpvoteGroupIntent, error) {
	author, resolved, err := resolveVoteSubjectAuthor(ctx, lookups, vote.SubjectURI)
	if err != nil {
		return UpvoteGroupIntent{}, err
	}
	if !resolved {
		return UpvoteGroupIntent{}, nil
	}
	deleteIfEmpty := UpvoteGroupIntent{
		Action: UpvoteGroupDeleteIfEmpty, RecipientDID: author, SubjectURI: vote.SubjectURI,
	}
	if vote.Direction != "up" {
		return deleteIfEmpty, nil
	}
	rootPostURI, hasRootPost := voteBumpRootPostURI(vote)
	if !hasRootPost {
		return deleteIfEmpty, nil
	}
	if author == vote.VoterDID {
		return deleteIfEmpty, nil
	}
	withdrawn, err := anyWithdrawn(ctx, lookups, vote.SubjectURI, rootPostURI)
	if err != nil {
		return UpvoteGroupIntent{}, err
	}
	if withdrawn {
		return deleteIfEmpty, nil
	}
	allowed, err := notificationRecordAllowed(ctx, lookups, vote.CreatedAt)
	if err != nil {
		return UpvoteGroupIntent{}, err
	}
	if !allowed {
		return deleteIfEmpty, nil
	}
	if vote.VoterErased {
		return deleteIfEmpty, nil
	}
	aggregator, err := lookups.IsAggregator(ctx, vote.VoterDID)
	if err != nil {
		return UpvoteGroupIntent{}, fmt.Errorf("look up notification voter aggregator status: %w", err)
	}
	if aggregator {
		return deleteIfEmpty, nil
	}
	recipients, err := notificationAllowedRecipients(ctx, lookups, bridgeHosts, vote.VoterDID, []string{author})
	if err != nil {
		return UpvoteGroupIntent{}, err
	}
	if _, ok := recipients[author]; !ok {
		return deleteIfEmpty, nil
	}
	earlierUpvote, err := lookups.EarlierUpvoteExists(ctx, vote.VoterDID, vote.SubjectURI, vote.URI)
	if err != nil {
		return UpvoteGroupIntent{}, fmt.Errorf("look up notification voter earlier upvote: %w", err)
	}
	if earlierUpvote {
		return deleteIfEmpty, nil
	}
	return UpvoteGroupIntent{
		Action: UpvoteGroupBump, RecipientDID: author,
		SubjectURI: vote.SubjectURI, RootPostURI: rootPostURI,
	}, nil
}

// resolveVoteSubjectAuthor identifies the author of a supported vote subject,
// who receives its upvote group: a postv2's URI authority, a legacy post's row
// author, or a comment's URI authority whatever its root. Unparsable URIs, other
// collections and legacy posts without a row are unresolved.
func resolveVoteSubjectAuthor(ctx context.Context, lookups Lookups, subjectURI string) (author string, resolved bool, err error) {
	subject, err := syntax.ParseATURI(subjectURI)
	if err != nil {
		return "", false, nil
	}
	switch subject.Collection().String() {
	case posts.PostV2Collection, commentCollection:
		return subject.Authority().String(), true, nil
	case posts.LegacyPostCollection:
		author, found, err := lookups.LegacyPostAuthor(ctx, subjectURI)
		if err != nil {
			return "", false, fmt.Errorf("look up notification legacy post author: %w", err)
		}
		return author, found, nil
	default:
		return "", false, nil
	}
}

// voteBumpRootPostURI returns the root post a bump records for a vote on a
// resolved subject: the post itself, or a comment's stored root when that
// root parses and names a post collection. Otherwise the vote cannot bump.
func voteBumpRootPostURI(vote VoteRecord) (string, bool) {
	return bumpRootPostURI(vote.SubjectURI, vote.SubjectRootURI)
}

func bumpRootPostURI(subjectURI, subjectRootURI string) (string, bool) {
	subject, err := syntax.ParseATURI(subjectURI)
	if err != nil {
		return "", false
	}
	switch subject.Collection().String() {
	case posts.PostV2Collection, posts.LegacyPostCollection:
		return subjectURI, true
	case commentCollection:
		root, err := syntax.ParseATURI(subjectRootURI)
		if err != nil || !posts.IsPostCollection(root.Collection().String()) {
			return "", false
		}
		return subjectRootURI, true
	default:
		return "", false
	}
}

// FanoutVoteRemoval asks the repository to delete an empty group when a vote
// leaves a resolvable subject's vote set, regardless of vote eligibility or a
// comment's root. It reads only vote.SubjectURI.
// Unresolvable subjects give no intent and no error; lookup errors propagate.
func FanoutVoteRemoval(ctx context.Context, lookups Lookups, vote VoteRecord) (UpvoteGroupIntent, error) {
	author, resolved, err := resolveVoteSubjectAuthor(ctx, lookups, vote.SubjectURI)
	if err != nil {
		return UpvoteGroupIntent{}, err
	}
	if !resolved {
		return UpvoteGroupIntent{}, nil
	}
	return UpvoteGroupIntent{
		Action: UpvoteGroupDeleteIfEmpty, RecipientDID: author, SubjectURI: vote.SubjectURI,
	}, nil
}

// BridgedUpvoteChange is one observed change in an item's stored bridged upvote total.
type BridgedUpvoteChange struct {
	SubjectURI      string
	SubjectRootURI  string
	PreviousUpvotes int
	Upvotes         int
	// PeakUpvotes is the item's stored bridged upvote high-water mark. The
	// Jetstream record path writes the count without raising it, so it can lag
	// PreviousUpvotes; the effective mark is max(PeakUpvotes, PreviousUpvotes).
	PeakUpvotes int
}

// FanoutBridgedUpvoteChange resolves the subject author first. A decrease asks
// to delete an empty group. A bump needs a total above the item's high-water
// mark; decreases and re-rises up to it change only the displayed count. A new
// high checks for a post root, withdrawn subject or root, and an eligible
// recipient (with no actor). The high-water mark is backfilled from the stored
// total at launch; new highs are exempt from activation cutoff, 7-day
// freshness and asOf age checks, even for old content.
func FanoutBridgedUpvoteChange(ctx context.Context, lookups Lookups, bridgeHosts BridgeHostChecker, change BridgedUpvoteChange) (UpvoteGroupIntent, error) {
	author, resolved, err := resolveVoteSubjectAuthor(ctx, lookups, change.SubjectURI)
	if err != nil || !resolved {
		return UpvoteGroupIntent{}, err
	}
	if change.Upvotes < change.PreviousUpvotes {
		return UpvoteGroupIntent{Action: UpvoteGroupDeleteIfEmpty, RecipientDID: author, SubjectURI: change.SubjectURI}, nil
	}
	if change.Upvotes <= max(change.PeakUpvotes, change.PreviousUpvotes) {
		return UpvoteGroupIntent{}, nil
	}
	rootPostURI, hasRootPost := bumpRootPostURI(change.SubjectURI, change.SubjectRootURI)
	if !hasRootPost {
		return UpvoteGroupIntent{}, nil
	}
	withdrawn, err := anyWithdrawn(ctx, lookups, change.SubjectURI, rootPostURI)
	if err != nil || withdrawn {
		return UpvoteGroupIntent{}, err
	}
	recipients, err := notificationAllowedRecipients(ctx, lookups, bridgeHosts, "", []string{author})
	if err != nil {
		return UpvoteGroupIntent{}, err
	}
	if _, allowed := recipients[author]; !allowed {
		return UpvoteGroupIntent{}, nil
	}
	return UpvoteGroupIntent{
		Action: UpvoteGroupBump, RecipientDID: author,
		SubjectURI: change.SubjectURI, RootPostURI: rootPostURI,
	}, nil
}
