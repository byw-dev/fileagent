"""FileEntry model — represents an indexed file record from the control plane."""
from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Dict, Optional


@dataclass
class FileEntry:
    """A file record as returned by the control plane API.

    Attributes:
        id: UUID of the file entry.
        file_name: Base name of the file (e.g. ``"data_2025-04-01.csv"``).
        size_bytes: File size in bytes.
        storage_path: Full path in MinIO (e.g. ``"bucket/path/to/file"``).
        original_path: Original path on the edge device, if known.
        sha256: Hex-encoded SHA-256 checksum of the file content.
        content_type: MIME content type, if known.
        file_type_id: UUID of the associated :class:`FileType`, if any.
        agent_id: UUID of the agent that uploaded the file.
        rule_id: UUID of the collection rule that triggered the upload.
        bucket_id: UUID of the MinIO bucket containing the file.
        status: Upload status (``"uploading"`` / ``"completed"`` / ``"failed"``).
        file_mtime: ISO-8601 modification time of the original file.
        uploaded_at: ISO-8601 timestamp when the upload completed.
        created_at: ISO-8601 timestamp when the record was created.
        updated_at: ISO-8601 timestamp when the record was last updated.
    """

    id: str
    file_name: str
    size_bytes: int
    storage_path: str
    original_path: Optional[str]
    sha256: Optional[str]
    content_type: Optional[str]
    file_type_id: Optional[str]
    agent_id: Optional[str]
    rule_id: Optional[str]
    bucket_id: str
    status: str
    file_mtime: Optional[str]
    uploaded_at: Optional[str]
    created_at: str
    updated_at: str

    @classmethod
    def from_dict(cls, data: Dict[str, Any]) -> "FileEntry":
        """Construct a FileEntry from an API response dict.

        Args:
            data: Decoded JSON dict from the ``/api/v1/files`` endpoint.

        Returns:
            A populated :class:`FileEntry` instance.
        """
        return cls(
            id=data["id"],
            file_name=data["file_name"],
            size_bytes=data.get("size_bytes", 0),
            storage_path=data.get("storage_path", ""),
            original_path=data.get("original_path"),
            sha256=data.get("sha256"),
            content_type=data.get("content_type"),
            file_type_id=data.get("file_type_id"),
            agent_id=data.get("agent_id"),
            rule_id=data.get("rule_id"),
            bucket_id=data.get("bucket_id", ""),
            status=data.get("status", ""),
            file_mtime=data.get("file_mtime"),
            uploaded_at=data.get("uploaded_at"),
            created_at=data.get("created_at", ""),
            updated_at=data.get("updated_at", ""),
        )
