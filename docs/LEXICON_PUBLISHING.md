# Publishing the social.coves.* Lexicons

The lexicons under `internal/atproto/lexicon/social/coves/` are published to the
AT Protocol network as `com.atproto.lexicon.schema` records, resolvable through
`_lexicon` DNS TXT records on `coves.social`. Once a schema is published, the
atproto evolution rules apply: additive changes only (new optional fields, new
`knownValues`). Anything breaking needs a new NSID.

`tests/lexicon_publishing_runbook_test.go` pins the executable parts of this
file: the held-back list, that every `goat lex publish` operand is an explicit
`.json` file (never a directory, never a held-back file), that every
publishable schema appears in a publish command, and the moderation release
order (`moderation/defs.json`, then the comment and post `defs.json`, then the
moderation endpoints). It checks no other ordering.
Edit the commands with that test in mind.

## Design decisions

- Nested NSIDs are deliberate for the published content APIs. Operations live
  under their record noun (`community.post.create` under the `community.post`
  record) rather than Bluesky-style flat verbs (`createPost`). This is a legacy
  convention of the published APIs, not a reason to rename them. The extra DNS
  delegations it needs are handled by `scripts/publish-lexicon-dns.sh`.
- `moderation.*` uses verb-noun endpoint names (`removeContent`,
  `restoreContent`, `labelContent`, `retractContentLabel`, `listActions`,
  `listAdminActions`, `getSubjectState`) with shared types in
  `moderation.defs`, following the published Lexicon style guide. The
  verb-noun rule covers new moderation names only. Existing groups keep their
  nested convention.
- `moderation.defs#banView` is published as it stands. It is a generic
  `uri`/`cid`/`record`/`indexedAt` record view referenced only by the held-back
  `banUser`, `listBans` and `getBanStatus`. Publishing `moderation.defs`
  publishes it unchanged, and it cannot be removed afterwards.
- Post search is `feed.searchPosts`, not `community.post.search` or
  `community.searchPosts`. `feed.*` is the read-many namespace (output
  `{feed, cursor}`, mirrors `app.bsky.feed.searchPosts`); `community.post.*`
  is single-post CRUD, and `community.search` returns communities.
- Embed `alt` text is optional, permanently. It matches live bridge content
  and clients.
- `updateProfile` input takes `bio`; the profile record stores `description`.
  Both are wire truth.
- The post body cap is 50,000 graphemes / 500,000 bytes, set on `postv2`
  before its first publish and raised on `post.create` / `post.update`
  (2026-09-07). It matches Lemmy's 50k post-body limit, the platform we bridge
  from. The old 10k cap was the comment tier and rejected live kagi-news
  digests. `MaxContentLength` in `internal/core/posts/service.go` and
  `MaxPostContentBytes` in `internal/atproto/jetstream/authorpost.go` mirror
  the byte cap.

New moderation-style schemas follow the same rules: `knownValues` rather than
closed enums, open unions, no property named `$type`, and no ref changed into a
union or required field made optional in place. Use a new endpoint or type for
those.

## Accepted evolution-rule exceptions

`goat lex breaking` flags these on every run. Each was accepted because the
published schema described something the AppView never served or had already
stopped accepting, so the local schema is the wire truth. Do not add to this
list without the same justification.

Accepted 2026-09-07:

- The `community` parameter on `community.get`, `getMembers`,
  `getSubscribers`, `subscribe`, `unsubscribe`, `post.create`,
  `feed.getCommunity` and `aggregator.listForCommunity` dropped
  `format: at-identifier` for a plain string so `name@origin` and bare names
  resolve. This loosens input only; old inputs stay valid. The same
  application-identifier exception applies to the `community` filter on
  `moderation.listActions` and `listAdminActions`.
- `embed.external#viewExternal.images[]` moved its ref from `images#image`
  (blob) to `images#viewImage` (URLs), which is what `projectExternal` serves.
  The published schema described a blob the AppView never served.
