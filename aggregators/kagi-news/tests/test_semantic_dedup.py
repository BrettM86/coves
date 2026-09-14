"""
Tests for Semantic Deduplication module.

Tests the SemanticDeduplicator with mocked Anthropic API responses.

Story ids are full Kagi-style URL GUIDs, as they are in production. The model is
never shown them: the prompt and the per-call tool schema use short opaque
handles ([r1]..[rM], [n1]..[nN]), and find_duplicates translates the handles the
model returns back into GUIDs.
"""
import copy
import logging

import pytest
from unittest.mock import Mock, MagicMock, patch
import httpx2 as httpx

import anthropic
from src.semantic_dedup import (
    SemanticDeduplicator,
    SemanticDedupUnavailable,
    REPORT_DUPLICATES_TOOL,
)


RECENT_STORIES = [
    {
        "id": "https://kite.kagi.com/world/2026091012/us-announces-new-tariffs-on-china",
        "title": "US announces new tariffs on China",
        "summary": "The United States has announced sweeping new tariffs on Chinese goods.",
    },
    {
        "id": "https://kite.kagi.com/science/2026091013/spacex-launches-starship-fifth-test-flight",
        "title": "SpaceX launches Starship for 5th test flight",
        "summary": "SpaceX successfully launched Starship on its fifth test flight.",
    },
    {
        "id": "https://kite.kagi.com/health/2026091014/coffee-reduces-heart-disease-risk",
        "title": "New study finds coffee reduces heart disease risk",
        "summary": "A comprehensive study shows moderate coffee consumption lowers cardiovascular risk.",
    },
]

NEW_STORIES = [
    {
        "id": "https://kite.kagi.com/world/2026091212/trade-tensions-escalate-as-tariffs-take-effect",
        "title": "Trade tensions escalate as US tariffs take effect",
        "summary": "US tariffs on Chinese goods go into effect amid growing trade war.",
    },
    {
        "id": "https://kite.kagi.com/world/2026091213/earthquake-hits-turkey-killing-dozens",
        "title": "Earthquake hits Turkey killing dozens",
        "summary": "A 6.5 magnitude earthquake struck southeastern Turkey.",
    },
    {
        "id": "https://kite.kagi.com/world/2026091214/eu-considers-new-trade-deal-with-japan",
        "title": "EU considers new trade deal with Japan",
        "summary": "European Union opens talks on a new bilateral trade agreement with Japan.",
    },
]

RECENT_GUIDS = [story["id"] for story in RECENT_STORIES]
NEW_GUIDS = [story["id"] for story in NEW_STORIES]


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
    """Sample recent stories for comparison, keyed by full Kagi GUID."""
    return copy.deepcopy(RECENT_STORIES)


@pytest.fixture
def new_stories():
    """Sample new candidate stories, keyed by full Kagi GUID."""
    return copy.deepcopy(NEW_STORIES)


def _make_tool_response(results, stop_reason="tool_use"):
    """Helper to create a mock Anthropic tool use response."""
    mock_response = Mock()
    mock_block = Mock()
    mock_block.type = "tool_use"
    mock_block.name = "report_duplicates"
    mock_block.input = {"results": results}
    mock_response.content = [mock_block]
    mock_response.stop_reason = stop_reason
    return mock_response


def _warning_messages(caplog):
    """Every WARNING message captured so far."""
    return [
        record.getMessage() for record in caplog.records
        if record.levelno == logging.WARNING
    ]


def _warnings_mentioning(caplog, needle):
    """Captured WARNING messages containing needle."""
    return [message for message in _warning_messages(caplog) if needle in message]


def _sent_prompt(dedup):
    """The prompt text of the most recent messages.create call."""
    return dedup.client.messages.create.call_args.kwargs["messages"][0]["content"]


def _sent_result_properties(dedup):
    """The per-result properties of the tool schema sent on the most recent call."""
    tool = dedup.client.messages.create.call_args.kwargs["tools"][0]
    return tool["input_schema"]["properties"]["results"]["items"]["properties"]


