# Publishing the social.coves.* Lexicons

The lexicons under `internal/atproto/lexicon/social/coves/` are published to the
AT Protocol network as `com.atproto.lexicon.schema` records, resolvable via
`_lexicon` DNS TXT records on `coves.social`. Once published, atproto schema
evolution rules apply: **additive changes only** (new optional fields, new
knownValues). Anything breaking-shaped needs a new NSID.

`tests/lexicon_publishing_runbook_test.go` pins the executable parts of this
document: the held-back file list, that every `goat lex publish` operand is an
explicit `.json` file (never a directory, never a held-back file), that every
publishable schema appears in a publish command, and the dependency order.

## Design decisions (do not revisit casually)

- **Nested NSIDs are deliberate for the published content APIs.** Operations
  live under their record noun (`community.post.create` under the
  `community.post` record) rather than Bluesky-style flat verbs
  (`createPost`). This is a documented legacy convention of the published
  APIs, not a reason to rename them; the extra DNS delegations it requires are
  handled by the script below.
- **`moderation.*` uses verb–noun endpoint names** (`removeContent`,
  `restoreContent`, `labelContent`, `retractContentLabel`, `listActions`,
  `listAdminActions`, `getSubjectState`) with shared types in
  `moderation.defs`, following the published Lexicon style guide (decided
  2026-09-21, PRD §14.1). The verb-noun decision is limited to new moderation
  names; existing groups keep their nested convention.
- **`moderation.defs#banView` is published as it stands** (decided
  2026-09-20). It is a generic `uri`/`cid`/`record`/`indexedAt` record view
  referenced only by the held-back `banUser`, `listBans` and `getBanStatus`;
  publishing `moderation.defs` publishes it unchanged, and it cannot be
  removed afterwards.
- **Embed `alt` text is optional, permanently** (decided 2026-07-23) — matches
  live bridge content and clients.
- **`updateProfile` input takes `bio`; the record stores `description`.** Wire
  truth on both sides.
- **Post body cap is 50,000 graphemes / 500,000 bytes** (decided 2026-09-07,
  set on `postv2` before its first publish and raised on `post.create` /
  `post.update`). Matches Lemmy's 50k post-body limit, the platform we bridge
  from; the old 10k cap was the comment tier and rejected ~100 live kagi-news
  digests. `internal/core/posts/service.go` mirrors the byte cap.

## Accepted evolution-rule exceptions

`goat lex breaking` flags these on every run. They were accepted because the
published schema described something the AppView never served or had already
stopped accepting; the local schema is the wire truth. Do not add to this list
without the same justification.

Accepted 2026-09-07:

- **`community` query parameter** on `community.get`, `getMembers`,
  `getSubscribers`, `subscribe`, `unsubscribe`, `post.create`,
  `feed.getCommunity`, `aggregator.listForCommunity`: dropped
  `format: at-identifier` for a plain string so `name@origin` and bare names
  resolve (loosens input only; old inputs stay valid). The same
  application-identifier exception applies to the `community` filter on
  `moderation.listActions` / `listAdminActions`.
- **`embed.external#viewExternal.images[]`** ref moved from `images#image`
  (blob) to `images#viewImage` (URLs), which is what `projectExternal` has
  served since the media work (output ref correction: the published schema
  described a blob the AppView never served).
- **`content` cap** on `post.create` / `post.update` raised 10k→50k graphemes
  (see above; loosens input only).

Accepted 2026-09-21 (**output-loosening**; the only exception that relaxes a
required output field; PRD §14.4):

- **`community.comment.defs#commentView.record`** is no longer `required` and
  is declared `nullable`; the exception covers both the `required` removal and
  the `nullable` declaration. The published schema required a verbatim record,
  but the AppView has served author-deleted comments as a content-free
  placeholder with `record: null` since soft deletion shipped, and moderator
  removal reuses that placeholder. No reader that works today breaks, because
  the wire shape is unchanged. Beyond this entry, no other nullability change
  and no ref-to-union change is authorized (PRD §14.6).

## What is published vs held back

