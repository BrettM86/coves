"""
Tests for Rich Text Formatter.

Tests conversion of KagiStory to Coves rich text format with facets.
"""
from __future__ import annotations

import pytest
from datetime import datetime

from src.richtext_formatter import RichTextFormatter
from src.models import KagiStory, Perspective, Quote, Source


@pytest.fixture
def sample_story():
    """Create a sample KagiStory for testing."""
    return KagiStory(
        title="Trump to meet Xi in South Korea",
        link="https://kite.kagi.com/test/world/10",
        guid="https://kite.kagi.com/test/world/10",
        pub_date=datetime(2025, 10, 23, 20, 56, 0),
        categories=["World", "World/Diplomacy"],
        summary="The White House confirmed President Trump will hold a bilateral meeting with Chinese President Xi Jinping in South Korea on October 30.",
        highlights=[
            "Itinerary details: The Asia swing begins in Malaysia, continues to Japan.",
            "APEC context: US officials indicated the leaders will meet on the sidelines."
        ],
        perspectives=[
            Perspective(
                actor="President Trump",
                description="He said his first question to President Xi would be about fentanyl.",
                source_url="https://www.straitstimes.com/world/test"
            ),
            Perspective(
                actor="White House (press secretary)",
                description="Karoline Leavitt confirmed the bilateral meeting.",
                source_url="https://www.scmp.com/news/test"
            )
        ],
        quote=Quote(
            text="Work out a lot of our doubts and questions",
            attribution="President Trump"
        ),
        sources=[
            Source(
                title="Trump to meet Xi in South Korea",
                url="https://www.straitstimes.com/world/test",
                domain="straitstimes.com"
            ),
            Source(
                title="Trump meeting Xi next Thursday",
                url="https://www.scmp.com/news/test",
                domain="scmp.com"
            )
        ],
        image_url="https://kagiproxy.com/img/test123",
        image_alt="Test image"
    )


