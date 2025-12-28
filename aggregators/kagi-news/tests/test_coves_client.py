"""
Unit tests for CovesClient.

Tests the client's local functionality without requiring live infrastructure.
"""
import pytest
from unittest.mock import Mock
from src.coves_client import (
    CovesClient,
    CovesAPIError,
    CovesAuthenticationError,
    CovesForbiddenError,
    CovesNotFoundError,
    CovesRateLimitError,
)


# Valid test API key (70 chars total: 6 prefix + 64 hex chars)
VALID_TEST_API_KEY = "ckapi_" + "a" * 64


class TestAPIKeyValidation:
    """Tests for API key format validation in constructor."""

    def test_rejects_empty_api_key(self):
        """Empty API key should raise ValueError."""
        with pytest.raises(ValueError, match="cannot be empty"):
            CovesClient(api_url="http://localhost", api_key="")

    def test_rejects_wrong_prefix(self):
        """API key with wrong prefix should raise ValueError."""
        wrong_prefix_key = "wrong_" + "a" * 64
        with pytest.raises(ValueError, match="must start with 'ckapi_'"):
            CovesClient(api_url="http://localhost", api_key=wrong_prefix_key)

    def test_rejects_short_api_key(self):
        """API key that is too short should raise ValueError."""
        short_key = "ckapi_tooshort"
        with pytest.raises(ValueError, match="must be 70 characters"):
            CovesClient(api_url="http://localhost", api_key=short_key)

    def test_rejects_long_api_key(self):
        """API key that is too long should raise ValueError."""
        long_key = "ckapi_" + "a" * 100
        with pytest.raises(ValueError, match="must be 70 characters"):
            CovesClient(api_url="http://localhost", api_key=long_key)

    def test_accepts_valid_api_key(self):
        """Valid API key format should be accepted."""
        client = CovesClient(api_url="http://localhost", api_key=VALID_TEST_API_KEY)
        assert client.api_key == VALID_TEST_API_KEY


class TestRaiseForStatus:
    """Tests for _raise_for_status method."""

    @pytest.fixture
    def client(self):
        """Create a CovesClient instance for testing."""
        return CovesClient(api_url="http://localhost", api_key=VALID_TEST_API_KEY)

    def test_raises_authentication_error_for_401(self, client):
        """401 response should raise CovesAuthenticationError."""
        mock_response = Mock()
        mock_response.status_code = 401
        mock_response.text = "Invalid API key"

        with pytest.raises(CovesAuthenticationError) as exc_info:
            client._raise_for_status(mock_response)

        assert exc_info.value.status_code == 401
        assert "Authentication failed" in str(exc_info.value)

    def test_raises_forbidden_error_for_403(self, client):
        """403 response should raise CovesForbiddenError."""
        mock_response = Mock()
        mock_response.status_code = 403
        mock_response.text = "Not authorized for this community"

        with pytest.raises(CovesForbiddenError) as exc_info:
            client._raise_for_status(mock_response)

        assert exc_info.value.status_code == 403
        assert "Access forbidden" in str(exc_info.value)

    def test_raises_not_found_error_for_404(self, client):
        """404 response should raise CovesNotFoundError."""
        mock_response = Mock()
        mock_response.status_code = 404
        mock_response.text = "Community not found"

        with pytest.raises(CovesNotFoundError) as exc_info:
            client._raise_for_status(mock_response)

        assert exc_info.value.status_code == 404
        assert "Resource not found" in str(exc_info.value)

    def test_raises_rate_limit_error_for_429(self, client):
        """429 response should raise CovesRateLimitError."""
        mock_response = Mock()
        mock_response.status_code = 429
        mock_response.text = "Rate limit exceeded"

        with pytest.raises(CovesRateLimitError) as exc_info:
            client._raise_for_status(mock_response)

        assert exc_info.value.status_code == 429
        assert "Rate limit exceeded" in str(exc_info.value)

    def test_raises_generic_api_error_for_500(self, client):
        """500 response should raise generic CovesAPIError."""
        mock_response = Mock()
        mock_response.status_code = 500
        mock_response.text = "Internal server error"

        with pytest.raises(CovesAPIError) as exc_info:
            client._raise_for_status(mock_response)

        assert exc_info.value.status_code == 500
        assert not isinstance(exc_info.value, CovesAuthenticationError)
        assert not isinstance(exc_info.value, CovesNotFoundError)

    def test_exception_includes_response_body(self, client):
        """Exception should include the response body."""
        mock_response = Mock()
        mock_response.status_code = 400
        mock_response.text = '{"error": "Bad request details"}'

        with pytest.raises(CovesAPIError) as exc_info:
            client._raise_for_status(mock_response)

        assert exc_info.value.response_body == '{"error": "Bad request details"}'


