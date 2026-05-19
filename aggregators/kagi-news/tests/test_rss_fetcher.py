"""
Tests for RSS and JSON feed fetching functionality.
"""
import json
import pytest
import responses
from pathlib import Path

from src.rss_fetcher import RSSFetcher, JSONFetcher


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

    def test_rss_fetcher_max_retries_zero_raises(self):
        """max_retries=0 raises ValueError on construction (no None re-raise bug)."""
        with pytest.raises(ValueError, match="max_retries"):
            RSSFetcher(max_retries=0)


@pytest.fixture
def sample_json_payload():
    """A minimal Kagi News JSON payload with two clusters."""
    return {
        "category": "Technology",
        "timestamp": 1778574146,
        "clusters": [
            {"cluster_number": 1, "title": "Story One", "short_summary": "First story."},
            {"cluster_number": 2, "title": "Story Two", "short_summary": "Second story."},
        ],
    }


class TestJSONFetcher:
    """Test suite for JSONFetcher."""

    @responses.activate
    def test_fetch_clusters_success(self, sample_json_payload):
        """Returns a dict keyed by cluster_number."""
        url = "https://news.kagi.com/tech.json"
        responses.add(responses.GET, url, json=sample_json_payload, status=200)

        fetcher = JSONFetcher()
        clusters = fetcher.fetch_clusters(url)

        assert isinstance(clusters, dict)
        assert set(clusters.keys()) == {1, 2}
        assert clusters[1]["title"] == "Story One"
        assert clusters[2]["title"] == "Story Two"

    @responses.activate
    def test_fetch_clusters_with_retry(self, sample_json_payload):
        """Retries once then succeeds."""
        url = "https://news.kagi.com/tech.json"
        responses.add(responses.GET, url, body="boom", status=500)
        responses.add(responses.GET, url, json=sample_json_payload, status=200)

        fetcher = JSONFetcher(max_retries=2)
        clusters = fetcher.fetch_clusters(url)

        assert len(clusters) == 2

    @responses.activate
    def test_fetch_clusters_timeout(self):
        """Server error after all retries raises."""
        url = "https://news.kagi.com/tech.json"
        responses.add(responses.GET, url, body="timeout", status=408)

        fetcher = JSONFetcher(max_retries=1)

        with pytest.raises(Exception):
            fetcher.fetch_clusters(url)

    @responses.activate
    def test_fetch_clusters_invalid_json(self):
        """Non-JSON body raises after retries."""
        url = "https://news.kagi.com/tech.json"
        responses.add(responses.GET, url, body="not json at all", status=200)

        fetcher = JSONFetcher(max_retries=1)

        with pytest.raises(Exception):
            fetcher.fetch_clusters(url)

    @responses.activate
    def test_fetch_clusters_missing_clusters_field(self):
        """JSON without 'clusters' list raises ValueError."""
        url = "https://news.kagi.com/tech.json"
        responses.add(responses.GET, url, json={"category": "tech"}, status=200)

        fetcher = JSONFetcher(max_retries=1)

        with pytest.raises(ValueError, match="clusters"):
            fetcher.fetch_clusters(url)

    @responses.activate
    def test_fetch_clusters_skips_entries_without_cluster_number(self, caplog):
        """Cluster objects missing cluster_number are skipped, others kept."""
        url = "https://news.kagi.com/tech.json"
        responses.add(
            responses.GET,
            url,
            json={"clusters": [{"title": "no number"}, {"cluster_number": 5, "title": "ok"}]},
            status=200,
        )

        fetcher = JSONFetcher()
        clusters = fetcher.fetch_clusters(url)

        assert set(clusters.keys()) == {5}

    def test_fetch_clusters_requires_url(self):
        """Empty URL raises ValueError."""
        fetcher = JSONFetcher()

        with pytest.raises(ValueError):
            fetcher.fetch_clusters("")

    def test_json_fetcher_max_retries_zero_raises(self):
        """max_retries=0 raises ValueError on construction (sensible floor)."""
        with pytest.raises(ValueError, match="max_retries"):
            JSONFetcher(max_retries=0)

    @responses.activate
    def test_fetch_clusters_clusters_field_is_dict(self):
        """'clusters' field as a dict raises ValueError and does NOT retry."""
        url = "https://news.kagi.com/tech.json"
        responses.add(responses.GET, url, json={"clusters": {"foo": "bar"}}, status=200)

        fetcher = JSONFetcher(max_retries=3)

        with pytest.raises(ValueError, match="not a list"):
            fetcher.fetch_clusters(url)

        # Exactly one HTTP call: payload-shape errors are not retryable.
        assert responses.assert_call_count(url, 1)

    @responses.activate
    def test_fetch_clusters_clusters_field_is_string(self):
        """'clusters' field as a string raises ValueError and does NOT retry."""
        url = "https://news.kagi.com/tech.json"
        responses.add(responses.GET, url, json={"clusters": "oops"}, status=200)

        fetcher = JSONFetcher(max_retries=3)

        with pytest.raises(ValueError, match="not a list"):
            fetcher.fetch_clusters(url)

        assert responses.assert_call_count(url, 1)

    @responses.activate
    def test_fetch_clusters_cluster_number_string_coerced(self):
        """cluster_number as a numeric string is coerced to int key (pinned behavior)."""
        url = "https://news.kagi.com/tech.json"
        responses.add(
            responses.GET,
            url,
            json={"clusters": [{"cluster_number": "5", "title": "stringy"}]},
            status=200,
        )

        fetcher = JSONFetcher()
        clusters = fetcher.fetch_clusters(url)

        assert set(clusters.keys()) == {5}
        assert clusters[5]["title"] == "stringy"

    @responses.activate
    def test_fetch_clusters_skips_non_coercible_cluster_number(self, caplog):
        """A non-coercible cluster_number is skipped; other clusters still indexed."""
        import logging

        url = "https://news.kagi.com/tech.json"
        responses.add(
            responses.GET,
            url,
            json={
                "clusters": [
                    {"cluster_number": [], "title": "bad-list"},
                    {"cluster_number": {}, "title": "bad-dict"},
                    {"cluster_number": 7, "title": "good"},
                ]
            },
            status=200,
        )

        fetcher = JSONFetcher()
        with caplog.at_level(logging.WARNING):
            clusters = fetcher.fetch_clusters(url)

        assert set(clusters.keys()) == {7}
        # Each malformed cluster_number produced a warning.
        skip_warnings = [r for r in caplog.records if "non-numeric cluster_number" in r.message]
        assert len(skip_warnings) == 2

    @responses.activate
    def test_fetch_clusters_payload_shape_does_not_retry(self):
        """Payload-shape ValueError is raised on the first attempt; no retry."""
        url = "https://news.kagi.com/tech.json"
        responses.add(responses.GET, url, json={"category": "tech"}, status=200)

        fetcher = JSONFetcher(max_retries=3)

        with pytest.raises(ValueError, match="clusters"):
            fetcher.fetch_clusters(url)

        assert responses.assert_call_count(url, 1)

    @responses.activate
    def test_fetch_clusters_decode_failure_does_not_retry(self):
        """Invalid JSON body raises immediately; no retry."""
        url = "https://news.kagi.com/tech.json"
        responses.add(responses.GET, url, body="not json at all", status=200)

        fetcher = JSONFetcher(max_retries=3)

        with pytest.raises(ValueError, match="failed to decode"):
            fetcher.fetch_clusters(url)

        assert responses.assert_call_count(url, 1)

    @responses.activate
    def test_fetch_clusters_empty_result_logs_warning(self, caplog):
        """Empty cluster_map after parsing logs at WARNING, not INFO."""
        import logging

        url = "https://news.kagi.com/tech.json"
        responses.add(responses.GET, url, json={"clusters": []}, status=200)

        fetcher = JSONFetcher()
        with caplog.at_level(logging.WARNING):
            clusters = fetcher.fetch_clusters(url)

        assert clusters == {}
        empty_warnings = [
            r for r in caplog.records
            if r.levelno == logging.WARNING and "0 clusters" in r.message and url in r.message
        ]
        assert len(empty_warnings) == 1
