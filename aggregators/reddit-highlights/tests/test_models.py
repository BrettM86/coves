"""
Tests for models module.
"""
import pytest
from datetime import datetime

from src.models import RedditPost, SubredditConfig, AggregatorConfig, LogLevel


class TestLogLevel:
    """Tests for LogLevel enum."""

    def test_all_values(self):
        """Test all log level values."""
        assert LogLevel.DEBUG.value == "debug"
        assert LogLevel.INFO.value == "info"
        assert LogLevel.WARNING.value == "warning"
        assert LogLevel.ERROR.value == "error"
        assert LogLevel.CRITICAL.value == "critical"

    def test_from_string(self):
        """Test creating LogLevel from string."""
        assert LogLevel("debug") == LogLevel.DEBUG
        assert LogLevel("info") == LogLevel.INFO
        assert LogLevel("warning") == LogLevel.WARNING

    def test_invalid_value_raises(self):
        """Test that invalid value raises ValueError."""
        with pytest.raises(ValueError):
            LogLevel("invalid")


class TestRedditPost:
    """Tests for RedditPost dataclass."""

    def test_valid_post(self):
        """Test creating a valid RedditPost."""
        post = RedditPost(
            id="t3_abc123",
            title="Test Post",
            link="https://streamable.com/xyz",
            reddit_url="https://reddit.com/r/nba/comments/abc123",
            subreddit="nba",
            author="testuser",
        )
        assert post.id == "t3_abc123"
        assert post.title == "Test Post"
        assert post.subreddit == "nba"

    def test_optional_fields(self):
        """Test optional fields have correct defaults."""
        post = RedditPost(
            id="t3_abc123",
            title="Test Post",
            link="https://example.com",
            reddit_url="https://reddit.com/r/nba",
            subreddit="nba",
            author="testuser",
        )
        assert post.published is None
        assert post.streamable_url is None

    def test_with_optional_fields(self):
        """Test creating post with optional fields."""
        now = datetime.now()
        post = RedditPost(
            id="t3_abc123",
            title="Test Post",
            link="https://example.com",
            reddit_url="https://reddit.com/r/nba",
            subreddit="nba",
            author="testuser",
            published=now,
            streamable_url="https://streamable.com/xyz",
        )
        assert post.published == now
        assert post.streamable_url == "https://streamable.com/xyz"

    def test_empty_id_raises(self):
        """Test that empty id raises ValueError."""
        with pytest.raises(ValueError, match="id cannot be empty"):
            RedditPost(
                id="",
                title="Test",
                link="https://example.com",
                reddit_url="https://reddit.com",
                subreddit="nba",
                author="test",
            )

    def test_empty_title_raises(self):
        """Test that empty title raises ValueError."""
        with pytest.raises(ValueError, match="title cannot be empty"):
            RedditPost(
                id="t3_abc",
                title="",
                link="https://example.com",
                reddit_url="https://reddit.com",
                subreddit="nba",
                author="test",
            )

    def test_empty_subreddit_raises(self):
        """Test that empty subreddit raises ValueError."""
        with pytest.raises(ValueError, match="subreddit cannot be empty"):
            RedditPost(
                id="t3_abc",
                title="Test",
                link="https://example.com",
                reddit_url="https://reddit.com",
                subreddit="",
                author="test",
            )