- The `content` cap on `post.create` / `post.update` rose from 10k to 50k
  graphemes (input loosening, see above).

Accepted 2026-09-21, output-loosening:

- `community.comment.defs#commentView.record` is no longer `required` and is
  declared `nullable`. The exception covers both changes. The published schema
  required a verbatim record, but the AppView has served author-deleted
  comments as a content-free placeholder with `record: null` since soft
  deletion shipped, and moderator removal reuses that placeholder. No reader
  that works today breaks, because the wire shape is unchanged. This is the
  only exception that relaxes a required output field. No other nullability
  change and no ref-to-union change is authorized.

## Published and held back

Published: `richtext.*`, `embed.*`, `actor.*` except
`actor/getViewerAuthority.json`, `feed.*`, `aggregator.*`, and
`community.*` (including `post.*` and `comment.*`) except the held-back files
below. Once the moderation release runs, also the reviewed moderation package:
`moderation/defs.json` and the seven endpoint files
`moderation/removeContent.json`, `moderation/restoreContent.json`,
`moderation/labelContent.json`, `moderation/retractContentLabel.json`,
`moderation/listActions.json`, `moderation/listAdminActions.json`,
`moderation/getSubjectState.json`, plus `actor/getViewerAuthority.json`, which
tells clients whether the viewer is an instance admin and ships with them; and,
once the notifications release below runs, `notification/defs.json` and its
five endpoint files.

Held back, because the governance and tribunal design has not landed. Paths
are relative to `internal/atproto/lexicon/social/coves/`, and the test fails if
a publish command names any of them:

- `moderation/ban.json`
- `moderation/banUser.json`
- `moderation/getBanStatus.json`
- `moderation/listBans.json`
- `moderation/unbanUser.json`
- `moderation/ruleProposal.json`
- `moderation/tribunalVote.json`
- `moderation/vote.json`
- `community/rules.json`
- `community/moderator.json`

`goat lex publish` sweeps every schema in a directory operand, so the commands
below name files, never the `actor`, `moderation` or `community` directory.
`community/rules.json` and `community/moderator.json` share the published
`community` authority, so only the explicit file list keeps them off the
network.

## Publishing identity and DNS delegation

The publisher is `did:web:coves.social` (handle `coves.social`) on
`https://pds.coves.me`. This is the instance's own account: `INSTANCE_DID` and
`PDS_INSTANCE_HANDLE` in `.env.prod.example`. The schema records live in its
repo.

DNS delegation is one TXT record per unique NSID authority, each with content
`did=did:web:coves.social`:

| Record name | Status |
|---|---|
| `_lexicon.actor.coves.social` | live |
| `_lexicon.aggregator.coves.social` | live |
| `_lexicon.community.coves.social` | live |
| `_lexicon.comment.community.coves.social` | live |
| `_lexicon.post.community.coves.social` | live |
| `_lexicon.embed.coves.social` | live |
| `_lexicon.feed.coves.social` | live |
| `_lexicon.vote.feed.coves.social` | live |
| `_lexicon.richtext.coves.social` | live |
| `_lexicon.moderation.coves.social` | created in the moderation release, step 3a |
| `_lexicon.notification.coves.social` | created in the notifications release, step 4a |

`scripts/publish-lexicon-dns.sh` creates or updates them through the
Cloudflare API and is idempotent. It needs `CF_API_TOKEN` (Zone:DNS:Edit on
`coves.social`) and `LEXICON_DID`, read from the environment or from the
gitignored `scripts/.env.lexicon`. Never put the token in the script.
`--include-moderation` adds `_lexicon.moderation`, and `--include-notification`
adds `_lexicon.notification`.

## Publish and update workflow

Step 0 runs on every publish. Steps 1 and 2 republish the live namespaces.
Step 3 is the moderation release and step 4 the notifications release, each
run once in the order given; neither has run yet. Step 5 verifies. Every
operand is an explicit file.

