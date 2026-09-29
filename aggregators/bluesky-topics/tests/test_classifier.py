"""
Tests for the topic classifier: request shape, batching, verdict mapping,
and failure handling. The Anthropic client is a fake object except for one
real-SDK offline test that captures the wire request through a mock transport.

Convention assumed for the user message: it is a plain string (or a list of
text blocks whose texts are concatenated) that contains the topic description
and whose LAST LINE is the JSON array of candidate objects, serialized on one
line with ``json.dumps`` (no indent).
"""
import json
import math
from datetime import datetime, timezone

import anthropic
import httpx2
import pydantic
import pytest

from src.classifier import (
    ClassificationBatch,
    ClassificationError,
    TopicClassifier,
    Verdict,
)
from src.models import BlueskyPost

NOW = datetime(2026, 9, 18, 12, 0, 0, tzinfo=timezone.utc)
MODEL = "claude-opus-5"
TOPIC_DESCRIPTION = "Posts about the NBA: teams, players, games, trades, injuries, league news."


def make_post(index=0, **overrides) -> BlueskyPost:
    fields = {
        "uri": f"at://did:plc:author{index}/app.bsky.feed.post/p{index}",
        "cid": f"bafyp{index}",
        "author_did": f"did:plc:author{index}",
        "author_handle": f"author{index}.bsky.social",
        "text": f"post number {index}",
        "created_at": NOW,
    }
    fields.update(overrides)
    return BlueskyPost(**fields)


def make_posts(count: int):
    return [make_post(index) for index in range(count)]


DEFAULT_BATCH_SIZE = 200
LEGACY_BATCH_SIZE = 25


def batch(*verdicts) -> ClassificationBatch:
    """
    Build a canned ClassificationBatch from (id, on_topic, confidence[, story])
    tuples. `story` is required by the schema, so a canned verdict without one
    gets a placeholder key unique to its id.
    """
    built = []
    for entry in verdicts:
        id_, on_topic, confidence = entry[:3]
        story = entry[3] if len(entry) > 3 else f"story-{id_}"
        built.append(Verdict(id=id_, on_topic=on_topic, confidence=confidence, story=story))
    return ClassificationBatch(verdicts=built)


def all_on_topic(count: int) -> ClassificationBatch:
    return batch(*[(index, True, 0.9) for index in range(count)])


class FakeParsedResponse:
    def __init__(self, parsed_output):
        self.parsed_output = parsed_output


class FakeMessages:
    """Records every parse() call and returns queued outputs in order."""

    def __init__(self, outputs):
        self.outputs = list(outputs)
        self.calls = []

    def parse(self, **kwargs):
        self.calls.append(kwargs)
        output = self.outputs.pop(0)
        if isinstance(output, Exception):
            raise output
        return FakeParsedResponse(output)


class FakeClient:
    def __init__(self, *outputs):
        self.messages = FakeMessages(outputs)

    @property
    def calls(self):
        return self.messages.calls


def user_message_text(call_kwargs) -> str:
    messages = call_kwargs["messages"]
    assert len(messages) == 1
    assert messages[0]["role"] == "user"
    content = messages[0]["content"]
    if isinstance(content, str):
        return content
    return "".join(block["text"] for block in content if block.get("type") == "text")


def candidate_array(call_kwargs):
    text = user_message_text(call_kwargs).rstrip()
    return json.loads(text.splitlines()[-1])


def wire_verdict_schema(schema):
    """Resolve the JSON schema of one element of `verdicts` as sent on the wire."""
    items = schema["properties"]["verdicts"]["items"]
    if "$ref" in items:
        definition_name = items["$ref"].rsplit("/", 1)[1]
        return schema["$defs"][definition_name]
    return items


def connection_error() -> anthropic.APIConnectionError:
    return anthropic.APIConnectionError(request=httpx2.Request("POST", "https://anthropic.test/v1/messages"))


def status_error(status_code: int) -> anthropic.APIStatusError:
    request = httpx2.Request("POST", "https://anthropic.test/v1/messages")
    response = httpx2.Response(status_code, request=request, json={"error": {"message": "boom"}})
    return anthropic.APIStatusError("boom", response=response, body={"error": {"message": "boom"}})


