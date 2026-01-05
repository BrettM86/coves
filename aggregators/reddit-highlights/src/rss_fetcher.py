"""
RSS feed fetcher with retry logic and error handling.
"""
import time
import logging
import requests
import feedparser
from typing import Optional

logger = logging.getLogger(__name__)


class RSSFetcher:
    """
    Fetches and parses RSS feeds with retry logic and error handling.

    Features:
    - Configurable timeout and retry count
    - Exponential backoff on failures
    - Custom User-Agent header (required by Reddit)
    - Automatic HTTP to HTTPS upgrade handling
    """

    DEFAULT_USER_AGENT = "Coves-Reddit-Aggregator/1.0 (https://coves.social; contact@coves.social)"

    def __init__(self, timeout: int = 30, max_retries: int = 3, user_agent: Optional[str] = None):
        """
        Initialize RSS fetcher.

        Args:
            timeout: Request timeout in seconds
            max_retries: Maximum number of retry attempts
            user_agent: Custom User-Agent string (Reddit requires this)
        """
        self.timeout = timeout
        self.max_retries = max_retries
        self.user_agent = user_agent or self.DEFAULT_USER_AGENT

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

                headers = {"User-Agent": self.user_agent}
                response = requests.get(url, timeout=self.timeout, headers=headers)
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