**Published:** `richtext.*`, `embed.*`, `actor.*`, `feed.*`, `aggregator.*`,
`community.*` except the held-back files below (including `post.*` and
`comment.*`), and — once the moderation release below runs — the reviewed
moderation package: `moderation/defs.json` and the seven endpoint files
`moderation/removeContent.json`, `moderation/restoreContent.json`,
`moderation/labelContent.json`, `moderation/retractContentLabel.json`,
`moderation/listActions.json`, `moderation/listAdminActions.json`,
`moderation/getSubjectState.json`.

**Held back** (governance/tribunal design has not landed; each is listed by
path relative to `internal/atproto/lexicon/social/coves/`, and the test fails
if a publish command names any of them or a directory that contains them):

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
below name files, never the `moderation` or `community` directory. Retired
schemas stay live until explicitly unpublished (see step 4).

## One-time setup

1. **Publishing account.** The schema records live in a normal atproto repo.
   Create a dedicated account so the publishing identity is project-owned:
   ```sh
   goat account create \
     --pds-host https://pds.coves.me \
     --handle lexicons.coves.social \
     --password '<strong password>' \
     --email <admin email> \
     [--invite-code <code if the PDS requires one>]
   ```
   Note the DID it prints (`goat resolve lexicons.coves.social` shows it).
   An existing account works too — the DNS records just point at whichever DID
   holds the schema records.

2. **DNS delegation.** One TXT record per unique NSID authority:

   | Record name | Content |
   |---|---|
   | `_lexicon.actor.coves.social` | `did=<publishing DID>` |
   | `_lexicon.aggregator.coves.social` | `did=<publishing DID>` |
   | `_lexicon.community.coves.social` | `did=<publishing DID>` |
   | `_lexicon.comment.community.coves.social` | `did=<publishing DID>` |
   | `_lexicon.post.community.coves.social` | `did=<publishing DID>` |
   | `_lexicon.embed.coves.social` | `did=<publishing DID>` |
   | `_lexicon.feed.coves.social` | `did=<publishing DID>` |
   | `_lexicon.vote.feed.coves.social` | `did=<publishing DID>` |
   | `_lexicon.richtext.coves.social` | `did=<publishing DID>` |
   | `_lexicon.moderation.coves.social` | `did=<publishing DID>` — created in the moderation release, step 3 |

   Via the Cloudflare API (token needs Zone:DNS:Edit on `coves.social`):
   ```sh
   CF_API_TOKEN=... LEXICON_DID=did:plc:... ./scripts/publish-lexicon-dns.sh
   # add --include-moderation for the moderation release (step 3 below)
   ```

## Publish / update workflow

Step 0 is run on every publish. Steps 1–2 republish the already-live
namespaces. Step 3 is the moderation release, run once in the order given.
Step 4 verifies. Every operand is an explicit file.

