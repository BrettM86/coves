"""
Topic classifier: asks Claude whether candidate Bluesky posts are on-topic.

Posts are sent in batches with batch-local ids; verdicts are mapped back to
the caller's input order, one Verdict per input post. Post text and the API
key are never logged; warnings cite ids only.
"""
import json
import logging
import math
import re
from typing import Any, Collection, Dict, List, Mapping, Sequence, Tuple

import anthropic
import pydantic
from pydantic import BaseModel

from src.models import BlueskyPost

logger = logging.getLogger(__name__)

DEFAULT_BATCH_SIZE = 200
MAX_OUTPUT_TOKENS = 16000
BIO_MAX_CHARACTERS = 200
TEXT_MAX_CHARACTERS = 500

# Module constant so the bytes are stable across runs and prompt caching hits.
SYSTEM_PROMPT = """You classify social media posts for a topic-specific community feed.

You will receive a topic description followed by a JSON array of candidate posts. Each candidate has an integer "id", the author's "handle", the author's profile "bio", and the post "text".

For each candidate, decide whether the post is PRIMARILY about the topic described. Rules:
- Judge the post text itself. A post is on-topic only when its main subject is the topic, not when it merely mentions it in passing.
- The author's bio is context, not evidence. An author who usually posts about the topic can still post about something else; such a post is off-topic.
- A post about a different sport, league, show, or subject from an on-topic author is off-topic, even if the author is a well-known figure in the topic.
- Promotional, meta, or feed-announcement posts are off-topic unless their main subject is the topic itself.
- When the text is too short or ambiguous to tell, mark it off-topic.

Return exactly one verdict per candidate id, using the same ids you were given. Each verdict has "id", "on_topic" (true or false), "confidence" (a calibrated probability between 0 and 1 that your on_topic decision is correct), and "story": a short lowercase hyphenated key naming the specific news event or subject of the post (e.g. ausar-thompson-extension). Posts about the same event MUST share the same story key. Posts that are general chatter with no specific subject get a story key unique to that post."""


class ClassificationError(Exception):
    """Raised when a classification batch cannot be completed."""


class Verdict(BaseModel):
    id: int
    on_topic: bool
    confidence: float
    story: str


class ClassificationBatch(BaseModel):
    verdicts: List[Verdict]


class TopicClassifier:
    """Classifies posts against a topic description using an injected Anthropic client."""

    def __init__(self, client: Any, model: str, batch_size: int = DEFAULT_BATCH_SIZE) -> None:
        if batch_size < 1:
            raise ValueError(f"batch_size must be at least 1, got {batch_size}")
        self.client = client
        self.model = model
        self.batch_size = batch_size

    def classify(
        self,
        topic_description: str,
        posts: Sequence[BlueskyPost],
        bios: Mapping[str, str],
        *,
        required_ids: Collection[int] = (),
    ) -> List[Verdict]:
        """
        Return one Verdict per post, in input order, with ids equal to the
        post's index in ``posts``.

        ``required_ids`` are indexes into ``posts`` whose verdict the caller
        depends on; a missing verdict for one of them is an error rather than
        the lenient off-topic fallback used for every other post.

        Raises:
            ClassificationError: on any Anthropic API failure, when a batch
                returns output that is not a ClassificationBatch, or when the
                model returned no verdict for a required id. Nothing partial
                is returned.
        """
        required = set(required_ids)
        verdicts: List[Verdict] = []
        for batch_start in range(0, len(posts), self.batch_size):
            batch_posts = posts[batch_start : batch_start + self.batch_size]
            parsed = self._classify_batch(topic_description, batch_posts, bios)
            mapped, missing_ids = self._map_verdicts(parsed, batch_start, batch_posts)
            missing_required = required.intersection(missing_ids)
            if missing_required:
                raise ClassificationError(
                    f"Classifier returned no verdict for {len(missing_required)} required id(s)"
                )
            verdicts.extend(mapped)
        return verdicts

    def _classify_batch(
        self,
        topic_description: str,
        batch_posts: Sequence[BlueskyPost],
        bios: Mapping[str, str],
    ) -> ClassificationBatch:
        candidates = [
            {
                "id": local_id,
                "handle": post.author_handle,
                "bio": bios.get(post.author_did, "")[:BIO_MAX_CHARACTERS],
                "text": post.text[:TEXT_MAX_CHARACTERS],
            }
            for local_id, post in enumerate(batch_posts)
        ]
        user_message = (
            f"Topic description:\n{topic_description}\n\n"
            f"Candidates (JSON array):\n{json.dumps(candidates)}"
        )

        try:
            response = self.client.messages.parse(
                model=self.model,
                max_tokens=MAX_OUTPUT_TOKENS,
                system=[
                    {
                        "type": "text",
                        "text": SYSTEM_PROMPT,
                        "cache_control": {"type": "ephemeral"},
                    }
                ],
                output_config={"effort": "low"},
                output_format=ClassificationBatch,
                messages=[{"role": "user", "content": user_message}],
            )
        except anthropic.APIError as error:
            raise ClassificationError(f"Anthropic API request failed: {type(error).__name__}") from error
        except (pydantic.ValidationError, json.JSONDecodeError) as error:
            raise ClassificationError(
                f"Classifier output did not match the expected schema: {type(error).__name__}"
            ) from error

        parsed = getattr(response, "parsed_output", None)
        if not isinstance(parsed, ClassificationBatch):
            raise ClassificationError(
                f"Classifier returned unexpected output type: {type(parsed).__name__}"
            )
        return parsed

    @staticmethod
    def _map_verdicts(
        parsed: ClassificationBatch, batch_start: int, batch_posts: Sequence[BlueskyPost]
    ) -> Tuple[List[Verdict], List[int]]:
        """Return the batch's verdicts in input order and the ids the model left out."""
        batch_length = len(batch_posts)
        by_local_id: Dict[int, Verdict] = {}
        for verdict in parsed.verdicts:
            local_id = verdict.id
            if not 0 <= local_id < batch_length:
                logger.warning(f"Classifier returned out-of-range id {local_id}; ignoring")
                continue
            if local_id in by_local_id:
                logger.warning(f"Classifier returned duplicate id {local_id}; keeping the first verdict")
                continue
            by_local_id[local_id] = verdict

        mapped: List[Verdict] = []
        missing_ids: List[int] = []
        for local_id in range(batch_length):
            global_id = batch_start + local_id
            post_uri = batch_posts[local_id].uri
            verdict = by_local_id.get(local_id)
            if verdict is None:
                logger.warning(f"Classifier returned no verdict for id {local_id}; treating as off-topic")
                mapped.append(Verdict(id=global_id, on_topic=False, confidence=0.0, story=post_uri))
                missing_ids.append(global_id)
                continue
            mapped.append(
                Verdict(
                    id=global_id,
                    on_topic=bool(verdict.on_topic),
                    confidence=_normalize_confidence(verdict.confidence),
                    story=_normalize_story_key(verdict.story, post_uri),
                )
            )
        return mapped, missing_ids


def _normalize_confidence(value: Any) -> float:
    """Coerce to a float in [0, 1]; non-numeric or non-finite values become 0.0."""
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return 0.0
    if not math.isfinite(value):
        return 0.0
    return min(1.0, max(0.0, float(value)))


def _normalize_story_key(value: Any, post_uri: str) -> str:
    """
    Lowercase, collapse runs of non-alphanumerics to "-", strip leading and
    trailing "-". An empty result falls back to the post's uri, which is unique.
    """
    key = re.sub(r"[^a-z0-9]+", "-", str(value).lower()).strip("-")
    return key or post_uri
