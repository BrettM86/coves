"""
Tests for config module.
"""
import os
import pytest
from pathlib import Path

from src.config import ConfigLoader, ConfigError
from src.models import LogLevel


class TestConfigLoaderInit:
    """Tests for ConfigLoader initialization."""

    def test_stores_config_path(self, tmp_path):
        """Test that config path is stored."""
        config_path = tmp_path / "config.yaml"
        loader = ConfigLoader(config_path)
        assert loader.config_path == config_path


class TestConfigLoaderLoad:
    """Tests for ConfigLoader.load method."""

    def test_raises_if_file_not_found(self, tmp_path):
        """Test that ConfigError is raised if file doesn't exist."""
        config_path = tmp_path / "nonexistent.yaml"
        loader = ConfigLoader(config_path)

        with pytest.raises(ConfigError, match="not found"):
            loader.load()

    def test_raises_on_invalid_yaml(self, tmp_path):
        """Test that ConfigError is raised for invalid YAML."""
        config_path = tmp_path / "config.yaml"
        config_path.write_text("invalid: yaml: ::::")
        loader = ConfigLoader(config_path)

        with pytest.raises(ConfigError, match="Failed to parse YAML"):
            loader.load()

    def test_raises_on_empty_file(self, tmp_path):
        """Test that ConfigError is raised for empty file."""
        config_path = tmp_path / "config.yaml"
        config_path.write_text("")
        loader = ConfigLoader(config_path)

        with pytest.raises(ConfigError, match="empty"):
            loader.load()

    def test_raises_if_coves_api_url_missing(self, tmp_path):
        """Test that ConfigError is raised if coves_api_url is missing."""
        config_path = tmp_path / "config.yaml"
        config_path.write_text("""
subreddits:
  - name: nba
    community_handle: nba.coves.social
""")
        loader = ConfigLoader(config_path)

        with pytest.raises(ConfigError, match="coves_api_url"):
            loader.load()

    def test_raises_on_invalid_url(self, tmp_path):
        """Test that ConfigError is raised for invalid URL."""
        config_path = tmp_path / "config.yaml"
        config_path.write_text("""
coves_api_url: "not-a-valid-url"
subreddits:
  - name: nba
    community_handle: nba.coves.social
""")
        loader = ConfigLoader(config_path)

        with pytest.raises(ConfigError, match="Invalid URL"):
            loader.load()

    def test_raises_on_file_url_scheme(self, tmp_path):
        """Test that ConfigError is raised for file:// URL scheme."""
        config_path = tmp_path / "config.yaml"
        config_path.write_text("""
coves_api_url: "file:///etc/passwd"
subreddits:
  - name: nba
    community_handle: nba.coves.social
""")
        loader = ConfigLoader(config_path)

        with pytest.raises(ConfigError, match="Invalid URL"):
            loader.load()

    def test_raises_if_no_subreddits(self, tmp_path):
        """Test that ConfigError is raised if no subreddits defined."""
        config_path = tmp_path / "config.yaml"
        config_path.write_text("""
coves_api_url: "https://coves.social"
subreddits: []
""")
        loader = ConfigLoader(config_path)

        with pytest.raises(ConfigError, match="at least one subreddit"):
            loader.load()

    def test_loads_valid_config(self, tmp_path):
        """Test successful loading of valid config."""
        config_path = tmp_path / "config.yaml"
        config_path.write_text("""
coves_api_url: "https://coves.social"
subreddits:
  - name: nba
    community_handle: nba.coves.social
    enabled: true
  - name: soccer
    community_handle: soccer.coves.social
    enabled: false
allowed_domains:
  - streamable.com
  - gfycat.com
log_level: debug
""")
        loader = ConfigLoader(config_path)
        config = loader.load()

        assert config.coves_api_url == "https://coves.social"
        assert len(config.subreddits) == 2
        assert config.subreddits[0].name == "nba"
        assert config.subreddits[0].community_handle == "nba.coves.social"
        assert config.subreddits[0].enabled is True
        assert config.subreddits[1].name == "soccer"
        assert config.subreddits[1].enabled is False
        assert "streamable.com" in config.allowed_domains
        assert "gfycat.com" in config.allowed_domains
        assert config.log_level == LogLevel.DEBUG

    def test_uses_default_allowed_domains(self, tmp_path):
        """Test that default allowed_domains is used when not specified."""
        config_path = tmp_path / "config.yaml"
        config_path.write_text("""
coves_api_url: "https://coves.social"
subreddits:
  - name: nba
    community_handle: nba.coves.social
""")
        loader = ConfigLoader(config_path)
        config = loader.load()

        assert "streamable.com" in config.allowed_domains

    def test_uses_default_log_level(self, tmp_path):
        """Test that default log_level is used when not specified."""
        config_path = tmp_path / "config.yaml"
        config_path.write_text("""
coves_api_url: "https://coves.social"
subreddits:
  - name: nba
    community_handle: nba.coves.social
""")
        loader = ConfigLoader(config_path)
        config = loader.load()

        assert config.log_level == LogLevel.INFO

    def test_invalid_log_level_raises(self, tmp_path):
        """Test that invalid log_level raises ConfigError."""
        config_path = tmp_path / "config.yaml"
        config_path.write_text("""
coves_api_url: "https://coves.social"
subreddits:
  - name: nba
    community_handle: nba.coves.social
log_level: invalid_level
""")
        loader = ConfigLoader(config_path)

        with pytest.raises(ConfigError, match="Invalid log_level"):
            loader.load()

    def test_environment_variable_override(self, tmp_path, monkeypatch):
        """Test that COVES_API_URL env var overrides config file."""
        config_path = tmp_path / "config.yaml"
        config_path.write_text("""
coves_api_url: "https://coves.social"
subreddits:
  - name: nba
    community_handle: nba.coves.social
""")
        monkeypatch.setenv("COVES_API_URL", "https://custom.coves.social")
        loader = ConfigLoader(config_path)
        config = loader.load()

        assert config.coves_api_url == "https://custom.coves.social"