def response_validation_error() -> anthropic.APIResponseValidationError:
    # A direct APIError subclass that is neither a connection nor a status error.
    request = httpx2.Request("POST", "https://anthropic.test/v1/messages")
    response = httpx2.Response(200, request=request, json=[])
    return anthropic.APIResponseValidationError(response=response, body=[])


def validation_error() -> pydantic.ValidationError:
    # A bare JSON array is what the live gateway returned; it fails schema validation.
    try:
        ClassificationBatch.model_validate([])
    except pydantic.ValidationError as error:
        return error
    raise AssertionError("expected model_validate([]) to raise ValidationError")


def json_decode_error() -> json.JSONDecodeError:
    try:
        json.loads("Invalid JSON: expected value at line 1 column 1")
    except json.JSONDecodeError as error:
        return error
    raise AssertionError("expected json.loads to raise JSONDecodeError")


class TestRequestShape:
    def test_passes_model_effort_output_format_and_cached_system_prompt(self):
        client = FakeClient(all_on_topic(2))
        classifier = TopicClassifier(client, model=MODEL)

        classifier.classify(TOPIC_DESCRIPTION, make_posts(2), bios={})

        assert len(client.calls) == 1
        call = client.calls[0]
        assert call["model"] == MODEL
        assert call["output_config"]["effort"] == "low"
        assert call["output_format"] is ClassificationBatch
        assert call["max_tokens"] == 16000
        system = call["system"]
        assert isinstance(system, list) and system
        assert system[0]["type"] == "text"
        assert system[0]["cache_control"] == {"type": "ephemeral"}
        assert system[0]["text"].strip()

    def test_system_prompt_describes_the_story_key_contract(self):
        client = FakeClient(all_on_topic(1))
        classifier = TopicClassifier(client, model=MODEL)

        classifier.classify(TOPIC_DESCRIPTION, make_posts(1), bios={})

        assert "story" in client.calls[0]["system"][0]["text"]

    def test_user_message_contains_topic_description_and_candidate_array(self):
        posts = [
            make_post(0, author_handle="lebron.bsky.social", text="Lakers win again"),
            make_post(1, author_handle="curry.bsky.social", text="Warriors practice"),
        ]
        bios = {posts[0].author_did: "NBA reporter", posts[1].author_did: "Coach"}
        client = FakeClient(all_on_topic(2))
        classifier = TopicClassifier(client, model=MODEL)

        classifier.classify(TOPIC_DESCRIPTION, posts, bios)

        call = client.calls[0]
        assert TOPIC_DESCRIPTION in user_message_text(call)
        assert candidate_array(call) == [
            {"id": 0, "handle": "lebron.bsky.social", "bio": "NBA reporter", "text": "Lakers win again"},
            {"id": 1, "handle": "curry.bsky.social", "bio": "Coach", "text": "Warriors practice"},
        ]

    def test_missing_bio_is_sent_as_empty_string(self):
        client = FakeClient(all_on_topic(1))
        classifier = TopicClassifier(client, model=MODEL)

        classifier.classify(TOPIC_DESCRIPTION, make_posts(1), bios={})

        assert candidate_array(client.calls[0])[0]["bio"] == ""

    def test_truncates_bio_to_200_and_text_to_500_characters(self):
        post = make_post(0, text="t" * 700)
        client = FakeClient(all_on_topic(1))
        classifier = TopicClassifier(client, model=MODEL)

        classifier.classify(TOPIC_DESCRIPTION, [post], bios={post.author_did: "b" * 300})

        candidate = candidate_array(client.calls[0])[0]
        assert len(candidate["bio"]) == 200
        assert len(candidate["text"]) == 500
        assert candidate["bio"] == "b" * 200
        assert candidate["text"] == "t" * 500


