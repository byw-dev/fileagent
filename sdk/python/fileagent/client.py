"""Top-level FileAgentClient entry point.

Wires together the HTTPClient and TokenManager and exposes typed resource
accessors for each API domain.
"""
from __future__ import annotations

from fileagent.auth import TokenManager
from fileagent.http import HTTPClient
from fileagent.resources.agents import AgentsResource
from fileagent.resources.file_types import FileTypesResource
from fileagent.resources.files import FilesResource
from fileagent.resources.upload_logs import UploadLogsResource


class FileAgentClient:
    """Synchronous client for the FileAgent control plane API.

    Creates an :class:`~fileagent.http.HTTPClient` and a
    :class:`~fileagent.auth.TokenManager`, then wires them together so
    every request automatically carries a valid Bearer token.

    Resource accessors (``files``, ``file_types``, ``agents``,
    ``upload_logs``) provide typed access to all API domains.

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
        >>> for entry in client.files.iter():
        ...     print(entry.file_name)
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

        # Instantiate resource accessors.
        self._files = FilesResource(self._http, verify_ssl=verify_ssl)
        self._file_types = FileTypesResource(self._http)
        self._agents = AgentsResource(self._http)
        self._upload_logs = UploadLogsResource(self._http)

    # ------------------------------------------------------------------
    # Resource accessors
    # ------------------------------------------------------------------

    @property
    def files(self) -> FilesResource:
        """Resource accessor for file entries.

        Returns:
            :class:`~fileagent.resources.files.FilesResource` instance.
        """
        return self._files

    @property
    def file_types(self) -> FileTypesResource:
        """Resource accessor for file types.

        Returns:
            :class:`~fileagent.resources.file_types.FileTypesResource` instance.
        """
        return self._file_types

    @property
    def agents(self) -> AgentsResource:
        """Resource accessor for agent records.

        Returns:
            :class:`~fileagent.resources.agents.AgentsResource` instance.
        """
        return self._agents

    @property
    def upload_logs(self) -> UploadLogsResource:
        """Resource accessor for upload logs.

        Returns:
            :class:`~fileagent.resources.upload_logs.UploadLogsResource` instance.
        """
        return self._upload_logs

    def close(self) -> None:
        """Close the underlying HTTP client and release resources."""
        self._http.close()

    def __enter__(self) -> "FileAgentClient":
        """Support use as a context manager."""
        return self

    def __exit__(self, *args: object) -> None:
        """Close the client on context manager exit."""
        self.close()
