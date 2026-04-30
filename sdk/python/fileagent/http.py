"""HTTP client layer for the FileAgent SDK.

Wraps httpx.Client with retry logic, error mapping, and automatic
Authorization header injection.
"""
from __future__ import annotations

import time
from collections.abc import Callable
from typing import Any

import httpx

from fileagent.exceptions import (
    AuthenticationError,
    FileAgentError,
    NetworkError,
    NotFoundError,
    PermissionError,
    RateLimitError,
    ServerError,
)

# HTTP status codes that trigger a retry.
_RETRYABLE_STATUS = frozenset(range(500, 600))

# Initial back-off delay in seconds; doubles on each retry attempt.
_BACKOFF_BASE = 0.5


def _map_status_to_error(response: httpx.Response) -> FileAgentError:
    """Map an HTTP response to the appropriate SDK exception.

    Args:
        response: The httpx response object with a non-2xx status code.

    Returns:
        A FileAgentError subclass instance populated with the response body.
    """
    try:
        body = response.text
    except Exception:
        body = ""

    status = response.status_code
    if status == 401:
        return AuthenticationError(body)
    if status == 403:
        return PermissionError(body)
    if status == 404:
        return NotFoundError(body)
    if status == 429:
        return RateLimitError(body)
    if 500 <= status < 600:
        return ServerError(body, status_code=status)
    return FileAgentError(f"Unexpected HTTP {status}: {body}")


class HTTPClient:
    """Synchronous HTTP client wrapping httpx.Client.

    Handles Authorization header injection, error mapping, and retry
    logic with exponential back-off for transient errors.

    Attributes:
        base_url: Base URL of the FileAgent control plane API.
        timeout: Default request timeout in seconds.
        max_retries: Maximum number of retry attempts for retryable errors.
        verify_ssl: Whether to verify TLS certificates.
        get_access_token: Optional callable that returns the current
            Bearer token string. Injected by FileAgentClient after the
            TokenManager is created.

    Example:
        >>> client = HTTPClient(base_url="https://control.example.com")
        >>> data = client.get("/api/v1/files")
    """

    def __init__(
        self,
        base_url: str,
        timeout: float = 30.0,
        max_retries: int = 3,
        verify_ssl: bool = True,
    ) -> None:
        """Initialize the HTTPClient.

        Args:
            base_url: Base URL for all requests (no trailing slash needed).
            timeout: Default timeout for each request in seconds.
            max_retries: Number of retry attempts for NetworkError and
                ServerError (5xx).  Set to 0 to disable retries.
            verify_ssl: Pass False to skip TLS certificate verification
                (useful for development with self-signed certificates).
        """
        self.base_url = base_url.rstrip("/")
        self.timeout = timeout
        self.max_retries = max_retries
        self.verify_ssl = verify_ssl
        self.get_access_token: Callable[[], str] | None = None

        self._client = httpx.Client(
            base_url=self.base_url,
            timeout=timeout,
            verify=verify_ssl,
        )

    # ------------------------------------------------------------------
    # Public helpers
    # ------------------------------------------------------------------

    def get(self, path: str, **kwargs: Any) -> Any:
        """Perform a GET request.

        Args:
            path: URL path relative to base_url.
            **kwargs: Additional arguments forwarded to :meth:`request`.

        Returns:
            Parsed JSON body.
        """
        return self.request("GET", path, **kwargs)

    def post(self, path: str, **kwargs: Any) -> Any:
        """Perform a POST request.

        Args:
            path: URL path relative to base_url.
            **kwargs: Additional arguments forwarded to :meth:`request`.

        Returns:
            Parsed JSON body.
        """
        return self.request("POST", path, **kwargs)

    def put(self, path: str, **kwargs: Any) -> Any:
        """Perform a PUT request.

        Args:
            path: URL path relative to base_url.
            **kwargs: Additional arguments forwarded to :meth:`request`.

        Returns:
            Parsed JSON body.
        """
        return self.request("PUT", path, **kwargs)

    def delete(self, path: str, **kwargs: Any) -> Any:
        """Perform a DELETE request.

        Args:
            path: URL path relative to base_url.
            **kwargs: Additional arguments forwarded to :meth:`request`.

        Returns:
            Parsed JSON body.
        """
        return self.request("DELETE", path, **kwargs)

    # ------------------------------------------------------------------
    # Core request logic
    # ------------------------------------------------------------------

    def request(self, method: str, path: str, **kwargs: Any) -> Any:
        """Execute an HTTP request with retry and error mapping.

        Injects the Authorization header automatically if
        :attr:`get_access_token` is set.  Retries on
        :class:`~fileagent.exceptions.NetworkError` and
        :class:`~fileagent.exceptions.ServerError` (5xx) using
        exponential back-off.

        Args:
            method: HTTP method (GET, POST, PUT, DELETE, …).
            path: URL path relative to base_url.
            **kwargs: Extra keyword arguments passed to httpx (e.g.
                ``json``, ``params``, ``headers``).

        Returns:
            Parsed JSON body of the response (dict or list).

        Raises:
            AuthenticationError: On HTTP 401.
            PermissionError: On HTTP 403.
            NotFoundError: On HTTP 404.
            RateLimitError: On HTTP 429.
            ServerError: On HTTP 5xx after exhausting retries.
            NetworkError: On transport-level failure after exhausting
                retries.
            FileAgentError: On other non-2xx responses.
        """
        headers: dict[str, str] = dict(kwargs.pop("headers", {}) or {})
        if self.get_access_token is not None:
            headers["Authorization"] = f"Bearer {self.get_access_token()}"

        last_exc: Exception | None = None
        attempts = self.max_retries + 1

        for attempt in range(attempts):
            if attempt > 0:
                delay = _BACKOFF_BASE * (2 ** (attempt - 1))
                time.sleep(delay)
            try:
                response = self._client.request(
                    method, path, headers=headers, **kwargs
                )
            except httpx.TransportError as exc:
                last_exc = NetworkError(str(exc))
                continue  # retry

            if response.status_code < 400:
                try:
                    return response.json()
                except Exception:
                    return {}

            error = _map_status_to_error(response)

            if isinstance(error, ServerError):
                last_exc = error
                continue  # retry

            # Non-retryable error — raise immediately.
            raise error

        if last_exc is None:
            raise FileAgentError("Unexpected error: retry loop exited without exception")
        raise last_exc

    def close(self) -> None:
        """Close the underlying httpx client and free resources."""
        self._client.close()

    def __enter__(self) -> "HTTPClient":
        """Support use as a context manager."""
        return self

    def __exit__(self, *args: Any) -> None:
        """Close client on context manager exit."""
        self.close()
