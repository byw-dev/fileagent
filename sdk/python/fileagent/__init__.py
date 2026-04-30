"""FileAgent Python SDK.

Provides a synchronous client for interacting with the FileAgent control plane API.

Example:
    >>> from fileagent import FileAgentClient
    >>> client = FileAgentClient(
    ...     base_url="https://control.example.com",
    ...     username="api-user",
    ...     password="secret",
    ... )
"""
from __future__ import annotations

from fileagent.client import FileAgentClient
from fileagent.exceptions import (
    AuthenticationError,
    FileAgentError,
    NetworkError,
    NotFoundError,
    PermissionError,
    RateLimitError,
    ServerError,
)

__all__ = [
    "FileAgentClient",
    "FileAgentError",
    "AuthenticationError",
    "PermissionError",
    "NotFoundError",
    "RateLimitError",
    "ServerError",
    "NetworkError",
]
