"""
Data models for Kagi News RSS aggregator.
"""
from dataclasses import dataclass, field
from datetime import datetime
from typing import List, Optional


@dataclass
class Source:
    """A news source citation."""
    title: str
    url: str
    domain: str


@dataclass
class Perspective:
    """A perspective from a particular actor/stakeholder."""
    actor: str
    description: str
    source_url: str
    source_name: str = ""  # Name of the source (e.g., "The Straits Times")


@dataclass
class Quote:
    """A notable quote from the story."""
    text: str
    attribution: str


@dataclass
class KagiStory:
    """
    Structured representation of a Kagi News story.

    Parsed from RSS feed item with HTML description.
    """
    # RSS metadata
    title: str
    link: str  # Kagi story permalink
    guid: str
    pub_date: datetime
    categories: List[str] = field(default_factory=list)

    # Parsed from HTML description
    summary: str = ""
    highlights: List[str] = field(default_factory=list)
    perspectives: List[Perspective] = field(default_factory=list)
    quote: Optional[Quote] = None
    sources: List[Source] = field(default_factory=list)
    image_url: Optional[str] = None
    image_alt: Optional[str] = None

    def __post_init__(self):
        """Validate required fields."""
        if not self.title:
            raise ValueError("title is required")
        if not self.link:
            raise ValueError("link is required")
        if not self.guid:
            raise ValueError("guid is required")


@dataclass
class FeedConfig:
    """Configuration for a single RSS feed."""
    name: str
    url: str
    community_handle: str
    enabled: bool = True


@dataclass
class AggregatorConfig:
    """Full aggregator configuration."""
    coves_api_url: str
    feeds: List[FeedConfig]
    log_level: str = "info"
