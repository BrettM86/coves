"""
Tests for Rich Text Formatter.

Tests conversion of KagiStory to Coves rich text format with facets.
"""
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
        highlights_pos = content.find("Highlights:")

        # Should have a bold facet covering "Highlights:"
        has_highlights_bold = any(
            f['index']['byteStart'] <= highlights_pos and
            f['index']['byteEnd'] >= highlights_pos + len("Highlights:")
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

        # Should have links for: 2 sources + 2 perspectives + 1 Kagi News link = 5 minimum
        assert len(link_facets) >= 5

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

        if actor_char_pos != -1:  # If found in perspectives
            # Convert character position to byte position
            actor_byte_start = len(content[:actor_char_pos].encode('utf-8'))
            actor_byte_end = len(content[:actor_char_pos + len(actor)].encode('utf-8'))

            has_actor_bold = any(
                f['index']['byteStart'] <= actor_byte_start and
                f['index']['byteEnd'] >= actor_byte_end
                for f in bold_facets
            )
            assert has_actor_bold

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
