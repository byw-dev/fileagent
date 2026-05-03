"""Upload logs resource — query upload history records.

T2-D2 implementation.  See system-design.md §5.10.
"""
from __future__ import annotations

from typing import TYPE_CHECKING, List, Optional

from fileagent.models.upload_log import UploadLog

if TYPE_CHECKING:
    from fileagent.http import HTTPClient

_UPLOAD_LOGS_PATH = "/api/v1/upload-logs"


class UploadLogsResource:
    """Access upload log records on the FileAgent control plane.

    Args:
        http: The authenticated :class:`~fileagent.http.HTTPClient` instance.

    Example:
        >>> logs = client.upload_logs.list(agent_id="uuid", status="failed")
        >>> for log in logs:
        ...     print(log.original_path, log.error_message)
    """

    def __init__(self, http: "HTTPClient") -> None:
        """Initialise UploadLogsResource.

        Args:
            http: Authenticated HTTP client.
        """
        self._http = http

    def list(
        self,
        agent_id: Optional[str] = None,
        status: Optional[str] = None,
        page_size: int = 50,
        cursor: Optional[str] = None,
    ) -> List[UploadLog]:
        """List upload log records with optional filters.

        Args:
            agent_id: Filter to logs produced by a specific agent UUID.
            status: Filter by upload outcome (e.g. ``"success"`` / ``"failed"``).
            page_size: Maximum number of records to return.
            cursor: Opaque pagination cursor from a previous call.

        Returns:
            A list of :class:`~fileagent.models.upload_log.UploadLog` objects.
        """
        params: dict = {"page_size": page_size}
        if agent_id is not None:
            params["agent_id"] = agent_id
        if status is not None:
            params["status"] = status
        if cursor is not None:
            params["cursor"] = cursor

        data = self._http.get(_UPLOAD_LOGS_PATH, params=params)
        items = data if isinstance(data, list) else data.get("items", [])
        return [UploadLog.from_dict(item) for item in items]

    def get(self, log_id: str) -> UploadLog:
        """Retrieve a single upload log record by its UUID.

        Args:
            log_id: UUID of the upload log record.

        Returns:
            An :class:`~fileagent.models.upload_log.UploadLog` instance.

        Raises:
            NotFoundError: If no log with the given ID exists.
        """
        data = self._http.get(f"{_UPLOAD_LOGS_PATH}/{log_id}")
        return UploadLog.from_dict(data)