class TestRichTextFormatter:
    """Test suite for RichTextFormatter."""

    def test_format_full_returns_content_and_facets(self, sample_story):
        """Test that format_full returns content and facets."""
        formatter = RichTextFormatter()
        result = formatter.format_full(sample_story)

        assert 'content' in result
        assert 'facets' in result
        assert isinstance(result['content'], str)
        assert isinstance(result['facets'], list)

    def test_content_structure(self, sample_story):
        """Test that content has correct structure."""
        formatter = RichTextFormatter()
        result = formatter.format_full(sample_story)
        content = result['content']

        # Check all sections are present
        assert sample_story.summary in content
        assert "Highlights:" in content
        assert "Perspectives:" in content
        assert "Sources:" in content
        assert sample_story.quote.text in content
        assert "📰 Story aggregated by Kagi News" in content

    def test_facets_for_bold_headers(self, sample_story):
        """Test that section headers have bold facets."""
        formatter = RichTextFormatter()
        result = formatter.format_full(sample_story)

        # Find bold facets
        bold_facets = [
            f for f in result['facets']
            if any(feat.get('$type') == 'social.coves.richtext.facet#bold'
                   for feat in f['features'])
        ]

        assert len(bold_facets) > 0

        # Check that "Highlights:" is bolded
        content = result['content']
        highlights_byte_start, highlights_byte_end = _find_visible_byte_span(
            content, "Highlights:"
        )

        # Should have a bold facet covering "Highlights:"
        has_highlights_bold = any(
            f['index']['byteStart'] <= highlights_byte_start and
            f['index']['byteEnd'] >= highlights_byte_end
            for f in bold_facets
        )
        assert has_highlights_bold

    def test_facets_for_italic_quote(self, sample_story):
        """Test that quotes have italic facets."""
        formatter = RichTextFormatter()
        result = formatter.format_full(sample_story)

        # Find italic facets
        italic_facets = [
            f for f in result['facets']
            if any(feat.get('$type') == 'social.coves.richtext.facet#italic'
                   for feat in f['features'])
        ]

        assert len(italic_facets) > 0

        # The quote text is wrapped with quotes, so search for that
        content = result['content']
        quote_with_quotes = f'"{sample_story.quote.text}"'
        quote_char_pos = content.find(quote_with_quotes)

        # Convert character position to byte position
        quote_byte_start = len(content[:quote_char_pos].encode('utf-8'))
        quote_byte_end = len(content[:quote_char_pos + len(quote_with_quotes)].encode('utf-8'))

        has_quote_italic = any(
            f['index']['byteStart'] <= quote_byte_start and
            f['index']['byteEnd'] >= quote_byte_end
            for f in italic_facets
        )
        assert has_quote_italic

    def test_quote_with_empty_attribution_omits_em_dash(self, sample_story):
        """A quote whose attribution is the empty string should render the quoted
        text but no orphan em-dash suffix."""
        sample_story.quote = Quote(text="something said", attribution="")
        formatter = RichTextFormatter()
        result = formatter.format_full(sample_story)

        content = result['content']
        # The visible quoted text should still be present.
        assert '"something said"' in content
        # No orphan em-dash separator anywhere in the rendered content.
        assert " — " not in content
        # And no trailing-space-before-newline artifact from a half-written suffix.
        assert '" \n' not in content

    def test_facets_for_links(self, sample_story):
        """Test that URLs have link facets."""
        formatter = RichTextFormatter()
        result = formatter.format_full(sample_story)

        # Find link facets
        link_facets = [
            f for f in result['facets']
            if any(feat.get('$type') == 'social.coves.richtext.facet#link'
                   for feat in f['features'])
        ]

        # Expect exactly: 2 sources + 2 perspectives + 1 Kagi News attribution = 5
        assert len(link_facets) == 5

        # Check that first source URL has a link facet
        source_urls = [s.url for s in sample_story.sources]
        for url in source_urls:
            has_link = any(
                any(feat.get('uri') == url for feat in f['features'])
                for f in link_facets
            )
            assert has_link, f"Missing link facet for {url}"

    def test_utf8_byte_positions(self):
        """Test UTF-8 byte position calculation with multi-byte characters."""
        # Create story with emoji and non-ASCII characters
        story = KagiStory(
            title="Test 👋 Story",
            link="https://test.com",
            guid="https://test.com",
            pub_date=datetime.now(),
            categories=["Test"],
            summary="Hello 世界 this is a test with emoji 🎉",
            highlights=["Test highlight"],
            perspectives=[],
            quote=None,
            sources=[],
        )

        formatter = RichTextFormatter()
        result = formatter.format_full(story)

        # Verify content contains the emoji
        assert "👋" in result['content'] or "🎉" in result['content']

        # Verify all facet byte positions are valid
        content_bytes = result['content'].encode('utf-8')
        for facet in result['facets']:
            start = facet['index']['byteStart']
            end = facet['index']['byteEnd']

            # Positions should be within bounds
            assert 0 <= start < len(content_bytes)
            assert start < end <= len(content_bytes)

    def test_format_story_without_optional_fields(self):
        """Test formatting story with missing optional fields."""
        minimal_story = KagiStory(
            title="Minimal Story",
            link="https://test.com",
            guid="https://test.com",
            pub_date=datetime.now(),
            categories=["Test"],
            summary="Just a summary.",
            highlights=[],  # Empty
            perspectives=[],  # Empty
            quote=None,  # Missing
            sources=[],  # Empty
        )

        formatter = RichTextFormatter()
        result = formatter.format_full(minimal_story)

        # Should still have content and facets
        assert result['content']
        assert result['facets']

        # Should have summary
        assert "Just a summary." in result['content']

        # Should NOT have empty sections
        assert "Highlights:" not in result['content']
        assert "Perspectives:" not in result['content']

    def test_perspective_actor_is_bolded(self, sample_story):
        """Test that perspective actor names are bolded."""
        formatter = RichTextFormatter()
        result = formatter.format_full(sample_story)

        content = result['content']
        bold_facets = [
            f for f in result['facets']
            if any(feat.get('$type') == 'social.coves.richtext.facet#bold'
                   for feat in f['features'])
        ]

        # Find "President Trump:" in perspectives section
        actor = "President Trump:"
        perspectives_start = content.find("Perspectives:")
        actor_char_pos = content.find(actor, perspectives_start)
        assert actor_char_pos != -1

        # Convert character position to byte position
        actor_byte_start = len(content[:actor_char_pos].encode('utf-8'))
        actor_byte_end = len(content[:actor_char_pos + len(actor)].encode('utf-8'))

        has_actor_bold = any(
            f['index']['byteStart'] <= actor_byte_start and
            f['index']['byteEnd'] >= actor_byte_end
            for f in bold_facets
        )
        assert has_actor_bold

    def test_perspective_without_actor_renders_plain_description(self, sample_story):
        """Perspectives whose JSON `text` has no actor prefix render the description
        without a leading bolded colon."""
        sample_story.perspectives = [
            Perspective(
                actor="",
                description="A plain observation without a leading actor name.",
                source_url="https://example.com/source",
                source_name="Example",
            )
        ]
        formatter = RichTextFormatter()
        result = formatter.format_full(sample_story)

        # No stray ": " bolded prefix from an empty actor
        assert "**:" not in result['content']
        assert "A plain observation without a leading actor name." in result['content']
        # The empty actor must not contribute a zero-length bold facet either
        bold_facets = [
            f for f in result['facets']
            if any(feat.get('$type') == 'social.coves.richtext.facet#bold'
                   for feat in f['features'])
        ]
        for f in bold_facets:
            assert f['index']['byteEnd'] > f['index']['byteStart']

    def test_kagi_attribution_link(self, sample_story):
        """Test that Kagi News attribution has a link to the story."""
        formatter = RichTextFormatter()
        result = formatter.format_full(sample_story)

        # Should have link to Kagi story
        link_facets = [
            f for f in result['facets']
            if any(feat.get('$type') == 'social.coves.richtext.facet#link'
                   for feat in f['features'])
        ]

        # Find link to the Kagi story URL
        kagi_link = any(
            any(feat.get('uri') == sample_story.link for feat in f['features'])
            for f in link_facets
        )
        assert kagi_link, "Missing link to Kagi story in attribution"

    def test_facets_do_not_overlap(self, sample_story):
        """Test that facets with same feature type don't overlap."""
        formatter = RichTextFormatter()
        result = formatter.format_full(sample_story)

        # Group facets by type
        facets_by_type = {}
        for facet in result['facets']:
            for feature in facet['features']:
                ftype = feature['$type']
                if ftype not in facets_by_type:
                    facets_by_type[ftype] = []
                facets_by_type[ftype].append(facet)

        # Check for overlaps within each type
        for ftype, facets in facets_by_type.items():
            for i, f1 in enumerate(facets):
                for f2 in facets[i+1:]:
                    start1, end1 = f1['index']['byteStart'], f1['index']['byteEnd']
                    start2, end2 = f2['index']['byteStart'], f2['index']['byteEnd']

                    # Check if they overlap
                    overlaps = (start1 < end2 and start2 < end1)
                    assert not overlaps, f"Overlapping facets of type {ftype}: {f1} and {f2}"


