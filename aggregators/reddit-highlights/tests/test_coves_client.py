"""
Tests for coves_client module.
"""
import pytest
from unittest.mock import MagicMock, patch
import requests

from src.coves_client import (
    CovesClient,
    CovesAPIError,
    CovesAuthenticationError,
    CovesForbiddenError,
    CovesNotFoundError,
    CovesRateLimitError,
)


class TestCovesClientInit:
    """Tests for CovesClient initialization."""

    def test_valid_api_key(self):
        """Test initialization with valid API key."""
        # Generate a valid 70-character API key
        api_key = "ckapi_" + "a" * 64
        client = CovesClient("https://coves.social", api_key)
        assert client.api_key == api_key
        assert client.api_url == "https://coves.social"

    def test_strips_trailing_slash_from_url(self):
        """Test that trailing slash is stripped from API URL."""
        api_key = "ckapi_" + "a" * 64
        client = CovesClient("https://coves.social/", api_key)
        assert client.api_url == "https://coves.social"

    def test_empty_api_key_raises(self):
        """Test that empty API key raises ValueError."""
        with pytest.raises(ValueError, match="cannot be empty"):
            CovesClient("https://coves.social", "")

    def test_wrong_prefix_raises(self):
        """Test that API key with wrong prefix raises ValueError."""
        with pytest.raises(ValueError, match="must start with"):
            CovesClient("https://coves.social", "invalid_" + "a" * 63)

    def test_wrong_length_raises(self):
        """Test that API key with wrong length raises ValueError."""
        with pytest.raises(ValueError, match="must be 70 characters"):
            CovesClient("https://coves.social", "ckapi_tooshort")

    def test_session_headers_set(self):
        """Test that session headers are properly set."""
        api_key = "ckapi_" + "b" * 64
        client = CovesClient("https://coves.social", api_key)
        assert client.session.headers["Authorization"] == f"Bearer {api_key}"
        assert client.session.headers["Content-Type"] == "application/json"


class TestCovesClientAuthenticate:
    """Tests for authenticate method."""

    def test_authenticate_is_noop(self):
        """Test that authenticate is a no-op for API key auth."""
        api_key = "ckapi_" + "c" * 64
        client = CovesClient("https://coves.social", api_key)
        # Should not raise any exceptions
        client.authenticate()


class TestCovesClientCreatePost:
    """Tests for create_post method."""

    @pytest.fixture
    def client(self):
        api_key = "ckapi_" + "d" * 64
        return CovesClient("https://coves.social", api_key)

    def test_successful_post_creation(self, client):
        """Test successful post creation."""
        mock_response = MagicMock()
        mock_response.ok = True
        mock_response.status_code = 200
        mock_response.json.return_value = {"uri": "at://did:plc:test/social.coves.post/abc123"}

        with patch.object(client.session, "post", return_value=mock_response):
            uri = client.create_post(
                community_handle="test.coves.social",
                content="Test content",
                facets=[],
                title="Test Title",
            )
            assert uri == "at://did:plc:test/social.coves.post/abc123"

    def test_post_with_embed(self, client):
        """Test post creation with embed."""
        mock_response = MagicMock()
        mock_response.ok = True
        mock_response.json.return_value = {"uri": "at://did:plc:test/social.coves.post/xyz"}

        with patch.object(client.session, "post", return_value=mock_response) as mock_post:
            embed = {"$type": "social.coves.embed.external", "external": {"uri": "https://example.com"}}
            client.create_post(
                community_handle="test.coves.social",
                content="",
                facets=[],
                embed=embed,
            )
            # Verify embed was included in request
            call_args = mock_post.call_args
            assert call_args[1]["json"]["embed"] == embed

    def test_post_with_thumbnail_url(self, client):
        """Test post creation with thumbnail URL."""
        mock_response = MagicMock()
        mock_response.ok = True
        mock_response.json.return_value = {"uri": "at://did:plc:test/social.coves.post/thumb"}

        with patch.object(client.session, "post", return_value=mock_response) as mock_post:
            client.create_post(
                community_handle="test.coves.social",
                content="",
                facets=[],
                thumbnail_url="https://example.com/thumb.jpg",
            )
            call_args = mock_post.call_args
            assert call_args[1]["json"]["thumbnailUrl"] == "https://example.com/thumb.jpg"

    def test_401_raises_authentication_error(self, client):
        """Test that 401 response raises CovesAuthenticationError."""
        mock_response = MagicMock()
        mock_response.ok = False
        mock_response.status_code = 401
        mock_response.text = "Unauthorized"

        with patch.object(client.session, "post", return_value=mock_response):
            with pytest.raises(CovesAuthenticationError):
                client.create_post("test.coves.social", "", [])

    def test_403_raises_forbidden_error(self, client):
        """Test that 403 response raises CovesForbiddenError."""
        mock_response = MagicMock()
        mock_response.ok = False
        mock_response.status_code = 403
        mock_response.text = "Forbidden"

        with patch.object(client.session, "post", return_value=mock_response):
            with pytest.raises(CovesForbiddenError):
                client.create_post("test.coves.social", "", [])

    def test_404_raises_not_found_error(self, client):
        """Test that 404 response raises CovesNotFoundError."""
        mock_response = MagicMock()
        mock_response.ok = False
        mock_response.status_code = 404
        mock_response.text = "Not Found"

        with patch.object(client.session, "post", return_value=mock_response):
            with pytest.raises(CovesNotFoundError):
                client.create_post("test.coves.social", "", [])

    def test_429_raises_rate_limit_error(self, client):
        """Test that 429 response raises CovesRateLimitError."""
        mock_response = MagicMock()
        mock_response.ok = False
        mock_response.status_code = 429
        mock_response.text = "Rate Limited"

        with patch.object(client.session, "post", return_value=mock_response):
            with pytest.raises(CovesRateLimitError):
                client.create_post("test.coves.social", "", [])

    def test_500_raises_api_error(self, client):
        """Test that 500 response raises CovesAPIError."""
        mock_response = MagicMock()
        mock_response.ok = False
        mock_response.status_code = 500
        mock_response.text = "Internal Server Error"

        with patch.object(client.session, "post", return_value=mock_response):
            with pytest.raises(CovesAPIError):
                client.create_post("test.coves.social", "", [])

    def test_invalid_json_response_raises_api_error(self, client):
        """Test that invalid JSON response raises CovesAPIError."""
        mock_response = MagicMock()
        mock_response.ok = True
        mock_response.status_code = 200
        mock_response.text = "not json"
        mock_response.json.side_effect = ValueError("Invalid JSON")

        with patch.object(client.session, "post", return_value=mock_response):
            with pytest.raises(CovesAPIError, match="Invalid response"):
                client.create_post("test.coves.social", "", [])

    def test_missing_uri_in_response_raises_api_error(self, client):
        """Test that missing uri in response raises CovesAPIError."""
        mock_response = MagicMock()
        mock_response.ok = True
        mock_response.status_code = 200
        mock_response.text = '{"cid": "abc"}'
        mock_response.json.return_value = {"cid": "abc"}  # No uri field

        with patch.object(client.session, "post", return_value=mock_response):
            with pytest.raises(CovesAPIError, match="Invalid response"):
                client.create_post("test.coves.social", "", [])

    def test_network_error_propagates(self, client):
        """Test that network errors propagate."""
        with patch.object(client.session, "post", side_effect=requests.ConnectionError("Network error")):
            with pytest.raises(requests.RequestException):
                client.create_post("test.coves.social", "", [])