```sh
export LEXICONS_DIR=internal/atproto/lexicon   # goat defaults to ./lexicons/

# 0. Gates. All must pass; review the lint/breaking/diff output by hand.
go run ./cmd/validate-lexicon                              # schemas + fixtures (also runs at T0)
go run ./cmd/validate-live -pds https://pds.coves.me       # live-record compatibility: no live record invalidated
goat lex lint internal/atproto/lexicon/social/coves        # style (large-string warnings are intentional)
goat lex breaking internal/atproto/lexicon/social/coves    # evolution rules vs published state; only the accepted exceptions may appear
goat lex diff internal/atproto/lexicon/social/coves        # read every changed schema before --update

# 1. Log in as the publishing account (goat caches the session).
goat account login -u coves.social -p '<password>' --pds-host https://pds.coves.me

# 2. Already-published namespaces. `goat lex publish`:
#    - refuses NSIDs whose _lexicon DNS does not point at the logged-in account
#      (never pass --skip-dns-check)
#    - skips schemas identical to what is live; --update is required to
#      overwrite changed ones
#    community/comment/defs.json and community/post/defs.json are not here:
#    they reference moderation.defs and move to step 3. community/post/get.json
#    is deferred to step 3d too: its output union references post.defs#moderatedPost.
goat lex publish --update \
  internal/atproto/lexicon/social/coves/richtext/facet.json \
  internal/atproto/lexicon/social/coves/embed/external.json \
  internal/atproto/lexicon/social/coves/embed/images.json \
  internal/atproto/lexicon/social/coves/embed/post.json \
  internal/atproto/lexicon/social/coves/embed/video.json \
  internal/atproto/lexicon/social/coves/actor/block.json \
  internal/atproto/lexicon/social/coves/actor/defs.json \
  internal/atproto/lexicon/social/coves/actor/getComments.json \
  internal/atproto/lexicon/social/coves/actor/getPosts.json \
  internal/atproto/lexicon/social/coves/actor/getProfile.json \
  internal/atproto/lexicon/social/coves/actor/profile.json \
  internal/atproto/lexicon/social/coves/actor/signup.json \
  internal/atproto/lexicon/social/coves/actor/updateProfile.json \
  internal/atproto/lexicon/social/coves/feed/defs.json \
  internal/atproto/lexicon/social/coves/feed/getAll.json \
  internal/atproto/lexicon/social/coves/feed/getCommunity.json \
  internal/atproto/lexicon/social/coves/feed/getDiscover.json \
  internal/atproto/lexicon/social/coves/feed/getTimeline.json \
  internal/atproto/lexicon/social/coves/feed/searchPosts.json \
  internal/atproto/lexicon/social/coves/feed/vote.json \
  internal/atproto/lexicon/social/coves/feed/vote/create.json \
  internal/atproto/lexicon/social/coves/feed/vote/delete.json \
  internal/atproto/lexicon/social/coves/aggregator/authorization.json \
  internal/atproto/lexicon/social/coves/aggregator/createApiKey.json \
  internal/atproto/lexicon/social/coves/aggregator/defs.json \
  internal/atproto/lexicon/social/coves/aggregator/disable.json \
  internal/atproto/lexicon/social/coves/aggregator/enable.json \
  internal/atproto/lexicon/social/coves/aggregator/getApiKey.json \
  internal/atproto/lexicon/social/coves/aggregator/getAuthorizations.json \
  internal/atproto/lexicon/social/coves/aggregator/getServices.json \
  internal/atproto/lexicon/social/coves/aggregator/listForCommunity.json \
  internal/atproto/lexicon/social/coves/aggregator/register.json \
  internal/atproto/lexicon/social/coves/aggregator/revokeApiKey.json \
  internal/atproto/lexicon/social/coves/aggregator/service.json \
  internal/atproto/lexicon/social/coves/aggregator/updateConfig.json \
  internal/atproto/lexicon/social/coves/community/acceptance.json \
  internal/atproto/lexicon/social/coves/community/block.json \
  internal/atproto/lexicon/social/coves/community/comment.json \
  internal/atproto/lexicon/social/coves/community/comment/create.json \
  internal/atproto/lexicon/social/coves/community/comment/delete.json \
  internal/atproto/lexicon/social/coves/community/comment/getComments.json \
  internal/atproto/lexicon/social/coves/community/comment/update.json \
  internal/atproto/lexicon/social/coves/community/create.json \
  internal/atproto/lexicon/social/coves/community/defs.json \
  internal/atproto/lexicon/social/coves/community/get.json \
  internal/atproto/lexicon/social/coves/community/getBlockedCommunities.json \
  internal/atproto/lexicon/social/coves/community/getMembers.json \
  internal/atproto/lexicon/social/coves/community/getSubscribers.json \
  internal/atproto/lexicon/social/coves/community/list.json \
  internal/atproto/lexicon/social/coves/community/post.json \
  internal/atproto/lexicon/social/coves/community/post/create.json \
  internal/atproto/lexicon/social/coves/community/post/delete.json \
  internal/atproto/lexicon/social/coves/community/post/getStatus.json \
  internal/atproto/lexicon/social/coves/community/post/update.json \
  internal/atproto/lexicon/social/coves/community/postv2.json \
  internal/atproto/lexicon/social/coves/community/profile.json \
  internal/atproto/lexicon/social/coves/community/removal.json \
  internal/atproto/lexicon/social/coves/community/search.json \
  internal/atproto/lexicon/social/coves/community/subscribe.json \
  internal/atproto/lexicon/social/coves/community/subscription.json \
  internal/atproto/lexicon/social/coves/community/unsubscribe.json \
  internal/atproto/lexicon/social/coves/community/update.json \
  internal/atproto/lexicon/social/coves/community/wiki.json

# 3. Moderation release (once, after the release gates below pass). Order
#    matters: the content-view definitions reference moderation.defs, and the
#    endpoints reference both, so each publish must resolve before the next.
#    3a. DNS delegation for the new authority, then confirm resolution:
CF_API_TOKEN=... LEXICON_DID=did:web:coves.social ./scripts/publish-lexicon-dns.sh --include-moderation
goat lex check-dns internal/atproto/lexicon/social/coves/moderation/defs.json
#    3b. Shared definitions first (publishes #banView as it stands):
goat lex publish \
  internal/atproto/lexicon/social/coves/moderation/defs.json
#    3c. Dependency closure: every ref in moderation.defs must resolve over
#        DNS/PDS before anything that references it is published.
goat lex resolve social.coves.moderation.defs
#    3d. Content-view definitions that reference moderation.defs (the
#        commentView.record exception is expected in `goat lex breaking`),
#        then community.post.get, whose output union references
#        post.defs#moderatedPost and so must follow post.defs:
goat lex publish --update \
  internal/atproto/lexicon/social/coves/community/comment/defs.json \
  internal/atproto/lexicon/social/coves/community/post/defs.json
goat lex resolve social.coves.community.comment.defs
goat lex resolve social.coves.community.post.defs
goat lex publish --update internal/atproto/lexicon/social/coves/community/post/get.json
goat lex resolve social.coves.community.post.get
#    3e. The seven reviewed endpoints and actor.getViewerAuthority, last:
goat lex publish \
  internal/atproto/lexicon/social/coves/moderation/removeContent.json \
  internal/atproto/lexicon/social/coves/moderation/restoreContent.json \
  internal/atproto/lexicon/social/coves/moderation/labelContent.json \
  internal/atproto/lexicon/social/coves/moderation/retractContentLabel.json \
  internal/atproto/lexicon/social/coves/moderation/listActions.json \
  internal/atproto/lexicon/social/coves/moderation/listAdminActions.json \
  internal/atproto/lexicon/social/coves/moderation/getSubjectState.json \
  internal/atproto/lexicon/social/coves/actor/getViewerAuthority.json

# 4. Notifications release (once). notification.defs references actor.defs
#    and community.post.defs, so it runs after step 3d; the endpoints
#    reference notification.defs, so defs go first.
#    4a. DNS delegation for the new authority, then confirm resolution:
CF_API_TOKEN=... LEXICON_DID=did:web:coves.social ./scripts/publish-lexicon-dns.sh --include-notification
goat lex check-dns internal/atproto/lexicon/social/coves/notification/defs.json
#    4b. Shared definitions, then the five endpoints:
goat lex publish \
  internal/atproto/lexicon/social/coves/notification/defs.json
goat lex resolve social.coves.notification.defs
goat lex publish \
  internal/atproto/lexicon/social/coves/notification/getUnreadCount.json \
  internal/atproto/lexicon/social/coves/notification/listNotifications.json \
  internal/atproto/lexicon/social/coves/notification/updateSeen.json \
  internal/atproto/lexicon/social/coves/notification/getPreferences.json \
  internal/atproto/lexicon/social/coves/notification/putPreferences.json

# 5. Verify resolution end to end.
goat lex check-dns internal/atproto/lexicon/social/coves
goat lex status internal/atproto/lexicon/social/coves
goat lex resolve social.coves.community.post
goat lex resolve social.coves.moderation.listActions
goat lex resolve social.coves.notification.listNotifications
```