class TestBatching:
    def test_default_batch_size_is_200(self):
        # Story keys are only consistent within one call, so a default run of
        # up to classify_candidates (max 200) posts is a single call.
        client = FakeClient(all_on_topic(DEFAULT_BATCH_SIZE), all_on_topic(1))
        classifier = TopicClassifier(client, model=MODEL)

        classifier.classify(TOPIC_DESCRIPTION, make_posts(DEFAULT_BATCH_SIZE + 1), bios={})

        assert len(client.calls) == 2
        assert len(candidate_array(client.calls[0])) == DEFAULT_BATCH_SIZE
        assert len(candidate_array(client.calls[1])) == 1

    def test_default_run_of_fifty_posts_is_one_call(self):
        client = FakeClient(all_on_topic(50))
        classifier = TopicClassifier(client, model=MODEL)

        classifier.classify(TOPIC_DESCRIPTION, make_posts(50), bios={})

        assert len(client.calls) == 1

    def test_ids_restart_at_zero_in_each_batch(self):
        client = FakeClient(all_on_topic(25), all_on_topic(1))
        classifier = TopicClassifier(client, model=MODEL, batch_size=LEGACY_BATCH_SIZE)

        classifier.classify(TOPIC_DESCRIPTION, make_posts(26), bios={})

        first_ids = [candidate["id"] for candidate in candidate_array(client.calls[0])]
        second_ids = [candidate["id"] for candidate in candidate_array(client.calls[1])]
        assert first_ids == list(range(25))
        assert second_ids == [0]
        assert candidate_array(client.calls[1])[0]["handle"] == "author25.bsky.social"

    def test_custom_batch_size_is_respected(self):
        client = FakeClient(all_on_topic(2), all_on_topic(2), all_on_topic(1))
        classifier = TopicClassifier(client, model=MODEL, batch_size=2)

        verdicts = classifier.classify(TOPIC_DESCRIPTION, make_posts(5), bios={})

        assert [len(candidate_array(call)) for call in client.calls] == [2, 2, 1]
        assert [verdict.id for verdict in verdicts] == [0, 1, 2, 3, 4]

    def test_verdicts_map_by_batch_local_id_not_position_and_carry_global_ids(self):
        # Reverse the order inside each batch, and give distinct values per post,
        # so a positional mapping would produce the wrong answer.
        first = batch(*[(index, index % 2 == 0, index / 100) for index in reversed(range(25))])
        second = batch((0, True, 0.55))
        client = FakeClient(first, second)
        classifier = TopicClassifier(client, model=MODEL, batch_size=LEGACY_BATCH_SIZE)

        verdicts = classifier.classify(TOPIC_DESCRIPTION, make_posts(26), bios={})

        assert len(verdicts) == 26
        assert [verdict.id for verdict in verdicts] == list(range(26))
        for index in range(25):
            assert verdicts[index].on_topic is (index % 2 == 0)
            assert verdicts[index].confidence == pytest.approx(index / 100)
        assert verdicts[25].on_topic is True
        assert verdicts[25].confidence == pytest.approx(0.55)

    def test_empty_posts_returns_empty_list_without_calling_the_api(self):
        client = FakeClient()
        classifier = TopicClassifier(client, model=MODEL)

        assert classifier.classify(TOPIC_DESCRIPTION, [], bios={}) == []
        assert client.calls == []


