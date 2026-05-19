"""
RSS feed fetcher with retry logic and error handling.
"""
import time
import logging
import requests
import feedparser
from typing import Dict, Optional

logger = logging.getLogger(__name__)


class RSSFetcher:
    """Fetches RSS feeds with retry logic."""

    def __init__(self, timeout: int = 30, max_retries: int = 3):
        """
        Initialize RSS fetcher.

        Args:
            timeout: Request timeout in seconds
            max_retries: Maximum number of retry attempts (must be >= 1)
        """
        if max_retries < 1:
            raise ValueError(f"max_retries must be >= 1, got {max_retries}")
        self.timeout = timeout
        self.max_retries = max_retries

    def fetch_feed(self, url: str) -> feedparser.FeedParserDict:
        """
        Fetch and parse an RSS feed.

        Args:
            url: RSS feed URL

        Returns:
            Parsed feed object

        Raises:
            ValueError: If URL is empty
            requests.RequestException: If all retry attempts fail
        """
        if not url:
            raise ValueError("URL cannot be empty")

        last_error = None

        for attempt in range(self.max_retries):
            try:
                logger.info(f"Fetching feed from {url} (attempt {attempt + 1}/{self.max_retries})")

                response = requests.get(url, timeout=self.timeout)
                response.raise_for_status()

                # Parse with feedparser
                feed = feedparser.parse(response.content)

                logger.info(f"Successfully fetched feed: {feed.feed.get('title', 'Unknown')}")
                return feed

            except requests.RequestException as e:
                last_error = e
                logger.warning(f"Fetch attempt {attempt + 1} failed: {e}")

                if attempt < self.max_retries - 1:
                    # Exponential backoff
                    sleep_time = 2 ** attempt
                    logger.info(f"Retrying in {sleep_time} seconds...")
                    time.sleep(sleep_time)

        # All retries exhausted
        logger.error(f"Failed to fetch feed after {self.max_retries} attempts")
        raise last_error


class JSONFetcher:
    """Fetches Kagi News JSON feeds with retry logic.

    Returns clusters keyed by their `cluster_number` for O(1) lookup.
    """

    def __init__(self, timeout: int = 30, max_retries: int = 3):
        """
        Initialize JSON fetcher.

        Args:
            timeout: Request timeout in seconds
            max_retries: Maximum number of retry attempts (must be >= 1)
        """
        if max_retries < 1:
            raise ValueError(f"max_retries must be >= 1, got {max_retries}")
        self.timeout = timeout
        self.max_retries = max_retries

    def fetch_clusters(self, url: str) -> Dict[int, dict]:
        """
        Fetch a Kagi News JSON feed and return its clusters keyed by cluster_number.

        Args:
            url: JSON feed URL (e.g., https://news.kagi.com/tech.json)

        Returns:
            Dict mapping cluster_number (int) -> cluster object (dict)

        Raises:
            ValueError: If URL is empty or response payload is malformed
            requests.RequestException: If all retry attempts fail
        """
        if not url:
            raise ValueError("URL cannot be empty")

        response = None

        # Retry loop: only transport-level errors (RequestException) are retried.
        # Payload-shape errors won't change between attempts, so they propagate immediately.
        for attempt in range(self.max_retries):
            try:
                logger.info(f"Fetching JSON feed from {url} (attempt {attempt + 1}/{self.max_retries})")

                response = requests.get(url, timeout=self.timeout)
                response.raise_for_status()
                break  # Successful HTTP response; exit retry loop.

            except requests.RequestException as e:
                logger.warning(f"JSON fetch attempt {attempt + 1} failed: {e}")

                if attempt < self.max_retries - 1:
                    sleep_time = 2 ** attempt
                    logger.info(f"Retrying in {sleep_time} seconds...")
                    time.sleep(sleep_time)
                    continue

                logger.error(f"Failed to fetch JSON feed after {self.max_retries} attempts")
                raise

        # Decode + validate the payload. These failures are NOT retried.
        try:
            data = response.json()
        except ValueError as e:
            raise ValueError(f"Malformed JSON feed at {url}: failed to decode: {e}") from e

        clusters = data.get("clusters")
        if not isinstance(clusters, list):
            raise ValueError(
                f"Malformed JSON feed at {url}: 'clusters' field missing or not a list"
            )

        cluster_map = {}
        for cluster in clusters:
            cn = cluster.get("cluster_number")
            if cn is None:
                logger.warning(f"Skipping cluster with no cluster_number in {url}")
                continue
            try:
                cluster_map[int(cn)] = cluster
            except (ValueError, TypeError) as e:
                logger.warning(
                    f"Skipping cluster with non-numeric cluster_number {cn!r} in {url}: {e}"
                )
                continue

        if not cluster_map:
            logger.warning(f"Successfully fetched JSON feed from {url}: 0 clusters")
        else:
            logger.info(f"Successfully fetched JSON feed: {len(cluster_map)} clusters")
        return cluster_map
