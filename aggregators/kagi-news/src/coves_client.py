"""
Coves API Client for posting to communities.

Handles authentication and posting via XRPC.
"""
import logging
import requests
from typing import Dict, List, Optional
from atproto import Client

logger = logging.getLogger(__name__)


class CovesClient:
    """
    Client for posting to Coves communities via XRPC.

    Handles:
    - Authentication with aggregator credentials
    - Creating posts in communities (social.coves.post.create)
    - External embed formatting
    """

    def __init__(self, api_url: str, handle: str, password: str, pds_url: Optional[str] = None):
        """
        Initialize Coves client.

        Args:
            api_url: Coves AppView URL for posting (e.g., "http://localhost:8081")
            handle: Aggregator handle (e.g., "kagi-news.coves.social")
            password: Aggregator password/app password
            pds_url: Optional PDS URL for authentication (defaults to api_url)
        """
        self.api_url = api_url
        self.pds_url = pds_url or api_url  # Auth through PDS, post through AppView
        self.handle = handle
        self.password = password
        self.client = Client(base_url=self.pds_url)  # Use PDS for auth
        self._authenticated = False

    def authenticate(self):
        """
        Authenticate with Coves API.

        Uses com.atproto.server.createSession directly to avoid
        Bluesky-specific endpoints that don't exist on Coves PDS.

        Raises:
            Exception: If authentication fails
        """
        try:
            logger.info(f"Authenticating as {self.handle}")

            # Use createSession directly (avoid app.bsky.actor.getProfile)
            session = self.client.com.atproto.server.create_session(
                {"identifier": self.handle, "password": self.password}
            )

            # Manually set session (skip profile fetch)
            self.client._session = session
            self._authenticated = True
            self.did = session.did

            logger.info(f"Authentication successful (DID: {self.did})")
        except Exception as e:
            logger.error(f"Authentication failed: {e}")
            raise

    def create_post(
        self,
        community_handle: str,
        content: str,
        facets: List[Dict],
        embed: Optional[Dict] = None
    ) -> str:
        """
        Create a post in a community.

        Args:
            community_handle: Community handle (e.g., "world-news.coves.social")
            content: Post content (rich text)
            facets: Rich text facets (formatting, links)
            embed: Optional external embed

        Returns:
            AT Proto URI of created post (e.g., "at://did:plc:.../social.coves.post/...")

        Raises:
            Exception: If post creation fails
        """
        if not self._authenticated:
            self.authenticate()

        try:
            # Prepare post data for social.coves.post.create endpoint
            post_data = {
                "community": community_handle,
                "content": content,
                "facets": facets
            }

            # Add embed if provided
            if embed:
                post_data["embed"] = embed

            # Use Coves-specific endpoint (not direct PDS write)
            # This provides validation, authorization, and business logic
            logger.info(f"Creating post in community: {community_handle}")

            # Make direct HTTP request to XRPC endpoint
            url = f"{self.api_url}/xrpc/social.coves.post.create"
            headers = {
                "Authorization": f"Bearer {self.client._session.access_jwt}",
                "Content-Type": "application/json"
            }

            response = requests.post(url, json=post_data, headers=headers, timeout=30)

            # Log detailed error if request fails
            if not response.ok:
                error_body = response.text
                logger.error(f"Post creation failed ({response.status_code}): {error_body}")
                response.raise_for_status()

            result = response.json()
            post_uri = result["uri"]
            logger.info(f"Post created: {post_uri}")
            return post_uri

        except Exception as e:
            logger.error(f"Failed to create post: {e}")
            raise

    def create_external_embed(
        self,
        uri: str,
        title: str,
        description: str,
        thumb: Optional[str] = None
    ) -> Dict:
        """
        Create external embed object for hot-linked content.

        Args:
            uri: External URL (story link)
            title: Story title
            description: Story description/summary
            thumb: Optional thumbnail image URL

        Returns:
            External embed dictionary
        """
        embed = {
            "$type": "social.coves.embed.external",
            "external": {
                "uri": uri,
                "title": title,
                "description": description
            }
        }

        if thumb:
            embed["external"]["thumb"] = thumb

        return embed

    def _get_timestamp(self) -> str:
        """
        Get current timestamp in ISO 8601 format.

        Returns:
            ISO timestamp string
        """
        from datetime import datetime, timezone
        return datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")