class TestVerdictSemantics:
    def test_duplicate_id_first_wins(self):
        client = FakeClient(batch((0, True, 0.9), (0, False, 0.1), (1, False, 0.2)))
        classifier = TopicClassifier(client, model=MODEL)

        verdicts = classifier.classify(TOPIC_DESCRIPTION, make_posts(2), bios={})

        assert verdicts[0].on_topic is True
        assert verdicts[0].confidence == pytest.approx(0.9)
        assert verdicts[1].on_topic is False

    def test_unknown_and_negative_ids_are_ignored(self):
        client = FakeClient(batch((99, True, 0.9), (-1, True, 0.9), (0, True, 0.8), (1, False, 0.3)))
        classifier = TopicClassifier(client, model=MODEL)

        verdicts = classifier.classify(TOPIC_DESCRIPTION, make_posts(2), bios={})

        assert len(verdicts) == 2
        assert [verdict.id for verdict in verdicts] == [0, 1]
        assert verdicts[0].on_topic is True
        assert verdicts[0].confidence == pytest.approx(0.8)
        assert verdicts[1].on_topic is False

    def test_missing_id_becomes_off_topic(self):
        client = FakeClient(batch((0, True, 0.9), (2, True, 0.9)))
        classifier = TopicClassifier(client, model=MODEL)

        verdicts = classifier.classify(TOPIC_DESCRIPTION, make_posts(3), bios={})

        assert len(verdicts) == 3
        assert verdicts[1].id == 1
        assert verdicts[1].on_topic is False
        assert verdicts[0].on_topic is True
        assert verdicts[2].on_topic is True

    def test_missing_id_story_is_the_posts_uri(self):
        posts = make_posts(3)
        client = FakeClient(batch((0, True, 0.9, "cavs-gm"), (2, True, 0.9, "cavs-gm")))
        classifier = TopicClassifier(client, model=MODEL)

        verdicts = classifier.classify(TOPIC_DESCRIPTION, posts, bios={})

        assert verdicts[1].story == posts[1].uri
        assert verdicts[0].story == "cavs-gm"
        assert verdicts[2].story == "cavs-gm"

    def test_non_finite_confidence_becomes_zero(self):
        client = FakeClient(batch((0, True, math.nan), (1, True, math.inf), (2, True, -math.inf)))
        classifier = TopicClassifier(client, model=MODEL)

        verdicts = classifier.classify(TOPIC_DESCRIPTION, make_posts(3), bios={})

        assert [verdict.confidence for verdict in verdicts] == [0.0, 0.0, 0.0]
        assert all(verdict.on_topic is True for verdict in verdicts)

    def test_confidence_is_clamped_to_unit_interval(self):
        client = FakeClient(batch((0, True, 1.7), (1, True, -0.4), (2, True, 0.5)))
        classifier = TopicClassifier(client, model=MODEL)

        verdicts = classifier.classify(TOPIC_DESCRIPTION, make_posts(3), bios={})

        assert [verdict.confidence for verdict in verdicts] == [1.0, 0.0, 0.5]


class TestStoryKeys:
    def test_verdicts_carry_the_story_key_returned_by_the_model(self):
        client = FakeClient(batch((0, True, 0.9, "ausar-extension"), (1, True, 0.8, "ausar-extension"), (2, False, 0.3, "cavs-gm")))
        classifier = TopicClassifier(client, model=MODEL)

        verdicts = classifier.classify(TOPIC_DESCRIPTION, make_posts(3), bios={})

        assert [verdict.story for verdict in verdicts] == ["ausar-extension", "ausar-extension", "cavs-gm"]

    @pytest.mark.parametrize(
        ("raw", "expected"),
        [
            ("Ausar Thompson -- Extension!", "ausar-thompson-extension"),
            ("CAVS GM", "cavs-gm"),
            ("  duren_2027  ", "duren-2027"),
            ("--larsson---extension--", "larsson-extension"),
            ("Trade: Lakers/Kings (rumor)", "trade-lakers-kings-rumor"),
            ("already-normal", "already-normal"),
        ],
    )
    def test_story_keys_are_normalized(self, raw, expected):
        client = FakeClient(batch((0, True, 0.9, raw)))
        classifier = TopicClassifier(client, model=MODEL)

        verdicts = classifier.classify(TOPIC_DESCRIPTION, make_posts(1), bios={})

        assert verdicts[0].story == expected

    def test_two_verdicts_whose_keys_normalize_alike_share_a_story(self):
        client = FakeClient(batch((0, True, 0.9, "Ausar Extension"), (1, True, 0.9, "ausar--extension!")))
        classifier = TopicClassifier(client, model=MODEL)

        verdicts = classifier.classify(TOPIC_DESCRIPTION, make_posts(2), bios={})

        assert verdicts[0].story == verdicts[1].story == "ausar-extension"

    @pytest.mark.parametrize("raw", ["", "   ", "---", "!!! ???", "\t\n"])
    def test_empty_or_punctuation_only_key_becomes_the_posts_uri(self, raw):
        posts = make_posts(2)
        client = FakeClient(batch((0, True, 0.9, raw), (1, True, 0.9, "cavs-gm")))
        classifier = TopicClassifier(client, model=MODEL)

        verdicts = classifier.classify(TOPIC_DESCRIPTION, posts, bios={})

        assert verdicts[0].story == posts[0].uri
        assert verdicts[1].story == "cavs-gm"

    def test_story_keys_map_by_batch_local_id_across_batches(self):
        posts = make_posts(3)
        first = batch((1, True, 0.9, "second-story"), (0, True, 0.9, "first-story"))
        second = batch((0, True, 0.9, "third-story"))
        client = FakeClient(first, second)
        classifier = TopicClassifier(client, model=MODEL, batch_size=2)

        verdicts = classifier.classify(TOPIC_DESCRIPTION, posts, bios={})

        assert [verdict.story for verdict in verdicts] == ["first-story", "second-story", "third-story"]

    def test_verdict_schema_requires_story(self):
        with pytest.raises(pydantic.ValidationError):
            Verdict.model_validate({"id": 0, "on_topic": True, "confidence": 0.9})


