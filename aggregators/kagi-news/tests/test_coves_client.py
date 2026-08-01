"""
Unit tests for CovesClient.

Tests the client's local functionality without requiring live infrastructure.
"""
import logging
import pytest
from unittest.mock import Mock
from src.coves_client import (
    CovesClient,
    CovesAPIError,
    CovesAuthenticationError,
    CovesForbiddenError,
    CovesNotFoundError,
    CovesRateLimitError,
    MAX_EMBED_SOURCES,
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


class TestCreateExternalEmbedCapsSources:
    """
    `social.coves.embed.external` caps `sources` at 50. Kagi world-news clusters
    routinely carry 59-75 articles, and the AppView rejects the whole post with
    a 400 ("too many sources: 69 (max 50)"), so the bridge must trim to fit.
    """

    @pytest.fixture
    def client(self):
        return CovesClient(api_url="http://localhost:8081", api_key=VALID_TEST_API_KEY)

    def _sources(self, count):
        return [
            {"uri": f"https://example.com/{i}", "title": f"T{i}", "domain": "example.com"}
            for i in range(count)
        ]

    def test_over_limit_is_trimmed_to_fifty(self, client):
        embed = client.create_external_embed(
            uri="https://kagi.com/news/daily", title="T", description="D",
            sources=self._sources(75),
        )
        assert len(embed["external"]["sources"]) == MAX_EMBED_SOURCES

    def test_trim_keeps_the_leading_entries_in_order(self, client):
        """Kagi orders articles by relevance -- keep the front, not an arbitrary slice."""
        embed = client.create_external_embed(
            uri="https://kagi.com/news/daily", title="T", description="D",
            sources=self._sources(75),
        )
        titles = [s["title"] for s in embed["external"]["sources"]]
        assert titles == [f"T{i}" for i in range(MAX_EMBED_SOURCES)]

    def test_exactly_at_limit_is_untouched(self, client):
        embed = client.create_external_embed(
            uri="https://kagi.com/news/daily", title="T", description="D",
            sources=self._sources(MAX_EMBED_SOURCES),
        )
        assert len(embed["external"]["sources"]) == MAX_EMBED_SOURCES

    def test_under_limit_is_untouched(self, client):
        embed = client.create_external_embed(
            uri="https://kagi.com/news/daily", title="T", description="D",
            sources=self._sources(3),
        )
        assert len(embed["external"]["sources"]) == 3

    def test_trim_counts_survivors_not_submissions(self, client):
        """
        The cap applies after unusable URIs are dropped, so a batch that only
        exceeds 50 before sanitizing still submits every valid source it has.
        """
        sources = self._sources(52)
        for i in (0, 1, 2):
            sources[i]["uri"] = "not a url"
        embed = client.create_external_embed(
            uri="https://kagi.com/news/daily", title="T", description="D",
            sources=sources,
        )
        assert len(embed["external"]["sources"]) == 49


class TestCreateExternalEmbedSanitizesURIs:
    """
    The bridge is the source of the schema-invalid records this fix exists for.
    These cover the branches added when URI sanitizing was wired in: encoding,
    dropping an unusable source, and refusing to publish without a primary link.
    """

    @pytest.fixture
    def client(self):
        return CovesClient(api_url="http://localhost:8081", api_key=VALID_TEST_API_KEY)

    def test_primary_uri_is_encoded(self, client):
        embed = client.create_external_embed(
            uri="https://kagi.com/news/rudy_gobert_pokémon_lineup/",
            title="T", description="D",
        )
        assert embed["external"]["uri"] == (
            "https://kagi.com/news/rudy_gobert_pok%C3%A9mon_lineup/"
        )

    def test_every_source_uri_is_encoded(self, client):
        embed = client.create_external_embed(
            uri="https://kagi.com/news/daily", title="T", description="D",
            sources=[
                {"uri": "https://example.com/café", "title": "A", "domain": "example.com"},
                {"uri": "https://exämple.com/x", "title": "B", "domain": "x.com"},
            ],
        )
        assert [s["uri"] for s in embed["external"]["sources"]] == [
            "https://example.com/caf%C3%A9",
            "https://xn--exmple-cua.com/x",
        ]

    def test_source_metadata_survives_sanitizing(self, client):
        """The rebuild must not drop sibling keys of a source that needed encoding."""
        embed = client.create_external_embed(
            uri="https://kagi.com/news/daily", title="T", description="D",
            sources=[{
                "uri": "https://example.com/café",
                "title": "Accented",
                "domain": "example.com",
                "sourcePost": {"uri": "at://did:plc:x/c/1", "cid": "bafy"},
            }],
        )
        source = embed["external"]["sources"][0]
        assert source["title"] == "Accented"
        assert source["domain"] == "example.com"
        assert source["sourcePost"] == {"uri": "at://did:plc:x/c/1", "cid": "bafy"}

    def test_unusable_source_is_dropped_and_the_rest_survive(self, client):
        embed = client.create_external_embed(
            uri="https://kagi.com/news/daily", title="T", description="D",
            sources=[
                {"uri": "https://example.com/ok", "title": "keep"},
                {"uri": "not a url at all", "title": "drop"},
                {"uri": "https://example.com/café", "title": "keep"},
            ],
        )
        assert [s["uri"] for s in embed["external"]["sources"]] == [
            "https://example.com/ok",
            "https://example.com/caf%C3%A9",
        ]

    def test_source_missing_uri_key_is_dropped_not_raised(self, client):
        embed = client.create_external_embed(
            uri="https://kagi.com/news/daily", title="T", description="D",
            sources=[{"title": "no uri"}, {"uri": "https://example.com/ok"}],
        )
        assert [s["uri"] for s in embed["external"]["sources"]] == ["https://example.com/ok"]

    def test_non_string_source_uri_is_dropped_not_raised(self, client):
        """A malformed feed must cost one source, never the whole post."""
        embed = client.create_external_embed(
            uri="https://kagi.com/news/daily", title="T", description="D",
            sources=[{"uri": 42}, {"uri": None}, {"uri": "https://example.com/ok"}],
        )
        assert [s["uri"] for s in embed["external"]["sources"]] == ["https://example.com/ok"]

    def test_all_sources_unusable_omits_the_key(self, client):
        """Documented degradation: the embed ships, but with no sources key."""
        embed = client.create_external_embed(
            uri="https://kagi.com/news/daily", title="T", description="D",
            sources=[{"uri": "not a url"}, {"uri": "also not a url"}],
        )
        assert "sources" not in embed["external"]

    def test_all_sources_dropped_is_logged_at_error(self, client, caplog):
        """Total attribution loss must be visible above warning level."""
        with caplog.at_level(logging.ERROR):
            client.create_external_embed(
                uri="https://kagi.com/news/daily", title="T", description="D",
                sources=[{"uri": "not a url"}],
            )
        assert any(r.levelno >= logging.ERROR for r in caplog.records)

    def test_logs_do_not_leak_the_rejected_uri(self, client, caplog):
        """Feed URLs can carry signed-URL tokens; they must not reach the logs."""
        secret = "https://example.com/x?token=SUPERSECRETVALUE&z=é"
        with caplog.at_level(logging.WARNING):
            client.create_external_embed(
                uri="https://kagi.com/news/daily", title="T", description="D",
                sources=[{"uri": secret + "\x00"}],
            )
        assert "SUPERSECRETVALUE" not in caplog.text

    def test_unusable_primary_uri_raises(self, client):
        """A post with no resolvable link is not worth publishing."""
        with pytest.raises(ValueError):
            client.create_external_embed(uri="not a url at all", title="T", description="D")

    def test_forbidden_scheme_in_primary_uri_raises(self, client):
        with pytest.raises(ValueError, match="not allowed in a rendered link"):
            client.create_external_embed(
                uri="javascript:alert(1)", title="T", description="D",
            )
