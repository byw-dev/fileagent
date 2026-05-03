"""FileAgent Python SDK.

Provides a synchronous client for interacting with the FileAgent control plane API.

Example:
    >>> from fileagent import FileAgentClient, FileQuery
    >>> client = FileAgentClient(
    ...     base_url="https://control.example.com",
    ...     username="api-user",
    ...     password="secret",
    ... )
    >>> for entry in client.files.iter(FileQuery(file_type_name="logs")):
    ...     print(entry.file_name, entry.size_bytes)
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
from fileagent.models import Agent, FileEntry, FileQuery, FileType, Page, UploadLog

__all__ = [
    "FileAgentClient",
    "FileAgentError",
    "AuthenticationError",
    "PermissionError",
    "NotFoundError",
    "RateLimitError",
    "ServerError",
    "NetworkError",
    # Models
    "Agent",
    "FileEntry",
    "FileQuery",
    "FileType",
    "Page",
    "UploadLog",
]
