"""
Tests for Semantic Deduplication module.

Tests the SemanticDeduplicator with mocked Anthropic API responses.
"""
import pytest
from unittest.mock import Mock, MagicMock, patch
import httpx

import anthropic
from src.semantic_dedup import SemanticDeduplicator, REPORT_DUPLICATES_TOOL


@pytest.fixture
def dedup():
    """Create a SemanticDeduplicator with mocked client."""
    with patch('src.semantic_dedup.anthropic.Anthropic') as mock_anthropic_cls:
        mock_client = Mock()
        mock_anthropic_cls.return_value = mock_client
        d = SemanticDeduplicator(api_key="test-key", threshold=0.8)
        d.client = mock_client
        yield d


@pytest.fixture
def recent_stories():
    """Sample recent stories for comparison."""
    return [
        {"id": "recent-1", "title": "US announces new tariffs on China", "summary": "The United States has announced sweeping new tariffs on Chinese goods."},
        {"id": "recent-2", "title": "SpaceX launches Starship for 5th test flight", "summary": "SpaceX successfully launched Starship on its fifth test flight."},
        {"id": "recent-3", "title": "New study finds coffee reduces heart disease risk", "summary": "A comprehensive study shows moderate coffee consumption lowers cardiovascular risk."},
    ]


@pytest.fixture
def new_stories():
    """Sample new candidate stories."""
    return [
        {"id": "new-1", "title": "Trade tensions escalate as US tariffs take effect", "summary": "US tariffs on Chinese goods go into effect amid growing trade war."},
        {"id": "new-2", "title": "Earthquake hits Turkey killing dozens", "summary": "A 6.5 magnitude earthquake struck southeastern Turkey."},
        {"id": "new-3", "title": "EU considers new trade deal with Japan", "summary": "European Union opens talks on a new bilateral trade agreement with Japan."},
    ]


def _make_tool_response(results):
    """Helper to create a mock Anthropic tool use response."""
    mock_response = Mock()
    mock_block = Mock()
    mock_block.type = "tool_use"
    mock_block.name = "report_duplicates"
    mock_block.input = {"results": results}
    mock_response.content = [mock_block]
    return mock_response


