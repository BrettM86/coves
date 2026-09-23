"""
Bluesky public AppView client and PostView parsing.
"""
import logging
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any, Dict, List, Optional

import requests

from src.models import BlueskyPost

logger = logging.getLogger(__name__)

VERSION = "1.0.0"
USER_AGENT = f"coves-bluesky-topics/{VERSION} (+https://coves.social)"
PAGE_LIMIT = 100
MAX_PAGES = 5


class BlueskyAPIError(Exception):
    """Raised when a Bluesky AppView request fails."""

    def __init__(self, message: str, status_code: Optional[int] = None):
        super().__init__(message)
        self.status_code = status_code


@dataclass(frozen=True)
class FeedFetchResult:
    """Collected feed items; ``partial`` is True when a later page failed."""

    items: List[Dict] = field(default_factory=list)
    partial: bool = False


class BlueskyClient:
    """Unauthenticated client for the public Bluesky AppView."""

    def __init__(self, base_url: str, timeout: int = 30):
        self.base_url = base_url.rstrip("/")
        self.timeout = timeout
        self.session = requests.Session()
        self.session.headers["User-Agent"] = USER_AGENT

    def get_feed(self, feed_uri: str) -> FeedFetchResult:
        return self._fetch_pages("app.bsky.feed.getFeed", "feed", feed_uri)

    def get_list_feed(self, list_uri: str) -> FeedFetchResult:
        return self._fetch_pages("app.bsky.feed.getListFeed", "list", list_uri)

    def get_profile(self, did: str) -> str:
        """Return the actor's profile description, or "" when absent."""
        data = self._get_json("app.bsky.actor.getProfile", {"actor": did})
        description = data.get("description")
        return description if isinstance(description, str) else ""

    def _fetch_pages(self, method: str, source_param: str, source_uri: str) -> FeedFetchResult:
        """
        Follow cursors until the response has no cursor, the cursor repeats, or
        MAX_PAGES have been fetched. A failure on the first page raises; a failure
        on a later page keeps the earlier pages and marks the result partial.
        """
        items: List[Dict] = []
        cursor: Optional[str] = None
        seen_cursors = set()

        for page_number in range(1, MAX_PAGES + 1):
            params = {source_param: source_uri, "limit": PAGE_LIMIT}
            if cursor is not None:
                params["cursor"] = cursor
            try:
                page = self._get_json(method, params)
            except BlueskyAPIError as error:
                if page_number == 1:
                    raise
                logger.warning(
                    f"{method} for {source_uri} failed on page {page_number}; "
                    f"keeping {len(items)} items from earlier pages: {error}"
                )
                return FeedFetchResult(items=items, partial=True)

            page_items = page.get("feed")
            if not isinstance(page_items, list):
                if page_number == 1:
                    raise BlueskyAPIError(f"{method} response has no 'feed' list")
                logger.warning(f"{method} for {source_uri} returned no 'feed' list on page {page_number}")
                return FeedFetchResult(items=items, partial=True)
            items.extend(page_items)

            next_cursor = page.get("cursor")
            if not next_cursor or next_cursor in seen_cursors:
                break
            seen_cursors.add(next_cursor)
            cursor = next_cursor

        return FeedFetchResult(items=items, partial=False)

    def _get_json(self, method: str, params: Dict[str, Any]) -> Dict[str, Any]:
        url = f"{self.base_url}/xrpc/{method}"
        try:
            response = self.session.request("GET", url, params=params, timeout=self.timeout)
        except requests.RequestException as error:
            raise BlueskyAPIError(f"{method} request failed: {error}")

        if not 200 <= response.status_code < 300:
            raise BlueskyAPIError(
                f"{method} returned HTTP {response.status_code}", status_code=response.status_code
            )

        try:
            data = response.json()
        except ValueError as error:
            raise BlueskyAPIError(f"{method} returned malformed JSON: {error}", status_code=response.status_code)
        if not isinstance(data, dict):
            raise BlueskyAPIError(f"{method} returned a non-object JSON body", status_code=response.status_code)
        return data


def parse_created_at(value: Any) -> Optional[datetime]:
    """Parse an ISO-8601 timestamp to an aware UTC datetime; None when unparseable."""
    if not isinstance(value, str) or not value:
        return None
    try:
        parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        return None
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=timezone.utc)
    return parsed.astimezone(timezone.utc)


def _label_values(labels: Any) -> tuple:
    if not isinstance(labels, list):
        return ()
    return tuple(label["val"] for label in labels if isinstance(label, dict) and isinstance(label.get("val"), str))


def _count(post_view: Dict[str, Any], key: str) -> int:
    value = post_view.get(key, 0)
    return value if isinstance(value, int) and not isinstance(value, bool) else 0


def _embed_type(embed: Any) -> Optional[str]:
    if not isinstance(embed, dict):
        return None
    embed_type = embed.get("$type")
    if embed_type == "app.bsky.embed.recordWithMedia#view":
        media = embed.get("media")
        embed_type = media.get("$type") if isinstance(media, dict) else None
    return embed_type if isinstance(embed_type, str) else None


def parse_post_view(item: Dict) -> Optional[BlueskyPost]:
    """
    Convert a feed item to a BlueskyPost.

    Returns None (never raises) for items with a ``reason`` (reposts, pins) and
    for items missing uri, author.did, author.handle, or a parseable
    record.createdAt; those are logged at warning.
    """
    if not isinstance(item, dict) or item.get("reason") is not None:
        return None

    post_view = item.get("post")
    if not isinstance(post_view, dict):
        logger.warning("Skipping feed item without a post object")
        return None

    author = post_view.get("author")
    record = post_view.get("record")
    if not isinstance(author, dict):
        author = {}
    if not isinstance(record, dict):
        record = {}

    uri = post_view.get("uri")
    author_did = author.get("did")
    author_handle = author.get("handle")
    if not all(isinstance(value, str) and value for value in (uri, author_did, author_handle)):
        logger.warning(f"Skipping post with missing identity fields (uri={uri!r})")
        return None

    created_at = parse_created_at(record.get("createdAt"))
    if created_at is None:
        logger.warning(f"Skipping post with missing or unparseable createdAt: {uri}")
        return None

    display_name = author.get("displayName")
    text = record.get("text")
    cid = post_view.get("cid")

    return BlueskyPost(
        uri=uri,
        cid=cid if isinstance(cid, str) else "",
        author_did=author_did,
        author_handle=author_handle,
        text=text if isinstance(text, str) else "",
        created_at=created_at,
        author_display_name=display_name if isinstance(display_name, str) else "",
        like_count=_count(post_view, "likeCount"),
        repost_count=_count(post_view, "repostCount"),
        reply_count=_count(post_view, "replyCount"),
        quote_count=_count(post_view, "quoteCount"),
        is_reply=record.get("reply") is not None,
        labels=_label_values(post_view.get("labels")),
        author_labels=_label_values(author.get("labels")),
        embed_type=_embed_type(post_view.get("embed")),
    )
