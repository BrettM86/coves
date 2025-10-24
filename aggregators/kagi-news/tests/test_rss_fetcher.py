"""
Tests for RSS feed fetching functionality.
"""
import pytest
import responses
from pathlib import Path

from src.rss_fetcher import RSSFetcher


@pytest.fixture
def sample_rss_feed():
    """Load sample RSS feed from fixtures."""
    fixture_path = Path(__file__).parent / "fixtures" / "world.xml"
    # For now, use a minimal test feed
    return """<?xml version='1.0' encoding='UTF-8'?>
<rss version="2.0">
  <channel>
    <title>Kagi News - World</title>
    <item>
      <title>Test Story</title>
      <link>https://kite.kagi.com/test/world/1</link>
      <guid>https://kite.kagi.com/test/world/1</guid>
      <pubDate>Fri, 24 Oct 2025 12:00:00 +0000</pubDate>
      <category>World</category>
    </item>
  </channel>
</rss>"""


class TestRSSFetcher:
    """Test suite for RSSFetcher."""

    @responses.activate
    def test_fetch_feed_success(self, sample_rss_feed):
        """Test successful RSS feed fetch."""
        url = "https://news.kagi.com/world.xml"
        responses.add(responses.GET, url, body=sample_rss_feed, status=200)

        fetcher = RSSFetcher()
        feed = fetcher.fetch_feed(url)

        assert feed is not None
        assert feed.feed.title == "Kagi News - World"
        assert len(feed.entries) == 1
        assert feed.entries[0].title == "Test Story"

    @responses.activate
    def test_fetch_feed_timeout(self):
        """Test fetch with timeout."""
        url = "https://news.kagi.com/world.xml"
        responses.add(responses.GET, url, body="timeout", status=408)

        fetcher = RSSFetcher(timeout=5)

        with pytest.raises(Exception):  # Should raise on timeout
            fetcher.fetch_feed(url)

    @responses.activate
    def test_fetch_feed_with_retry(self, sample_rss_feed):
        """Test fetch with retry on failure then success."""
        url = "https://news.kagi.com/world.xml"

        # First call fails, second succeeds
        responses.add(responses.GET, url, body="error", status=500)
        responses.add(responses.GET, url, body=sample_rss_feed, status=200)

        fetcher = RSSFetcher(max_retries=2)
        feed = fetcher.fetch_feed(url)

        assert feed is not None
        assert len(feed.entries) == 1

    @responses.activate
    def test_fetch_feed_invalid_xml(self):
        """Test handling of invalid XML."""
        url = "https://news.kagi.com/world.xml"
        responses.add(responses.GET, url, body="Not valid XML!", status=200)

        fetcher = RSSFetcher()
        feed = fetcher.fetch_feed(url)

        # feedparser is lenient, but should have bozo flag set
        assert feed.bozo == 1  # feedparser uses 1 for True

    def test_fetch_feed_requires_url(self):
        """Test that fetch_feed requires a URL."""
        fetcher = RSSFetcher()

        with pytest.raises((ValueError, TypeError)):
            fetcher.fetch_feed("")
