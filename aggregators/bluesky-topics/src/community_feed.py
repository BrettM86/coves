"""
Reader for a Coves community feed: collects external link uris posted to a
community since a given time.
"""
import logging
from datetime import datetime
from typing import Any, Dict, Optional, Set

import requests

from src.bluesky_client import USER_AGENT, parse_created_at

logger = logging.getLogger(__name__)

FEED_METHOD = "social.coves.communityFeed.getCommunity"
PAGE_LIMIT = 50
MAX_PAGES = 5
EXTERNAL_EMBED_PREFIX = "social.coves.embed.external"
POST_EMBED_PREFIX = "social.coves.embed.post"
AT_URI_PREFIX = "at://"


class CommunityFeedError(Exception):
    """Raised when the community feed cannot be read."""

    def __init__(self, message: str, status_code: int = 0):
        super().__init__(message)
        self.status_code = status_code


class CommunityFeedReader:
    """Reads a public Coves community feed newest-first and collects external link uris."""

    def __init__(self, api_url: str, timeout: int = 30):
        self.api_url = api_url.rstrip("/")
        self.timeout = timeout
        self.session = requests.Session()
        self.session.headers["User-Agent"] = USER_AGENT

    def external_uris_since(self, community_handle: str, since: datetime) -> Set[str]:
        """
        Collect linked uris from posts created at or after ``since``.

        An external embed contributes ``embed.external.uri``; a Bluesky post
        embed — what the AppView makes of a bsky.app permalink posted as a
        link — contributes the at:// uri in ``embed.post.uri``.

        Pagination stops when a page holds a post older than ``since``, when
        there is no cursor or the cursor repeats, or after MAX_PAGES pages.

        Raises:
            CommunityFeedError: when the first page cannot be read. A failure
                on a later page returns what was collected so far.
        """
        collected: Set[str] = set()
        cursor: Optional[str] = None
        for page_number in range(1, MAX_PAGES + 1):
            params: Dict[str, Any] = {"community": community_handle, "sort": "new", "limit": PAGE_LIMIT}
            if cursor is not None:
                params["cursor"] = cursor
            try:
                page = self._get_page(params)
            except CommunityFeedError as error:
                if page_number == 1:
                    raise
                logger.warning(f"Community feed page {page_number} failed ({error}); using pages read so far")
                return collected

            reached_older_post = False
            for item in page["feed"]:
                post = item.get("post") if isinstance(item, dict) else None
                if not isinstance(post, dict) or not isinstance(post.get("uri"), str):
                    continue
                created_at = parse_created_at(post.get("createdAt"))
                if created_at is None:
                    continue
                if created_at < since:
                    reached_older_post = True
                    continue
                linked_uri = _linked_uri(post.get("embed"))
                if linked_uri is not None:
                    collected.add(linked_uri)

            next_cursor = page.get("cursor")
            if reached_older_post or not isinstance(next_cursor, str) or not next_cursor or next_cursor == cursor:
                break
            cursor = next_cursor
        return collected

    def _get_page(self, params: Dict[str, Any]) -> Dict[str, Any]:
        url = f"{self.api_url}/xrpc/{FEED_METHOD}"
        try:
            response = self.session.request("GET", url, params=params, timeout=self.timeout)
        except requests.RequestException as error:
            raise CommunityFeedError(f"{FEED_METHOD} request failed: {type(error).__name__}")
        if not 200 <= response.status_code < 300:
            raise CommunityFeedError(
                f"{FEED_METHOD} returned HTTP {response.status_code}", status_code=response.status_code
            )
        try:
            data = response.json()
        except ValueError:
            raise CommunityFeedError(f"{FEED_METHOD} returned malformed JSON", status_code=response.status_code)
        if not isinstance(data, dict):
            raise CommunityFeedError(f"{FEED_METHOD} returned no 'feed' list", status_code=response.status_code)
        feed = data.get("feed")
        if feed is None:
            # A community with no posts serialises as {"feed": null}: an empty page, not a failure.
            return {"feed": []}
        if not isinstance(feed, list):
            raise CommunityFeedError(f"{FEED_METHOD} returned no 'feed' list", status_code=response.status_code)
        return data


def _linked_uri(embed: Any) -> Optional[str]:
    if not isinstance(embed, dict):
        return None
    embed_type = str(embed.get("$type", ""))
    if embed_type.startswith(EXTERNAL_EMBED_PREFIX):
        external = embed.get("external")
        if not isinstance(external, dict):
            return None
        uri = external.get("uri")
        return uri if isinstance(uri, str) and uri else None
    if embed_type.startswith(POST_EMBED_PREFIX):
        quoted = embed.get("post")
        if not isinstance(quoted, dict):
            return None
        uri = quoted.get("uri")
        return uri if isinstance(uri, str) and uri.startswith(AT_URI_PREFIX) else None
    return None
