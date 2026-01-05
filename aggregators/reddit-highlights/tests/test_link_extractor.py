"""
Tests for link_extractor module.
"""
import pytest
from unittest.mock import MagicMock, patch
import requests
from src.link_extractor import LinkExtractor


class TestLinkExtractor:
    """Tests for LinkExtractor class."""

    def test_init_default_domains(self):
        """Test default allowed domains."""
        extractor = LinkExtractor()
        assert "streamable.com" in extractor.allowed_domains

    def test_init_custom_domains(self):
        """Test custom allowed domains."""
        extractor = LinkExtractor(allowed_domains=["example.com", "test.org"])
        assert "example.com" in extractor.allowed_domains
        assert "test.org" in extractor.allowed_domains
        assert "streamable.com" not in extractor.allowed_domains


class TestIsAllowedUrl:
    """Tests for is_allowed_url method."""

    @pytest.fixture
    def extractor(self):
        return LinkExtractor()

    def test_streamable_url(self, extractor):
        """Test streamable.com URL detection."""
        assert extractor.is_allowed_url("https://streamable.com/abc123")
        assert extractor.is_allowed_url("http://streamable.com/xyz789")
        assert extractor.is_allowed_url("https://www.streamable.com/test")

    def test_non_streamable_url(self, extractor):
        """Test rejection of non-streamable URLs."""
        assert not extractor.is_allowed_url("https://youtube.com/watch?v=123")
        assert not extractor.is_allowed_url("https://reddit.com/r/nba")
        assert not extractor.is_allowed_url("https://twitter.com/video")

    def test_empty_url(self, extractor):
        """Test empty URL handling."""
        assert not extractor.is_allowed_url("")
        assert not extractor.is_allowed_url(None)

    def test_invalid_url(self, extractor):
        """Test invalid URL handling."""
        assert not extractor.is_allowed_url("not a url")
        assert not extractor.is_allowed_url("streamable.com/abc")  # Missing scheme


class TestExtractVideoUrl:
    """Tests for extract_video_url method."""

    @pytest.fixture
    def extractor(self):
        return LinkExtractor()

    def test_direct_link(self, extractor):
        """Test extraction from direct link."""
        entry = MagicMock()
        entry.link = "https://streamable.com/abc123"
        entry.content = None
        entry.description = None
        entry.summary = None

        result = extractor.extract_video_url(entry)
        assert result == "https://streamable.com/abc123"

    def test_link_in_content(self, extractor):
        """Test extraction from entry content."""
        entry = MagicMock()
        entry.link = "https://reddit.com/r/nba/comments/123"

        content_item = MagicMock()
        content_item.value = 'Check out this play: <a href="https://streamable.com/xyz789">video</a>'
        entry.content = [content_item]
        entry.description = None
        entry.summary = None

        result = extractor.extract_video_url(entry)
        assert result == "https://streamable.com/xyz789"

    def test_link_in_description(self, extractor):
        """Test extraction from entry description."""
        entry = MagicMock()
        entry.link = "https://reddit.com/r/nba/comments/123"
        entry.content = None
        entry.description = "Amazing dunk! https://streamable.com/dunk99"
        entry.summary = None

        result = extractor.extract_video_url(entry)
        assert result == "https://streamable.com/dunk99"

    def test_link_in_summary(self, extractor):
        """Test extraction from entry summary."""
        entry = MagicMock()
        entry.link = "https://reddit.com/r/nba/comments/123"
        entry.content = None
        entry.description = None
        entry.summary = "Game winner: https://streamable.com/winner1"

        result = extractor.extract_video_url(entry)
        assert result == "https://streamable.com/winner1"

    def test_no_video_link(self, extractor):
        """Test when no video link is present."""
        entry = MagicMock()
        entry.link = "https://reddit.com/r/nba/comments/123"
        entry.content = None
        entry.description = "Just a text post about basketball"
        entry.summary = None

        result = extractor.extract_video_url(entry)
        assert result is None

    def test_multiple_links_returns_first(self, extractor):
        """Test that first video link is returned when multiple present."""
        entry = MagicMock()
        entry.link = "https://reddit.com/r/nba/comments/123"
        entry.content = None
        entry.description = (
            "First: https://streamable.com/first "
            "Second: https://streamable.com/second"
        )
        entry.summary = None

        result = extractor.extract_video_url(entry)
        assert result == "https://streamable.com/first"