class TestCreateExternalEmbed:
    """Tests for create_external_embed method."""

    @pytest.fixture
    def client(self):
        """Create a CovesClient instance for testing."""
        return CovesClient(
            api_url="http://localhost:8081",
            api_key=VALID_TEST_API_KEY
        )

    def test_creates_embed_without_sources(self, client):
        """Test basic embed creation without sources."""
        embed = client.create_external_embed(
            uri="https://example.com/article",
            title="Test Article",
            description="Test description"
        )

        assert embed["$type"] == "social.coves.embed.external"
        assert embed["external"]["uri"] == "https://example.com/article"
        assert embed["external"]["title"] == "Test Article"
        assert embed["external"]["description"] == "Test description"
        assert "sources" not in embed["external"]

    def test_creates_embed_with_sources(self, client):
        """Test embed creation with sources array."""
        sources = [
            {"uri": "https://source1.com/article", "title": "Source 1", "domain": "source1.com"},
            {"uri": "https://source2.com/article", "title": "Source 2", "domain": "source2.com"},
        ]

        embed = client.create_external_embed(
            uri="https://example.com/article",
            title="Test Article",
            description="Test description",
            sources=sources
        )

        assert embed["$type"] == "social.coves.embed.external"
        assert embed["external"]["uri"] == "https://example.com/article"
        assert "sources" in embed["external"]
        assert len(embed["external"]["sources"]) == 2
        assert embed["external"]["sources"][0]["uri"] == "https://source1.com/article"
        assert embed["external"]["sources"][0]["title"] == "Source 1"
        assert embed["external"]["sources"][0]["domain"] == "source1.com"
        assert embed["external"]["sources"][1]["uri"] == "https://source2.com/article"

    def test_creates_embed_with_empty_sources_list(self, client):
        """Test that empty sources list is excluded from embed."""
        embed = client.create_external_embed(
            uri="https://example.com/article",
            title="Test Article",
            description="Test description",
            sources=[]
        )

        assert embed["$type"] == "social.coves.embed.external"
        assert "sources" not in embed["external"]

    def test_creates_embed_with_none_sources(self, client):
        """Test that None sources is handled correctly."""
        embed = client.create_external_embed(
            uri="https://example.com/article",
            title="Test Article",
            description="Test description",
            sources=None
        )

        assert embed["$type"] == "social.coves.embed.external"
        assert "sources" not in embed["external"]

    def test_creates_embed_with_single_source(self, client):
        """Test embed creation with single source."""
        sources = [
            {"uri": "https://single.com/article", "title": "Single Source", "domain": "single.com"}
        ]

        embed = client.create_external_embed(
            uri="https://example.com/article",
            title="Test Article",
            description="Test description",
            sources=sources
        )

        assert len(embed["external"]["sources"]) == 1
        assert embed["external"]["sources"][0]["uri"] == "https://single.com/article"

    def test_embed_structure_matches_lexicon(self, client):
        """Test that embed structure matches social.coves.embed.external lexicon."""
        sources = [
            {"uri": "https://source.com/article", "title": "Source", "domain": "source.com"}
        ]

        embed = client.create_external_embed(
            uri="https://example.com/article",
            title="Test Article",
            description="Test description",
            sources=sources
        )

        # Verify top-level structure
        assert "$type" in embed
        assert "external" in embed
        assert len(embed) == 2  # Only $type and external

        # Verify external object structure
        external = embed["external"]
        assert "uri" in external
        assert "title" in external
        assert "description" in external
        assert "sources" in external

    def test_preserves_source_structure(self, client):
        """Test that source dictionaries are passed through unchanged."""
        sources = [
            {
                "uri": "https://source.com/article",
                "title": "Source Title",
                "domain": "source.com",
                "extra_field": "should be preserved"  # Extra fields should pass through
            }
        ]

        embed = client.create_external_embed(
            uri="https://example.com/article",
            title="Test Article",
            description="Test description",
            sources=sources
        )

        assert embed["external"]["sources"][0]["extra_field"] == "should be preserved"
