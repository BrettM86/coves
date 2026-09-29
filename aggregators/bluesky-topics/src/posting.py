"""
Selection of on-topic posts and Coves post building (permalink, title, embed).

All functions are pure apart from ``build_embed`` delegating to the Coves client.
"""
from typing import Any, Collection, Dict, Iterable, List, Optional, Sequence, Tuple
from urllib.parse import urlparse

from src.candidates import score
from src.classifier import Verdict
from src.models import BlueskyPost

BLUESKY_WEB_URL = "https://bsky.app"
BLUESKY_DOMAIN = "bsky.app"
PROVIDER = "bluesky"
INVALID_HANDLE = "handle.invalid"
TITLE_MAX_CODE_POINTS = 280
DESCRIPTION_MAX_CODE_POINTS = 300
ELLIPSIS = "…"


def select_posts(
    posts: Sequence[BlueskyPost],
    verdicts: Sequence[Verdict],
    *,
    min_confidence: float,
    limit: int,
    already_posted: Collection[str],
) -> List[BlueskyPost]:
    """
    Pick one representative post per story, at most ``limit`` stories.

    Every post with a verdict (matched by id) is grouped by story key. A story
    is blocked when any member's uri is in ``already_posted``, whatever that
    member's verdict. A blocked story is ranked by its highest-scored member
    regardless of verdicts, so a story already posted keeps its slot even when
    no member is eligible this run. An unblocked story is ranked by its
    representative: its highest-scored member that is on-topic with confidence
    >= ``min_confidence`` (ties prefer the newer post); unblocked stories
    without one are dropped and occupy no slot. Stories are ordered by that
    post's score descending (ties newer first), the top ``limit`` are taken,
    blocked ones are removed without backfill, and the representatives of the
    rest are returned in that order.
    """
    verdict_by_id = {verdict.id: verdict for verdict in verdicts}
    members_by_story: Dict[str, List[BlueskyPost]] = {}
    eligible_by_story: Dict[str, List[BlueskyPost]] = {}
    for index, post in enumerate(posts):
        verdict = verdict_by_id.get(index)
        if verdict is None:
            continue
        members_by_story.setdefault(verdict.story, []).append(post)
        if verdict.on_topic and verdict.confidence >= min_confidence:
            eligible_by_story.setdefault(verdict.story, []).append(post)

    def rank_key(post: BlueskyPost):
        return (score(post), post.created_at)

    blocked_stories = {
        story
        for story, members in members_by_story.items()
        if any(member.uri in already_posted for member in members)
    }
    ranking_posts = {
        story: max(members if story in blocked_stories else eligible_by_story.get(story, []), key=rank_key)
        for story, members in members_by_story.items()
        if story in blocked_stories or story in eligible_by_story
    }
    ordered_stories = sorted(ranking_posts, key=lambda story: rank_key(ranking_posts[story]), reverse=True)
    return [ranking_posts[story] for story in ordered_stories[:limit] if story not in blocked_stories]


def permalink_key(url: str) -> Optional[Tuple[str, str]]:
    """
    Parse a bsky.app post permalink into (profile segment, rkey).

    Accepts http/https, host bsky.app (case-insensitive), path exactly
    /profile/<segment>/post/<rkey>; trailing slash, query, and fragment are
    ignored. Anything else returns None.
    """
    parsed = urlparse(url)
    if parsed.scheme not in ("http", "https") or parsed.hostname != BLUESKY_DOMAIN:
        return None
    segments = parsed.path.rstrip("/").split("/")
    if len(segments) != 5 or segments[1] != "profile" or segments[3] != "post":
        return None
    _, _, profile_segment, _, rkey = segments
    if not profile_segment or not rkey:
        return None
    return profile_segment, rkey


def already_posted_uris(posts: Iterable[BlueskyPost], community_external_uris: Iterable[str]) -> set:
    """
    Uris of candidates already linked from the community: either their at://
    uri appears verbatim (the AppView turns a bsky.app link post into a
    Bluesky post embed) or their bsky.app permalink appears (by did or by a
    valid handle).
    """
    linked_uris = set(community_external_uris)
    keys = {key for key in map(permalink_key, linked_uris) if key is not None}
    matched = set()
    for post in posts:
        rkey = post.uri.rsplit("/", 1)[-1]
        by_did = (post.author_did, rkey) in keys
        by_handle = post.author_handle != INVALID_HANDLE and (post.author_handle, rkey) in keys
        if post.uri in linked_uris or by_did or by_handle:
            matched.add(post.uri)
    return matched


def build_permalink(post: BlueskyPost) -> str:
    """bsky.app permalink; uses the DID when the handle could not be resolved."""
    actor = post.author_did if post.author_handle == INVALID_HANDLE else post.author_handle
    rkey = post.uri.rsplit("/", 1)[-1]
    return f"{BLUESKY_WEB_URL}/profile/{actor}/post/{rkey}"


def _author_name(post: BlueskyPost) -> str:
    return post.author_display_name or post.author_handle


def _collapsed_text(post: BlueskyPost) -> str:
    return " ".join(post.text.split())


def build_title(post: BlueskyPost) -> str:
    """"{author}: {text}" cut to 280 code points including a trailing ellipsis; "{author} on Bluesky" when text-less."""
    text = _collapsed_text(post)
    if not text:
        return f"{_author_name(post)} on Bluesky"
    title = f"{_author_name(post)}: {text}"
    if len(title) > TITLE_MAX_CODE_POINTS:
        title = title[: TITLE_MAX_CODE_POINTS - 1] + ELLIPSIS
    return title


def _embed_type(post: BlueskyPost) -> str:
    resolved = post.embed_type or ""
    if "video" in resolved:
        return "video"
    if "images" in resolved:
        return "image"
    return "website"


def build_embed(post: BlueskyPost, coves_client: Any) -> Dict:
    """Build the Coves external embed for a Bluesky post via the client."""
    permalink = build_permalink(post)
    return coves_client.create_external_embed(
        uri=permalink,
        title=f"{_author_name(post)} (@{post.author_handle}) on Bluesky",
        description=_collapsed_text(post)[:DESCRIPTION_MAX_CODE_POINTS],
        sources=[{"uri": permalink, "title": "Bluesky", "domain": BLUESKY_DOMAIN}],
        embed_type=_embed_type(post),
        provider=PROVIDER,
        domain=BLUESKY_DOMAIN,
    )