class TestSubredditParsing:
    """Tests for subreddit configuration parsing."""

    def test_missing_name_raises(self, tmp_path):
        """Test that missing subreddit name raises ConfigError."""
        config_path = tmp_path / "config.yaml"
        config_path.write_text("""
coves_api_url: "https://coves.social"
subreddits:
  - community_handle: nba.coves.social
""")
        loader = ConfigLoader(config_path)

        with pytest.raises(ConfigError, match="name"):
            loader.load()

    def test_missing_community_handle_raises(self, tmp_path):
        """Test that missing community_handle raises ConfigError."""
        config_path = tmp_path / "config.yaml"
        config_path.write_text("""
coves_api_url: "https://coves.social"
subreddits:
  - name: nba
""")
        loader = ConfigLoader(config_path)

        with pytest.raises(ConfigError, match="community_handle"):
            loader.load()

    def test_empty_name_raises(self, tmp_path):
        """Test that empty subreddit name raises ConfigError."""
        config_path = tmp_path / "config.yaml"
        config_path.write_text("""
coves_api_url: "https://coves.social"
subreddits:
  - name: ""
    community_handle: nba.coves.social
""")
        loader = ConfigLoader(config_path)

        with pytest.raises(ConfigError, match="cannot be empty"):
            loader.load()

    def test_empty_community_handle_raises(self, tmp_path):
        """Test that empty community_handle raises ConfigError."""
        config_path = tmp_path / "config.yaml"
        config_path.write_text("""
coves_api_url: "https://coves.social"
subreddits:
  - name: nba
    community_handle: ""
""")
        loader = ConfigLoader(config_path)

        with pytest.raises(ConfigError, match="cannot be empty"):
            loader.load()

    def test_defaults_enabled_to_true(self, tmp_path):
        """Test that enabled defaults to True."""
        config_path = tmp_path / "config.yaml"
        config_path.write_text("""
coves_api_url: "https://coves.social"
subreddits:
  - name: nba
    community_handle: nba.coves.social
""")
        loader = ConfigLoader(config_path)
        config = loader.load()

        assert config.subreddits[0].enabled is True

    def test_normalizes_name_to_lowercase(self, tmp_path):
        """Test that subreddit name is normalized to lowercase."""
        config_path = tmp_path / "config.yaml"
        config_path.write_text("""
coves_api_url: "https://coves.social"
subreddits:
  - name: NBA
    community_handle: nba.coves.social
""")
        loader = ConfigLoader(config_path)
        config = loader.load()

        assert config.subreddits[0].name == "nba"

    def test_strips_whitespace(self, tmp_path):
        """Test that whitespace is stripped from names."""
        config_path = tmp_path / "config.yaml"
        config_path.write_text("""
coves_api_url: "https://coves.social"
subreddits:
  - name: "  nba  "
    community_handle: "  nba.coves.social  "
""")
        loader = ConfigLoader(config_path)
        config = loader.load()

        assert config.subreddits[0].name == "nba"
        assert config.subreddits[0].community_handle == "nba.coves.social"


class TestUrlValidation:
    """Tests for URL validation."""

    def test_accepts_https_url(self, tmp_path):
        """Test that HTTPS URLs are accepted."""
        config_path = tmp_path / "config.yaml"
        config_path.write_text("""
coves_api_url: "https://coves.social"
subreddits:
  - name: nba
    community_handle: nba.coves.social
""")
        loader = ConfigLoader(config_path)
        config = loader.load()

        assert config.coves_api_url == "https://coves.social"

    def test_accepts_http_url(self, tmp_path):
        """Test that HTTP URLs are accepted (for local dev)."""
        config_path = tmp_path / "config.yaml"
        config_path.write_text("""
coves_api_url: "http://localhost:8080"
subreddits:
  - name: nba
    community_handle: nba.coves.social
""")
        loader = ConfigLoader(config_path)
        config = loader.load()

        assert config.coves_api_url == "http://localhost:8080"

    def test_rejects_javascript_url(self, tmp_path):
        """Test that javascript: URLs are rejected."""
        config_path = tmp_path / "config.yaml"
        config_path.write_text("""
coves_api_url: "javascript:alert(1)"
subreddits:
  - name: nba
    community_handle: nba.coves.social
""")
        loader = ConfigLoader(config_path)

        with pytest.raises(ConfigError, match="Invalid URL"):
            loader.load()

    def test_rejects_data_url(self, tmp_path):
        """Test that data: URLs are rejected."""
        config_path = tmp_path / "config.yaml"
        config_path.write_text("""
coves_api_url: "data:text/html,<script>alert(1)</script>"
subreddits:
  - name: nba
    community_handle: nba.coves.social
""")
        loader = ConfigLoader(config_path)

        with pytest.raises(ConfigError, match="Invalid URL"):
            loader.load()
