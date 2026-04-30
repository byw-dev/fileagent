"""Top-level FileAgentClient entry point.

Wires together the HTTPClient and TokenManager and exposes typed resource
accessors for each API domain.
"""
from __future__ import annotations

from fileagent.auth import TokenManager
from fileagent.http import HTTPClient


class FileAgentClient:
    """Synchronous client for the FileAgent control plane API.

    Creates an :class:`~fileagent.http.HTTPClient` and a
    :class:`~fileagent.auth.TokenManager`, then wires them together so
    every request automatically carries a valid Bearer token.

    Resource accessors (``files``, ``file_types``, ``agents``,
    ``upload_logs``) will be populated in Phase 2.  They are exposed as
    properties here to keep the public API stable.

    Args:
        base_url: Base URL of the FileAgent control plane
            (e.g. ``"https://control.example.com"``).
        username: Username for API authentication.
        password: Password for API authentication.
        timeout: Default HTTP request timeout in seconds.
        max_retries: Number of retry attempts for transient errors.
        verify_ssl: Set to False to skip TLS certificate verification.

    Example:
        >>> client = FileAgentClient(
        ...     base_url="https://control.example.com",
        ...     username="api-user",
        ...     password="secret",
        ... )
    """

    def __init__(
        self,
        base_url: str,
        username: str,
        password: str,
        timeout: float = 30.0,
        max_retries: int = 3,
        verify_ssl: bool = True,
    ) -> None:
        """Initialize the FileAgentClient.

        Args:
            base_url: Base URL of the control plane API.
            username: Username for login.
            password: Password for login.
            timeout: Request timeout in seconds.
            max_retries: Maximum retry attempts for transient errors.
            verify_ssl: Whether to verify TLS certificates.
        """
        self._http = HTTPClient(
            base_url=base_url,
            timeout=timeout,
            max_retries=max_retries,
            verify_ssl=verify_ssl,
        )
        self._token_manager = TokenManager(
            http_client=self._http,
            username=username,
            password=password,
        )
        # Wire the token callback so every request gets a fresh token.
        self._http.get_access_token = self._token_manager.get_access_token

    # ------------------------------------------------------------------
    # Resource accessors (stubs — implemented in Phase 2)
    # ------------------------------------------------------------------

    @property
    def files(self):  # type: ignore[return]
        """Resource accessor for file entries.

        Returns:
            FilesResource instance (Phase 2).
        """
        return None  # TODO: Phase 2 T2-D1

    @property
    def file_types(self):  # type: ignore[return]
        """Resource accessor for file types.

        Returns:
            FileTypesResource instance (Phase 2).
        """
        return None  # TODO: Phase 2 T2-D2

    @property
    def agents(self):  # type: ignore[return]
        """Resource accessor for agent records.

        Returns:
            AgentsResource instance (Phase 2).
        """
        return None  # TODO: Phase 2 T2-D2

    @property
    def upload_logs(self):  # type: ignore[return]
        """Resource accessor for upload logs.

        Returns:
            UploadLogsResource instance (Phase 2).
        """
        return None  # TODO: Phase 2 T2-D2

    def close(self) -> None:
        """Close the underlying HTTP client and release resources."""
        self._http.close()

    def __enter__(self) -> "FileAgentClient":
        """Support use as a context manager."""
        return self

    def __exit__(self, *args: object) -> None:
        """Close the client on context manager exit."""
        self.close()
