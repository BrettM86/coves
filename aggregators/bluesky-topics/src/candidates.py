"""
Candidate filtering, dedupe, and ranking for the Bluesky Topics aggregator.

All functions are pure: no I/O, no logging.
"""
from datetime import datetime, timedelta
from typing import Dict, Iterable, List

from src.models import BlueskyPost, TopicConfig

BLOCKED_POST_LABELS = frozenset({"porn", "sexual", "nudity", "graphic-media", "!hide", "!warn"})
OPTED_OUT_AUTHOR_LABEL = "!no-unauthenticated"
FUTURE_TOLERANCE = timedelta(minutes=5)


def filter_candidates(
    posts: Iterable[BlueskyPost],
    *,
    topic: TopicConfig,
    now: datetime,
    window_hours: int,
) -> List[BlueskyPost]:
    """
    Apply the eligibility rules and dedupe by URI (keeping the higher like count).

    Drops replies, posts outside [now - window_hours, now + 5 minutes], posts
    carrying a blocked label, authors opted out of unauthenticated view,
    excluded handles, and posts below the topic's min_likes. Already-posted
    posts stay candidates so their story can block others at selection time.
    """
    oldest_allowed = now - timedelta(hours=window_hours)
    newest_allowed = now + FUTURE_TOLERANCE
    excluded_handles = set(topic.excluded_handles)

    kept: Dict[str, BlueskyPost] = {}
    for post in posts:
        if post.is_reply:
            continue
        if post.created_at < oldest_allowed or post.created_at > newest_allowed:
            continue
        if BLOCKED_POST_LABELS.intersection(post.labels):
            continue
        if OPTED_OUT_AUTHOR_LABEL in post.author_labels:
            continue
        if post.author_handle in excluded_handles:
            continue
        if post.like_count < topic.min_likes:
            continue

        existing = kept.get(post.uri)
        if existing is None or post.like_count > existing.like_count:
            kept[post.uri] = post

    return list(kept.values())


def score(post: BlueskyPost) -> int:
    """Ranking score: likes plus reposts plus quotes (quotes are a form of repost)."""
    return post.like_count + post.repost_count + post.quote_count


def rank_candidates(posts: Iterable[BlueskyPost], limit: int) -> List[BlueskyPost]:
    """Sort by score descending, newer first on ties, and truncate to ``limit``."""
    ranked = sorted(posts, key=lambda post: (score(post), post.created_at), reverse=True)
    return ranked[:limit]