```sh
# 0. Gates — all must pass; review the lint/breaking/diff output by hand
go run ./cmd/validate-lexicon                              # schemas + fixtures (also runs at T0)
go run ./cmd/validate-live -pds https://pds.coves.me       # live-record compatibility: no live record invalidated
goat lex lint internal/atproto/lexicon/social/coves        # style (large-string warns are intentional)
goat lex breaking internal/atproto/lexicon/social/coves    # evolution rules vs published state; only the accepted exceptions above may appear
goat lex diff internal/atproto/lexicon/social/coves        # read every changed schema before --update

# 1. Log in as the publishing account (session cached by goat)
goat account login -u lexicons.coves.social -p '<password>' --pds-host https://pds.coves.me
export LEXICONS_DIR=internal/atproto/lexicon

# 2. Already-published namespaces. Safety properties of `goat lex publish`:
#    - refuses NSIDs whose _lexicon DNS doesn't point at the logged-in account
#      (never pass --skip-dns-check)
#    - skips schemas identical to what's already live; --update required to
#      overwrite changed ones
#    community/comment/defs.json and community/post/defs.json are NOT here:
#    they now reference moderation.defs and move to step 3. community/post/get.json
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

# 3. Moderation release (once). Order matters: the content-view definitions
#    reference moderation.defs, and the endpoints reference both, so each
#    publish must fully resolve before the next.
#    3a. DNS delegation for the new authority, then confirm resolution:
CF_API_TOKEN=... LEXICON_DID=did:plc:... ./scripts/publish-lexicon-dns.sh --include-moderation
goat lex check-dns internal/atproto/lexicon/social/coves/moderation/defs.json
#    3b. Shared definitions first (publishes #banView as it stands):
goat lex publish \
  internal/atproto/lexicon/social/coves/moderation/defs.json
#    3c. Dependency closure: every ref in moderation.defs must resolve over
#        DNS/PDS before anything that references it is published.
goat lex resolve social.coves.moderation.defs
#    3d. Content-view definitions that reference moderation.defs (the
#        commentView.record exception above is expected in `goat lex breaking`),
#        then community.post.get, whose output union now references
#        post.defs#moderatedPost and so must follow post.defs:
goat lex publish --update \
  internal/atproto/lexicon/social/coves/community/comment/defs.json \
  internal/atproto/lexicon/social/coves/community/post/defs.json
goat lex resolve social.coves.community.comment.defs
goat lex resolve social.coves.community.post.defs
goat lex publish --update internal/atproto/lexicon/social/coves/community/post/get.json
goat lex resolve social.coves.community.post.get
#    3e. The seven reviewed endpoints, last:
goat lex publish \
  internal/atproto/lexicon/social/coves/moderation/removeContent.json \
  internal/atproto/lexicon/social/coves/moderation/restoreContent.json \
  internal/atproto/lexicon/social/coves/moderation/labelContent.json \
  internal/atproto/lexicon/social/coves/moderation/retractContentLabel.json \
  internal/atproto/lexicon/social/coves/moderation/listActions.json \
  internal/atproto/lexicon/social/coves/moderation/listAdminActions.json \
  internal/atproto/lexicon/social/coves/moderation/getSubjectState.json

# 4. Verify resolution end-to-end, and unpublish anything retired.
goat lex check-dns internal/atproto/lexicon/social/coves
goat lex status internal/atproto/lexicon/social/coves
goat lex resolve social.coves.community.post
goat lex resolve social.coves.moderation.listActions
# Retired schemas: publish only writes what it is given, so a schema whose
# file was deleted stays live until explicitly unpublished.
# social.coves.community.post.search was published by the 2026-07 directory
# sweep, never served, and deleted in favour of social.coves.feed.searchPosts:
goat lex unpublish social.coves.community.post.search
goat lex resolve social.coves.community.post.search   # must FAIL once unpublished
# The 2026-07 sweep also published the held-back community.rules and
# community.moderator; keep them unpublished:
goat lex unpublish social.coves.community.rules social.coves.community.moderator
```

Record updates use the same `goat lex publish --update` — records are keyed by
NSID and overwritten in place. Run `goat lex breaking` + `goat lex diff` first,
always. Anything `breaking` flags that is not in the accepted-exceptions list
above needs a new NSID, not an `--update`.

## Release gates the build does not run

`make ci` proves the schemas load, every fragment resolves locally, and the
fixtures behave. These gates are operator- or client-owned and must pass
before the moderation release (PRD §14.6) and before the admin workflow is
enabled:

1. **`goat lex lint` / `goat lex breaking` / `goat lex diff` review** of the
   whole `social/coves` tree, by hand, with only the accepted exceptions
   above appearing in `breaking`.
2. **DNS delegation** of `_lexicon.moderation.coves.social` and
   `goat lex check-dns` before step 3b.
3. **Dependency closure resolution** through DNS/PDS after each publish in
   step 3 (`goat lex resolve` on `moderation.defs`, then on
   `comment.defs`/`post.defs`, then on each endpoint) — a reference that does
   not resolve on the network blocks the next publish.
4. **Live-record compatibility check** (`cmd/validate-live` against an
   explicitly selected real PDS; never a public identity/PDS dependency in the
   hermetic stack).
5. **Older-decoder gate** for `community.post.get` in the client repos
   (`coves-frontend`, `coves-mobile`): supported web and mobile builds decode
   a 25-URI response containing normal posts plus an unknown union variant,
   render `deletionReason: moderator`, tolerate the optional `moderation`
   object, and handle thread `NotFound` without a crash loop. Until it passes,
   instance removals stay `#notFoundPost` for all callers of that endpoint;
   never substitute the community-only `#removedPost`.