def _link_facets(facets):
    return [
        f for f in facets
        if any(feat.get('$type') == 'social.coves.richtext.facet#link' for feat in f['features'])
    ]


def _find_visible_byte_span(content: str, visible_text: str) -> tuple[int, int]:
    """Find the UTF-8 byte span of `visible_text` inside `content` (first match)."""
    char_position = content.find(visible_text)
    assert char_position != -1, (
        f"expected visible text {visible_text!r} in rendered content but not found:\n{content!r}"
    )
    start_byte = len(content[:char_position].encode('utf-8'))
    end_byte = len(content[:char_position + len(visible_text)].encode('utf-8'))
    return start_byte, end_byte


class TestRichTextFormatterCitations:
    """Inline `[domain#N]` markers in story fields become link facets."""

    def _story_with(self, **overrides):
        base = KagiStory(
            title="Test Story",
            link="https://kite.kagi.com/abc/world/1",
            guid="https://kite.kagi.com/abc/world/1",
            pub_date=datetime(2026, 5, 11, 12, 0, 0),
            categories=["World"],
            summary="",
            highlights=[],
            perspectives=[],
            quote=None,
            sources=[
                # Sources order = articles order; per-domain bucket is 1-indexed
                Source(title="Reuters A", url="https://reuters.com/a", domain="reuters.com"),
                Source(title="Reuters B", url="https://reuters.com/b", domain="reuters.com"),
                Source(title="AP A", url="https://apnews.com/a", domain="apnews.com"),
            ],
        )
        for key, value in overrides.items():
            setattr(base, key, value)
        return base

    def test_summary_marker_renders_as_link_facet(self):
        story = self._story_with(
            summary="Tensions remained stalled [reuters.com#1]. Things continued."
        )
        result = RichTextFormatter().format_full(story)

        assert "[reuters.com]" in result['content']
        assert "[reuters.com#1]" not in result['content']

        start_byte, end_byte = _find_visible_byte_span(result['content'], "[reuters.com]")
        matching = [
            f for f in _link_facets(result['facets'])
            if f['index']['byteStart'] == start_byte
            and f['index']['byteEnd'] == end_byte
            and any(feat.get('uri') == "https://reuters.com/a" for feat in f['features'])
        ]
        assert matching, (
            f"expected link facet at bytes {start_byte}..{end_byte} pointing at the reuters article; "
            f"got facets={result['facets']}"
        )

    def test_highlight_marker_renders_as_link_facet(self):
        story = self._story_with(
            summary="overview",
            highlights=["First bullet refers [reuters.com#2] in detail."],
        )
        result = RichTextFormatter().format_full(story)

        # The N=2 reuters article is the second source in the reuters bucket
        start_byte, end_byte = _find_visible_byte_span(result['content'], "[reuters.com]")
        # However the FIRST [reuters.com] occurrence in content might also appear in the Sources list.
        # The highlight bullet appears BEFORE the Sources block, so first match is the right one.
        matching = [
            f for f in _link_facets(result['facets'])
            if f['index']['byteStart'] == start_byte
            and f['index']['byteEnd'] == end_byte
            and any(feat.get('uri') == "https://reuters.com/b" for feat in f['features'])
        ]
        assert matching, (
            f"expected highlight citation facet linking to reuters/b at {start_byte}..{end_byte}; got {result['facets']}"
        )

    def test_perspective_description_marker_renders_as_link_facet(self):
        story = self._story_with(
            summary="overview",
            perspectives=[
                Perspective(
                    actor="Reuters",
                    description="They reported [apnews.com#1] confirmed the figures.",
                    source_url="https://reuters.com/persp",
                    source_name="Reuters",
                )
            ],
        )
        result = RichTextFormatter().format_full(story)

        start_byte, end_byte = _find_visible_byte_span(result['content'], "[apnews.com]")
        matching = [
            f for f in _link_facets(result['facets'])
            if f['index']['byteStart'] == start_byte
            and f['index']['byteEnd'] == end_byte
            and any(feat.get('uri') == "https://apnews.com/a" for feat in f['features'])
        ]
        assert matching, (
            f"expected perspective citation facet linking to apnews; got {result['facets']}"
        )

    def test_quote_marker_renders_as_link_facet_inside_italic_span(self):
        story = self._story_with(
            summary="overview",
            quote=Quote(
                text="we will respond [reuters.com#1] today",
                attribution="Spokesperson",
            ),
        )
        result = RichTextFormatter().format_full(story)

        # Citation link visible inside the quote
        start_byte, end_byte = _find_visible_byte_span(result['content'], "[reuters.com]")
        link_matches = [
            f for f in _link_facets(result['facets'])
            if f['index']['byteStart'] == start_byte
            and f['index']['byteEnd'] == end_byte
            and any(feat.get('uri') == "https://reuters.com/a" for feat in f['features'])
        ]
        assert link_matches, f"expected link facet inside quote; got {result['facets']}"

        # And a single italic facet still spans the whole quoted segment
        italic_facets = [
            f for f in result['facets']
            if any(feat.get('$type') == 'social.coves.richtext.facet#italic' for feat in f['features'])
        ]
        full_quote_visible = '"we will respond [reuters.com] today"'
        quote_start, quote_end = _find_visible_byte_span(result['content'], full_quote_visible)
        wrapping = [
            f for f in italic_facets
            if f['index']['byteStart'] == quote_start and f['index']['byteEnd'] == quote_end
        ]
        assert wrapping, (
            f"expected single italic facet spanning the whole quote {quote_start}..{quote_end}; "
            f"got italic facets={italic_facets}"
        )

    def test_multi_byte_text_around_citation_marker(self):
        """Facet bytes must be UTF-8 byte offsets of the visible `[domain]` text,
        not character offsets — multi-byte characters before the marker must shift
        the byte positions accordingly."""
        story = self._story_with(
            summary="東京で reported — stalled [reuters.com#1] today…",
        )
        result = RichTextFormatter().format_full(story)

        content = result['content']
        assert "[reuters.com]" in content

        start_byte, end_byte = _find_visible_byte_span(content, "[reuters.com]")
        # Sanity: the citation byte start should be larger than its character index
        # because of the leading multi-byte "東京で".
        char_start = content.find("[reuters.com]")
        assert start_byte > char_start, (
            f"UTF-8 byte start must exceed char index after multi-byte prefix "
            f"(char_start={char_start}, byte_start={start_byte})"
        )

        matching = [
            f for f in _link_facets(result['facets'])
            if f['index']['byteStart'] == start_byte
            and f['index']['byteEnd'] == end_byte
            and any(feat.get('uri') == "https://reuters.com/a" for feat in f['features'])
        ]
        assert matching, (
            f"expected multi-byte-aware link facet at bytes {start_byte}..{end_byte}; got {result['facets']}"
        )


