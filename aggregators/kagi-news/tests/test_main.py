"""
Tests for Main Orchestration Script.

Tests the complete flow: fetch → parse → format → dedupe → post → update state.
"""
import pytest
from pathlib import Path
from datetime import datetime
from unittest.mock import Mock, MagicMock, patch, call
import feedparser

from src.main import Aggregator
from src.models import KagiStory, AggregatorConfig, FeedConfig, Perspective, Quote, Source


@pytest.fixture
def mock_config():
    """Mock aggregator configuration."""
    return AggregatorConfig(
        coves_api_url="https://api.coves.social",
        feeds=[
            FeedConfig(
                name="World News",
                url="https://news.kagi.com/world.xml",
                community_handle="world-news.coves.social",
                enabled=True
            ),
            FeedConfig(
                name="Tech News",
                url="https://news.kagi.com/tech.xml",
                community_handle="tech.coves.social",
                enabled=True
            ),
            FeedConfig(
                name="Disabled Feed",
                url="https://news.kagi.com/disabled.xml",
                community_handle="disabled.coves.social",
                enabled=False
            )
        ],
        log_level="info"
    )


@pytest.fixture
def sample_story():
    """Sample KagiStory for testing."""
    return KagiStory(
        title="Test Story",
        link="https://kite.kagi.com/test/world/1",
        guid="https://kite.kagi.com/test/world/1",
        pub_date=datetime(2024, 1, 15, 12, 0, 0),
        categories=["World"],
        summary="Test summary",
        highlights=["Highlight 1", "Highlight 2"],
        perspectives=[
            Perspective(
                actor="Test Actor",
                description="Test description",
                source_url="https://example.com/source"
            )
        ],
        quote=Quote(text="Test quote", attribution="Test Author"),
        sources=[
            Source(title="Source 1", url="https://example.com/1", domain="example.com")
        ],
        image_url="https://example.com/image.jpg",
        image_alt="Test image"
    )


@pytest.fixture
def mock_rss_feed():
    """Mock RSS feed with sample entries."""
    feed = MagicMock()
    feed.bozo = 0
    feed.entries = [
        MagicMock(
            title="Story 1",
            link="https://kite.kagi.com/test/world/1",
            guid="https://kite.kagi.com/test/world/1",
            published_parsed=(2024, 1, 15, 12, 0, 0, 0, 15, 0),
            tags=[MagicMock(term="World")],
            description="<p>Story 1 description</p>"
        ),
        MagicMock(
            title="Story 2",
            link="https://kite.kagi.com/test/world/2",
            guid="https://kite.kagi.com/test/world/2",
            published_parsed=(2024, 1, 15, 13, 0, 0, 0, 15, 0),
            tags=[MagicMock(term="World")],
            description="<p>Story 2 description</p>"
        )
    ]
    return feed


