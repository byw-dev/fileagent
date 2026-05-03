"""FileType model — represents a logical file categorisation rule."""
from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Dict, Optional


@dataclass
class FileType:
    """A logical file type as returned by the control plane API.

    Attributes:
        id: UUID of the file type.
        name: Unique name within the organisation (e.g. ``"var_hourly"``).
        description: Human-readable description, if any.
        created_by: UUID of the user who created this type.
        created_at: ISO-8601 creation timestamp.
    """

    id: str
    name: str
    description: Optional[str]
    created_by: Optional[str]
    created_at: str

    @classmethod
    def from_dict(cls, data: Dict[str, Any]) -> "FileType":
        """Construct a FileType from an API response dict.

        Args:
            data: Decoded JSON dict from the ``/api/v1/file-types`` endpoint.

        Returns:
            A populated :class:`FileType` instance.
        """
        return cls(
            id=data["id"],
            name=data["name"],
            description=data.get("description"),
            created_by=data.get("created_by"),
            created_at=data.get("created_at", ""),
        )
