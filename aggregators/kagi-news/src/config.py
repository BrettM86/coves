"""
Configuration Loader for Kagi News Aggregator.

Loads and validates configuration from YAML files.
"""
import os
import logging
from pathlib import Path
from typing import Dict, Any
import yaml
from urllib.parse import urlparse

from src.models import AggregatorConfig, FeedConfig

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
    - URL validation
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
        # Check file exists
        if not self.config_path.exists():
            raise ConfigError(f"Configuration file not found: {self.config_path}")

        # Load YAML
        try:
            with open(self.config_path, 'r') as f:
                config_data = yaml.safe_load(f)
        except yaml.YAMLError as e:
            raise ConfigError(f"Failed to parse YAML: {e}")

        if not config_data:
            raise ConfigError("Configuration file is empty")

        # Validate and parse
        try:
            return self._parse_config(config_data)
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
        # Get coves_api_url (with env override)
        coves_api_url = os.getenv('COVES_API_URL', data.get('coves_api_url'))
        if not coves_api_url:
            raise ConfigError("Missing required field: coves_api_url")

        # Validate URL
        if not self._is_valid_url(coves_api_url):
            raise ConfigError(f"Invalid URL for coves_api_url: {coves_api_url}")

        # Get log level (default to info)
        log_level = data.get('log_level', 'info')

        # Parse feeds
        feeds_data = data.get('feeds', [])
        if not feeds_data:
            raise ConfigError("Configuration must include at least one feed")

        feeds = []
        for feed_data in feeds_data:
            feed = self._parse_feed(feed_data)
            feeds.append(feed)

        logger.info(f"Loaded configuration with {len(feeds)} feeds ({sum(1 for f in feeds if f.enabled)} enabled)")

        return AggregatorConfig(
            coves_api_url=coves_api_url,
            feeds=feeds,
            log_level=log_level
        )

    def _parse_feed(self, data: Dict[str, Any]) -> FeedConfig:
        """
        Parse and validate a single feed configuration.

        Args:
            data: Feed configuration data

        Returns:
            FeedConfig object

        Raises:
            ConfigError: If validation fails
        """
        # Required fields
        required_fields = ['name', 'url', 'community_handle']
        for field in required_fields:
            if field not in data:
                raise ConfigError(f"Missing required field in feed config: {field}")

        name = data['name']
        url = data['url']
        community_handle = data['community_handle']
        enabled = data.get('enabled', True)  # Default to True

        # Validate URL
        if not self._is_valid_url(url):
            raise ConfigError(f"Invalid URL for feed '{name}': {url}")

        return FeedConfig(
            name=name,
            url=url,
            community_handle=community_handle,
            enabled=enabled
        )

    def _is_valid_url(self, url: str) -> bool:
        """
        Validate URL format.

        Args:
            url: URL to validate

        Returns:
            True if valid, False otherwise
        """
        try:
            result = urlparse(url)
            return all([result.scheme, result.netloc])
        except Exception:
            return False