class TestAggregator:
    """Test suite for Aggregator orchestration."""

    def test_initialize_aggregator(self, mock_config, tmp_path):
        """Test aggregator initialization."""
        state_file = tmp_path / "state.json"

        with patch('src.main.ConfigLoader') as MockConfigLoader:
            mock_loader = Mock()
            mock_loader.load.return_value = mock_config
            MockConfigLoader.return_value = mock_loader

            aggregator = Aggregator(
                config_path=Path("config.yaml"),
                state_file=state_file,
                coves_client=Mock()
            )

            assert aggregator.config == mock_config
            assert aggregator.state_file == state_file

    def test_process_enabled_feeds_only(self, mock_config, tmp_path):
        """Test that only enabled feeds are processed."""
        state_file = tmp_path / "state.json"
        mock_client = Mock()

        with patch('src.main.ConfigLoader') as MockConfigLoader, \
             patch('src.main.RSSFetcher') as MockRSSFetcher:

            mock_loader = Mock()
            mock_loader.load.return_value = mock_config
            MockConfigLoader.return_value = mock_loader

            mock_fetcher = Mock()
            MockRSSFetcher.return_value = mock_fetcher

            aggregator = Aggregator(
                config_path=Path("config.yaml"),
                state_file=state_file,
                coves_client=mock_client
            )

            # Mock empty feeds
            mock_fetcher.fetch_feed.return_value = MagicMock(bozo=0, entries=[])

            aggregator.run()

            # Should only fetch enabled feeds (2)
            assert mock_fetcher.fetch_feed.call_count == 2

    def test_full_successful_flow(self, mock_config, mock_rss_feed, sample_story, tmp_path):
        """Test complete flow: fetch → parse → format → post → update state."""
        state_file = tmp_path / "state.json"
        mock_client = Mock()
        mock_client.create_post.return_value = "at://did:plc:test/social.coves.post/abc123"

        with patch('src.main.ConfigLoader') as MockConfigLoader, \
             patch('src.main.RSSFetcher') as MockRSSFetcher, \
             patch('src.main.KagiHTMLParser') as MockHTMLParser, \
             patch('src.main.RichTextFormatter') as MockFormatter:

            # Setup mocks
            mock_loader = Mock()
            mock_loader.load.return_value = mock_config
            MockConfigLoader.return_value = mock_loader

            mock_fetcher = Mock()
            mock_fetcher.fetch_feed.return_value = mock_rss_feed
            MockRSSFetcher.return_value = mock_fetcher

            mock_parser = Mock()
            mock_parser.parse_to_story.return_value = sample_story
            MockHTMLParser.return_value = mock_parser

            mock_formatter = Mock()
            mock_formatter.format_full.return_value = {
                "content": "Test content",
                "facets": []
            }
            MockFormatter.return_value = mock_formatter

            # Run aggregator
            aggregator = Aggregator(
                config_path=Path("config.yaml"),
                state_file=state_file,
                coves_client=mock_client
            )
            aggregator.run()

            # Verify RSS fetching
            assert mock_fetcher.fetch_feed.call_count == 2

            # Verify parsing (2 entries per feed * 2 feeds = 4 total)
            assert mock_parser.parse_to_story.call_count == 4

            # Verify formatting
            assert mock_formatter.format_full.call_count == 4

            # Verify posting (should call create_post for each story)
            assert mock_client.create_post.call_count == 4

    def test_deduplication_skips_posted_stories(self, mock_config, mock_rss_feed, sample_story, tmp_path):
        """Test that already-posted stories are skipped."""
        state_file = tmp_path / "state.json"
        mock_client = Mock()
        mock_client.create_post.return_value = "at://did:plc:test/social.coves.post/abc123"

        with patch('src.main.ConfigLoader') as MockConfigLoader, \
             patch('src.main.RSSFetcher') as MockRSSFetcher, \
             patch('src.main.KagiHTMLParser') as MockHTMLParser, \
             patch('src.main.RichTextFormatter') as MockFormatter:

            # Setup mocks
            mock_loader = Mock()
            mock_loader.load.return_value = mock_config
            MockConfigLoader.return_value = mock_loader

            mock_fetcher = Mock()
            mock_fetcher.fetch_feed.return_value = mock_rss_feed
            MockRSSFetcher.return_value = mock_fetcher

            mock_parser = Mock()
            mock_parser.parse_to_story.return_value = sample_story
            MockHTMLParser.return_value = mock_parser

            mock_formatter = Mock()
            mock_formatter.format_full.return_value = {
                "content": "Test content",
                "facets": []
            }
            MockFormatter.return_value = mock_formatter

            # First run: posts all stories
            aggregator = Aggregator(
                config_path=Path("config.yaml"),
                state_file=state_file,
                coves_client=mock_client
            )
            aggregator.run()

            # Verify first run posted stories
            first_run_posts = mock_client.create_post.call_count
            assert first_run_posts == 4

            # Second run: should skip all (already posted)
            mock_client.reset_mock()
            aggregator2 = Aggregator(
                config_path=Path("config.yaml"),
                state_file=state_file,
                coves_client=mock_client
            )
            aggregator2.run()

            # Should not post any (all duplicates)
            assert mock_client.create_post.call_count == 0

    def test_continue_on_feed_error(self, mock_config, tmp_path):
        """Test that processing continues if one feed fails."""
        state_file = tmp_path / "state.json"
        mock_client = Mock()

        with patch('src.main.ConfigLoader') as MockConfigLoader, \
             patch('src.main.RSSFetcher') as MockRSSFetcher:

            mock_loader = Mock()
            mock_loader.load.return_value = mock_config
            MockConfigLoader.return_value = mock_loader

            mock_fetcher = Mock()
            # First feed fails, second succeeds
            mock_fetcher.fetch_feed.side_effect = [
                Exception("Network error"),
                MagicMock(bozo=0, entries=[])
            ]
            MockRSSFetcher.return_value = mock_fetcher

            aggregator = Aggregator(
                config_path=Path("config.yaml"),
                state_file=state_file,
                coves_client=mock_client
            )

            # Should not raise exception
            aggregator.run()

            # Should have attempted both feeds
            assert mock_fetcher.fetch_feed.call_count == 2

    def test_handle_empty_feed(self, mock_config, tmp_path):
        """Test handling of empty RSS feeds."""
        state_file = tmp_path / "state.json"
        mock_client = Mock()

        with patch('src.main.ConfigLoader') as MockConfigLoader, \
             patch('src.main.RSSFetcher') as MockRSSFetcher:

            mock_loader = Mock()
            mock_loader.load.return_value = mock_config
            MockConfigLoader.return_value = mock_loader

            mock_fetcher = Mock()
            mock_fetcher.fetch_feed.return_value = MagicMock(bozo=0, entries=[])
            MockRSSFetcher.return_value = mock_fetcher

            aggregator = Aggregator(
                config_path=Path("config.yaml"),
                state_file=state_file,
                coves_client=mock_client
            )
            aggregator.run()

            # Should not post anything
            assert mock_client.create_post.call_count == 0

    def test_dont_update_state_on_failed_post(self, mock_config, mock_rss_feed, sample_story, tmp_path):
        """Test that state is not updated if posting fails."""
        state_file = tmp_path / "state.json"
        mock_client = Mock()
        mock_client.create_post.side_effect = Exception("Post failed")

        with patch('src.main.ConfigLoader') as MockConfigLoader, \
             patch('src.main.RSSFetcher') as MockRSSFetcher, \
             patch('src.main.KagiHTMLParser') as MockHTMLParser, \
             patch('src.main.RichTextFormatter') as MockFormatter:

            # Setup mocks
            mock_loader = Mock()
            mock_loader.load.return_value = mock_config
            MockConfigLoader.return_value = mock_loader

            mock_fetcher = Mock()
            mock_fetcher.fetch_feed.return_value = mock_rss_feed
            MockRSSFetcher.return_value = mock_fetcher

            mock_parser = Mock()
            mock_parser.parse_to_story.return_value = sample_story
            MockHTMLParser.return_value = mock_parser

            mock_formatter = Mock()
            mock_formatter.format_full.return_value = {
                "content": "Test content",
                "facets": []
            }
            MockFormatter.return_value = mock_formatter

            # Run aggregator (posts will fail)
            aggregator = Aggregator(
                config_path=Path("config.yaml"),
                state_file=state_file,
                coves_client=mock_client
            )
            aggregator.run()

            # Reset client to succeed
            mock_client.reset_mock()
            mock_client.create_post.return_value = "at://did:plc:test/social.coves.post/abc123"

            # Second run: should try to post again (state wasn't updated)
            aggregator2 = Aggregator(
                config_path=Path("config.yaml"),
                state_file=state_file,
                coves_client=mock_client
            )
            aggregator2.run()

            # Should post stories (they weren't marked as posted)
            assert mock_client.create_post.call_count == 4

    def test_update_last_run_timestamp(self, mock_config, tmp_path):
        """Test that last_run timestamp is updated after successful processing."""
        state_file = tmp_path / "state.json"
        mock_client = Mock()

        with patch('src.main.ConfigLoader') as MockConfigLoader, \
             patch('src.main.RSSFetcher') as MockRSSFetcher:

            mock_loader = Mock()
            mock_loader.load.return_value = mock_config
            MockConfigLoader.return_value = mock_loader

            mock_fetcher = Mock()
            mock_fetcher.fetch_feed.return_value = MagicMock(bozo=0, entries=[])
            MockRSSFetcher.return_value = mock_fetcher

            aggregator = Aggregator(
                config_path=Path("config.yaml"),
                state_file=state_file,
                coves_client=mock_client
            )
            aggregator.run()

            # Verify last_run was updated for both feeds
            feed1_last_run = aggregator.state_manager.get_last_run(
                "https://news.kagi.com/world.xml"
            )
            feed2_last_run = aggregator.state_manager.get_last_run(
                "https://news.kagi.com/tech.xml"
            )

            assert feed1_last_run is not None
            assert feed2_last_run is not None

    def test_create_post_with_image_embed(self, mock_config, mock_rss_feed, sample_story, tmp_path):
        """Test that posts include external image embeds."""
        state_file = tmp_path / "state.json"
        mock_client = Mock()
        mock_client.create_post.return_value = "at://did:plc:test/social.coves.post/abc123"

        # Mock create_external_embed to return proper embed structure
        mock_client.create_external_embed.return_value = {
            "$type": "social.coves.embed.external",
            "external": {
                "uri": sample_story.link,
                "title": sample_story.title,
                "description": sample_story.summary,
                "thumb": sample_story.image_url
            }
        }

        with patch('src.main.ConfigLoader') as MockConfigLoader, \
             patch('src.main.RSSFetcher') as MockRSSFetcher, \
             patch('src.main.KagiHTMLParser') as MockHTMLParser, \
             patch('src.main.RichTextFormatter') as MockFormatter:

            # Setup mocks
            mock_loader = Mock()
            mock_loader.load.return_value = mock_config
            MockConfigLoader.return_value = mock_loader

            mock_fetcher = Mock()
            # Only one entry for simplicity
            single_entry_feed = MagicMock(bozo=0, entries=[mock_rss_feed.entries[0]])
            mock_fetcher.fetch_feed.return_value = single_entry_feed
            MockRSSFetcher.return_value = mock_fetcher

            mock_parser = Mock()
            mock_parser.parse_to_story.return_value = sample_story
            MockHTMLParser.return_value = mock_parser

            mock_formatter = Mock()
            mock_formatter.format_full.return_value = {
                "content": "Test content",
                "facets": []
            }
            MockFormatter.return_value = mock_formatter

            # Run aggregator
            aggregator = Aggregator(
                config_path=Path("config.yaml"),
                state_file=state_file,
                coves_client=mock_client
            )
            aggregator.run()

            # Verify create_post was called with embed
            mock_client.create_post.assert_called()
            call_kwargs = mock_client.create_post.call_args.kwargs

            assert "embed" in call_kwargs
            assert call_kwargs["embed"]["$type"] == "social.coves.embed.external"
            assert call_kwargs["embed"]["external"]["uri"] == sample_story.link
            assert call_kwargs["embed"]["external"]["title"] == sample_story.title
            assert call_kwargs["embed"]["external"]["thumb"] == sample_story.image_url
