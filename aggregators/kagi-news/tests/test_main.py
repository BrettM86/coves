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
from src.models import KagiStory, AggregatorConfig, FeedConfig, DedupConfig, Perspective, Quote, Source


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
        log_level="info",
        dedup=DedupConfig(semantic_enabled=False)
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
    """Mock RSS feed with sample entries (XML cluster_numbers 1 and 2)."""
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


@pytest.fixture
def mock_json_clusters():
    """JSON clusters keyed at XML.cluster_number + 1, matching mock_rss_feed titles."""
    return {
        2: {"cluster_number": 2, "title": "Story 1", "short_summary": "JSON summary 1"},
        3: {"cluster_number": 3, "title": "Story 2", "short_summary": "JSON summary 2"},
    }


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
             patch('src.main.RSSFetcher') as MockRSSFetcher, \
             patch('src.main.JSONFetcher') as MockJSONFetcher:

            mock_loader = Mock()
            mock_loader.load.return_value = mock_config
            MockConfigLoader.return_value = mock_loader

            mock_fetcher = Mock()
            MockRSSFetcher.return_value = mock_fetcher

            mock_json_fetcher = Mock()
            mock_json_fetcher.fetch_clusters.return_value = {}
            MockJSONFetcher.return_value = mock_json_fetcher

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

    def test_full_successful_flow(self, mock_config, mock_rss_feed, mock_json_clusters, sample_story, tmp_path):
        """Test complete flow: fetch → parse → format → post → update state."""
        state_file = tmp_path / "state.json"
        mock_client = Mock()
        mock_client.create_post.return_value = "at://did:plc:test/social.coves.post/abc123"

        with patch('src.main.ConfigLoader') as MockConfigLoader, \
             patch('src.main.RSSFetcher') as MockRSSFetcher, \
             patch('src.main.JSONFetcher') as MockJSONFetcher, \
             patch('src.main.KagiJSONParser') as MockJSONParser, \
             patch('src.main.RichTextFormatter') as MockFormatter:

            # Setup mocks
            mock_loader = Mock()
            mock_loader.load.return_value = mock_config
            MockConfigLoader.return_value = mock_loader

            mock_fetcher = Mock()
            mock_fetcher.fetch_feed.return_value = mock_rss_feed
            MockRSSFetcher.return_value = mock_fetcher

            mock_json_fetcher = Mock()
            mock_json_fetcher.fetch_clusters.return_value = mock_json_clusters
            MockJSONFetcher.return_value = mock_json_fetcher

            mock_parser = Mock()
            mock_parser.parse_to_story.return_value = sample_story
            MockJSONParser.return_value = mock_parser

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
            # Verify JSON fetching (one per feed)
            assert mock_json_fetcher.fetch_clusters.call_count == 2

            # Verify parsing (2 entries per feed * 2 feeds = 4 total)
            assert mock_parser.parse_to_story.call_count == 4

            # Verify formatting
            assert mock_formatter.format_full.call_count == 4

            # Verify posting (should call create_post for each story)
            assert mock_client.create_post.call_count == 4

    def test_deduplication_skips_posted_stories(self, mock_config, mock_rss_feed, mock_json_clusters, sample_story, tmp_path):
        """Test that already-posted stories are skipped."""
        state_file = tmp_path / "state.json"
        mock_client = Mock()
        mock_client.create_post.return_value = "at://did:plc:test/social.coves.post/abc123"

        with patch('src.main.ConfigLoader') as MockConfigLoader, \
             patch('src.main.RSSFetcher') as MockRSSFetcher, \
             patch('src.main.JSONFetcher') as MockJSONFetcher, \
             patch('src.main.KagiJSONParser') as MockJSONParser, \
             patch('src.main.RichTextFormatter') as MockFormatter:

            # Setup mocks
            mock_loader = Mock()
            mock_loader.load.return_value = mock_config
            MockConfigLoader.return_value = mock_loader

            mock_fetcher = Mock()
            mock_fetcher.fetch_feed.return_value = mock_rss_feed
            MockRSSFetcher.return_value = mock_fetcher

            mock_json_fetcher = Mock()
            mock_json_fetcher.fetch_clusters.return_value = mock_json_clusters
            MockJSONFetcher.return_value = mock_json_fetcher

            mock_parser = Mock()
            mock_parser.parse_to_story.return_value = sample_story
            MockJSONParser.return_value = mock_parser

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
             patch('src.main.RSSFetcher') as MockRSSFetcher, \
             patch('src.main.JSONFetcher') as MockJSONFetcher:

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

            mock_json_fetcher = Mock()
            mock_json_fetcher.fetch_clusters.return_value = {}
            MockJSONFetcher.return_value = mock_json_fetcher

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
             patch('src.main.RSSFetcher') as MockRSSFetcher, \
             patch('src.main.JSONFetcher') as MockJSONFetcher:

            mock_loader = Mock()
            mock_loader.load.return_value = mock_config
            MockConfigLoader.return_value = mock_loader

            mock_fetcher = Mock()
            mock_fetcher.fetch_feed.return_value = MagicMock(bozo=0, entries=[])
            MockRSSFetcher.return_value = mock_fetcher

            mock_json_fetcher = Mock()
            mock_json_fetcher.fetch_clusters.return_value = {}
            MockJSONFetcher.return_value = mock_json_fetcher

            aggregator = Aggregator(
                config_path=Path("config.yaml"),
                state_file=state_file,
                coves_client=mock_client
            )
            aggregator.run()

            # Should not post anything
            assert mock_client.create_post.call_count == 0

    def test_dont_update_state_on_failed_post(self, mock_config, mock_rss_feed, mock_json_clusters, sample_story, tmp_path):
        """Test that state is not updated if posting fails."""
        state_file = tmp_path / "state.json"
        mock_client = Mock()
        mock_client.create_post.side_effect = Exception("Post failed")

        with patch('src.main.ConfigLoader') as MockConfigLoader, \
             patch('src.main.RSSFetcher') as MockRSSFetcher, \
             patch('src.main.JSONFetcher') as MockJSONFetcher, \
             patch('src.main.KagiJSONParser') as MockJSONParser, \
             patch('src.main.RichTextFormatter') as MockFormatter:

            # Setup mocks
            mock_loader = Mock()
            mock_loader.load.return_value = mock_config
            MockConfigLoader.return_value = mock_loader

            mock_fetcher = Mock()
            mock_fetcher.fetch_feed.return_value = mock_rss_feed
            MockRSSFetcher.return_value = mock_fetcher

            mock_json_fetcher = Mock()
            mock_json_fetcher.fetch_clusters.return_value = mock_json_clusters
            MockJSONFetcher.return_value = mock_json_fetcher

            mock_parser = Mock()
            mock_parser.parse_to_story.return_value = sample_story
            MockJSONParser.return_value = mock_parser

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
             patch('src.main.RSSFetcher') as MockRSSFetcher, \
             patch('src.main.JSONFetcher') as MockJSONFetcher:

            mock_loader = Mock()
            mock_loader.load.return_value = mock_config
            MockConfigLoader.return_value = mock_loader

            mock_fetcher = Mock()
            mock_fetcher.fetch_feed.return_value = MagicMock(bozo=0, entries=[])
            MockRSSFetcher.return_value = mock_fetcher

            mock_json_fetcher = Mock()
            mock_json_fetcher.fetch_clusters.return_value = {}
            MockJSONFetcher.return_value = mock_json_fetcher

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

    def test_create_post_with_image_embed(self, mock_config, mock_rss_feed, mock_json_clusters, sample_story, tmp_path):
        """Test that posts include external image embeds."""
        state_file = tmp_path / "state.json"
        mock_client = Mock()
        mock_client.create_post.return_value = "at://did:plc:test/social.coves.post/abc123"

        # Mock create_external_embed to return proper embed structure
        # Note: Thumbnails are handled by server's unfurl service, not client
        mock_client.create_external_embed.return_value = {
            "$type": "social.coves.embed.external",
            "external": {
                "uri": sample_story.link,
                "title": sample_story.title,
                "description": sample_story.summary
            }
        }

        with patch('src.main.ConfigLoader') as MockConfigLoader, \
             patch('src.main.RSSFetcher') as MockRSSFetcher, \
             patch('src.main.JSONFetcher') as MockJSONFetcher, \
             patch('src.main.KagiJSONParser') as MockJSONParser, \
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

            mock_json_fetcher = Mock()
            mock_json_fetcher.fetch_clusters.return_value = mock_json_clusters
            MockJSONFetcher.return_value = mock_json_fetcher

            mock_parser = Mock()
            mock_parser.parse_to_story.return_value = sample_story
            MockJSONParser.return_value = mock_parser

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
            # Thumbnail is not included - server's unfurl service handles it
            assert "thumb" not in call_kwargs["embed"]["external"]

    def test_create_post_with_sources_in_embed(self, mock_config, mock_rss_feed, mock_json_clusters, sample_story, tmp_path):
        """Test that posts include sources in external embeds when available."""
        state_file = tmp_path / "state.json"
        mock_client = Mock()
        mock_client.create_post.return_value = "at://did:plc:test/social.coves.post/abc123"

        # Mock create_external_embed to return proper embed structure with sources
        mock_client.create_external_embed.return_value = {
            "$type": "social.coves.embed.external",
            "external": {
                "uri": sample_story.link,
                "title": sample_story.title,
                "description": sample_story.summary,
                "sources": [
                    {"uri": "https://example.com/1", "title": "Source 1", "domain": "example.com"}
                ]
            }
        }

        with patch('src.main.ConfigLoader') as MockConfigLoader, \
             patch('src.main.RSSFetcher') as MockRSSFetcher, \
             patch('src.main.JSONFetcher') as MockJSONFetcher, \
             patch('src.main.KagiJSONParser') as MockJSONParser, \
             patch('src.main.RichTextFormatter') as MockFormatter:

            # Setup mocks
            mock_loader = Mock()
            mock_loader.load.return_value = mock_config
            MockConfigLoader.return_value = mock_loader

            mock_fetcher = Mock()
            single_entry_feed = MagicMock(bozo=0, entries=[mock_rss_feed.entries[0]])
            mock_fetcher.fetch_feed.return_value = single_entry_feed
            MockRSSFetcher.return_value = mock_fetcher

            mock_json_fetcher = Mock()
            mock_json_fetcher.fetch_clusters.return_value = mock_json_clusters
            MockJSONFetcher.return_value = mock_json_fetcher

            mock_parser = Mock()
            mock_parser.parse_to_story.return_value = sample_story
            MockJSONParser.return_value = mock_parser

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

            # Verify create_external_embed was called with sources
            mock_client.create_external_embed.assert_called()
            call_kwargs = mock_client.create_external_embed.call_args.kwargs

            # Verify sources were passed
            assert "sources" in call_kwargs
            assert len(call_kwargs["sources"]) == 1
            assert call_kwargs["sources"][0]["uri"] == "https://example.com/1"
            assert call_kwargs["sources"][0]["title"] == "Source 1"
            assert call_kwargs["sources"][0]["domain"] == "example.com"

    def test_create_post_without_sources(self, mock_config, mock_rss_feed, mock_json_clusters, tmp_path):
        """Test that posts without sources don't include sources in embed."""
        state_file = tmp_path / "state.json"
        mock_client = Mock()
        mock_client.create_post.return_value = "at://did:plc:test/social.coves.post/abc123"

        # Create a story without sources
        story_without_sources = KagiStory(
            title="Test Story No Sources",
            link="https://kite.kagi.com/test/world/1",
            guid="https://kite.kagi.com/test/world/1",
            pub_date=datetime(2024, 1, 15, 12, 0, 0),
            categories=["World"],
            summary="Test summary",
            highlights=[],
            perspectives=[],
            quote=None,
            sources=[],  # No sources
            image_url=None,
            image_alt=None
        )

        # Mock create_external_embed to return proper embed structure without sources
        mock_client.create_external_embed.return_value = {
            "$type": "social.coves.embed.external",
            "external": {
                "uri": story_without_sources.link,
                "title": story_without_sources.title,
                "description": story_without_sources.summary
            }
        }

        with patch('src.main.ConfigLoader') as MockConfigLoader, \
             patch('src.main.RSSFetcher') as MockRSSFetcher, \
             patch('src.main.JSONFetcher') as MockJSONFetcher, \
             patch('src.main.KagiJSONParser') as MockJSONParser, \
             patch('src.main.RichTextFormatter') as MockFormatter:

            # Setup mocks
            mock_loader = Mock()
            mock_loader.load.return_value = mock_config
            MockConfigLoader.return_value = mock_loader

            mock_fetcher = Mock()
            single_entry_feed = MagicMock(bozo=0, entries=[mock_rss_feed.entries[0]])
            mock_fetcher.fetch_feed.return_value = single_entry_feed
            MockRSSFetcher.return_value = mock_fetcher

            mock_json_fetcher = Mock()
            mock_json_fetcher.fetch_clusters.return_value = mock_json_clusters
            MockJSONFetcher.return_value = mock_json_fetcher

            mock_parser = Mock()
            mock_parser.parse_to_story.return_value = story_without_sources
            MockJSONParser.return_value = mock_parser

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

            # Verify create_external_embed was called
            mock_client.create_external_embed.assert_called()
            call_kwargs = mock_client.create_external_embed.call_args.kwargs

            # Verify sources is None (empty list becomes None)
            assert call_kwargs.get("sources") is None

    def test_semantic_dedup_filters_duplicates(self, mock_rss_feed, mock_json_clusters, tmp_path):
        """Test that semantic dedup filters out similar stories when recent stories exist."""
        import json

        # Config with semantic dedup enabled
        config = AggregatorConfig(
            coves_api_url="https://api.coves.social",
            feeds=[
                FeedConfig(
                    name="World News",
                    url="https://news.kagi.com/world.xml",
                    community_handle="world-news.coves.social",
                    enabled=True
                )
            ],
            log_level="info",
            dedup=DedupConfig(semantic_enabled=True, lookback_days=4)
        )

        # Pre-populate state with recent stories so get_recent_stories returns data
        state_file = tmp_path / "state.json"
        state_file.write_text(json.dumps({
            "feeds": {
                "https://news.kagi.com/world.xml": {
                    "posted_guids": [
                        {
                            "guid": "existing-1",
                            "post_uri": "at://test/1",
                            "posted_at": datetime.now().isoformat(),
                            "title": "US announces new tariffs on China",
                            "summary_snippet": "The United States has announced new tariffs."
                        }
                    ],
                    "last_successful_run": None
                }
            }
        }))

        mock_client = Mock()
        mock_client.create_post.return_value = "at://did:plc:test/social.coves.post/abc123"

        # Create two distinct stories so we can verify one is filtered and one is not
        story_1 = KagiStory(
            title="Trade tensions escalate as US tariffs take effect",
            link="https://kite.kagi.com/test/world/1",
            guid="https://kite.kagi.com/test/world/1",
            pub_date=datetime(2024, 1, 15, 12, 0, 0),
            categories=["World"],
            summary="US tariffs on Chinese goods go into effect.",
            highlights=[], perspectives=[], quote=None, sources=[],
            image_url=None, image_alt=None
        )
        story_2 = KagiStory(
            title="Earthquake hits Turkey killing dozens",
            link="https://kite.kagi.com/test/world/2",
            guid="https://kite.kagi.com/test/world/2",
            pub_date=datetime(2024, 1, 15, 13, 0, 0),
            categories=["World"],
            summary="A 6.5 magnitude earthquake struck southeastern Turkey.",
            highlights=[], perspectives=[], quote=None, sources=[],
            image_url=None, image_alt=None
        )

        # Mock semantic dedup to mark first story as duplicate of existing-1
        mock_dedup = Mock()
        mock_dedup.find_duplicates.return_value = {"https://kite.kagi.com/test/world/1"}

        with patch('src.main.ConfigLoader') as MockConfigLoader, \
             patch('src.main.RSSFetcher') as MockRSSFetcher, \
             patch('src.main.JSONFetcher') as MockJSONFetcher, \
             patch('src.main.KagiJSONParser') as MockJSONParser, \
             patch('src.main.RichTextFormatter') as MockFormatter:

            mock_loader = Mock()
            mock_loader.load.return_value = config
            MockConfigLoader.return_value = mock_loader

            mock_fetcher = Mock()
            mock_fetcher.fetch_feed.return_value = mock_rss_feed
            MockRSSFetcher.return_value = mock_fetcher

            mock_json_fetcher = Mock()
            mock_json_fetcher.fetch_clusters.return_value = mock_json_clusters
            MockJSONFetcher.return_value = mock_json_fetcher

            mock_parser = Mock()
            # Return different stories for the two entries
            mock_parser.parse_to_story.side_effect = [story_1, story_2]
            MockJSONParser.return_value = mock_parser

            mock_formatter = Mock()
            mock_formatter.format_full.return_value = {
                "content": "Test content",
                "facets": []
            }
            MockFormatter.return_value = mock_formatter

            aggregator = Aggregator(
                config_path=Path("config.yaml"),
                state_file=state_file,
                coves_client=mock_client,
                semantic_dedup=mock_dedup
            )
            aggregator.run()

            # find_duplicates should have been called with the new stories
            mock_dedup.find_duplicates.assert_called_once()
            call_args = mock_dedup.find_duplicates.call_args
            new_for_comparison = call_args[0][0]
            recent_for_comparison = call_args[0][1]

            # Should have passed both new candidates
            assert len(new_for_comparison) == 2
            # Should have passed the pre-populated recent story
            assert len(recent_for_comparison) == 1
            assert recent_for_comparison[0]["id"] == "existing-1"

            # Only story_2 should be posted (story_1 was marked as duplicate)
            assert mock_client.create_post.call_count == 1
            posted_title = mock_client.create_post.call_args.kwargs.get("title")
            assert posted_title == "Earthquake hits Turkey killing dozens"

    def test_semantic_dedup_disabled_skips_check(self, mock_config, mock_rss_feed, mock_json_clusters, sample_story, tmp_path):
        """Test that semantic dedup is skipped when disabled."""
        state_file = tmp_path / "state.json"
        mock_client = Mock()
        mock_client.create_post.return_value = "at://did:plc:test/social.coves.post/abc123"

        mock_dedup = Mock()

        with patch('src.main.ConfigLoader') as MockConfigLoader, \
             patch('src.main.RSSFetcher') as MockRSSFetcher, \
             patch('src.main.JSONFetcher') as MockJSONFetcher, \
             patch('src.main.KagiJSONParser') as MockJSONParser, \
             patch('src.main.RichTextFormatter') as MockFormatter:

            mock_loader = Mock()
            mock_loader.load.return_value = mock_config  # dedup disabled
            MockConfigLoader.return_value = mock_loader

            mock_fetcher = Mock()
            mock_fetcher.fetch_feed.return_value = mock_rss_feed
            MockRSSFetcher.return_value = mock_fetcher

            mock_json_fetcher = Mock()
            mock_json_fetcher.fetch_clusters.return_value = mock_json_clusters
            MockJSONFetcher.return_value = mock_json_fetcher

            mock_parser = Mock()
            mock_parser.parse_to_story.return_value = sample_story
            MockJSONParser.return_value = mock_parser

            mock_formatter = Mock()
            mock_formatter.format_full.return_value = {
                "content": "Test content",
                "facets": []
            }
            MockFormatter.return_value = mock_formatter

            aggregator = Aggregator(
                config_path=Path("config.yaml"),
                state_file=state_file,
                coves_client=mock_client
            )
            # semantic_dedup should be None when disabled
            assert aggregator.semantic_dedup is None

            aggregator.run()
            # All stories should be posted (no semantic filtering)
            assert mock_client.create_post.call_count == 4

    def test_skip_entry_when_no_matching_json_cluster(self, mock_config, mock_rss_feed, sample_story, tmp_path, caplog):
        """If the JSON has no cluster matching by offset OR title, that entry is skipped."""
        state_file = tmp_path / "state.json"
        mock_client = Mock()
        mock_client.create_post.return_value = "at://did:plc:test/social.coves.post/abc123"

        with patch('src.main.ConfigLoader') as MockConfigLoader, \
             patch('src.main.RSSFetcher') as MockRSSFetcher, \
             patch('src.main.JSONFetcher') as MockJSONFetcher, \
             patch('src.main.KagiJSONParser') as MockJSONParser, \
             patch('src.main.RichTextFormatter') as MockFormatter:

            mock_loader = Mock()
            mock_loader.load.return_value = mock_config
            MockConfigLoader.return_value = mock_loader

            mock_fetcher = Mock()
            mock_fetcher.fetch_feed.return_value = mock_rss_feed
            MockRSSFetcher.return_value = mock_fetcher

            mock_json_fetcher = Mock()
            mock_json_fetcher.fetch_clusters.return_value = {}  # no clusters at all
            MockJSONFetcher.return_value = mock_json_fetcher

            mock_parser = Mock()
            mock_parser.parse_to_story.return_value = sample_story
            MockJSONParser.return_value = mock_parser

            mock_formatter = Mock()
            mock_formatter.format_full.return_value = {"content": "x", "facets": []}
            MockFormatter.return_value = mock_formatter

            import logging
            with caplog.at_level(logging.WARNING):
                Aggregator(
                    config_path=Path("config.yaml"),
                    state_file=state_file,
                    coves_client=mock_client,
                ).run()

            assert mock_parser.parse_to_story.call_count == 0
            assert mock_client.create_post.call_count == 0
            assert any("No matching JSON cluster" in r.message for r in caplog.records)

    def test_title_fallback_when_offset_mismatch(self, mock_config, mock_rss_feed, sample_story, tmp_path, caplog):
        """When the +1 offset misses but a title-scan finds the cluster, parsing proceeds."""
        state_file = tmp_path / "state.json"
        mock_client = Mock()
        mock_client.create_post.return_value = "at://did:plc:test/social.coves.post/abc123"

        # XML entries are at cn=1,2 — expect lookup at 2,3. Instead, place clusters
        # at 99,100 so the offset misses but the titles still match.
        mismatched_clusters = {
            99: {"cluster_number": 99, "title": "Story 1", "short_summary": "match by title 1"},
            100: {"cluster_number": 100, "title": "Story 2", "short_summary": "match by title 2"},
        }

        with patch('src.main.ConfigLoader') as MockConfigLoader, \
             patch('src.main.RSSFetcher') as MockRSSFetcher, \
             patch('src.main.JSONFetcher') as MockJSONFetcher, \
             patch('src.main.KagiJSONParser') as MockJSONParser, \
             patch('src.main.RichTextFormatter') as MockFormatter:

            mock_loader = Mock()
            mock_loader.load.return_value = mock_config
            MockConfigLoader.return_value = mock_loader

            mock_fetcher = Mock()
            mock_fetcher.fetch_feed.return_value = mock_rss_feed
            MockRSSFetcher.return_value = mock_fetcher

            mock_json_fetcher = Mock()
            mock_json_fetcher.fetch_clusters.return_value = mismatched_clusters
            MockJSONFetcher.return_value = mock_json_fetcher

            mock_parser = Mock()
            mock_parser.parse_to_story.return_value = sample_story
            MockJSONParser.return_value = mock_parser

            mock_formatter = Mock()
            mock_formatter.format_full.return_value = {"content": "x", "facets": []}
            MockFormatter.return_value = mock_formatter

            import logging
            with caplog.at_level(logging.WARNING):
                Aggregator(
                    config_path=Path("config.yaml"),
                    state_file=state_file,
                    coves_client=mock_client,
                ).run()

            # Both entries resolved via title fallback, both posted (2 feeds * 2 entries = 4)
            assert mock_client.create_post.call_count == 4
            assert any("offset mismatch" in r.message for r in caplog.records)

    def test_ambiguous_title_returns_none(self, mock_config, sample_story, tmp_path, caplog):
        """Two clusters with the same title -> None, with a distinct 'ambiguous' warning."""
        state_file = tmp_path / "state.json"
        mock_client = Mock()
        mock_client.create_post.return_value = "at://did:plc:test/social.coves.post/abc123"

        # Single-entry feed where the offset target (cn+1) does NOT match by title,
        # but TWO other clusters share the title -> ambiguous.
        feed = MagicMock()
        feed.bozo = 0
        feed.entries = [
            MagicMock(
                title="Story 1",
                link="https://kite.kagi.com/test/world/1",
                guid="https://kite.kagi.com/test/world/1",
                published_parsed=(2024, 1, 15, 12, 0, 0, 0, 15, 0),
                tags=[MagicMock(term="World")],
            )
        ]
        ambiguous_clusters = {
            2: {"cluster_number": 2, "title": "Other Title", "short_summary": "no match"},
            50: {"cluster_number": 50, "title": "Story 1", "short_summary": "first ambiguous"},
            51: {"cluster_number": 51, "title": "Story 1", "short_summary": "second ambiguous"},
        }

        with patch('src.main.ConfigLoader') as MockConfigLoader, \
             patch('src.main.RSSFetcher') as MockRSSFetcher, \
             patch('src.main.JSONFetcher') as MockJSONFetcher, \
             patch('src.main.KagiJSONParser') as MockJSONParser, \
             patch('src.main.RichTextFormatter') as MockFormatter:

            mock_loader = Mock()
            mock_loader.load.return_value = mock_config
            MockConfigLoader.return_value = mock_loader

            mock_fetcher = Mock()
            mock_fetcher.fetch_feed.return_value = feed
            MockRSSFetcher.return_value = mock_fetcher

            mock_json_fetcher = Mock()
            mock_json_fetcher.fetch_clusters.return_value = ambiguous_clusters
            MockJSONFetcher.return_value = mock_json_fetcher

            mock_parser = Mock()
            mock_parser.parse_to_story.return_value = sample_story
            MockJSONParser.return_value = mock_parser

            mock_formatter = Mock()
            mock_formatter.format_full.return_value = {"content": "x", "facets": []}
            MockFormatter.return_value = mock_formatter

            import logging
            with caplog.at_level(logging.WARNING):
                Aggregator(
                    config_path=Path("config.yaml"),
                    state_file=state_file,
                    coves_client=mock_client,
                ).run()

            # Should not have parsed or posted any entries
            assert mock_parser.parse_to_story.call_count == 0
            assert mock_client.create_post.call_count == 0
            # Distinct "ambiguous" wording fired
            assert any("Ambiguous title match" in r.message for r in caplog.records)
            # Should NOT have logged a "no match" warning for this entry
            assert not any(
                "No matching JSON cluster for XML entry cn=1" in r.message
                for r in caplog.records
            )

    def test_offset_title_mismatch_and_no_title_scan_hit(
        self, mock_config, sample_story, tmp_path, caplog
    ):
        """Offset cluster exists but title differs AND title scan finds nothing -> 'no match' warning."""
        state_file = tmp_path / "state.json"
        mock_client = Mock()
        mock_client.create_post.return_value = "at://did:plc:test/social.coves.post/abc123"

        feed = MagicMock()
        feed.bozo = 0
        feed.entries = [
            MagicMock(
                title="Story 1",
                link="https://kite.kagi.com/test/world/1",
                guid="https://kite.kagi.com/test/world/1",
                published_parsed=(2024, 1, 15, 12, 0, 0, 0, 15, 0),
                tags=[MagicMock(term="World")],
            )
        ]
        # Offset target (cn=2) exists but title differs; no other cluster matches title.
        clusters = {
            2: {"cluster_number": 2, "title": "Different Title", "short_summary": "wrong"},
            3: {"cluster_number": 3, "title": "Also Different", "short_summary": "wrong"},
        }

        with patch('src.main.ConfigLoader') as MockConfigLoader, \
             patch('src.main.RSSFetcher') as MockRSSFetcher, \
             patch('src.main.JSONFetcher') as MockJSONFetcher, \
             patch('src.main.KagiJSONParser') as MockJSONParser, \
             patch('src.main.RichTextFormatter') as MockFormatter:

            mock_loader = Mock()
            mock_loader.load.return_value = mock_config
            MockConfigLoader.return_value = mock_loader

            mock_fetcher = Mock()
            mock_fetcher.fetch_feed.return_value = feed
            MockRSSFetcher.return_value = mock_fetcher

            mock_json_fetcher = Mock()
            mock_json_fetcher.fetch_clusters.return_value = clusters
            MockJSONFetcher.return_value = mock_json_fetcher

            mock_parser = Mock()
            mock_parser.parse_to_story.return_value = sample_story
            MockJSONParser.return_value = mock_parser

            mock_formatter = Mock()
            mock_formatter.format_full.return_value = {"content": "x", "facets": []}
            MockFormatter.return_value = mock_formatter

            import logging
            with caplog.at_level(logging.WARNING):
                Aggregator(
                    config_path=Path("config.yaml"),
                    state_file=state_file,
                    coves_client=mock_client,
                ).run()

            assert mock_parser.parse_to_story.call_count == 0
            assert mock_client.create_post.call_count == 0
            # "No matching" path fired, NOT the ambiguous path
            assert any(
                "No matching JSON cluster for XML entry cn=1" in r.message
                for r in caplog.records
            )
            assert not any("Ambiguous title match" in r.message for r in caplog.records)

    def test_unparseable_link_trailing_segment(
        self, mock_config, sample_story, tmp_path, caplog
    ):
        """Non-integer trailing segment in entry.link -> None with a parse-failure warning."""
        state_file = tmp_path / "state.json"
        mock_client = Mock()
        mock_client.create_post.return_value = "at://did:plc:test/social.coves.post/abc123"

        feed = MagicMock()
        feed.bozo = 0
        feed.entries = [
            MagicMock(
                title="Story X",
                link="https://kite.kagi.com/abc/world/notanumber",
                guid="https://kite.kagi.com/abc/world/notanumber",
                published_parsed=(2024, 1, 15, 12, 0, 0, 0, 15, 0),
                tags=[MagicMock(term="World")],
            )
        ]

        with patch('src.main.ConfigLoader') as MockConfigLoader, \
             patch('src.main.RSSFetcher') as MockRSSFetcher, \
             patch('src.main.JSONFetcher') as MockJSONFetcher, \
             patch('src.main.KagiJSONParser') as MockJSONParser, \
             patch('src.main.RichTextFormatter') as MockFormatter:

            mock_loader = Mock()
            mock_loader.load.return_value = mock_config
            MockConfigLoader.return_value = mock_loader

            mock_fetcher = Mock()
            mock_fetcher.fetch_feed.return_value = feed
            MockRSSFetcher.return_value = mock_fetcher

            mock_json_fetcher = Mock()
            mock_json_fetcher.fetch_clusters.return_value = {1: {"cluster_number": 1, "title": "x"}}
            MockJSONFetcher.return_value = mock_json_fetcher

            mock_parser = Mock()
            mock_parser.parse_to_story.return_value = sample_story
            MockJSONParser.return_value = mock_parser

            mock_formatter = Mock()
            mock_formatter.format_full.return_value = {"content": "x", "facets": []}
            MockFormatter.return_value = mock_formatter

            import logging
            with caplog.at_level(logging.WARNING):
                Aggregator(
                    config_path=Path("config.yaml"),
                    state_file=state_file,
                    coves_client=mock_client,
                ).run()

            assert mock_parser.parse_to_story.call_count == 0
            assert mock_client.create_post.call_count == 0
            assert any(
                "Could not parse cluster_number from" in r.message
                for r in caplog.records
            )

    def test_missing_link_attribute(self, mock_config, sample_story, tmp_path, caplog):
        """Entry with link=None -> None with a 'missing or not a string' warning (no AttributeError mask)."""
        state_file = tmp_path / "state.json"
        mock_client = Mock()
        mock_client.create_post.return_value = "at://did:plc:test/social.coves.post/abc123"

        # MagicMock auto-creates .link unless we explicitly set it to None.
        bad_entry = MagicMock(spec=['title', 'guid', 'link', 'published_parsed', 'tags'])
        bad_entry.title = "Story Bad"
        bad_entry.guid = "https://example.com/bad"
        bad_entry.link = None
        bad_entry.published_parsed = (2024, 1, 15, 12, 0, 0, 0, 15, 0)
        bad_entry.tags = [MagicMock(term="World")]

        feed = MagicMock()
        feed.bozo = 0
        feed.entries = [bad_entry]

        with patch('src.main.ConfigLoader') as MockConfigLoader, \
             patch('src.main.RSSFetcher') as MockRSSFetcher, \
             patch('src.main.JSONFetcher') as MockJSONFetcher, \
             patch('src.main.KagiJSONParser') as MockJSONParser, \
             patch('src.main.RichTextFormatter') as MockFormatter:

            mock_loader = Mock()
            mock_loader.load.return_value = mock_config
            MockConfigLoader.return_value = mock_loader

            mock_fetcher = Mock()
            mock_fetcher.fetch_feed.return_value = feed
            MockRSSFetcher.return_value = mock_fetcher

            mock_json_fetcher = Mock()
            mock_json_fetcher.fetch_clusters.return_value = {}
            MockJSONFetcher.return_value = mock_json_fetcher

            mock_parser = Mock()
            mock_parser.parse_to_story.return_value = sample_story
            MockJSONParser.return_value = mock_parser

            mock_formatter = Mock()
            mock_formatter.format_full.return_value = {"content": "x", "facets": []}
            MockFormatter.return_value = mock_formatter

            import logging
            with caplog.at_level(logging.WARNING):
                Aggregator(
                    config_path=Path("config.yaml"),
                    state_file=state_file,
                    coves_client=mock_client,
                ).run()

            assert mock_parser.parse_to_story.call_count == 0
            assert mock_client.create_post.call_count == 0
            assert any(
                "entry.link missing or not a string" in r.message
                for r in caplog.records
            )

    def test_title_normalization_tolerates_case_and_whitespace(
        self, mock_config, sample_story, tmp_path
    ):
        """Trivial casing/whitespace differences between XML and JSON titles still match."""
        state_file = tmp_path / "state.json"
        mock_client = Mock()
        mock_client.create_post.return_value = "at://did:plc:test/social.coves.post/abc123"

        feed = MagicMock()
        feed.bozo = 0
        feed.entries = [
            MagicMock(
                title="Story 1",
                link="https://kite.kagi.com/test/world/1",
                guid="https://kite.kagi.com/test/world/1",
                published_parsed=(2024, 1, 15, 12, 0, 0, 0, 15, 0),
                tags=[MagicMock(term="World")],
            )
        ]
        # Fast-path hit at cn+1=2, but title differs by case and whitespace.
        clusters = {
            2: {"cluster_number": 2, "title": "  story 1  ", "short_summary": "matched via normalization"},
        }

        with patch('src.main.ConfigLoader') as MockConfigLoader, \
             patch('src.main.RSSFetcher') as MockRSSFetcher, \
             patch('src.main.JSONFetcher') as MockJSONFetcher, \
             patch('src.main.KagiJSONParser') as MockJSONParser, \
             patch('src.main.RichTextFormatter') as MockFormatter:

            mock_loader = Mock()
            mock_loader.load.return_value = mock_config
            MockConfigLoader.return_value = mock_loader

            mock_fetcher = Mock()
            mock_fetcher.fetch_feed.return_value = feed
            MockRSSFetcher.return_value = mock_fetcher

            mock_json_fetcher = Mock()
            mock_json_fetcher.fetch_clusters.return_value = clusters
            MockJSONFetcher.return_value = mock_json_fetcher

            mock_parser = Mock()
            mock_parser.parse_to_story.return_value = sample_story
            MockJSONParser.return_value = mock_parser

            mock_formatter = Mock()
            mock_formatter.format_full.return_value = {"content": "x", "facets": []}
            MockFormatter.return_value = mock_formatter

            Aggregator(
                config_path=Path("config.yaml"),
                state_file=state_file,
                coves_client=mock_client,
            ).run()

            # Both feeds in mock_config use this same setup; entry was resolved
            # through normalization, so parse_to_story is called for each feed.
            assert mock_parser.parse_to_story.call_count == 2

    def test_aggregate_zero_resolved_logs_error(
        self, mock_config, mock_rss_feed, tmp_path, caplog
    ):
        """When non-empty feed yields 0 resolved entries, an ERROR is logged (not just WARN)."""
        state_file = tmp_path / "state.json"
        mock_client = Mock()
        mock_client.create_post.return_value = "at://did:plc:test/social.coves.post/abc123"

        with patch('src.main.ConfigLoader') as MockConfigLoader, \
             patch('src.main.RSSFetcher') as MockRSSFetcher, \
             patch('src.main.JSONFetcher') as MockJSONFetcher, \
             patch('src.main.KagiJSONParser') as MockJSONParser, \
             patch('src.main.RichTextFormatter') as MockFormatter:

            mock_loader = Mock()
            mock_loader.load.return_value = mock_config
            MockConfigLoader.return_value = mock_loader

            mock_fetcher = Mock()
            mock_fetcher.fetch_feed.return_value = mock_rss_feed
            MockRSSFetcher.return_value = mock_fetcher

            # Empty cluster map -> every entry fails to resolve
            mock_json_fetcher = Mock()
            mock_json_fetcher.fetch_clusters.return_value = {}
            MockJSONFetcher.return_value = mock_json_fetcher

            MockJSONParser.return_value = Mock()
            MockFormatter.return_value = Mock()

            import logging
            with caplog.at_level(logging.ERROR):
                Aggregator(
                    config_path=Path("config.yaml"),
                    state_file=state_file,
                    coves_client=mock_client,
                ).run()

            # ERROR log emitted naming the feed and entry count
            error_records = [r for r in caplog.records if r.levelno == logging.ERROR]
            assert any(
                "resolved 0 of 2 entries" in r.message and "World News" in r.message
                for r in error_records
            )
            # And no posts went out
            assert mock_client.create_post.call_count == 0

    def test_non_xml_url_raises_value_error(self, mock_config, tmp_path, caplog):
        """A feed URL that doesn't end with .xml triggers ValueError, surfaced in the run log."""
        state_file = tmp_path / "state.json"
        mock_client = Mock()

        bad_config = AggregatorConfig(
            coves_api_url="https://api.coves.social",
            feeds=[
                FeedConfig(
                    name="Bad URL Feed",
                    url="https://news.kagi.com/world",  # no .xml suffix
                    community_handle="bad.coves.social",
                    enabled=True,
                )
            ],
            log_level="info",
            dedup=DedupConfig(semantic_enabled=False),
        )

        with patch('src.main.ConfigLoader') as MockConfigLoader, \
             patch('src.main.RSSFetcher') as MockRSSFetcher, \
             patch('src.main.JSONFetcher') as MockJSONFetcher:

            mock_loader = Mock()
            mock_loader.load.return_value = bad_config
            MockConfigLoader.return_value = mock_loader

            mock_fetcher = Mock()
            mock_fetcher.fetch_feed.return_value = MagicMock(bozo=0, entries=[])
            MockRSSFetcher.return_value = mock_fetcher

            mock_json_fetcher = Mock()
            MockJSONFetcher.return_value = mock_json_fetcher

            import logging
            with caplog.at_level(logging.ERROR):
                # run() catches per-feed exceptions and logs them; should not raise
                Aggregator(
                    config_path=Path("config.yaml"),
                    state_file=state_file,
                    coves_client=mock_client,
                ).run()

            # JSONFetcher must NOT have been called (guarded out before fetch)
            assert mock_json_fetcher.fetch_clusters.call_count == 0
            # Error from the per-feed exception handler in run()
            assert any(
                "Bad URL Feed" in r.message and "must end with '.xml'" in r.message
                for r in caplog.records
            )

    def test_embed_description_strips_citation_markers_before_truncation(
        self, mock_config, mock_rss_feed, mock_json_clusters, tmp_path
    ):
        """The dev embed.description must have `[domain#N]` markers stripped BEFORE
        the 200-char truncation, not after — otherwise the visible description
        leaks raw tokens that the old HTML path pre-rendered as anchor tags."""
        state_file = tmp_path / "state.json"
        mock_client = Mock()
        mock_client.create_post.return_value = "at://did:plc:test/social.coves.post/abc123"
        mock_client.create_external_embed.return_value = {
            "$type": "social.coves.embed.external",
            "external": {},
        }

        # A summary with citation markers within the first 200 characters; the
        # resolved sources back them so they're "real" tokens that should be
        # stripped (the unresolved-token path also strips, but logs a warning —
        # we want to verify the resolved-token path here).
        story_with_markers = KagiStory(
            title="Markers Story",
            link="https://kite.kagi.com/test/world/1",
            guid="https://kite.kagi.com/test/world/1",
            pub_date=datetime(2024, 1, 15, 12, 0, 0),
            categories=["World"],
            summary=(
                "Tensions remained stalled [reuters.com#1][apnews.com#1]. "
                "Officials warned of further disruptions across the region "
                "while diplomats continued negotiations behind closed doors."
            ),
            highlights=[],
            perspectives=[],
            quote=None,
            sources=[
                Source(title="Reuters", url="https://reuters.com/x", domain="reuters.com"),
                Source(title="AP", url="https://apnews.com/y", domain="apnews.com"),
            ],
            image_url=None,
            image_alt=None,
        )

        with patch('src.main.ConfigLoader') as MockConfigLoader, \
             patch('src.main.RSSFetcher') as MockRSSFetcher, \
             patch('src.main.JSONFetcher') as MockJSONFetcher, \
             patch('src.main.KagiJSONParser') as MockJSONParser, \
             patch('src.main.RichTextFormatter') as MockFormatter:

            mock_loader = Mock()
            mock_loader.load.return_value = mock_config
            MockConfigLoader.return_value = mock_loader

            mock_fetcher = Mock()
            single_entry_feed = MagicMock(bozo=0, entries=[mock_rss_feed.entries[0]])
            mock_fetcher.fetch_feed.return_value = single_entry_feed
            MockRSSFetcher.return_value = mock_fetcher

            mock_json_fetcher = Mock()
            mock_json_fetcher.fetch_clusters.return_value = mock_json_clusters
            MockJSONFetcher.return_value = mock_json_fetcher

            mock_parser = Mock()
            mock_parser.parse_to_story.return_value = story_with_markers
            MockJSONParser.return_value = mock_parser

            mock_formatter = Mock()
            mock_formatter.format_full.return_value = {"content": "x", "facets": []}
            MockFormatter.return_value = mock_formatter

            Aggregator(
                config_path=Path("config.yaml"),
                state_file=state_file,
                coves_client=mock_client,
            ).run()

            # The aggregator runs every enabled feed (2 of them here) so
            # create_external_embed is called once per feed-entry. Check the
            # first invocation: description has no `[` from citation markers
            # AND it's the stripped truncation, not the marker-included one.
            assert mock_client.create_external_embed.called
            for call_obj in mock_client.create_external_embed.call_args_list:
                description = call_obj.kwargs["description"]
                assert "[reuters.com#1]" not in description
                assert "[apnews.com#1]" not in description
                # No leaked bracket-with-hash artifacts
                assert "#1]" not in description
                # No orphan space before the period left by stripping
                assert "stalled ." not in description
                assert "stalled." in description

                # And the description is the stripped-then-truncated version
                from src.citations import build_index, strip as strip_citations
                expected_stripped = strip_citations(
                    story_with_markers.summary,
                    build_index(story_with_markers.sources),
                )
                expected = expected_stripped[:200]
                assert description == expected

            # The state snippet that semantic dedup compares against MUST also
            # be the stripped form — otherwise raw `[domain#N]` tokens leak into
            # the dedup prompt and confuse the LLM.
            import json
            with open(state_file) as fp:
                persisted = json.load(fp)
            snippet_values = set()
            for feed_state in persisted.get("feeds", {}).values():
                for posted in feed_state.get("posted_guids", []):
                    snippet = posted.get("summary_snippet", "")
                    snippet_values.add(snippet)
                    assert "[reuters.com#1]" not in snippet
                    assert "[apnews.com#1]" not in snippet
                    assert "#1]" not in snippet
            assert snippet_values, "expected at least one posted entry persisted"

    def test_feed_skipped_when_json_fetch_fails(self, mock_config, sample_story, tmp_path):
        """If JSONFetcher raises for a feed, that feed is skipped but others continue."""
        state_file = tmp_path / "state.json"
        mock_client = Mock()
        mock_client.create_post.return_value = "at://did:plc:test/social.coves.post/abc123"

        with patch('src.main.ConfigLoader') as MockConfigLoader, \
             patch('src.main.RSSFetcher') as MockRSSFetcher, \
             patch('src.main.JSONFetcher') as MockJSONFetcher, \
             patch('src.main.KagiJSONParser') as MockJSONParser, \
             patch('src.main.RichTextFormatter') as MockFormatter:

            mock_loader = Mock()
            mock_loader.load.return_value = mock_config
            MockConfigLoader.return_value = mock_loader

            mock_fetcher = Mock()
            mock_fetcher.fetch_feed.return_value = MagicMock(bozo=0, entries=[])
            MockRSSFetcher.return_value = mock_fetcher

            mock_json_fetcher = Mock()
            # First feed's JSON fetch fails; second feed succeeds
            mock_json_fetcher.fetch_clusters.side_effect = [
                Exception("JSON network error"),
                {},
            ]
            MockJSONFetcher.return_value = mock_json_fetcher

            MockJSONParser.return_value = Mock()
            MockFormatter.return_value = Mock()

            # Should not raise — exceptions are caught in run()
            Aggregator(
                config_path=Path("config.yaml"),
                state_file=state_file,
                coves_client=mock_client,
            ).run()

            # Both feeds attempted XML fetch
            assert mock_fetcher.fetch_feed.call_count == 2
            # Both feeds attempted JSON fetch
            assert mock_json_fetcher.fetch_clusters.call_count == 2
            # No posts (both feeds yielded no entries / failed)
            assert mock_client.create_post.call_count == 0

    def test_link_with_query_and_fragment_resolves_cluster(
        self, mock_config, sample_story, tmp_path
    ):
        """Entry link with `?query` and `#fragment` still parses the trailing cluster_number."""
        state_file = tmp_path / "state.json"
        mock_client = Mock()
        mock_client.create_post.return_value = "at://did:plc:test/social.coves.post/abc123"

        feed = MagicMock()
        feed.bozo = 0
        feed.entries = [
            MagicMock(
                title="Story 1",
                link="https://kite.kagi.com/abc/world/5?utm_source=x#frag",
                guid="https://kite.kagi.com/abc/world/5",
                published_parsed=(2024, 1, 15, 12, 0, 0, 0, 15, 0),
                tags=[MagicMock(term="World")],
            )
        ]
        clusters = {
            6: {"cluster_number": 6, "title": "Story 1", "short_summary": "matched"},
        }

        with patch('src.main.ConfigLoader') as MockConfigLoader, \
             patch('src.main.RSSFetcher') as MockRSSFetcher, \
             patch('src.main.JSONFetcher') as MockJSONFetcher, \
             patch('src.main.KagiJSONParser') as MockJSONParser, \
             patch('src.main.RichTextFormatter') as MockFormatter:

            mock_loader = Mock()
            mock_loader.load.return_value = mock_config
            MockConfigLoader.return_value = mock_loader

            mock_fetcher = Mock()
            mock_fetcher.fetch_feed.return_value = feed
            MockRSSFetcher.return_value = mock_fetcher

            mock_json_fetcher = Mock()
            mock_json_fetcher.fetch_clusters.return_value = clusters
            MockJSONFetcher.return_value = mock_json_fetcher

            mock_parser = Mock()
            mock_parser.parse_to_story.return_value = sample_story
            MockJSONParser.return_value = mock_parser

            mock_formatter = Mock()
            mock_formatter.format_full.return_value = {"content": "x", "facets": []}
            MockFormatter.return_value = mock_formatter

            Aggregator(
                config_path=Path("config.yaml"),
                state_file=state_file,
                coves_client=mock_client,
            ).run()

            # Resolved through fast-path on cluster_number=5+1=6 despite query/fragment.
            # 2 enabled feeds * 1 entry each = 2 parses & posts.
            assert mock_parser.parse_to_story.call_count == 2
            assert mock_client.create_post.call_count == 2

    def test_entry_skipped_when_published_parsed_is_none(
        self, mock_config, sample_story, tmp_path, caplog
    ):
        """An entry with published_parsed=None is skipped with a warning; parser not called."""
        state_file = tmp_path / "state.json"
        mock_client = Mock()
        mock_client.create_post.return_value = "at://did:plc:test/social.coves.post/abc123"

        bad_entry = MagicMock(
            spec=['title', 'link', 'guid', 'published_parsed', 'tags']
        )
        bad_entry.title = "No Date Story"
        bad_entry.link = "https://kite.kagi.com/test/world/1"
        bad_entry.guid = "https://kite.kagi.com/test/world/1"
        bad_entry.published_parsed = None
        bad_entry.tags = [MagicMock(term="World")]

        feed = MagicMock()
        feed.bozo = 0
        feed.entries = [bad_entry]

        clusters = {2: {"cluster_number": 2, "title": "No Date Story", "short_summary": "x"}}

        with patch('src.main.ConfigLoader') as MockConfigLoader, \
             patch('src.main.RSSFetcher') as MockRSSFetcher, \
             patch('src.main.JSONFetcher') as MockJSONFetcher, \
             patch('src.main.KagiJSONParser') as MockJSONParser, \
             patch('src.main.RichTextFormatter') as MockFormatter:

            mock_loader = Mock()
            mock_loader.load.return_value = mock_config
            MockConfigLoader.return_value = mock_loader

            mock_fetcher = Mock()
            mock_fetcher.fetch_feed.return_value = feed
            MockRSSFetcher.return_value = mock_fetcher

            mock_json_fetcher = Mock()
            mock_json_fetcher.fetch_clusters.return_value = clusters
            MockJSONFetcher.return_value = mock_json_fetcher

            mock_parser = Mock()
            mock_parser.parse_to_story.return_value = sample_story
            MockJSONParser.return_value = mock_parser

            mock_formatter = Mock()
            mock_formatter.format_full.return_value = {"content": "x", "facets": []}
            MockFormatter.return_value = mock_formatter

            import logging
            with caplog.at_level(logging.WARNING):
                Aggregator(
                    config_path=Path("config.yaml"),
                    state_file=state_file,
                    coves_client=mock_client,
                ).run()

            assert mock_parser.parse_to_story.call_count == 0
            assert mock_client.create_post.call_count == 0
            assert any("missing published_parsed" in r.message for r in caplog.records)

    def test_authentication_failure_aborts_run(self, mock_config, tmp_path, caplog):
        """If coves_client.authenticate() raises, the run aborts before any feed fetch."""
        state_file = tmp_path / "state.json"
        mock_client = Mock()
        mock_client.authenticate.side_effect = Exception("auth boom")

        with patch('src.main.ConfigLoader') as MockConfigLoader, \
             patch('src.main.RSSFetcher') as MockRSSFetcher, \
             patch('src.main.JSONFetcher') as MockJSONFetcher:

            mock_loader = Mock()
            mock_loader.load.return_value = mock_config
            MockConfigLoader.return_value = mock_loader

            mock_fetcher = Mock()
            MockRSSFetcher.return_value = mock_fetcher

            mock_json_fetcher = Mock()
            MockJSONFetcher.return_value = mock_json_fetcher

            import logging
            with caplog.at_level(logging.ERROR):
                Aggregator(
                    config_path=Path("config.yaml"),
                    state_file=state_file,
                    coves_client=mock_client,
                ).run()

            assert mock_fetcher.fetch_feed.call_count == 0
            assert mock_json_fetcher.fetch_clusters.call_count == 0
            assert mock_client.create_post.call_count == 0
            assert any("Failed to authenticate" in r.message for r in caplog.records)
            assert any(
                "Cannot continue without authentication" in r.message
                for r in caplog.records
            )

    def test_missing_coves_api_key_env_raises(self, mock_config, tmp_path, monkeypatch):
        """No COVES_API_KEY env var and no coves_client => Aggregator() raises ValueError."""
        monkeypatch.delenv("COVES_API_KEY", raising=False)
        state_file = tmp_path / "state.json"

        with patch('src.main.ConfigLoader') as MockConfigLoader:
            mock_loader = Mock()
            mock_loader.load.return_value = mock_config
            MockConfigLoader.return_value = mock_loader

            with pytest.raises(ValueError, match="COVES_API_KEY"):
                Aggregator(
                    config_path=Path("config.yaml"),
                    state_file=state_file,
                )

    def test_full_flow_with_real_parser_and_formatter(self, tmp_path):
        """End-to-end with real KagiJSONParser + real RichTextFormatter + real fixture.

        This guards against silent-citation-corruption and JSON type-drift bugs
        that only surface against real-shaped data, not bare MagicMock entries.
        """
        import json as _json

        fixture_path = Path(__file__).parent / "fixtures" / "sample_kagi_cluster.json"
        with open(fixture_path) as fp:
            real_cluster = _json.load(fp)

        # XML.cluster_number = JSON.cluster_number - 1 (the +1 fast-path).
        xml_cn = int(real_cluster["cluster_number"]) - 1
        entry_link = f"https://kite.kagi.com/abc/tech/{xml_cn}"
        entry_title = real_cluster["title"]
        entry_guid = entry_link

        config = AggregatorConfig(
            coves_api_url="https://api.coves.social",
            feeds=[
                FeedConfig(
                    name="Tech News",
                    url="https://news.kagi.com/tech.xml",
                    community_handle="tech.coves.social",
                    enabled=True,
                )
            ],
            log_level="info",
            dedup=DedupConfig(semantic_enabled=False),
        )

        # spec= guards against silent attribute fabrication on MagicMock.
        entry = MagicMock(spec=['title', 'link', 'guid', 'published_parsed', 'tags', 'description'])
        entry.title = entry_title
        entry.link = entry_link
        entry.guid = entry_guid
        entry.published_parsed = (2024, 1, 15, 12, 0, 0, 0, 15, 0)
        category_tag = MagicMock(spec=['term'])
        category_tag.term = "Technology"
        entry.tags = [category_tag]
        entry.description = "<p>real-shaped description</p>"

        feed = MagicMock()
        feed.bozo = 0
        feed.entries = [entry]

        clusters = {real_cluster["cluster_number"]: real_cluster}

        mock_client = Mock()
        mock_client.create_post.return_value = "at://did:plc:test/social.coves.post/abc"
        # Use the REAL create_external_embed shape via a real-ish stub:
        def _real_embed(uri, title, description, sources=None):
            external = {"uri": uri, "title": title, "description": description}
            if sources:
                external["sources"] = sources
            return {"$type": "social.coves.embed.external", "external": external}
        mock_client.create_external_embed.side_effect = _real_embed

        state_file = tmp_path / "state.json"

        with patch('src.main.ConfigLoader') as MockConfigLoader, \
             patch('src.main.RSSFetcher') as MockRSSFetcher, \
             patch('src.main.JSONFetcher') as MockJSONFetcher:

            mock_loader = Mock()
            mock_loader.load.return_value = config
            MockConfigLoader.return_value = mock_loader

            mock_fetcher = Mock()
            mock_fetcher.fetch_feed.return_value = feed
            MockRSSFetcher.return_value = mock_fetcher

            mock_json_fetcher = Mock()
            mock_json_fetcher.fetch_clusters.return_value = clusters
            MockJSONFetcher.return_value = mock_json_fetcher

            # NB: KagiJSONParser and RichTextFormatter are NOT patched.
            Aggregator(
                config_path=Path("config.yaml"),
                state_file=state_file,
                coves_client=mock_client,
            ).run()

        # One post for the one entry
        assert mock_client.create_post.call_count == 1
        post_kwargs = mock_client.create_post.call_args.kwargs
        assert post_kwargs["content"], "rich-text content must be non-empty"
        assert isinstance(post_kwargs["facets"], list)
        assert len(post_kwargs["facets"]) > 0, "real fixture should yield facets"

        # External embed description: contains real summary substring AND has
        # no leaked `[domain#N]` markers.
        mock_client.create_external_embed.assert_called_once()
        embed_kwargs = mock_client.create_external_embed.call_args.kwargs
        description = embed_kwargs["description"]
        assert "iOS 26.5" in description
        # Marker presence detection: any `[...#` would indicate a leak.
        import re as _re
        assert _re.search(r'\[[^\[\]]+#\d+\]', description) is None, (
            f"raw citation marker leaked into embed.description: {description!r}"
        )
