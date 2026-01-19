"""
Configuration Loader for Reddit Highlights Aggregator.

Loads and validates configuration from YAML files.
"""
import os
import logging
from pathlib import Path
from typing import Dict, Any, List
import yaml
from urllib.parse import urlparse

from src.models import AggregatorConfig, SubredditConfig, LogLevel

logger = logging.getLogger(__name__)


class ConfigError(Exception):
    """Configuration error."""

    pass


class ConfigLoader:
    """
    Loads and validates aggregator configuration.

    Supports:
    - Loading from YAML file
    - Environment variable overrides
    - Validation of required fields
    """

    def __init__(self, config_path: Path):
        """
        Initialize config loader.

        Args:
            config_path: Path to config.yaml file
        """
        self.config_path = Path(config_path)

    def load(self) -> AggregatorConfig:
        """
        Load and validate configuration.

        Returns:
            AggregatorConfig object

        Raises:
            ConfigError: If config is invalid or missing
        """
        if not self.config_path.exists():
            raise ConfigError(f"Configuration file not found: {self.config_path}")

        try:
            with open(self.config_path, "r") as f:
                config_data = yaml.safe_load(f)
        except yaml.YAMLError as e:
            raise ConfigError(f"Failed to parse YAML: {e}")

        if not config_data:
            raise ConfigError("Configuration file is empty")

        try:
            return self._parse_config(config_data)
        except ConfigError:
            raise
        except Exception as e:
            raise ConfigError(f"Invalid configuration: {e}")

    def _parse_config(self, data: Dict[str, Any]) -> AggregatorConfig:
        """
        Parse and validate configuration data.

        Args:
            data: Parsed YAML data

        Returns:
            AggregatorConfig object

        Raises:
            ConfigError: If validation fails
        """
        coves_api_url = os.getenv("COVES_API_URL", data.get("coves_api_url"))
        if not coves_api_url:
            raise ConfigError("Missing required field: coves_api_url")

        if not self._is_valid_url(coves_api_url):
            raise ConfigError(f"Invalid URL for coves_api_url: {coves_api_url}")

        # Parse log level with validation
        log_level_str = data.get("log_level", "info").lower()
        try:
            log_level = LogLevel(log_level_str)
        except ValueError:
            valid_levels = [level.value for level in LogLevel]
            raise ConfigError(f"Invalid log_level '{log_level_str}'. Valid values: {valid_levels}")

        subreddits_data = data.get("subreddits", [])
        if not subreddits_data:
            raise ConfigError("Configuration must include at least one subreddit")

        subreddits = []
        for sub_data in subreddits_data:
            subreddit = self._parse_subreddit(sub_data)
            subreddits.append(subreddit)

        allowed_domains = tuple(data.get("allowed_domains", ["streamable.com"]))

        max_posts_per_run = data.get("max_posts_per_run", 3)
        if type(max_posts_per_run) is not int or max_posts_per_run < 1:
            raise ConfigError(f"max_posts_per_run must be a positive integer, got: {max_posts_per_run}")

        enabled_count = sum(1 for s in subreddits if s.enabled)
        logger.info(
            f"Loaded configuration with {len(subreddits)} subreddits ({enabled_count} enabled), "
            f"max {max_posts_per_run} posts per run"
        )

        return AggregatorConfig(
            coves_api_url=coves_api_url,
            subreddits=tuple(subreddits),  # Convert to tuple for immutability
            allowed_domains=allowed_domains,
            log_level=log_level,
            max_posts_per_run=max_posts_per_run,
        )

    def _parse_subreddit(self, data: Dict[str, Any]) -> SubredditConfig:
        """
        Parse and validate a single subreddit configuration.

        Args:
            data: Subreddit configuration data

        Returns:
            SubredditConfig object

        Raises:
            ConfigError: If validation fails
        """
        required_fields = ["name", "community_handle"]
        for field in required_fields:
            if field not in data:
                raise ConfigError(
                    f"Missing required field in subreddit config: {field}"
                )

        name = data["name"]
        community_handle = data["community_handle"]
        enabled = data.get("enabled", True)

        if not name or not name.strip():
            raise ConfigError("Subreddit name cannot be empty")

        if not community_handle or not community_handle.strip():
            raise ConfigError(f"Community handle cannot be empty for subreddit '{name}'")

        return SubredditConfig(
            name=name.strip().lower(),
            community_handle=community_handle.strip(),
            enabled=enabled,
        )

    def _is_valid_url(self, url: str) -> bool:
        """
        Validate URL format.

        Only allows http and https schemes to prevent dangerous schemes
        like file://, javascript://, or data:// URIs.

        Args:
            url: URL to validate

        Returns:
            True if valid HTTP/HTTPS URL, False otherwise
        """
        try:
            result = urlparse(url)
            # Only allow http and https schemes
            if result.scheme not in ("http", "https"):
                logger.warning(f"URL has invalid scheme '{result.scheme}': {url}")
                return False
            return bool(result.netloc)
        except ValueError as e:
            logger.warning(f"Failed to parse URL '{url}': {e}")
            return False
