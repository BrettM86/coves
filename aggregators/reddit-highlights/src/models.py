"""
Data models for Reddit Highlights Aggregator.
"""
from dataclasses import dataclass, field
from enum import Enum
from typing import List, Optional, Tuple
from datetime import datetime
import re


class LogLevel(Enum):
    """Valid log levels for aggregator configuration."""
    DEBUG = "debug"
    INFO = "info"
    WARNING = "warning"
    ERROR = "error"
    CRITICAL = "critical"


@dataclass
class RedditPost:
    """
    Represents a Reddit post with video content.

    Parsed from Reddit RSS feed entries.
    """

    id: str  # Reddit post ID (e.g., "t3_1abc123" or just the rkey)
    title: str  # Post title
    link: str  # Direct link to content (may be streamable URL)
    reddit_url: str  # Permalink to Reddit post
    subreddit: str  # Subreddit name (without r/)
    author: str  # Reddit username
    published: Optional[datetime] = None  # Post publication time
    streamable_url: Optional[str] = None  # Extracted streamable URL (if found)

    def __post_init__(self):
        """Validate required fields."""
        if not self.id:
            raise ValueError("RedditPost.id cannot be empty")
        if not self.title:
            raise ValueError("RedditPost.title cannot be empty")
        if not self.subreddit:
            raise ValueError("RedditPost.subreddit cannot be empty")


@dataclass(frozen=True)
class SubredditConfig:
    """
    Configuration for a single subreddit source.

    Maps a subreddit to a Coves community.
    Immutable (frozen) to prevent accidental modification.
    """

    name: str  # Subreddit name (e.g., "nba")
    community_handle: str  # Coves community (e.g., "nba.coves.social")
    enabled: bool = True  # Whether to fetch from this subreddit

    def __post_init__(self):
        """Validate configuration fields."""
        if not self.name or not self.name.strip():
            raise ValueError("SubredditConfig.name cannot be empty")
        if not self.community_handle or not self.community_handle.strip():
            raise ValueError("SubredditConfig.community_handle cannot be empty")
        # Validate subreddit name format (alphanumeric, underscores, hyphens only)
        if not re.match(r'^[a-zA-Z0-9_-]+$', self.name):
            raise ValueError(f"Invalid subreddit name format: {self.name}")


@dataclass(frozen=True)
class AggregatorConfig:
    """
    Full aggregator configuration.

    Loaded from config.yaml.
    Immutable (frozen) to prevent accidental modification after loading.
    """

    coves_api_url: str
    subreddits: Tuple[SubredditConfig, ...]  # Use tuple for immutability
    allowed_domains: Tuple[str, ...] = ("streamable.com",)  # Default tuple
    log_level: LogLevel = LogLevel.INFO
    max_posts_per_run: int = 3  # Only consider top N entries from feed

    def __post_init__(self):
        """Validate configuration."""
        if not self.coves_api_url:
            raise ValueError("AggregatorConfig.coves_api_url cannot be empty")
        if not self.subreddits:
            raise ValueError("AggregatorConfig.subreddits cannot be empty")
        if type(self.max_posts_per_run) is not int or self.max_posts_per_run < 1:
            raise ValueError(
                f"AggregatorConfig.max_posts_per_run must be a positive integer, got: {self.max_posts_per_run}"
            )
