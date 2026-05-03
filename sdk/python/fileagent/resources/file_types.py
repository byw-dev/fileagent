"""File types resource — manage logical file type definitions.

T2-D2 implementation.  See system-design.md §5.11.3.
"""
from __future__ import annotations

from typing import TYPE_CHECKING, Any, Dict, List, Optional

from fileagent.models.file_type import FileType

if TYPE_CHECKING:
    from fileagent.http import HTTPClient

_FILE_TYPES_PATH = "/api/v1/file-types"


class FileTypesResource:
    """Access file type definitions on the FileAgent control plane.

    Args:
        http: The authenticated :class:`~fileagent.http.HTTPClient` instance.

    Example:
        >>> for ft in client.file_types.list():
        ...     print(ft.name)
    """

    def __init__(self, http: "HTTPClient") -> None:
        """Initialise FileTypesResource.

        Args:
            http: Authenticated HTTP client.
        """
        self._http = http

    def list(self) -> List[FileType]:
        """List all file types in the organisation.

        Returns:
            A list of :class:`~fileagent.models.file_type.FileType` objects.
        """
        data = self._http.get(_FILE_TYPES_PATH)
        items = data if isinstance(data, list) else data.get("items", [])
        return [FileType.from_dict(item) for item in items]

    def get(self, file_type_id: str) -> FileType:
        """Retrieve a single file type by its UUID.

        Args:
            file_type_id: UUID of the file type.

        Returns:
            A :class:`~fileagent.models.file_type.FileType` instance.

        Raises:
            NotFoundError: If no file type with the given ID exists.
        """
        data = self._http.get(f"{_FILE_TYPES_PATH}/{file_type_id}")
        return FileType.from_dict(data)

    def create(self, name: str, description: Optional[str] = None) -> FileType:
        """Create a new file type.

        Args:
            name: Unique name for the file type within the organisation.
            description: Optional human-readable description.

        Returns:
            The newly created :class:`~fileagent.models.file_type.FileType`.
        """
        payload: Dict[str, Any] = {"name": name}
        if description is not None:
            payload["description"] = description
        data = self._http.post(_FILE_TYPES_PATH, json=payload)
        return FileType.from_dict(data)

    def update(
        self,
        file_type_id: str,
        name: Optional[str] = None,
        description: Optional[str] = None,
    ) -> FileType:
        """Update an existing file type.

        Args:
            file_type_id: UUID of the file type to update.
            name: New name, if changing.
            description: New description, if changing.

        Returns:
            The updated :class:`~fileagent.models.file_type.FileType`.

        Raises:
            NotFoundError: If no file type with the given ID exists.
        """
        payload: Dict[str, Any] = {}
        if name is not None:
            payload["name"] = name
        if description is not None:
            payload["description"] = description
        data = self._http.put(f"{_FILE_TYPES_PATH}/{file_type_id}", json=payload)
        return FileType.from_dict(data)

    def delete(self, file_type_id: str) -> None:
        """Delete a file type by its UUID.

        Args:
            file_type_id: UUID of the file type to delete.

        Raises:
            NotFoundError: If no file type with the given ID exists.
        """
        self._http.delete(f"{_FILE_TYPES_PATH}/{file_type_id}")