class TestSemanticDeduplicator:
    """Test suite for SemanticDeduplicator."""

    def test_find_duplicates_detects_similar_story(self, dedup, new_stories, recent_stories):
        """Test that semantically similar stories are detected."""
        dedup.client.messages.create.return_value = _make_tool_response([
            {"new_id": "new-1", "duplicate_of": "recent-1", "confidence": 0.92},
            {"new_id": "new-2", "duplicate_of": "", "confidence": 0.0},
            {"new_id": "new-3", "duplicate_of": "", "confidence": 0.0},
        ])

        duplicates = dedup.find_duplicates(new_stories, recent_stories)

        assert duplicates == {"new-1"}
        dedup.client.messages.create.assert_called_once()

    def test_find_duplicates_no_matches(self, dedup, new_stories, recent_stories):
        """Test when no duplicates are found."""
        dedup.client.messages.create.return_value = _make_tool_response([
            {"new_id": "new-1", "duplicate_of": "", "confidence": 0.0},
            {"new_id": "new-2", "duplicate_of": "", "confidence": 0.0},
            {"new_id": "new-3", "duplicate_of": "", "confidence": 0.0},
        ])

        duplicates = dedup.find_duplicates(new_stories, recent_stories)

        assert duplicates == set()

    def test_threshold_filtering(self, dedup, new_stories, recent_stories):
        """Test that confidence below threshold is not filtered."""
        dedup.client.messages.create.return_value = _make_tool_response([
            {"new_id": "new-1", "duplicate_of": "recent-1", "confidence": 0.7},  # Below 0.8 threshold
            {"new_id": "new-2", "duplicate_of": "", "confidence": 0.0},
            {"new_id": "new-3", "duplicate_of": "recent-1", "confidence": 0.85},  # Above threshold
        ])

        duplicates = dedup.find_duplicates(new_stories, recent_stories)

        assert "new-1" not in duplicates  # Below threshold
        assert "new-3" in duplicates  # Above threshold

    def test_empty_new_stories_no_api_call(self, dedup, recent_stories):
        """Test that empty new stories list skips API call."""
        duplicates = dedup.find_duplicates([], recent_stories)

        assert duplicates == set()
        dedup.client.messages.create.assert_not_called()

    def test_empty_recent_stories_no_api_call(self, dedup, new_stories):
        """Test that empty recent stories list skips API call."""
        duplicates = dedup.find_duplicates(new_stories, [])

        assert duplicates == set()
        dedup.client.messages.create.assert_not_called()

    def test_fail_open_on_transient_error(self, dedup, new_stories, recent_stories):
        """Test that transient API errors result in no filtering (fail open)."""
        dedup.client.messages.create.side_effect = anthropic.APIConnectionError(
            request=httpx.Request("POST", "https://api.anthropic.com")
        )

        duplicates = dedup.find_duplicates(new_stories, recent_stories)

        assert duplicates == set()

    def test_auth_error_propagates(self, dedup, new_stories, recent_stories):
        """Test that authentication errors propagate instead of being silently swallowed."""
        mock_response = httpx.Response(401, request=httpx.Request("POST", "https://api.anthropic.com"))
        dedup.client.messages.create.side_effect = anthropic.AuthenticationError(
            message="Invalid API key",
            response=mock_response,
            body={"error": {"message": "Invalid API key"}},
        )

        with pytest.raises(anthropic.AuthenticationError):
            dedup.find_duplicates(new_stories, recent_stories)

    def test_multiple_duplicates_detected(self, dedup, recent_stories):
        """Test detecting multiple duplicates in one batch."""
        new_stories = [
            {"id": "new-1", "title": "Tariffs take effect on Chinese imports", "summary": "US tariffs begin."},
            {"id": "new-2", "title": "Starship fifth flight declared success", "summary": "SpaceX celebrates."},
            {"id": "new-3", "title": "Unique story about Mars", "summary": "Mars exploration update."},
        ]

        dedup.client.messages.create.return_value = _make_tool_response([
            {"new_id": "new-1", "duplicate_of": "recent-1", "confidence": 0.90},
            {"new_id": "new-2", "duplicate_of": "recent-2", "confidence": 0.88},
            {"new_id": "new-3", "duplicate_of": "", "confidence": 0.0},
        ])

        duplicates = dedup.find_duplicates(new_stories, recent_stories)

        assert duplicates == {"new-1", "new-2"}

    def test_uses_tool_use_for_structured_output(self, dedup, new_stories, recent_stories):
        """Test that the API call uses tool_choice to force structured output."""
        dedup.client.messages.create.return_value = _make_tool_response([
            {"new_id": "new-1", "duplicate_of": "", "confidence": 0.0},
            {"new_id": "new-2", "duplicate_of": "", "confidence": 0.0},
            {"new_id": "new-3", "duplicate_of": "", "confidence": 0.0},
        ])

        dedup.find_duplicates(new_stories, recent_stories)

        call_kwargs = dedup.client.messages.create.call_args.kwargs
        assert call_kwargs["tool_choice"] == {"type": "tool", "name": "report_duplicates"}
        assert call_kwargs["tools"] == [REPORT_DUPLICATES_TOOL]

    def test_build_prompt_includes_all_stories(self, dedup):
        """Test that the prompt includes all recent and new stories."""
        new = [{"id": "n1", "title": "New Title", "summary": "New summary"}]
        recent = [{"id": "r1", "title": "Recent Title", "summary": "Recent summary"}]

        prompt = dedup._build_prompt(new, recent)

        assert "New Title" in prompt
        assert "New summary" in prompt
        assert "Recent Title" in prompt
        assert "Recent summary" in prompt
        assert "[n1]" in prompt
        assert "[r1]" in prompt

    def test_build_prompt_handles_empty_summary(self, dedup):
        """Test prompt building with empty summaries."""
        new = [{"id": "n1", "title": "New Title", "summary": ""}]
        recent = [{"id": "r1", "title": "Recent Title", "summary": ""}]

        prompt = dedup._build_prompt(new, recent)

        assert "New Title" in prompt
        assert "Recent Title" in prompt
        # Should not have dangling " -- " for empty summaries
        assert "-- \n" not in prompt

    def test_parse_response_no_tool_use_blocks(self, dedup):
        """Test _parse_response returns empty set when response has no tool_use blocks."""
        mock_response = Mock()
        mock_text_block = Mock()
        mock_text_block.type = "text"
        mock_text_block.text = "Here are some duplicates I found."
        mock_response.content = [mock_text_block]

        result = dedup._parse_response(mock_response)

        assert result == set()

    def test_parse_response_wrong_tool_name(self, dedup):
        """Test _parse_response returns empty set when tool_use block has wrong name."""
        mock_response = Mock()
        mock_block = Mock()
        mock_block.type = "tool_use"
        mock_block.name = "wrong_tool_name"
        mock_block.input = {"results": [
            {"new_id": "new-1", "duplicate_of": "recent-1", "confidence": 0.95}
        ]}
        mock_response.content = [mock_block]

        result = dedup._parse_response(mock_response)

        assert result == set()

    def test_threshold_boundary_exactly_equal(self, dedup, new_stories, recent_stories):
        """Test that confidence exactly equal to threshold (0.8) IS filtered (>= behavior)."""
        dedup.client.messages.create.return_value = _make_tool_response([
            {"new_id": "new-1", "duplicate_of": "recent-1", "confidence": 0.8},  # Exactly at 0.8 threshold
            {"new_id": "new-2", "duplicate_of": "", "confidence": 0.0},
            {"new_id": "new-3", "duplicate_of": "", "confidence": 0.0},
        ])

        duplicates = dedup.find_duplicates(new_stories, recent_stories)

        # 0.8 >= 0.8 threshold, so new-1 SHOULD be filtered
        assert "new-1" in duplicates

    def test_custom_threshold(self):
        """Test that custom threshold is respected."""
        with patch('src.semantic_dedup.anthropic.Anthropic') as mock_anthropic_cls:
            mock_client = Mock()
            mock_anthropic_cls.return_value = mock_client
            d = SemanticDeduplicator(api_key="test-key", threshold=0.95)
            d.client = mock_client

            d.client.messages.create.return_value = _make_tool_response([
                {"new_id": "n1", "duplicate_of": "r1", "confidence": 0.90},
            ])

            new = [{"id": "n1", "title": "Title", "summary": "Summary"}]
            recent = [{"id": "r1", "title": "Title", "summary": "Summary"}]

            duplicates = d.find_duplicates(new, recent)

            # 0.90 < 0.95 threshold, should NOT be filtered
            assert duplicates == set()
