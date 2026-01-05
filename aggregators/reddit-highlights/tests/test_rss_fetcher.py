"""
Tests for rss_fetcher module.
"""
import pytest
from unittest.mock import MagicMock, patch
import requests

from src.rss_fetcher import RSSFetcher


class TestRSSFetcherInit:
    """Tests for RSSFetcher initialization."""

    def test_default_timeout(self):
        """Test default timeout value."""
        fetcher = RSSFetcher()
        assert fetcher.timeout == 30

    def test_default_max_retries(self):
        """Test default max_retries value."""
        fetcher = RSSFetcher()
        assert fetcher.max_retries == 3

    def test_default_user_agent(self):
        """Test default user agent is set."""
        fetcher = RSSFetcher()
        assert "Coves-Reddit-Aggregator" in fetcher.user_agent
        assert "coves.social" in fetcher.user_agent

    def test_custom_timeout(self):
        """Test custom timeout value."""
        fetcher = RSSFetcher(timeout=60)
        assert fetcher.timeout == 60

    def test_custom_max_retries(self):
        """Test custom max_retries value."""
        fetcher = RSSFetcher(max_retries=5)
        assert fetcher.max_retries == 5

    def test_custom_user_agent(self):
        """Test custom user agent."""
        fetcher = RSSFetcher(user_agent="CustomBot/1.0")
        assert fetcher.user_agent == "CustomBot/1.0"


class TestRSSFetcherFetchFeed:
    """Tests for fetch_feed method."""

    @pytest.fixture
    def fetcher(self):
        return RSSFetcher(max_retries=2)

    def test_raises_on_empty_url(self, fetcher):
        """Test that ValueError is raised for empty URL."""
        with pytest.raises(ValueError, match="URL cannot be empty"):
            fetcher.fetch_feed("")

    def test_successful_fetch(self, fetcher):
        """Test successful feed fetch."""
        mock_response = MagicMock()
        mock_response.status_code = 200
        mock_response.content = b"""<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
    <channel>
        <title>Test Feed</title>
        <item>
            <title>Test Post</title>
            <link>https://reddit.com/r/test/post1</link>
        </item>
    </channel>
</rss>"""
        mock_response.raise_for_status = MagicMock()

        with patch("requests.get", return_value=mock_response):
            feed = fetcher.fetch_feed("https://reddit.com/r/test/.rss")

            assert feed.feed.get("title") == "Test Feed"
            assert len(feed.entries) == 1
            assert feed.entries[0].title == "Test Post"

    def test_sends_user_agent_header(self, fetcher):
        """Test that User-Agent header is sent."""
        mock_response = MagicMock()
        mock_response.status_code = 200
        mock_response.content = b"<rss><channel></channel></rss>"
        mock_response.raise_for_status = MagicMock()

        with patch("requests.get", return_value=mock_response) as mock_get:
            fetcher.fetch_feed("https://reddit.com/r/test/.rss")

            call_kwargs = mock_get.call_args[1]
            assert "User-Agent" in call_kwargs["headers"]
            assert call_kwargs["headers"]["User-Agent"] == fetcher.user_agent

    def test_retries_on_failure(self, fetcher):
        """Test that fetch is retried on failure."""
        mock_response = MagicMock()
        mock_response.status_code = 200
        mock_response.content = b"<rss><channel></channel></rss>"
        mock_response.raise_for_status = MagicMock()

        # First call fails, second succeeds
        with patch("requests.get", side_effect=[
            requests.ConnectionError("Connection failed"),
            mock_response
        ]) as mock_get:
            with patch("time.sleep"):  # Don't actually sleep in tests
                feed = fetcher.fetch_feed("https://reddit.com/r/test/.rss")

                assert mock_get.call_count == 2
                assert feed is not None

    def test_raises_after_max_retries(self, fetcher):
        """Test that exception is raised after max retries exhausted."""
        error = requests.ConnectionError("Connection failed")

        with patch("requests.get", side_effect=error):
            with patch("time.sleep"):
                with pytest.raises(requests.ConnectionError):
                    fetcher.fetch_feed("https://reddit.com/r/test/.rss")

    def test_exponential_backoff(self, fetcher):
        """Test that exponential backoff is used between retries."""
        mock_response = MagicMock()
        mock_response.status_code = 200
        mock_response.content = b"<rss><channel></channel></rss>"
        mock_response.raise_for_status = MagicMock()

        with patch("requests.get", side_effect=[
            requests.Timeout("Timeout"),
            mock_response
        ]):
            with patch("time.sleep") as mock_sleep:
                fetcher.fetch_feed("https://reddit.com/r/test/.rss")

                # First retry should have 1 second delay (2^0)
                mock_sleep.assert_called_once_with(1)

    def test_uses_timeout(self, fetcher):
        """Test that timeout is passed to requests."""
        mock_response = MagicMock()
        mock_response.status_code = 200
        mock_response.content = b"<rss><channel></channel></rss>"
        mock_response.raise_for_status = MagicMock()

        with patch("requests.get", return_value=mock_response) as mock_get:
            fetcher.fetch_feed("https://reddit.com/r/test/.rss")

            call_kwargs = mock_get.call_args[1]
            assert call_kwargs["timeout"] == fetcher.timeout

    def test_raises_for_http_error(self, fetcher):
        """Test that HTTP errors are propagated."""
        mock_response = MagicMock()
        mock_response.status_code = 404
        mock_response.raise_for_status.side_effect = requests.HTTPError("404 Not Found")

        with patch("requests.get", return_value=mock_response):
            with patch("time.sleep"):
                with pytest.raises(requests.HTTPError):
                    fetcher.fetch_feed("https://reddit.com/r/nonexistent/.rss")

    def test_handles_rate_limit(self, fetcher):
        """Test that 429 Too Many Requests is retried."""
        mock_response_429 = MagicMock()
        mock_response_429.status_code = 429
        mock_response_429.raise_for_status.side_effect = requests.HTTPError("429 Too Many Requests")

        mock_response_ok = MagicMock()
        mock_response_ok.status_code = 200
        mock_response_ok.content = b"<rss><channel><title>Success</title></channel></rss>"
        mock_response_ok.raise_for_status = MagicMock()

        with patch("requests.get", side_effect=[mock_response_429, mock_response_ok]):
            with patch("time.sleep"):
                feed = fetcher.fetch_feed("https://reddit.com/r/test/.rss")

                assert feed.feed.get("title") == "Success"