class TestNormalizeUrl:
    """Tests for URL normalization."""

    @pytest.fixture
    def extractor(self):
        return LinkExtractor()

    def test_removes_trailing_slash(self, extractor):
        """Test trailing slash removal."""
        url = extractor._normalize_url("https://streamable.com/abc123/")
        assert url == "https://streamable.com/abc123"

    def test_upgrades_http_to_https(self, extractor):
        """Test HTTP to HTTPS upgrade."""
        url = extractor._normalize_url("http://streamable.com/abc123")
        assert url == "https://streamable.com/abc123"


class TestGetVideoId:
    """Tests for get_video_id method."""

    @pytest.fixture
    def extractor(self):
        return LinkExtractor()

    def test_extracts_video_id(self, extractor):
        """Test video ID extraction."""
        video_id = extractor.get_video_id("https://streamable.com/abc123")
        assert video_id == "abc123"

    def test_handles_www_prefix(self, extractor):
        """Test www prefix handling."""
        video_id = extractor.get_video_id("https://www.streamable.com/xyz789")
        assert video_id == "xyz789"

    def test_empty_url(self, extractor):
        """Test empty URL handling."""
        assert extractor.get_video_id("") is None
        assert extractor.get_video_id(None) is None

    def test_url_with_query_params(self, extractor):
        """Test URL with query parameters."""
        video_id = extractor.get_video_id("https://streamable.com/test123?foo=bar")
        assert video_id == "test123"


class TestGetThumbnailUrl:
    """Tests for get_thumbnail_url method."""

    @pytest.fixture
    def extractor(self):
        return LinkExtractor()

    def test_returns_thumbnail_on_success(self, extractor):
        """Test successful thumbnail fetch."""
        mock_response = MagicMock()
        mock_response.status_code = 200
        mock_response.json.return_value = {
            "thumbnail_url": "https://cdn.streamable.com/image/abc123.jpg",
            "type": "video",
        }

        with patch("requests.get", return_value=mock_response):
            result = extractor.get_thumbnail_url("https://streamable.com/abc123")
            assert result == "https://cdn.streamable.com/image/abc123.jpg"

    def test_returns_none_on_network_error(self, extractor):
        """Test handling of network errors."""
        with patch("requests.get", side_effect=requests.ConnectionError("Network error")):
            result = extractor.get_thumbnail_url("https://streamable.com/abc123")
            assert result is None

    def test_returns_none_on_http_error(self, extractor):
        """Test handling of HTTP errors."""
        mock_response = MagicMock()
        mock_response.raise_for_status.side_effect = requests.HTTPError("404 Not Found")

        with patch("requests.get", return_value=mock_response):
            result = extractor.get_thumbnail_url("https://streamable.com/abc123")
            assert result is None

    def test_returns_none_on_invalid_json(self, extractor):
        """Test handling of invalid JSON response."""
        mock_response = MagicMock()
        mock_response.status_code = 200
        mock_response.json.side_effect = ValueError("Invalid JSON")

        with patch("requests.get", return_value=mock_response):
            result = extractor.get_thumbnail_url("https://streamable.com/abc123")
            assert result is None

    def test_returns_none_for_empty_url(self, extractor):
        """Test empty URL handling."""
        assert extractor.get_thumbnail_url("") is None
        assert extractor.get_thumbnail_url(None) is None

    def test_returns_none_for_non_allowed_url(self, extractor):
        """Test rejection of non-allowed URLs."""
        # Should not even make a request for non-allowed domains
        with patch("requests.get") as mock_get:
            result = extractor.get_thumbnail_url("https://youtube.com/watch?v=123")
            assert result is None
            mock_get.assert_not_called()

    def test_returns_none_when_thumbnail_missing(self, extractor):
        """Test handling when thumbnail_url is missing from response."""
        mock_response = MagicMock()
        mock_response.status_code = 200
        mock_response.json.return_value = {"type": "video", "title": "Test"}

        with patch("requests.get", return_value=mock_response):
            result = extractor.get_thumbnail_url("https://streamable.com/abc123")
            assert result is None

    def test_uses_correct_oembed_url(self, extractor):
        """Test that correct oembed URL is constructed."""
        mock_response = MagicMock()
        mock_response.status_code = 200
        mock_response.json.return_value = {"thumbnail_url": "https://example.com/thumb.jpg"}

        with patch("requests.get", return_value=mock_response) as mock_get:
            extractor.get_thumbnail_url("https://streamable.com/xyz789")
            mock_get.assert_called_once()
            call_url = mock_get.call_args[0][0]
            assert "api.streamable.com/oembed" in call_url
            assert "url=https://streamable.com/xyz789" in call_url
