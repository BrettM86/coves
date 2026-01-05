"""
Link extractor for detecting streamable.com URLs from Reddit RSS entries.
"""
import re
import logging
from typing import List, Optional
from urllib.parse import urlparse

logger = logging.getLogger(__name__)


class LinkExtractor:
    """
    Extracts video links from Reddit RSS entries.

    Focuses on streamable.com but can be extended to other video hosts.
    """

    # Supported video hosting domains
    DEFAULT_ALLOWED_DOMAINS = ["streamable.com"]

    # Pattern to find URLs in text/HTML content.
    # Matches http:// or https:// followed by any non-whitespace characters
    # that aren't typically URL delimiters in HTML/text (< > " ' ) ]).
    # This is intentionally permissive to catch URLs in various contexts,
    # with further validation done by is_allowed_url().
    URL_PATTERN = re.compile(
        r'https?://[^\s<>"\')\]]+',
        re.IGNORECASE,
    )

    def __init__(self, allowed_domains: Optional[List[str]] = None):
        """
        Initialize link extractor.

        Args:
            allowed_domains: List of allowed video hosting domains.
                            Defaults to ["streamable.com"]
        """
        self.allowed_domains = allowed_domains or self.DEFAULT_ALLOWED_DOMAINS
        # Normalize domains to lowercase
        self.allowed_domains = [d.lower() for d in self.allowed_domains]

    def extract_video_url(self, entry) -> Optional[str]:
        """
        Extract video URL from a Reddit RSS entry.

        Checks:
        1. Direct link (entry.link is a video URL)
        2. Entry content/description for embedded video URLs

        Args:
            entry: feedparser entry object from Reddit RSS

        Returns:
            Video URL if found, None otherwise
        """
        # Check 1: Direct link
        if hasattr(entry, "link") and entry.link:
            if self.is_allowed_url(entry.link):
                logger.debug(f"Found video URL in direct link: {entry.link}")
                return self._normalize_url(entry.link)

        # Check 2: Entry content (Reddit RSS uses 'content' field)
        if hasattr(entry, "content") and entry.content:
            for content_item in entry.content:
                if hasattr(content_item, "value"):
                    url = self._find_url_in_text(content_item.value)
                    if url:
                        logger.debug(f"Found video URL in content: {url}")
                        return url

        # Check 3: Entry description/summary
        if hasattr(entry, "description") and entry.description:
            url = self._find_url_in_text(entry.description)
            if url:
                logger.debug(f"Found video URL in description: {url}")
                return url

        if hasattr(entry, "summary") and entry.summary:
            url = self._find_url_in_text(entry.summary)
            if url:
                logger.debug(f"Found video URL in summary: {url}")
                return url

        return None

    def _find_url_in_text(self, text: str) -> Optional[str]:
        """
        Find first allowed video URL in text.

        Args:
            text: Text/HTML content to search

        Returns:
            First video URL found, or None
        """
        if not text:
            return None

        urls = self.URL_PATTERN.findall(text)
        for url in urls:
            # Clean up common trailing characters
            url = url.rstrip(".,;:!?")
            if self.is_allowed_url(url):
                return self._normalize_url(url)

        return None

    def is_allowed_url(self, url: str) -> bool:
        """
        Check if URL is from an allowed video hosting domain.

        Args:
            url: URL to check

        Returns:
            True if URL is from allowed domain
        """
        if not url:
            return False

        try:
            parsed = urlparse(url)
            domain = parsed.netloc.lower()

            # Remove www. prefix for comparison
            if domain.startswith("www."):
                domain = domain[4:]

            return domain in self.allowed_domains

        except ValueError as e:
            logger.debug(f"Failed to parse URL '{url}': {e}")
            return False

    def _normalize_url(self, url: str) -> str:
        """
        Normalize URL for consistent storage/comparison.

        Args:
            url: URL to normalize

        Returns:
            Normalized URL
        """
        # Remove trailing slashes
        url = url.rstrip("/")

        # Ensure https
        if url.startswith("http://"):
            url = "https://" + url[7:]

        return url

    def get_video_id(self, url: str) -> Optional[str]:
        """
        Extract video ID from streamable URL for deduplication.

        Args:
            url: Streamable URL

        Returns:
            Video ID or None
        """
        if not url:
            return None

        try:
            parsed = urlparse(url)
            # Streamable URLs are like: https://streamable.com/abc123
            path = parsed.path.strip("/")
            if path:
                # Return first path segment as video ID
                return path.split("/")[0]
        except ValueError as e:
            logger.debug(f"Failed to extract video ID from URL '{url}': {e}")

        return None

    def get_thumbnail_url(self, url: str, timeout: int = 10) -> Optional[str]:
        """
        Fetch thumbnail URL from Streamable's oembed API.

        Args:
            url: Streamable video URL
            timeout: Request timeout in seconds

        Returns:
            Thumbnail URL or None if fetch fails
        """
        if not url or not self.is_allowed_url(url):
            return None

        import requests

        oembed_url = f"https://api.streamable.com/oembed?url={url}"

        try:
            response = requests.get(oembed_url, timeout=timeout)
            response.raise_for_status()
            data = response.json()
            thumbnail = data.get("thumbnail_url")
            if thumbnail:
                logger.debug(f"Fetched thumbnail for {url}: {thumbnail[:50]}...")
            return thumbnail
        except requests.RequestException as e:
            logger.warning(f"Failed to fetch oembed for {url}: {e}")
            return None
        except (ValueError, KeyError) as e:
            logger.warning(f"Failed to parse oembed response for {url}: {e}")
            return None
