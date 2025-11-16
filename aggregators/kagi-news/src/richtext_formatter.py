"""
Rich Text Formatter for Coves posts.

Converts KagiStory objects to Coves rich text format with facets.
Handles UTF-8 byte position calculation for multi-byte characters.
"""
import logging
from typing import Dict, List, Tuple
from src.models import KagiStory, Perspective, Source

logger = logging.getLogger(__name__)


class RichTextFormatter:
    """
    Formats KagiStory into Coves rich text with facets.

    Applies:
    - Bold facets for section headers and perspective actors
    - Italic facets for quotes
    - Link facets for all URLs
    """

    def format_full(self, story: KagiStory) -> Dict:
        """
        Format KagiStory into full rich text format.

        Args:
            story: KagiStory object to format

        Returns:
            Dictionary with 'content' (str) and 'facets' (list)
        """
        builder = RichTextBuilder()

        # Summary
        builder.add_text(story.summary)
        builder.add_text("\n\n")

        # Highlights (if present)
        if story.highlights:
            builder.add_bold("Highlights:")
            builder.add_text("\n")
            for highlight in story.highlights:
                builder.add_text(f"• {highlight}\n\n")
            builder.add_text("\n")

        # Perspectives (if present)
        if story.perspectives:
            builder.add_bold("Perspectives:")
            builder.add_text("\n")
            for perspective in story.perspectives:
                # Bold the actor name
                actor_with_colon = f"{perspective.actor}:"
                builder.add_bold(actor_with_colon)
                builder.add_text(f" {perspective.description}")

                # Add link to source if available
                if perspective.source_url:
                    builder.add_text(" (")
                    source_link_text = perspective.source_name if perspective.source_name else "Source"
                    builder.add_link(source_link_text, perspective.source_url)
                    builder.add_text(")")

                builder.add_text("\n\n")
            builder.add_text("\n")

        # Quote (if present)
        if story.quote:
            quote_text = f'"{story.quote.text}"'
            builder.add_italic(quote_text)
            builder.add_text(f" — {story.quote.attribution}\n\n")

        # Sources (if present)
        if story.sources:
            builder.add_bold("Sources:")
            builder.add_text("\n")
            for source in story.sources:
                builder.add_text("• ")
                builder.add_link(source.title, source.url)
                builder.add_text(f" - {source.domain}\n\n")
            builder.add_text("\n")

        # Kagi News attribution
        builder.add_text("---\n📰 Story aggregated by ")
        builder.add_link("Kagi News", story.link)

        return builder.build()


class RichTextBuilder:
    """
    Helper class to build rich text content with facets.

    Handles UTF-8 byte position tracking automatically.
    """

    def __init__(self):
        self.content_parts = []
        self.facets = []

    def add_text(self, text: str):
        """Add plain text without any facets."""
        self.content_parts.append(text)

    def add_bold(self, text: str):
        """Add text with bold facet."""
        start_byte = self._get_current_byte_position()
        self.content_parts.append(text)
        end_byte = self._get_current_byte_position()

        self.facets.append({
            "index": {
                "byteStart": start_byte,
                "byteEnd": end_byte
            },
            "features": [
                {"$type": "social.coves.richtext.facet#bold"}
            ]
        })

    def add_italic(self, text: str):
        """Add text with italic facet."""
        start_byte = self._get_current_byte_position()
        self.content_parts.append(text)
        end_byte = self._get_current_byte_position()

        self.facets.append({
            "index": {
                "byteStart": start_byte,
                "byteEnd": end_byte
            },
            "features": [
                {"$type": "social.coves.richtext.facet#italic"}
            ]
        })

    def add_link(self, text: str, uri: str):
        """Add text with link facet."""
        start_byte = self._get_current_byte_position()
        self.content_parts.append(text)
        end_byte = self._get_current_byte_position()

        self.facets.append({
            "index": {
                "byteStart": start_byte,
                "byteEnd": end_byte
            },
            "features": [
                {
                    "$type": "social.coves.richtext.facet#link",
                    "uri": uri
                }
            ]
        })

    def _get_current_byte_position(self) -> int:
        """
        Get the current byte position in the content.

        Uses UTF-8 encoding to handle multi-byte characters correctly.
        """
        current_content = ''.join(self.content_parts)
        return len(current_content.encode('utf-8'))

    def build(self) -> Dict:
        """
        Build the final rich text object.

        Returns:
            Dictionary with 'content' and 'facets'
        """
        content = ''.join(self.content_parts)

        # Sort facets by start position for consistency
        sorted_facets = sorted(self.facets, key=lambda f: f['index']['byteStart'])

        return {
            "content": content,
            "facets": sorted_facets
        }
