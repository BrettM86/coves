"""
Durable per-topic log of Bluesky posts the aggregator has posted to Coves.

The file is a JSON object keyed by topic name; each value is a list of
entries {uri, author_did, author_handle, author_display_name, text,
created_at}. Entries older than RETENTION are pruned on every write.
"""
import json
import logging
import os
from datetime import datetime, timedelta
from pathlib import Path
from typing import Any, Dict, List

from src.models import BlueskyPost

logger = logging.getLogger(__name__)

RETENTION = timedelta(hours=48)


class StoryLog:
    def __init__(self, path):
        self.path = Path(path)

    def record(self, topic_name: str, post: BlueskyPost, now: datetime) -> None:
        """Store ``post`` under ``topic_name`` (deduped by uri) and prune entries older than 48h."""
        data = self._load()
        entries = [entry for entry in data.get(topic_name, []) if entry.get("uri") != post.uri]
        entries.append(
            {
                "uri": post.uri,
                "author_did": post.author_did,
                "author_handle": post.author_handle,
                "author_display_name": post.author_display_name,
                "text": post.text,
                "created_at": post.created_at.isoformat(),
            }
        )
        data[topic_name] = entries

        oldest_kept = now - RETENTION
        pruned = {
            topic: [entry for entry in topic_entries if _created_at(entry) >= oldest_kept]
            for topic, topic_entries in data.items()
        }
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._write(pruned)
        logger.debug(f"Story log recorded {post.uri} for topic {topic_name}")

    def _write(self, data: Dict[str, List[Dict[str, Any]]]) -> None:
        """Write ``data`` atomically, so an interrupted write cannot truncate the log."""
        temp_path = self.path.with_suffix(".json.tmp")
        try:
            temp_path.write_text(json.dumps(data, indent=2))
            os.replace(temp_path, self.path)
        except OSError:
            if temp_path.exists():
                try:
                    temp_path.unlink()
                except OSError:
                    pass
            raise

    def recent(self, topic_name: str, since: datetime) -> List[BlueskyPost]:
        """Entries for ``topic_name`` created at or after ``since``, newest first, as count-less posts."""
        entries = [entry for entry in self._load().get(topic_name, []) if _created_at(entry) >= since]
        entries.sort(key=_created_at, reverse=True)
        return [
            BlueskyPost(
                uri=entry["uri"],
                cid="",
                author_did=entry["author_did"],
                author_handle=entry["author_handle"],
                author_display_name=entry.get("author_display_name", ""),
                text=entry.get("text", ""),
                created_at=_created_at(entry),
            )
            for entry in entries
        ]

    def _load(self) -> Dict[str, List[Dict[str, Any]]]:
        try:
            data = json.loads(self.path.read_text())
        except FileNotFoundError:
            return {}
        except ValueError:
            logger.warning(f"Story log at {self.path} is not valid JSON; treating it as empty")
            return {}
        if not isinstance(data, dict):
            return {}
        return {
            topic: self._valid_entries(topic, entries)
            for topic, entries in data.items()
            if isinstance(entries, list)
        }

    def _valid_entries(self, topic: str, entries: List[Any]) -> List[Dict[str, Any]]:
        """Drop entries the rest of the class could not read, warning once per topic."""
        kept: List[Dict[str, Any]] = []
        dropped_uris: List[str] = []
        for entry in entries:
            if _is_valid_entry(entry):
                kept.append(entry)
                continue
            uri = entry.get("uri") if isinstance(entry, dict) else None
            dropped_uris.append(uri if isinstance(uri, str) else "<no uri>")
        if dropped_uris:
            logger.warning(
                f"Story log topic '{topic}': dropping {len(dropped_uris)} malformed "
                f"entries: {', '.join(dropped_uris)}"
            )
        return kept


def _is_valid_entry(entry: Any) -> bool:
    """True when ``entry`` carries every field ``recent`` and ``record`` read."""
    if not isinstance(entry, dict):
        return False
    for field in ("uri", "author_did", "author_handle"):
        if not isinstance(entry.get(field), str):
            return False
    for field in ("text", "author_display_name"):
        if field in entry and not isinstance(entry[field], str):
            return False
    if not isinstance(entry.get("created_at"), str):
        return False
    try:
        created_at = datetime.fromisoformat(entry["created_at"])
    except ValueError:
        return False
    return created_at.tzinfo is not None and created_at.utcoffset() is not None


def _created_at(entry: Dict[str, Any]) -> datetime:
    return datetime.fromisoformat(entry["created_at"])