class TestAddLinkSanitizesURIs:
    """
    facet#link.uri carries the same `format: uri` as the embed fields, so a
    citation URL with a raw accented character invalidates the whole record.
    """

    def _builder(self):
        from src.richtext_formatter import RichTextBuilder
        return RichTextBuilder()

    def test_link_uri_is_encoded(self):
        builder = self._builder()
        builder.add_link("Kagi", "https://kagi.com/news/pokémon/")
        assert builder.facets[0]["features"][0]["uri"] == (
            "https://kagi.com/news/pok%C3%A9mon/"
        )

    def test_reserved_escape_in_link_is_preserved(self):
        builder = self._builder()
        builder.add_link("Archived", "https://web.archive.org/a%2Fb/café")
        assert builder.facets[0]["features"][0]["uri"] == (
            "https://web.archive.org/a%2Fb/caf%C3%A9"
        )

    def test_unusable_uri_degrades_to_plain_text(self):
        builder = self._builder()
        builder.add_link("just words", "not a url at all")
        assert "".join(builder.content_parts) == "just words"
        assert builder.facets == []

    def test_forbidden_scheme_degrades_to_plain_text(self):
        """A javascript: target must never become a rendered link facet."""
        builder = self._builder()
        builder.add_link("click me", "javascript:alert(document.cookie)")
        assert "".join(builder.content_parts) == "click me"
        assert builder.facets == []

    def test_degraded_link_leaves_later_byte_offsets_correct(self):
        """
        The docstring claims offsets are unaffected by the fallback. Emitting the
        text without a facet must keep every subsequent facet aligned, or the
        whole annotation stream slides.
        """
        builder = self._builder()
        builder.add_link("bad", "not a url at all")   # degrades, no facet
        builder.add_link("gòòd", "https://example.com/café")

        content = "".join(builder.content_parts)
        assert content == "badgòòd"
        assert len(builder.facets) == 1

        index = builder.facets[0]["index"]
        encoded = content.encode("utf-8")
        # The surviving facet must slice exactly the text it annotates.
        assert encoded[index["byteStart"]:index["byteEnd"]].decode("utf-8") == "gòòd"

    def test_logs_do_not_leak_the_rejected_uri(self, caplog):
        import logging
        builder = self._builder()
        with caplog.at_level(logging.WARNING):
            builder.add_link("x", "https://example.com/a?token=SUPERSECRETVALUE\x00")
        assert "SUPERSECRETVALUE" not in caplog.text