Record updates use the same `goat lex publish --update`: records are keyed by
NSID and overwritten in place. Run `goat lex breaking` and `goat lex diff`
first, always. Anything `breaking` flags that is not in the accepted
exceptions needs a new NSID, not an `--update`.

## Retired schemas

`goat lex publish` writes only what it is given, so a schema whose file was
deleted stays live until it is explicitly removed with
`goat lex unpublish <nsid>`. A 2026-07 directory sweep published three schemas
that should not be live: `social.coves.community.post.search` (never served,
replaced by `feed.searchPosts`), and the held-back `community.rules` and
`community.moderator`. All three were unpublished; as of 2026-10-06 none of
them resolves. If `goat lex resolve` ever succeeds for one of them, unpublish
it again.

## Release gates the build does not run

`make ci` proves the schemas load, every fragment resolves locally, and the
fixtures behave. These gates are operator- or client-owned. They must pass
before the moderation release and before the admin workflow is enabled:

1. `goat lex lint` / `goat lex breaking` / `goat lex diff` review of the whole
   `social/coves` tree, by hand, with only the accepted exceptions appearing
   in `breaking`.
2. DNS delegation of `_lexicon.moderation.coves.social` and
   `goat lex check-dns` before step 3b.
3. Dependency closure resolution through DNS/PDS after each publish in step 3
   (`goat lex resolve` on `moderation.defs`, then on `comment.defs` and
   `post.defs`, then on each endpoint). A reference that does not resolve on
   the network blocks the next publish.
4. Live-record compatibility check (`cmd/validate-live` against an explicitly
   selected real PDS; never a public identity or PDS dependency in the
   hermetic stack).
5. Older-decoder gate for `community.post.get` in the client repos
   (`coves-frontend`, `coves-mobile`). Supported web and mobile builds must
   decode a 25-URI response containing normal posts plus an unknown union
   variant, render `deletionReason: moderator`, tolerate the optional
   `moderation` object, and handle thread `NotFound` without a crash loop.
   Until this passes, leave `MODERATION_ADMINS` empty so no admin action can
   create a removal: `community.post.get` serves `#moderatedPost` for every
   instance removal and has no `#notFoundPost` fallback switch. Never
   substitute the community-only `#removedPost`.

[MODERATION.md](MODERATION.md) cites release gate 5 by number, so keep the
numbering.
