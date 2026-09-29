"""
Tests for uri_sanitizer.

The bulk of the behaviour is pinned by the shared cross-language corpus at
internal/validation/testdata/uri_vectors.json, which the Go normalizer's test
suite reads too. That file — not a comment — is what makes "the Python bridge
and the AppView agree" an enforced property: a one-sided change to either
implementation turns the other language's suite red.

The tests below the vector runner cover Python-specific concerns the corpus
cannot express (type handling, the public predicate).
"""
import json
import pathlib

import pytest

from src.uri_sanitizer import is_valid_uri, sanitize_uri

# Walk up to the repo root: aggregators/<bridge>/tests/ -> repo root.
_REPO_ROOT = pathlib.Path(__file__).resolve().parents[3]
_VECTOR_FILE = _REPO_ROOT / "internal" / "validation" / "testdata" / "uri_vectors.json"

# Error classes in the shared corpus, mapped to a substring the Python message
# must contain. The Go side maps the same names to typed sentinels.
_ERROR_CLASS_MESSAGES = {
    "empty": "empty",
    "no_scheme": "scheme",
    "bad_scheme": "not valid for the atproto uri format",
    "scheme_forbidden": "not allowed in a rendered link",
    "no_authority": "no host",
    "too_long": "too long",
    "unnormalizable": "cannot",
}


def _load_vectors():
    if not _VECTOR_FILE.exists():
        pytest.fail(f"shared conformance corpus missing: {_VECTOR_FILE}")
    cases = json.loads(_VECTOR_FILE.read_text(encoding="utf-8"))["cases"]
    assert cases, "shared conformance corpus is empty"
    return cases


_VECTORS = _load_vectors()


@pytest.mark.parametrize(
    "case", _VECTORS, ids=[c["name"] for c in _VECTORS]
)
def test_conformance_vector(case):
    """Every shared vector must behave identically here and in the Go normalizer."""
    expected_error = case.get("error")

    if expected_error:
        assert expected_error in _ERROR_CLASS_MESSAGES, (
            f"vector names unknown error class {expected_error!r}"
        )
        with pytest.raises(ValueError) as excinfo:
            sanitize_uri(case["input"])
        expected_substring = _ERROR_CLASS_MESSAGES[expected_error]
        assert expected_substring in str(excinfo.value), (
            f"{case['name']}: message {str(excinfo.value)!r} should identify "
            f"error class {expected_error!r}"
        )
        return

    result = sanitize_uri(case["input"])
    assert result == case["output"], case["name"]
    assert is_valid_uri(result), f"{case['name']}: result does not satisfy the uri format"
    # Every successful vector must also be a fixed point.
    assert sanitize_uri(result) == result, f"{case['name']}: not idempotent"


class TestSanitizeUriTypeHandling:
    """Non-string input must fail as a ValueError, not an AttributeError.

    The bridges catch ValueError to drop a single bad source; an AttributeError
    escaping instead would turn "drop one link" into "lose the whole post".
    """

    @pytest.mark.parametrize("bad", [None, 42, 3.5, b"https://example.com/", ["x"], {}])
    def test_non_string_raises_value_error(self, bad):
        with pytest.raises(ValueError):
            sanitize_uri(bad)


class TestIsValidUri:
    """The public predicate must agree with the reference format exactly."""

    def test_raw_accented_character_is_invalid(self):
        assert not is_valid_uri("https://example.com/pokémon")

    def test_percent_encoded_form_is_valid(self):
        assert is_valid_uri("https://example.com/pok%C3%A9mon")

    def test_space_is_invalid(self):
        assert not is_valid_uri("https://example.com/with space")

    def test_uppercase_scheme_is_invalid(self):
        assert not is_valid_uri("HTTPS://example.com/a")

    def test_trailing_newline_is_invalid(self):
        # `re.match` with `$` would accept this; the reference parser does not.
        assert not is_valid_uri("https://example.com/a\n")

    def test_over_length_uri_is_invalid(self):
        # The reference parser caps at 8192 bytes.
        assert not is_valid_uri("https://example.com/" + "a" * 9000)

    def test_at_length_boundary_is_valid(self):
        boundary = "https://example.com/" + "a" * (8192 - len("https://example.com/"))
        assert len(boundary) == 8192
        assert is_valid_uri(boundary)


class TestEscapeBoundaries:
    """Exact edges of the printable-ASCII range."""

    def test_graph_low_boundary_is_preserved(self):
        assert sanitize_uri("https://example.com/!é") == "https://example.com/!%C3%A9"

    def test_graph_high_boundary_is_preserved(self):
        assert sanitize_uri("https://example.com/~é") == "https://example.com/~%C3%A9"

    def test_del_is_escaped(self):
        assert sanitize_uri("https://example.com/\x7fé") == "https://example.com/%7F%C3%A9"

    def test_nul_is_escaped(self):
        assert sanitize_uri("https://example.com/a\x00b/é") == "https://example.com/a%00b/%C3%A9"
