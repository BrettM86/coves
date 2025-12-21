"""
Unit tests for CovesClient.

Tests the client's local functionality without requiring live infrastructure.
"""
import pytest
from src.coves_client import CovesClient


class TestCreateExternalEmbed:
    """Tests for create_external_embed method."""

    @pytest.fixture
    def client(self):
        """Create a CovesClient instance for testing."""
        return CovesClient(
            api_url="http://localhost:8081",
            handle="test.handle",
            password="test_password"
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
