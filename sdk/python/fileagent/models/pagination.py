"""Pagination model — cursor-based pagination support.

See system-design.md §8.5.
"""
from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime
from typing import Generic, List, Optional, TypeVar

T = TypeVar("T")


@dataclass
class Page(Generic[T]):
    """A single page of results from a cursor-based paginated API response.

    Attributes:
        items: The list of result objects on this page.
        total: Total number of matching records across all pages.
        next_cursor: Opaque cursor string to fetch the next page, or None
            when this is the last page.
        has_more: True when more pages are available after this one.
    """

    items: List[T]
    total: int
    next_cursor: Optional[str]
    has_more: bool


@dataclass
class FileQuery:
    """Query parameters for listing file entries.

    All fields are optional filters; omitting a field means "no filter on
    that dimension".

    Attributes:
        file_type_name: Filter by logical file type name (e.g. ``"var_hourly"``).
        agent_ids: Restrict results to files collected by these agent IDs.
        start_time: Only return files uploaded at or after this UTC datetime.
        end_time: Only return files uploaded before or at this UTC datetime.
        path_prefix: Only return files whose ``original_path`` starts with this
            prefix.
        page_size: Maximum number of items per page (default 50).
        cursor: Opaque pagination cursor returned by a previous :meth:`list`
            call.  Omit on the first call.

    Example:
        >>> from datetime import datetime, timedelta
        >>> query = FileQuery(
        ...     file_type_name="var_hourly",
        ...     start_time=datetime.utcnow() - timedelta(days=7),
        ...     page_size=100,
        ... )
    """

    file_type_name: Optional[str] = None
    agent_ids: Optional[List[str]] = field(default=None)
    start_time: Optional[datetime] = None
    end_time: Optional[datetime] = None
    path_prefix: Optional[str] = None
    page_size: int = 50
    cursor: Optional[str] = None

    def to_params(self) -> dict:
        """Convert this query to a dict of HTTP query parameters.

        Returns:
            A dict suitable for passing as ``params=`` to an HTTP request.
        """
        params: dict = {"page_size": self.page_size}
        if self.file_type_name is not None:
            params["file_type_name"] = self.file_type_name
        if self.agent_ids:
            params["agent_ids"] = self.agent_ids
        if self.start_time is not None:
            params["start_time"] = self.start_time.isoformat()
        if self.end_time is not None:
            params["end_time"] = self.end_time.isoformat()
        if self.path_prefix is not None:
            params["path_prefix"] = self.path_prefix
        if self.cursor is not None:
            params["cursor"] = self.cursor
        return params
