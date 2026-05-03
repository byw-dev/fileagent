"""UploadLog model — represents an upload history record from the control plane."""
from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Dict, Optional


@dataclass
class UploadLog:
    """An upload log record as returned by the control plane API.

    Attributes:
        id: UUID of the upload log record.
        agent_id: UUID of the agent that performed the upload.
        file_entry_id: UUID of the resulting :class:`FileEntry`, if available.
        rule_id: UUID of the collection rule that triggered the upload.
        original_path: Original path of the file on the edge device.
        storage_path: Destination path in MinIO.
        size_bytes: Total file size in bytes.
        bytes_transferred: Number of bytes successfully transferred.
        status: Upload outcome (e.g. ``"success"`` / ``"failed"``).
        error_message: Error description when the upload failed, or None.
        retry_count: Number of retry attempts before the final outcome.
        started_at: ISO-8601 timestamp when the upload began.
        finished_at: ISO-8601 timestamp when the upload completed, or None.
        created_at: ISO-8601 timestamp when this log record was created.
    """

    id: str
    agent_id: str
    file_entry_id: Optional[str]
    rule_id: Optional[str]
    original_path: str
    storage_path: str
    size_bytes: int
    bytes_transferred: int
    status: str
    error_message: Optional[str]
    retry_count: int
    started_at: str
    finished_at: Optional[str]
    created_at: str

    @classmethod
    def from_dict(cls, data: Dict[str, Any]) -> "UploadLog":
        """Construct an UploadLog from an API response dict.

        Args:
            data: Decoded JSON dict from the ``/api/v1/upload-logs`` endpoint.

        Returns:
            A populated :class:`UploadLog` instance.
        """
        return cls(
            id=data["id"],
            agent_id=data.get("agent_id", ""),
            file_entry_id=data.get("file_entry_id"),
            rule_id=data.get("rule_id"),
            original_path=data.get("original_path", ""),
            storage_path=data.get("storage_path", ""),
            size_bytes=data.get("size_bytes", 0),
            bytes_transferred=data.get("bytes_transferred", 0),
            status=data.get("status", ""),
            error_message=data.get("error_message"),
            retry_count=data.get("retry_count", 0),
            started_at=data.get("started_at", ""),
            finished_at=data.get("finished_at"),
            created_at=data.get("created_at", ""),
        )
