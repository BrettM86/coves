"""
Coves API Client for posting to communities.

Handles API key authentication and posting via XRPC.
"""
import logging
import requests
from typing import Dict, List, Optional

logger = logging.getLogger(__name__)


class CovesAPIError(Exception):
    """Base exception for Coves API errors."""

    def __init__(self, message: str, status_code: int = None, response_body: str = None):
        super().__init__(message)
        self.status_code = status_code
        self.response_body = response_body


class CovesAuthenticationError(CovesAPIError):
    """Raised when authentication fails (401 Unauthorized)."""
    pass


class CovesNotFoundError(CovesAPIError):
    """Raised when a resource is not found (404 Not Found)."""
    pass


class CovesRateLimitError(CovesAPIError):
    """Raised when rate limit is exceeded (429 Too Many Requests)."""
    pass


class CovesForbiddenError(CovesAPIError):
    """Raised when access is forbidden (403 Forbidden)."""
    pass


class CovesClient:
    """
    Client for posting to Coves communities via XRPC.

    Handles:
    - API key authentication
    - Creating posts in communities (social.coves.community.post.create)
    - External embed formatting
    """

    # API key format constants (must match Go constants in apikey_service.go)
    API_KEY_PREFIX = "ckapi_"
    API_KEY_TOTAL_LENGTH = 70  # 6 (prefix) + 64 (32 bytes hex-encoded)

    def __init__(self, api_url: str, api_key: str):
        """
        Initialize Coves client with API key authentication.

        Args:
            api_url: Coves API URL for posting (e.g., "https://coves.social")
            api_key: Coves API key (e.g., "ckapi_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")

        Raises:
            ValueError: If api_key format is invalid
        """
        # Validate API key format for early failure with clear error
        if not api_key:
            raise ValueError("API key cannot be empty")
        if not api_key.startswith(self.API_KEY_PREFIX):
            raise ValueError(f"API key must start with '{self.API_KEY_PREFIX}'")
        if len(api_key) != self.API_KEY_TOTAL_LENGTH:
            raise ValueError(
                f"API key must be {self.API_KEY_TOTAL_LENGTH} characters "
                f"(got {len(api_key)})"
            )

        self.api_url = api_url.rstrip('/')
        self.api_key = api_key
        self.session = requests.Session()
        self.session.headers['Authorization'] = f'Bearer {api_key}'
        self.session.headers['Content-Type'] = 'application/json'

    def authenticate(self):
        """
        No-op for API key authentication.

        API key is set in the session headers during initialization.
        This method is kept for backward compatibility with existing code
        that calls authenticate() before making requests.
        """
        logger.info("Using API key authentication (no session creation needed)")

    def create_post(
        self,
        community_handle: str,
        content: str,
        facets: List[Dict],
        title: Optional[str] = None,
        embed: Optional[Dict] = None,
        thumbnail_url: Optional[str] = None
    ) -> str:
        """
        Create a post in a community.

        Args:
            community_handle: Community handle (e.g., "world-news.coves.social")
            content: Post content (rich text)
            facets: Rich text facets (formatting, links)
            title: Optional post title
            embed: Optional external embed
            thumbnail_url: Optional thumbnail URL (for trusted aggregators only)

        Returns:
            AT Proto URI of created post (e.g., "at://did:plc:.../social.coves.post/...")

        Raises:
            requests.HTTPError: If post creation fails
        """
        try:
            # Prepare post data for social.coves.community.post.create endpoint
            post_data = {
                "community": community_handle,
                "content": content,
                "facets": facets
            }

            # Add title if provided
            if title:
                post_data["title"] = title

            # Add embed if provided
            if embed:
                post_data["embed"] = embed

            # Add thumbnail URL at top level if provided (for trusted aggregators)
            if thumbnail_url:
                post_data["thumbnailUrl"] = thumbnail_url

            # Use Coves-specific endpoint (not direct PDS write)
            # This provides validation, authorization, and business logic
            logger.info(f"Creating post in community: {community_handle}")

            # Make HTTP request to XRPC endpoint using session with API key
            url = f"{self.api_url}/xrpc/social.coves.community.post.create"
            response = self.session.post(url, json=post_data, timeout=30)

            # Handle specific error cases
            if not response.ok:
                error_body = response.text
                logger.error(f"Post creation failed ({response.status_code}): {error_body}")
                self._raise_for_status(response)

            try:
                result = response.json()
                post_uri = result["uri"]
            except (ValueError, KeyError) as e:
                # ValueError for invalid JSON, KeyError for missing 'uri' field
                logger.error(f"Failed to parse post creation response: {e}")
                raise CovesAPIError(
                    f"Invalid response from server: {e}",
                    status_code=response.status_code,
                    response_body=response.text
                )

            logger.info(f"Post created: {post_uri}")
            return post_uri

        except requests.RequestException as e:
            # Network errors, timeouts, etc.
            logger.error(f"Network error creating post: {e}")
            raise
        except CovesAPIError:
            # Re-raise our custom exceptions as-is
            raise

    def create_external_embed(
        self,
        uri: str,
        title: str,
        description: str,
        sources: Optional[List[Dict]] = None
    ) -> Dict:
        """
        Create external embed object for hot-linked content.

        Args:
            uri: URL of the external content
            title: Title of the content
            description: Description/summary
            sources: Optional list of source dicts with uri, title, domain

        Returns:
            Embed dictionary ready for post creation
        """
        external = {
            "uri": uri,
            "title": title,
            "description": description
        }

        if sources:
            external["sources"] = sources

        return {
            "$type": "social.coves.embed.external",
            "external": external
        }

    def _raise_for_status(self, response: requests.Response) -> None:
        """
        Raise specific exceptions based on HTTP status code.

        Args:
            response: The HTTP response object

        Raises:
            CovesAuthenticationError: For 401 Unauthorized
            CovesNotFoundError: For 404 Not Found
            CovesRateLimitError: For 429 Too Many Requests
            CovesAPIError: For other 4xx/5xx errors
        """
        status_code = response.status_code
        error_body = response.text

        if status_code == 401:
            raise CovesAuthenticationError(
                f"Authentication failed: {error_body}",
                status_code=status_code,
                response_body=error_body
            )
        elif status_code == 403:
            raise CovesForbiddenError(
                f"Access forbidden: {error_body}",
                status_code=status_code,
                response_body=error_body
            )
        elif status_code == 404:
            raise CovesNotFoundError(
                f"Resource not found: {error_body}",
                status_code=status_code,
                response_body=error_body
            )
        elif status_code == 429:
            raise CovesRateLimitError(
                f"Rate limit exceeded: {error_body}",
                status_code=status_code,
                response_body=error_body
            )
        else:
            raise CovesAPIError(
                f"API request failed ({status_code}): {error_body}",
                status_code=status_code,
                response_body=error_body
            )

    def _get_timestamp(self) -> str:
        """
        Get current timestamp in ISO 8601 format.

        Returns:
            ISO timestamp string
        """
        from datetime import datetime, timezone
        return datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")
