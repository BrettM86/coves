# Publishing the social.coves.* Lexicons

The lexicons under `internal/atproto/lexicon/social/coves/` are published to the
AT Protocol network as `com.atproto.lexicon.schema` records, resolvable via
`_lexicon` DNS TXT records on `coves.social`. Once published, atproto schema
evolution rules apply: **additive changes only** (new optional fields, new
knownValues). Anything breaking-shaped needs a new NSID.

## Design decisions (do not revisit casually)

- **Nested NSIDs are deliberate.** Operations live under their record noun
  (`community.post.create` under the `community.post` record) rather than
  Bluesky-style flat verbs (`createPost`). This is an API-organization choice;
  the extra DNS delegations it requires are handled by the script below.
- **Embed `alt` text is optional, permanently** (decided 2026-07-23) — matches
  live bridge content and clients.
- **`updateProfile` input takes `bio`; the record stores `description`.** Wire
  truth on both sides.

## What is published vs held back

**Published:** `richtext.*`, `embed.*`, `actor.*`, `feed.*`, `community.*`
(including `post.*` and `comment.*` — but NOT `rules` or `moderator`),
`aggregator.*`.

**Held back until tribunal/governance design lands:** the entire
`moderation.*` namespace, `community.rules`, `community.moderator`. Publish
them later with `--include-moderation` on the DNS script plus a `goat lex
publish` of those files.

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
   | `_lexicon.moderation.coves.social` | *(deferred with moderation)* |

   Via the Cloudflare API (token needs Zone:DNS:Edit on `coves.social`):
   ```sh
   CF_API_TOKEN=... LEXICON_DID=did:plc:... ./scripts/publish-lexicon-dns.sh
   ```

## Publish / update workflow

```sh
# 0. Gates — all must pass
go run ./cmd/validate-lexicon                     # schemas + fixtures
go run ./cmd/validate-live -pds https://pds.coves.me   # no live record invalidated
goat lex lint internal/atproto/lexicon/social/coves    # style (large-string warns are intentional)
goat lex breaking internal/atproto/lexicon/social/coves # evolution rules vs published state

# 1. Log in as the publishing account (session cached by goat)
goat account login -u lexicons.coves.social -p '<password>' --pds-host https://pds.coves.me

# 2. Publish. Safety properties of `goat lex publish`:
#    - refuses NSIDs whose _lexicon DNS doesn't point at the logged-in account,
#      so the whole moderation/* namespace is auto-excluded until its DNS
#      record exists (never pass --skip-dns-check)
#    - skips schemas identical to what's already live; --update required to
#      overwrite changed ones
export LEXICONS_DIR=internal/atproto/lexicon
goat lex publish \
  internal/atproto/lexicon/social/coves/richtext \
  internal/atproto/lexicon/social/coves/embed \
  internal/atproto/lexicon/social/coves/actor \
  internal/atproto/lexicon/social/coves/feed \
  internal/atproto/lexicon/social/coves/aggregator \
  internal/atproto/lexicon/social/coves/community
# community/rules.json and community/moderator.json share the community DNS
# authority, so exclude them by unpublishing if swept in:
goat lex unpublish social.coves.community.rules social.coves.community.moderator
# Retired schemas: the sweep only publishes what is on disk, so a schema whose
# file was deleted stays live on the network until it is explicitly unpublished.
# social.coves.community.post.search was published by the 2026-07 sweep, never
# served, and deleted in favour of social.coves.feed.searchPosts (2026-09-01):
goat lex unpublish social.coves.community.post.search

# 3. Verify resolution end-to-end
goat lex check-dns internal/atproto/lexicon/social/coves
goat lex resolve social.coves.community.post
goat lex resolve social.coves.community.post.search   # must FAIL once unpublished
goat lex status internal/atproto/lexicon/social/coves
```

Record updates use the same `goat lex publish --update` — records are keyed by
NSID and overwritten in place. Run `goat lex breaking` + `goat lex diff` first,
always.