class TestSemanticDeduplicator:
    """Test suite for SemanticDeduplicator."""

    def test_find_duplicates_detects_similar_story(self, dedup, new_stories, recent_stories,
                                                   caplog):
        """Test that semantically similar stories are detected."""
        dedup.client.messages.create.return_value = _make_tool_response([
            {"new_id": "n1", "duplicate_of": "r1", "confidence": 0.92},
            {"new_id": "n2", "duplicate_of": "", "confidence": 0.0},
            {"new_id": "n3", "duplicate_of": "", "confidence": 0.0},
        ])

        with caplog.at_level(logging.WARNING, logger="src.semantic_dedup"):
            duplicates = dedup.find_duplicates(new_stories, recent_stories)

        assert duplicates == {NEW_GUIDS[0]: RECENT_GUIDS[0]}
        dedup.client.messages.create.assert_called_once()
        # A complete "tool_use" response is not a truncation; it must stay quiet.
        assert not _warnings_mentioning(caplog, "max_tokens")

    def test_find_duplicates_no_matches(self, dedup, new_stories, recent_stories):
        """Test when no duplicates are found."""
        dedup.client.messages.create.return_value = _make_tool_response([
            {"new_id": "n1", "duplicate_of": "", "confidence": 0.0},
            {"new_id": "n2", "duplicate_of": "", "confidence": 0.0},
            {"new_id": "n3", "duplicate_of": "", "confidence": 0.0},
        ])

        duplicates = dedup.find_duplicates(new_stories, recent_stories)

        assert duplicates == {}

    def test_threshold_filtering(self, dedup, new_stories, recent_stories):
        """Test that confidence below threshold is not filtered."""
        dedup.client.messages.create.return_value = _make_tool_response([
            {"new_id": "n1", "duplicate_of": "r1", "confidence": 0.7},  # Below 0.8 threshold
            {"new_id": "n2", "duplicate_of": "", "confidence": 0.0},
            {"new_id": "n3", "duplicate_of": "r1", "confidence": 0.85},  # Above threshold
        ])

        duplicates = dedup.find_duplicates(new_stories, recent_stories)

        assert duplicates == {NEW_GUIDS[2]: RECENT_GUIDS[0]}

    def test_empty_new_stories_no_api_call(self, dedup, recent_stories):
        """Test that empty new stories list skips API call."""
        duplicates = dedup.find_duplicates([], recent_stories)

        assert duplicates == {}
        dedup.client.messages.create.assert_not_called()

    def test_empty_recent_stories_no_api_call(self, dedup, new_stories):
        """Test that empty recent stories list skips API call."""
        duplicates = dedup.find_duplicates(new_stories, [])

        assert duplicates == {}
        dedup.client.messages.create.assert_not_called()

    def test_transient_error_raises_unavailable(self, dedup, new_stories, recent_stories,
                                                caplog):
        """A transient API error is unavailability, not a verdict of "no duplicates".

        Failing open returned {} and the caller posted the whole batch, so an
        outage silently published duplicates. The caller must be able to tell
        "nothing was flagged" from "we never got an answer".
        """
        dedup.client.messages.create.side_effect = anthropic.APIConnectionError(
            request=httpx.Request("POST", "https://api.anthropic.com")
        )

        with caplog.at_level(logging.WARNING, logger="src.semantic_dedup"):
            with pytest.raises(SemanticDedupUnavailable):
                dedup.find_duplicates(new_stories, recent_stories)

        assert _warning_messages(caplog), "the transient failure must be logged first"

    def test_rate_limit_error_raises_unavailable(self, dedup, new_stories, recent_stories):
        """Rate limiting is transient too, and must not read as "no duplicates"."""
        dedup.client.messages.create.side_effect = anthropic.RateLimitError(
            message="rate limited",
            response=httpx.Response(
                429, request=httpx.Request("POST", "https://api.anthropic.com")
            ),
            body=None,
        )

        with pytest.raises(SemanticDedupUnavailable):
            dedup.find_duplicates(new_stories, recent_stories)

    def test_internal_server_error_raises_unavailable(self, dedup, new_stories,
                                                      recent_stories):
        """A 5xx from the API is unavailability."""
        dedup.client.messages.create.side_effect = anthropic.InternalServerError(
            message="server error",
            response=httpx.Response(
                500, request=httpx.Request("POST", "https://api.anthropic.com")
            ),
            body=None,
        )

        with pytest.raises(SemanticDedupUnavailable):
            dedup.find_duplicates(new_stories, recent_stories)

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

    def test_multiple_duplicates_detected(self, dedup, new_stories, recent_stories):
        """Test detecting multiple duplicates in one batch."""
        dedup.client.messages.create.return_value = _make_tool_response([
            {"new_id": "n1", "duplicate_of": "r1", "confidence": 0.90},
            {"new_id": "n2", "duplicate_of": "r2", "confidence": 0.88},
            {"new_id": "n3", "duplicate_of": "", "confidence": 0.0},
        ])

        duplicates = dedup.find_duplicates(new_stories, recent_stories)

        assert duplicates == {
            NEW_GUIDS[0]: RECENT_GUIDS[0],
            NEW_GUIDS[1]: RECENT_GUIDS[1],
        }

    def test_uses_tool_use_for_structured_output(self, dedup, new_stories, recent_stories):
        """Test that the API call uses tool_choice to force structured output."""
        dedup.client.messages.create.return_value = _make_tool_response([
            {"new_id": "n1", "duplicate_of": "", "confidence": 0.0},
            {"new_id": "n2", "duplicate_of": "", "confidence": 0.0},
            {"new_id": "n3", "duplicate_of": "", "confidence": 0.0},
        ])

        dedup.find_duplicates(new_stories, recent_stories)

        call_kwargs = dedup.client.messages.create.call_args.kwargs
        assert call_kwargs["tool_choice"] == {"type": "tool", "name": "report_duplicates"}
        tools = call_kwargs["tools"]
        assert len(tools) == 1
        assert tools[0]["name"] == "report_duplicates"
        # Built per call from the module template, never the shared object itself.
        assert tools[0] is not REPORT_DUPLICATES_TOOL
        assert tools[0]["input_schema"]["properties"]["results"]["items"]["required"] == [
            "new_id", "duplicate_of", "confidence"
        ]

    def test_prompt_includes_all_stories(self, dedup, new_stories, recent_stories):
        """Test that the prompt includes every recent and new story's title and summary."""
        dedup.client.messages.create.return_value = _make_tool_response([
            {"new_id": "n1", "duplicate_of": "", "confidence": 0.0},
        ])

        dedup.find_duplicates(new_stories[:1], recent_stories[:1])

        prompt = _sent_prompt(dedup)
        assert new_stories[0]["title"] in prompt
        assert new_stories[0]["summary"] in prompt
        assert recent_stories[0]["title"] in prompt
        assert recent_stories[0]["summary"] in prompt
        assert "[n1]" in prompt
        assert "[r1]" in prompt

    def test_prompt_handles_empty_summary(self, dedup):
        """Test prompt building with empty summaries."""
        dedup.client.messages.create.return_value = _make_tool_response([
            {"new_id": "n1", "duplicate_of": "", "confidence": 0.0},
        ])

        new = [{"id": NEW_GUIDS[0], "title": "New Title", "summary": ""}]
        recent = [{"id": RECENT_GUIDS[0], "title": "Recent Title", "summary": ""}]
        dedup.find_duplicates(new, recent)

        prompt = _sent_prompt(dedup)
        assert "New Title" in prompt
        assert "Recent Title" in prompt
        # Should not have dangling " -- " for empty summaries
        assert "-- \n" not in prompt

    def test_no_tool_use_blocks_raises_unavailable(self, dedup, new_stories, recent_stories,
                                                   caplog):
        """A response with no tool_use block answered nothing; that is unavailability."""
        mock_response = Mock()
        mock_text_block = Mock()
        mock_text_block.type = "text"
        mock_text_block.text = "Here are some duplicates I found."
        mock_response.content = [mock_text_block]
        mock_response.stop_reason = "end_turn"
        dedup.client.messages.create.return_value = mock_response

        with caplog.at_level(logging.WARNING, logger="src.semantic_dedup"):
            with pytest.raises(SemanticDedupUnavailable):
                dedup.find_duplicates(new_stories, recent_stories)

        assert _warning_messages(caplog), "the missing tool result must be logged first"

    def test_wrong_tool_name_raises_unavailable(self, dedup, new_stories, recent_stories,
                                                caplog):
        """A tool_use block under another name carries no verdicts we can trust."""
        mock_response = Mock()
        mock_block = Mock()
        mock_block.type = "tool_use"
        mock_block.name = "wrong_tool_name"
        mock_block.input = {"results": [
            {"new_id": "n1", "duplicate_of": "r1", "confidence": 0.95}
        ]}
        mock_response.content = [mock_block]
        mock_response.stop_reason = "tool_use"
        dedup.client.messages.create.return_value = mock_response

        with caplog.at_level(logging.WARNING, logger="src.semantic_dedup"):
            with pytest.raises(SemanticDedupUnavailable):
                dedup.find_duplicates(new_stories, recent_stories)

        assert _warning_messages(caplog), "the unusable tool result must be logged first"

    def test_empty_results_with_candidates_raises_unavailable(self, dedup, new_stories,
                                                             recent_stories, caplog):
        """An empty verdict list for real candidates is silence, not an all-clear."""
        dedup.client.messages.create.return_value = _make_tool_response([])

        with caplog.at_level(logging.WARNING, logger="src.semantic_dedup"):
            with pytest.raises(SemanticDedupUnavailable):
                dedup.find_duplicates(new_stories, recent_stories)

        assert _warning_messages(caplog), "the empty verdict list must be logged first"

    def test_threshold_boundary_exactly_equal(self, dedup, new_stories, recent_stories):
        """Test that confidence exactly equal to threshold (0.8) IS filtered (>= behavior)."""
        dedup.client.messages.create.return_value = _make_tool_response([
            {"new_id": "n1", "duplicate_of": "r1", "confidence": 0.8},  # Exactly at 0.8 threshold
            {"new_id": "n2", "duplicate_of": "", "confidence": 0.0},
            {"new_id": "n3", "duplicate_of": "", "confidence": 0.0},
        ])

        duplicates = dedup.find_duplicates(new_stories, recent_stories)

        # 0.8 >= 0.8 threshold, so the first candidate SHOULD be filtered
        assert duplicates == {NEW_GUIDS[0]: RECENT_GUIDS[0]}

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

            new = [{"id": NEW_GUIDS[0], "title": "Title", "summary": "Summary"}]
            recent = [{"id": RECENT_GUIDS[0], "title": "Title", "summary": "Summary"}]

            duplicates = d.find_duplicates(new, recent)

            # 0.90 < 0.95 threshold, should NOT be filtered
            assert duplicates == {}

    def test_prompt_and_tool_schema_use_per_call_handles(self, dedup, new_stories, recent_stories):
        """Handles, not GUIDs, identify articles in both the prompt and the tool schema.

        Kagi GUIDs are long URLs. Asking the model to echo one back as an id got a
        truncated string that matched nothing, so flagged duplicates were posted
        anyway. Short handles are built per call, constrained by enum, and the
        shared module template is never mutated.
        """
        tool_template_before = copy.deepcopy(REPORT_DUPLICATES_TOOL)
        # Every candidate gets a verdict on both calls, so neither call is an
        # incomplete answer; only the prompt and schema are under test here.
        dedup.client.messages.create.side_effect = [
            _make_tool_response([
                {"new_id": "n1", "duplicate_of": "", "confidence": 0.0},
                {"new_id": "n2", "duplicate_of": "", "confidence": 0.0},
                {"new_id": "n3", "duplicate_of": "", "confidence": 0.0},
            ]),
            _make_tool_response([
                {"new_id": "n1", "duplicate_of": "", "confidence": 0.0},
            ]),
        ]

        recent = recent_stories[:2]
        new = new_stories[:3]
        dedup.find_duplicates(new, recent)

        # The prompt pairs each handle with its story, in input order.
        prompt = _sent_prompt(dedup)
        assert f"[r1] {recent[0]['title']}" in prompt
        assert f"[r2] {recent[1]['title']}" in prompt
        assert f"[n1] {new[0]['title']}" in prompt
        assert f"[n2] {new[1]['title']}" in prompt
        assert f"[n3] {new[2]['title']}" in prompt

        # No GUID is ever offered to the model as an id.
        for guid in [story["id"] for story in recent + new]:
            assert f"[{guid}]" not in prompt

        # The tool schema constrains the model to those same handles.
        properties = _sent_result_properties(dedup)
        assert properties["new_id"]["enum"] == ["n1", "n2", "n3"]
        duplicate_of_enum = properties["duplicate_of"]["enum"]
        assert set(duplicate_of_enum) == {"", "r1", "r2"}
        assert len(duplicate_of_enum) == 3
        assert "" in duplicate_of_enum

        # The module template is shared state; the per-call enums must not leak into it.
        assert REPORT_DUPLICATES_TOOL == tool_template_before
        template_properties = (
            REPORT_DUPLICATES_TOOL["input_schema"]["properties"]["results"]["items"]["properties"]
        )
        assert "enum" not in template_properties["new_id"]
        assert "enum" not in template_properties["duplicate_of"]

        # A second call on the same instance reflects only the second call's inputs.
        dedup.find_duplicates(new_stories[1:2], recent_stories[2:3])

        second_properties = _sent_result_properties(dedup)
        assert second_properties["new_id"]["enum"] == ["n1"]
        assert set(second_properties["duplicate_of"]["enum"]) == {"", "r1"}
        assert REPORT_DUPLICATES_TOOL == tool_template_before

    def test_handles_map_back_to_guids(self, dedup, new_stories, recent_stories):
        """The returned mapping is new GUID -> the recent GUID it duplicates."""
        dedup.client.messages.create.return_value = _make_tool_response([
            {"new_id": "n2", "duplicate_of": "r1", "confidence": 0.9},
            {"new_id": "n1", "duplicate_of": "", "confidence": 0.0},
            {"new_id": "n3", "duplicate_of": "r2", "confidence": 0.5},
        ])

        duplicates = dedup.find_duplicates(new_stories[:3], recent_stories[:2])

        # n2 is a duplicate of r1; n1 is not flagged; n3 is below the 0.8 threshold.
        assert duplicates == {NEW_GUIDS[1]: RECENT_GUIDS[0]}

    def test_unknown_handles_are_skipped_with_warning(self, dedup, new_stories, recent_stories,
                                                     caplog):
        """A handle outside the per-call map is skipped and warned about; siblings survive.

        Covers the production failure directly: a truncated id ("2026091212"), an
        invented recent handle ("r9"), and a new-side handle used where a recent
        one belongs ("n1" as duplicate_of).
        """
        dedup.client.messages.create.return_value = _make_tool_response([
            {"new_id": "2026091212", "duplicate_of": "r1", "confidence": 0.95},
            {"new_id": "n1", "duplicate_of": "r9", "confidence": 0.95},
            {"new_id": "n2", "duplicate_of": "n1", "confidence": 0.95},
            {"new_id": "n3", "duplicate_of": "r1", "confidence": 0.9},
        ])

        with caplog.at_level(logging.WARNING, logger="src.semantic_dedup"):
            duplicates = dedup.find_duplicates(new_stories[:3], recent_stories[:2])

        # The one valid result survives; nothing is invented for the bad handles.
        assert duplicates == {NEW_GUIDS[2]: RECENT_GUIDS[0]}

        warnings = [
            record.getMessage() for record in caplog.records
            if record.levelno == logging.WARNING
        ]
        assert any("2026091212" in message for message in warnings), warnings
        assert any("r9" in message for message in warnings), warnings
        # The swapped handle gets its own warning, distinct from the other two.
        swapped = [
            message for message in warnings
            if "n1" in message and "r9" not in message and "2026091212" not in message
        ]
        assert swapped, f"expected a warning naming 'n1' used as a duplicate_of handle: {warnings}"

    def test_max_tokens_truncation_raises_unavailable(self, dedup, new_stories,
                                                      recent_stories, caplog):
        """A truncated verdict list is an incomplete answer, so nothing is returned.

        stop_reason "max_tokens" means verdicts were cut off, so candidates past
        the cut-off got no answer at all. Returning the surviving verdicts let
        the caller treat unjudged candidates as cleared and post them.
        """
        dedup.client.messages.create.return_value = _make_tool_response(
            [{"new_id": "n1", "duplicate_of": "r1", "confidence": 0.9}],
            stop_reason="max_tokens",
        )

        with caplog.at_level(logging.WARNING, logger="src.semantic_dedup"):
            with pytest.raises(SemanticDedupUnavailable):
                dedup.find_duplicates(new_stories[:1], recent_stories[:1])

        assert _warnings_mentioning(caplog, "max_tokens"), _warning_messages(caplog)

    def test_max_tokens_without_tool_block_raises_unavailable(self, dedup, new_stories,
                                                              recent_stories, caplog):
        """Truncation before any tool_use block is unavailability, and still warns."""
        mock_response = Mock()
        mock_text_block = Mock()
        mock_text_block.type = "text"
        mock_text_block.text = "Comparing the articles"
        mock_response.content = [mock_text_block]
        mock_response.stop_reason = "max_tokens"
        dedup.client.messages.create.return_value = mock_response

        with caplog.at_level(logging.WARNING, logger="src.semantic_dedup"):
            with pytest.raises(SemanticDedupUnavailable):
                dedup.find_duplicates(new_stories[:1], recent_stories[:1])

        assert _warnings_mentioning(caplog, "max_tokens"), _warning_messages(caplog)

    def test_uncovered_candidates_get_one_warning_and_are_not_duplicates(
            self, dedup, new_stories, recent_stories, caplog):
        """Candidates the model never mentioned are reported once and allowed through.

        Verdicts did arrive, so this is an answer, not an outage. The handles
        with no verdict are named once so a model that quietly drops candidates
        is visible in the logs.
        """
        dedup.client.messages.create.return_value = _make_tool_response([
            {"new_id": "n1", "duplicate_of": "r1", "confidence": 0.9},
            {"new_id": "n2", "duplicate_of": "", "confidence": 0.0},
        ])

        with caplog.at_level(logging.WARNING, logger="src.semantic_dedup"):
            duplicates = dedup.find_duplicates(new_stories[:3], recent_stories[:2])

        assert duplicates == {NEW_GUIDS[0]: RECENT_GUIDS[0]}
        missing = _warnings_mentioning(caplog, "n3")
        assert len(missing) == 1, _warning_messages(caplog)

    def test_repeated_verdict_for_one_candidate_keeps_the_first(
            self, dedup, new_stories, recent_stories, caplog):
        """Two verdicts for the same handle: the first wins and the clash is logged.

        A model that reports the same candidate twice is contradicting itself.
        Letting the later verdict overwrite the earlier one made the recorded
        duplicate_of depend on result order.
        """
        dedup.client.messages.create.return_value = _make_tool_response([
            {"new_id": "n1", "duplicate_of": "r1", "confidence": 0.9},
            {"new_id": "n1", "duplicate_of": "r2", "confidence": 0.95},
        ])

        with caplog.at_level(logging.WARNING, logger="src.semantic_dedup"):
            duplicates = dedup.find_duplicates(new_stories[:1], recent_stories[:2])

        assert duplicates == {NEW_GUIDS[0]: RECENT_GUIDS[0]}
        assert _warnings_mentioning(caplog, "n1"), _warning_messages(caplog)

    def test_malformed_result_fields_are_skipped_with_warnings(
            self, dedup, new_stories, recent_stories, caplog):
        """A result with wrongly typed fields is dropped; well-formed siblings survive.

        The tool schema is a request, not a guarantee: a string confidence or a
        numeric handle used to reach the threshold comparison and the handle
        lookup, where it either raised TypeError or matched nothing silently.
        """
        dedup.client.messages.create.return_value = _make_tool_response([
            {"new_id": "n1", "duplicate_of": "r1", "confidence": "high"},
            {"new_id": 7, "duplicate_of": "r1", "confidence": 0.95},
            {"new_id": "n2", "duplicate_of": "r1", "confidence": 0.9},
        ])

        with caplog.at_level(logging.WARNING, logger="src.semantic_dedup"):
            duplicates = dedup.find_duplicates(new_stories[:2], recent_stories[:1])

        assert duplicates == {NEW_GUIDS[1]: RECENT_GUIDS[0]}
        assert _warnings_mentioning(caplog, "n1"), _warning_messages(caplog)
        assert _warnings_mentioning(caplog, "7"), _warning_messages(caplog)

    def test_max_tokens_budget_scales_with_batch_size(self, dedup, new_stories,
                                                      recent_stories):
        """The token budget grows with the candidate count, so batches are not truncated.

        A fixed 1024 budget fit roughly twenty verdicts; a larger feed ran past
        it and came back truncated.
        """
        dedup.client.messages.create.return_value = _make_tool_response([
            {"new_id": "n1", "duplicate_of": "", "confidence": 0.0},
        ])
        dedup.find_duplicates(new_stories[:1], recent_stories[:1])
        assert dedup.client.messages.create.call_args.kwargs["max_tokens"] == 256 + 48 * 1

        dedup.client.messages.create.return_value = _make_tool_response([
            {"new_id": "n1", "duplicate_of": "", "confidence": 0.0},
            {"new_id": "n2", "duplicate_of": "", "confidence": 0.0},
            {"new_id": "n3", "duplicate_of": "", "confidence": 0.0},
        ])
        dedup.find_duplicates(new_stories[:3], recent_stories[:1])
        assert dedup.client.messages.create.call_args.kwargs["max_tokens"] == 256 + 48 * 3

    def test_only_unknown_handle_verdict_raises_unavailable(self, dedup, new_stories,
                                                            recent_stories, caplog):
        """A verdict list that survives no validation answered nothing.

        Every result was discarded, so no candidate was judged. Returning {} here
        read as "the model cleared all of them" and posted the batch.
        """
        dedup.client.messages.create.return_value = _make_tool_response([
            {"new_id": "2026091212", "duplicate_of": "r1", "confidence": 0.9},
        ])

        with caplog.at_level(logging.WARNING, logger="src.semantic_dedup"):
            with pytest.raises(SemanticDedupUnavailable):
                dedup.find_duplicates(new_stories[:2], recent_stories[:2])

        assert _warning_messages(caplog), "the unusable verdict list must be logged first"

    def test_only_malformed_verdicts_raise_unavailable(self, dedup, new_stories,
                                                       recent_stories, caplog):
        """Wrongly typed fields on every result leave zero usable verdicts."""
        dedup.client.messages.create.return_value = _make_tool_response([
            {"new_id": "n1", "duplicate_of": "r1", "confidence": "high"},
            {"new_id": "n2", "duplicate_of": 7, "confidence": 0.9},
        ])

        with caplog.at_level(logging.WARNING, logger="src.semantic_dedup"):
            with pytest.raises(SemanticDedupUnavailable):
                dedup.find_duplicates(new_stories[:2], recent_stories[:2])

        assert _warning_messages(caplog), "the unusable verdict list must be logged first"

    def test_malformed_verdict_does_not_consume_a_candidates_turn(
            self, dedup, new_stories, recent_stories, caplog):
        """A discarded verdict leaves the candidate unjudged, so a later valid one counts.

        Treating the discarded result as the candidate's verdict made the real one
        look like a contradictory repeat: it was dropped, the duplicate was posted,
        and the logs claimed full coverage.
        """
        dedup.client.messages.create.return_value = _make_tool_response([
            {"new_id": "n1", "duplicate_of": "r1", "confidence": "high"},
            {"new_id": "n1", "duplicate_of": "r1", "confidence": 0.9},
            {"new_id": "n2", "duplicate_of": "", "confidence": 0.0},
        ])

        with caplog.at_level(logging.WARNING, logger="src.semantic_dedup"):
            duplicates = dedup.find_duplicates(new_stories[:2], recent_stories[:2])

        assert duplicates == {NEW_GUIDS[0]: RECENT_GUIDS[0]}
        # Both candidates ended up with a usable verdict, so neither is uncovered
        # and the second n1 result is the first usable one, not a repeat.
        assert not _warnings_mentioning(caplog, "no verdict"), _warning_messages(caplog)
        assert not _warnings_mentioning(caplog, "repeated"), _warning_messages(caplog)

    def test_candidate_left_with_only_a_malformed_verdict_is_uncovered(
            self, dedup, new_stories, recent_stories, caplog):
        """One usable verdict is an answer, and the discarded candidate is named.

        The batch was judged, so this is not unavailability; but the candidate
        whose only result was thrown away was never actually judged, and the log
        must say so rather than imply it was cleared.
        """
        dedup.client.messages.create.return_value = _make_tool_response([
            {"new_id": "n1", "duplicate_of": "r1", "confidence": "high"},
            {"new_id": "n2", "duplicate_of": "", "confidence": 0.0},
        ])

        with caplog.at_level(logging.WARNING, logger="src.semantic_dedup"):
            duplicates = dedup.find_duplicates(new_stories[:2], recent_stories[:2])

        assert duplicates == {}
        uncovered = [
            message for message in _warnings_mentioning(caplog, "no verdict")
            if "n1" in message
        ]
        assert uncovered, _warning_messages(caplog)