class TestFailures:
    def test_parsed_output_that_is_not_a_batch_raises_classification_error(self):
        client = FakeClient(None)
        classifier = TopicClassifier(client, model=MODEL)

        with pytest.raises(ClassificationError):
            classifier.classify(TOPIC_DESCRIPTION, make_posts(1), bios={})

    def test_api_connection_error_raises_classification_error(self):
        client = FakeClient(connection_error())
        classifier = TopicClassifier(client, model=MODEL)

        with pytest.raises(ClassificationError):
            classifier.classify(TOPIC_DESCRIPTION, make_posts(1), bios={})

    def test_api_status_error_raises_classification_error(self):
        client = FakeClient(status_error(529))
        classifier = TopicClassifier(client, model=MODEL)

        with pytest.raises(ClassificationError):
            classifier.classify(TOPIC_DESCRIPTION, make_posts(1), bios={})

    def test_pydantic_validation_error_from_parse_raises_classification_error(self):
        # The SDK's parse() raises pydantic.ValidationError when the model's text
        # block is not the requested schema (e.g. a bare JSON array, or non-JSON
        # text through an OpenAI-compatible gateway).
        client = FakeClient(validation_error())
        classifier = TopicClassifier(client, model=MODEL)

        with pytest.raises(ClassificationError) as raised:
            classifier.classify(TOPIC_DESCRIPTION, make_posts(1), bios={})

        assert isinstance(raised.value.__cause__, pydantic.ValidationError)

    def test_json_decode_error_from_parse_raises_classification_error(self):
        # The SDK can raise json.JSONDecodeError when the text block is not JSON at all.
        client = FakeClient(json_decode_error())
        classifier = TopicClassifier(client, model=MODEL)

        with pytest.raises(ClassificationError) as raised:
            classifier.classify(TOPIC_DESCRIPTION, make_posts(1), bios={})

        assert isinstance(raised.value.__cause__, json.JSONDecodeError)

    def test_api_response_validation_error_raises_classification_error(self):
        client = FakeClient(response_validation_error())
        classifier = TopicClassifier(client, model=MODEL)

        with pytest.raises(ClassificationError) as raised:
            classifier.classify(TOPIC_DESCRIPTION, make_posts(1), bios={})

        assert isinstance(raised.value.__cause__, anthropic.APIResponseValidationError)

    def test_failure_on_second_batch_raises_and_returns_nothing_partial(self):
        client = FakeClient(all_on_topic(25), status_error(500))
        classifier = TopicClassifier(client, model=MODEL, batch_size=LEGACY_BATCH_SIZE)

        with pytest.raises(ClassificationError):
            classifier.classify(TOPIC_DESCRIPTION, make_posts(26), bios={})

        assert len(client.calls) == 2


