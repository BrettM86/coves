"""
Rich Text Formatter for Coves posts.

Converts KagiStory objects to Coves rich text format with facets.
Handles UTF-8 byte position calculation for multi-byte characters.
"""
import logging
from typing import Dict
from src.citations import build_index, tokenize
from src.models import KagiStory
from src.uri_sanitizer import sanitize_uri

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
        citation_index = build_index(story.sources)
        context = story.link or story.title

        # Summary
        self._emit_with_citations(builder, story.summary, citation_index, context)
        builder.add_text("\n\n")

        # Highlights (if present)
        if story.highlights:
            builder.add_bold("Highlights:")
            builder.add_text("\n")
            for highlight in story.highlights:
                builder.add_text("• ")
                self._emit_with_citations(builder, highlight, citation_index, context)
                builder.add_text("\n\n")
            builder.add_text("\n")

        # Perspectives (if present)
        if story.perspectives:
            builder.add_bold("Perspectives:")
            builder.add_text("\n")
            for perspective in story.perspectives:
                if perspective.actor:
                    builder.add_bold(f"{perspective.actor}:")
                    builder.add_text(" ")
                    self._emit_with_citations(
                        builder, perspective.description, citation_index, context
                    )
                else:
                    self._emit_with_citations(
                        builder, perspective.description, citation_index, context
                    )

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
            # Defensive: quote text isn't currently observed to carry markers,
            # but the formatter is the only place that decides how the value
            # reaches the wire, so handle them here too. We emit the visible
            # opening quote, then text/link spans, then the closing quote,
            # and finally wrap the whole span in a single italic facet so the
            # quote-as-a-whole still has italic styling even when a citation
            # link facet sits inside it.
            italic_start_byte = builder.current_byte_position()
            builder.add_text('"')
            self._emit_with_citations(builder, story.quote.text, citation_index, context)
            builder.add_text('"')
            italic_end_byte = builder.current_byte_position()
            if italic_end_byte > italic_start_byte:
                builder.add_italic_span(italic_start_byte, italic_end_byte)
            # Only emit the em-dash + attribution suffix when an attribution
            # is actually present; otherwise we'd render a bare " — " orphan.
            if story.quote.attribution:
                builder.add_text(f" — {story.quote.attribution}\n\n")
            else:
                builder.add_text("\n\n")

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

    def _emit_with_citations(
        self,
        builder: "RichTextBuilder",
        text: str,
        citation_index,
        context: str,
    ) -> None:
        """Emit `text` to `builder` with `[domain#N]` markers turned into link facets."""
        for kind, payload in tokenize(text, citation_index, context):
            if kind == "text":
                builder.add_text(payload)
            else:
                visible_text, uri = payload
                builder.add_link(visible_text, uri)


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

    def add_italic_span(self, start_byte: int, end_byte: int):
        """
        Attach an italic facet to an already-emitted span of bytes. Used when
        the italic-styled region contains other facets (e.g. an inline citation
        link inside a quote) so the whole quote still appears italic without
        the inner facet fracturing it into multiple smaller italic facets.
        """
        self.facets.append({
            "index": {
                "byteStart": start_byte,
                "byteEnd": end_byte
            },
            "features": [
                {"$type": "social.coves.richtext.facet#italic"}
            ]
        })

    def current_byte_position(self) -> int:
        """Public byte-position accessor for callers building multi-facet spans."""
        return self._get_current_byte_position()

    def add_link(self, text: str, uri: str):
        """
        Add text with link facet.

        facet#link.uri carries the same `format: uri` as the embed fields, so a
        citation URL with a literal accented character invalidates the whole
        record. If the URL cannot be encoded at all the text is still emitted,
        just unlinked — losing a link beats losing the sentence, and the byte
        offsets of later facets are unaffected either way.
        """
        try:
            uri = sanitize_uri(uri)
        except ValueError as e:
            # The URI is deliberately not logged: citation URLs can carry
            # tracking tokens or credentials in the query string.
            logger.warning("Emitting %r as plain text, unusable link uri (%s)", text, e)
            self.content_parts.append(text)
            return

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