class TestSubredditConfig:
    """Tests for SubredditConfig dataclass."""

    def test_valid_config(self):
        """Test creating valid SubredditConfig."""
        config = SubredditConfig(
            name="nba",
            community_handle="nba.coves.social",
        )
        assert config.name == "nba"
        assert config.community_handle == "nba.coves.social"
        assert config.enabled is True  # Default

    def test_disabled_config(self):
        """Test creating disabled SubredditConfig."""
        config = SubredditConfig(
            name="nba",
            community_handle="nba.coves.social",
            enabled=False,
        )
        assert config.enabled is False

    def test_empty_name_raises(self):
        """Test that empty name raises ValueError."""
        with pytest.raises(ValueError, match="name cannot be empty"):
            SubredditConfig(
                name="",
                community_handle="nba.coves.social",
            )

    def test_whitespace_name_raises(self):
        """Test that whitespace-only name raises ValueError."""
        with pytest.raises(ValueError, match="name cannot be empty"):
            SubredditConfig(
                name="   ",
                community_handle="nba.coves.social",
            )

    def test_empty_community_handle_raises(self):
        """Test that empty community_handle raises ValueError."""
        with pytest.raises(ValueError, match="community_handle cannot be empty"):
            SubredditConfig(
                name="nba",
                community_handle="",
            )

    def test_invalid_subreddit_name_format_raises(self):
        """Test that invalid subreddit name format raises ValueError."""
        with pytest.raises(ValueError, match="Invalid subreddit name format"):
            SubredditConfig(
                name="nba/../../../etc/passwd",
                community_handle="nba.coves.social",
            )

    def test_special_chars_in_name_raises(self):
        """Test that special characters in name raise ValueError."""
        with pytest.raises(ValueError, match="Invalid subreddit name format"):
            SubredditConfig(
                name="nba<script>",
                community_handle="nba.coves.social",
            )

    def test_valid_name_with_underscore(self):
        """Test that underscores in name are allowed."""
        config = SubredditConfig(
            name="nba_discussion",
            community_handle="nba.coves.social",
        )
        assert config.name == "nba_discussion"

    def test_valid_name_with_hyphen(self):
        """Test that hyphens in name are allowed."""
        config = SubredditConfig(
            name="nba-highlights",
            community_handle="nba.coves.social",
        )
        assert config.name == "nba-highlights"

    def test_is_frozen(self):
        """Test that SubredditConfig is immutable."""
        config = SubredditConfig(
            name="nba",
            community_handle="nba.coves.social",
        )
        with pytest.raises(AttributeError):
            config.name = "soccer"


class TestAggregatorConfig:
    """Tests for AggregatorConfig dataclass."""

    def test_valid_config(self):
        """Test creating valid AggregatorConfig."""
        subreddit = SubredditConfig(name="nba", community_handle="nba.coves.social")
        config = AggregatorConfig(
            coves_api_url="https://coves.social",
            subreddits=(subreddit,),
        )
        assert config.coves_api_url == "https://coves.social"
        assert len(config.subreddits) == 1
        assert config.log_level == LogLevel.INFO  # Default

    def test_custom_log_level(self):
        """Test custom log level."""
        subreddit = SubredditConfig(name="nba", community_handle="nba.coves.social")
        config = AggregatorConfig(
            coves_api_url="https://coves.social",
            subreddits=(subreddit,),
            log_level=LogLevel.DEBUG,
        )
        assert config.log_level == LogLevel.DEBUG

    def test_custom_allowed_domains(self):
        """Test custom allowed domains."""
        subreddit = SubredditConfig(name="nba", community_handle="nba.coves.social")
        config = AggregatorConfig(
            coves_api_url="https://coves.social",
            subreddits=(subreddit,),
            allowed_domains=("streamable.com", "gfycat.com"),
        )
        assert "streamable.com" in config.allowed_domains
        assert "gfycat.com" in config.allowed_domains

    def test_empty_coves_api_url_raises(self):
        """Test that empty coves_api_url raises ValueError."""
        subreddit = SubredditConfig(name="nba", community_handle="nba.coves.social")
        with pytest.raises(ValueError, match="coves_api_url cannot be empty"):
            AggregatorConfig(
                coves_api_url="",
                subreddits=(subreddit,),
            )

    def test_empty_subreddits_raises(self):
        """Test that empty subreddits raises ValueError."""
        with pytest.raises(ValueError, match="subreddits cannot be empty"):
            AggregatorConfig(
                coves_api_url="https://coves.social",
                subreddits=(),
            )

    def test_is_frozen(self):
        """Test that AggregatorConfig is immutable."""
        subreddit = SubredditConfig(name="nba", community_handle="nba.coves.social")
        config = AggregatorConfig(
            coves_api_url="https://coves.social",
            subreddits=(subreddit,),
        )
        with pytest.raises(AttributeError):
            config.coves_api_url = "https://other.com"

    def test_default_allowed_domains(self):
        """Test default allowed domains."""
        subreddit = SubredditConfig(name="nba", community_handle="nba.coves.social")
        config = AggregatorConfig(
            coves_api_url="https://coves.social",
            subreddits=(subreddit,),
        )
        assert "streamable.com" in config.allowed_domains