class TestCreateExternalEmbed:
    """Tests for create_external_embed method."""

    @pytest.fixture
    def client(self):
        api_key = "ckapi_" + "e" * 64
        return CovesClient("https://coves.social", api_key)

    def test_basic_embed(self, client):
        """Test basic external embed creation."""
        embed = client.create_external_embed(
            uri="https://streamable.com/abc123",
            title="Test Video",
            description="A test video",
        )
        assert embed["$type"] == "social.coves.embed.external"
        assert embed["external"]["uri"] == "https://streamable.com/abc123"
        assert embed["external"]["title"] == "Test Video"
        assert embed["external"]["description"] == "A test video"

    def test_embed_with_sources(self, client):
        """Test embed with sources."""
        sources = [{"uri": "https://reddit.com/r/test", "title": "r/test", "domain": "reddit.com"}]
        embed = client.create_external_embed(
            uri="https://streamable.com/abc123",
            title="Test",
            description="Test",
            sources=sources,
        )
        assert embed["external"]["sources"] == sources

    def test_embed_with_video_metadata(self, client):
        """Test embed with video metadata."""
        embed = client.create_external_embed(
            uri="https://streamable.com/abc123",
            title="Test",
            description="Test",
            embed_type="video",
            provider="streamable",
            domain="streamable.com",
        )
        assert embed["external"]["embedType"] == "video"
        assert embed["external"]["provider"] == "streamable"
        assert embed["external"]["domain"] == "streamable.com"

    def test_optional_fields_not_included_when_none(self, client):
        """Test that optional fields are not included when None."""
        embed = client.create_external_embed(
            uri="https://example.com",
            title="Test",
            description="Test",
        )
        assert "sources" not in embed["external"]
        assert "embedType" not in embed["external"]
        assert "provider" not in embed["external"]
        assert "domain" not in embed["external"]


class TestGetTimestamp:
    """Tests for _get_timestamp method."""

    @pytest.fixture
    def client(self):
        api_key = "ckapi_" + "f" * 64
        return CovesClient("https://coves.social", api_key)

    def test_returns_iso_format(self, client):
        """Test that timestamp is in ISO 8601 format."""
        timestamp = client._get_timestamp()
        # Should end with Z (UTC)
        assert timestamp.endswith("Z")
        # Should be parseable as ISO format
        from datetime import datetime
        datetime.fromisoformat(timestamp.replace("Z", "+00:00"))
