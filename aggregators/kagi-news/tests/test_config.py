"""
Tests for Configuration Loader.

Tests loading and validating aggregator configuration.
"""
import pytest
import tempfile
from pathlib import Path

from src.config import ConfigLoader, ConfigError
from src.models import AggregatorConfig, FeedConfig


@pytest.fixture
def valid_config_yaml():
    """Valid configuration YAML."""
    return """
coves_api_url: "https://api.coves.social"

feeds:
  - name: "World News"
    url: "https://news.kagi.com/world.xml"
    community_handle: "world-news.coves.social"
    enabled: true

  - name: "Tech News"
    url: "https://news.kagi.com/tech.xml"
    community_handle: "tech.coves.social"
    enabled: true

  - name: "Science News"
    url: "https://news.kagi.com/science.xml"
    community_handle: "science.coves.social"
    enabled: false

log_level: "info"
"""


@pytest.fixture
def temp_config_file(valid_config_yaml):
    """Create a temporary config file."""
    with tempfile.NamedTemporaryFile(mode='w', delete=False, suffix='.yaml') as f:
        f.write(valid_config_yaml)
        temp_path = Path(f.name)
    yield temp_path
    # Cleanup
    if temp_path.exists():
        temp_path.unlink()


class TestConfigLoader:
    """Test suite for ConfigLoader."""

    def test_load_valid_config(self, temp_config_file):
        """Test loading valid configuration."""
        loader = ConfigLoader(temp_config_file)
        config = loader.load()

        assert isinstance(config, AggregatorConfig)
        assert config.coves_api_url == "https://api.coves.social"
        assert config.log_level == "info"
        assert len(config.feeds) == 3

    def test_parse_feed_configs(self, temp_config_file):
        """Test parsing feed configurations."""
        loader = ConfigLoader(temp_config_file)
        config = loader.load()

        # Check first feed
        feed1 = config.feeds[0]
        assert isinstance(feed1, FeedConfig)
        assert feed1.name == "World News"
        assert feed1.url == "https://news.kagi.com/world.xml"
        assert feed1.community_handle == "world-news.coves.social"
        assert feed1.enabled is True

        # Check disabled feed
        feed3 = config.feeds[2]
        assert feed3.name == "Science News"
        assert feed3.enabled is False

    def test_get_enabled_feeds_only(self, temp_config_file):
        """Test getting only enabled feeds."""
        loader = ConfigLoader(temp_config_file)
        config = loader.load()

        enabled_feeds = [f for f in config.feeds if f.enabled]
        assert len(enabled_feeds) == 2
        assert all(f.enabled for f in enabled_feeds)

    def test_missing_config_file_raises_error(self):
        """Test that missing config file raises error."""
        with pytest.raises(ConfigError, match="not found"):
            loader = ConfigLoader(Path("nonexistent.yaml"))
            loader.load()

    def test_invalid_yaml_raises_error(self):
        """Test that invalid YAML raises error."""
        with tempfile.NamedTemporaryFile(mode='w', delete=False, suffix='.yaml') as f:
            f.write("invalid: yaml: content: [[[")
            temp_path = Path(f.name)

        try:
            with pytest.raises(ConfigError, match="Failed to parse"):
                loader = ConfigLoader(temp_path)
                loader.load()
        finally:
            temp_path.unlink()

    def test_missing_required_field_raises_error(self):
        """Test that missing required fields raise error."""
        invalid_yaml = """
feeds:
  - name: "Test"
    url: "https://test.xml"
    # Missing community_handle!
"""
        with tempfile.NamedTemporaryFile(mode='w', delete=False, suffix='.yaml') as f:
            f.write(invalid_yaml)
            temp_path = Path(f.name)

        try:
            with pytest.raises(ConfigError, match="Missing required field"):
                loader = ConfigLoader(temp_path)
                loader.load()
        finally:
            temp_path.unlink()

    def test_missing_coves_api_url_raises_error(self):
        """Test that missing coves_api_url raises error."""
        invalid_yaml = """
feeds:
  - name: "Test"
    url: "https://test.xml"
    community_handle: "test.coves.social"
"""
        with tempfile.NamedTemporaryFile(mode='w', delete=False, suffix='.yaml') as f:
            f.write(invalid_yaml)
            temp_path = Path(f.name)

        try:
            with pytest.raises(ConfigError, match="coves_api_url"):
                loader = ConfigLoader(temp_path)
                loader.load()
        finally:
            temp_path.unlink()

    def test_default_log_level(self):
        """Test that log_level defaults to 'info' if not specified."""
        minimal_yaml = """
coves_api_url: "https://api.coves.social"
feeds:
  - name: "Test"
    url: "https://test.xml"
    community_handle: "test.coves.social"
"""
        with tempfile.NamedTemporaryFile(mode='w', delete=False, suffix='.yaml') as f:
            f.write(minimal_yaml)
            temp_path = Path(f.name)

        try:
            loader = ConfigLoader(temp_path)
            config = loader.load()
            assert config.log_level == "info"
        finally:
            temp_path.unlink()

    def test_default_enabled_true(self):
        """Test that feed enabled defaults to True if not specified."""
        yaml_content = """
coves_api_url: "https://api.coves.social"
feeds:
  - name: "Test"
    url: "https://test.xml"
    community_handle: "test.coves.social"
    # No 'enabled' field
"""
        with tempfile.NamedTemporaryFile(mode='w', delete=False, suffix='.yaml') as f:
            f.write(yaml_content)
            temp_path = Path(f.name)

        try:
            loader = ConfigLoader(temp_path)
            config = loader.load()
            assert config.feeds[0].enabled is True
        finally:
            temp_path.unlink()

    def test_invalid_url_format_raises_error(self):
        """Test that invalid URLs raise error."""
        invalid_yaml = """
coves_api_url: "https://api.coves.social"
feeds:
  - name: "Test"
    url: "not-a-valid-url"
    community_handle: "test.coves.social"
"""
        with tempfile.NamedTemporaryFile(mode='w', delete=False, suffix='.yaml') as f:
            f.write(invalid_yaml)
            temp_path = Path(f.name)

        try:
            with pytest.raises(ConfigError, match="Invalid URL"):
                loader = ConfigLoader(temp_path)
                loader.load()
        finally:
            temp_path.unlink()

    def test_empty_feeds_list_raises_error(self):
        """Test that empty feeds list raises error."""
        invalid_yaml = """
coves_api_url: "https://api.coves.social"
feeds: []
"""
        with tempfile.NamedTemporaryFile(mode='w', delete=False, suffix='.yaml') as f:
            f.write(invalid_yaml)
            temp_path = Path(f.name)

        try:
            with pytest.raises(ConfigError, match="at least one feed"):
                loader = ConfigLoader(temp_path)
                loader.load()
        finally:
            temp_path.unlink()

    def test_load_from_env_override(self, temp_config_file, monkeypatch):
        """Test that environment variables can override config values."""
        # Set environment variable
        monkeypatch.setenv("COVES_API_URL", "https://test.coves.social")

        loader = ConfigLoader(temp_config_file)
        config = loader.load()

        # Should use env var instead of config file
        assert config.coves_api_url == "https://test.coves.social"

    def test_get_feed_by_url(self, temp_config_file):
        """Test helper to get feed config by URL."""
        loader = ConfigLoader(temp_config_file)
        config = loader.load()

        feed = next((f for f in config.feeds if f.url == "https://news.kagi.com/tech.xml"), None)
        assert feed is not None
        assert feed.name == "Tech News"
        assert feed.community_handle == "tech.coves.social"