class TestRequiredIds:
    """Posts whose verdict the caller depends on (story-log entries fed in for dedup)."""

    def test_missing_verdict_for_a_required_id_raises_classification_error(self):
        client = FakeClient(batch((0, True, 0.9)))
        classifier = TopicClassifier(client, model=MODEL)

        with pytest.raises(ClassificationError) as raised:
            classifier.classify(TOPIC_DESCRIPTION, make_posts(2), bios={}, required_ids=[1])

        assert "required" in str(raised.value)

    def test_missing_verdict_for_a_non_required_id_is_still_lenient(self):
        posts = make_posts(2)
        client = FakeClient(batch((0, True, 0.9, "cavs-gm")))
        classifier = TopicClassifier(client, model=MODEL)

        verdicts = classifier.classify(TOPIC_DESCRIPTION, posts, bios={}, required_ids=[0])

        assert [verdict.id for verdict in verdicts] == [0, 1]
        assert verdicts[1].on_topic is False
        assert verdicts[1].story == posts[1].uri

    def test_required_id_with_a_verdict_is_returned_normally(self):
        client = FakeClient(batch((0, True, 0.9, "cavs-gm"), (1, True, 0.8, "cavs-gm")))
        classifier = TopicClassifier(client, model=MODEL)

        verdicts = classifier.classify(TOPIC_DESCRIPTION, make_posts(2), bios={}, required_ids=[1])

        assert [verdict.story for verdict in verdicts] == ["cavs-gm", "cavs-gm"]


class TestRealSdkOffline:
    def test_real_sdk_request_body_and_response_mapping(self):
        recorded = []
        canned_verdicts = [
            {"id": 2, "on_topic": True, "confidence": 0.95, "story": "Ausar Thompson -- Extension!"},
            {"id": 0, "on_topic": False, "confidence": 0.2, "story": "college-football"},
            {"id": 1, "on_topic": True, "confidence": 0.8, "story": "ausar-thompson-extension"},
        ]

        def handler(request: httpx2.Request) -> httpx2.Response:
            recorded.append(request)
            body = {
                "id": "msg_1",
                "type": "message",
                "role": "assistant",
                "model": MODEL,
                "content": [{"type": "text", "text": json.dumps({"verdicts": canned_verdicts})}],
                "stop_reason": "end_turn",
                "stop_sequence": None,
                "usage": {"input_tokens": 1, "output_tokens": 1},
            }
            return httpx2.Response(200, json=body)

        client = anthropic.Anthropic(
            api_key="test-key",
            base_url="https://anthropic.test",
            http_client=httpx2.Client(transport=httpx2.MockTransport(handler)),
            max_retries=0,
        )
        classifier = TopicClassifier(client, model=MODEL)
        posts = make_posts(3)

        verdicts = classifier.classify(TOPIC_DESCRIPTION, posts, bios={posts[0].author_did: "Reporter"})

        assert len(recorded) == 1
        request = recorded[0]
        assert request.url.path.endswith("/v1/messages")
        body = json.loads(request.read())
        assert body["model"] == MODEL
        assert body["output_config"]["effort"] == "low"
        assert body["output_config"]["format"]["type"] == "json_schema"
        assert body["system"][0]["type"] == "text"
        assert body["system"][0]["cache_control"] == {"type": "ephemeral"}
        assert len(body["messages"]) == 1
        assert body["messages"][0]["role"] == "user"
        assert "story" in body["system"][0]["text"]
        assert body["max_tokens"] == 16000
        verdict_schema = wire_verdict_schema(body["output_config"]["format"]["schema"])
        assert "story" in verdict_schema["properties"]
        assert verdict_schema["properties"]["story"]["type"] == "string"
        assert "story" in verdict_schema["required"]
        candidates = candidate_array({"messages": body["messages"]})
        assert len(candidates) <= 200
        assert [candidate["id"] for candidate in candidates] == [0, 1, 2]
        assert candidates[0]["bio"] == "Reporter"
        assert set(candidates[0]) == {"id", "handle", "bio", "text"}

        assert [verdict.id for verdict in verdicts] == [0, 1, 2]
        assert [verdict.on_topic for verdict in verdicts] == [False, True, True]
        assert [verdict.confidence for verdict in verdicts] == pytest.approx([0.2, 0.8, 0.95])
        assert [verdict.story for verdict in verdicts] == [
            "college-football",
            "ausar-thompson-extension",
            "ausar-thompson-extension",
        ]
