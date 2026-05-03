"""Files resource — query, download, and stream file entries.

T2-D1 implementation.  See system-design.md §8.1 / §8.5.
"""
from __future__ import annotations

import os
from typing import TYPE_CHECKING, Any, Dict, Iterator, List, Optional

import httpx

from fileagent.models.file_entry import FileEntry
from fileagent.models.pagination import FileQuery, Page

if TYPE_CHECKING:
    from fileagent.http import HTTPClient

_FILES_PATH = "/api/v1/files"


class StreamResponse:
    """Context manager wrapping a streaming HTTP download from a presigned URL.

    Use this as a ``with`` statement to obtain an open connection and iterate
    over the raw byte chunks:

    Example:
        >>> with files.stream("file-uuid") as resp:
        ...     with open("out.bin", "wb") as f:
        ...         for chunk in resp.iter_content(chunk_size=8192):
        ...             f.write(chunk)
    """

    def __init__(self, url: str, verify_ssl: bool = True) -> None:
        """Initialise StreamResponse.

        Args:
            url: Absolute presigned URL of the file to stream.
            verify_ssl: Whether to verify TLS certificates when fetching the
                presigned URL.
        """
        self._url = url
        self._verify_ssl = verify_ssl
        self._client: Optional[httpx.Client] = None
        self._response: Optional[httpx.Response] = None

    def __enter__(self) -> "StreamResponse":
        """Open the streaming connection.

        Returns:
            This :class:`StreamResponse` instance.
        """
        self._client = httpx.Client(verify=self._verify_ssl)
        request = self._client.build_request("GET", self._url)
        self._response = self._client.send(request, stream=True)
        self._response.raise_for_status()
        return self

    def __exit__(self, *args: Any) -> None:
        """Close the streaming connection and release resources."""
        if self._response is not None:
            self._response.close()
        if self._client is not None:
            self._client.close()

    def iter_content(self, chunk_size: int = 8192) -> Iterator[bytes]:
        """Yield raw byte chunks from the streaming response.

        Args:
            chunk_size: Number of bytes per chunk.

        Yields:
            Raw byte chunks from the downloaded file.

        Raises:
            RuntimeError: If called outside a ``with`` block.
        """
        if self._response is None:
            raise RuntimeError("StreamResponse must be used as a context manager")
        yield from self._response.iter_bytes(chunk_size=chunk_size)


class FilesResource:
    """Access file entries on the FileAgent control plane.

    All methods require a valid Bearer token, which is handled transparently
    by the :class:`~fileagent.http.HTTPClient`.

    Args:
        http: The authenticated :class:`~fileagent.http.HTTPClient` instance.
        verify_ssl: Forwarded to streaming downloads from presigned URLs.

    Example:
        >>> for entry in client.files.iter(FileQuery(file_type_name="logs")):
        ...     print(entry.file_name)
    """

    def __init__(self, http: "HTTPClient", verify_ssl: bool = True) -> None:
        """Initialise FilesResource.

        Args:
            http: Authenticated HTTP client.
            verify_ssl: Whether to verify TLS when downloading from presigned
                MinIO URLs.
        """
        self._http = http
        self._verify_ssl = verify_ssl

    # ------------------------------------------------------------------
    # Query helpers
    # ------------------------------------------------------------------

    def list(self, query: Optional[FileQuery] = None) -> Page[FileEntry]:
        """Fetch a single page of file entries matching the given query.

        Args:
            query: Optional :class:`~fileagent.models.pagination.FileQuery`
                with filter and pagination parameters.  Defaults to the first
                page with page_size=50.

        Returns:
            A :class:`~fileagent.models.pagination.Page` containing
            :class:`~fileagent.models.file_entry.FileEntry` objects.
        """
        params = (query or FileQuery()).to_params()
        data = self._http.get(_FILES_PATH, params=params)
        items = [FileEntry.from_dict(item) for item in data.get("items", [])]
        return Page(
            items=items,
            total=data.get("total", len(items)),
            next_cursor=data.get("next_cursor"),
            has_more=data.get("has_more", False),
        )

    def iter(self, query: Optional[FileQuery] = None) -> Iterator[FileEntry]:
        """Iterate over all file entries matching the query, handling pagination.

        Transparently follows ``next_cursor`` until all pages are exhausted.

        Args:
            query: Optional :class:`~fileagent.models.pagination.FileQuery`.
                A copy is made internally so the caller's instance is not
                mutated.

        Yields:
            :class:`~fileagent.models.file_entry.FileEntry` objects in
            server-side order.

        Example:
            >>> for entry in client.files.iter(FileQuery(page_size=100)):
            ...     process(entry)
        """
        # Work on a copy so we can safely set cursor.
        q = FileQuery(
            file_type_name=(query or FileQuery()).file_type_name,
            agent_ids=(query or FileQuery()).agent_ids,
            start_time=(query or FileQuery()).start_time,
            end_time=(query or FileQuery()).end_time,
            path_prefix=(query or FileQuery()).path_prefix,
            page_size=(query or FileQuery()).page_size,
        ) if query is not None else FileQuery()

        while True:
            page = self.list(q)
            yield from page.items
            if not page.has_more or page.next_cursor is None:
                break
            q.cursor = page.next_cursor

    def get(self, file_id: str) -> FileEntry:
        """Retrieve a single file entry by its UUID.

        Args:
            file_id: UUID of the file entry.

        Returns:
            A :class:`~fileagent.models.file_entry.FileEntry` instance.

        Raises:
            NotFoundError: If no file with the given ID exists.
        """
        data = self._http.get(f"{_FILES_PATH}/{file_id}")
        return FileEntry.from_dict(data)

    # ------------------------------------------------------------------
    # Download helpers
    # ------------------------------------------------------------------

    def get_download_url(self, file_id: str) -> str:
        """Get a presigned download URL for the given file.

        The URL is valid for 15 minutes (as defined in system-design.md §5.11.3).

        Args:
            file_id: UUID of the file entry.

        Returns:
            A presigned HTTPS URL string pointing to MinIO.

        Raises:
            NotFoundError: If no file with the given ID exists.
        """
        data = self._http.get(f"{_FILES_PATH}/{file_id}/download-url")
        return data["url"]

    def download(self, file_id: str, dest_path: str) -> None:
        """Download a file and write it to a local path.

        First retrieves a presigned URL, then downloads the content directly
        from MinIO (bypassing the control plane) and writes it to *dest_path*.
        Parent directories are created automatically.

        Args:
            file_id: UUID of the file entry.
            dest_path: Local filesystem path to write the downloaded content.

        Raises:
            NotFoundError: If no file with the given ID exists.
        """
        url = self.get_download_url(file_id)
        os.makedirs(os.path.dirname(os.path.abspath(dest_path)), exist_ok=True)
        with httpx.Client(verify=self._verify_ssl) as client:
            request = client.build_request("GET", url)
            with client.send(request, stream=True) as response:
                response.raise_for_status()
                with open(dest_path, "wb") as f:
                    for chunk in response.iter_bytes(chunk_size=8192):
                        f.write(chunk)

    def stream(self, file_id: str) -> StreamResponse:
        """Return a :class:`StreamResponse` context manager for streaming download.

        Args:
            file_id: UUID of the file entry.

        Returns:
            A :class:`StreamResponse` that can be used as a ``with`` statement.

        Example:
            >>> with client.files.stream("uuid") as resp:
            ...     for chunk in resp.iter_content(chunk_size=4096):
            ...         ...

        Raises:
            NotFoundError: If no file with the given ID exists.
        """
        url = self.get_download_url(file_id)
        return StreamResponse(url, verify_ssl=self._verify_ssl)

    def batch_download_urls(self, file_ids: List[str]) -> List[Dict[str, str]]:
        """Retrieve presigned download URLs for multiple files at once.

        Args:
            file_ids: List of file entry UUIDs.

        Returns:
            A list of dicts, each containing at minimum ``"id"`` and ``"url"``
            keys, as returned by the server.

        Raises:
            PermissionError: If the caller lacks download permission.
        """
        data = self._http.post(
            f"{_FILES_PATH}/batch-download-urls",
            json={"ids": file_ids},
        )
        return data if isinstance(data, list) else data.get("urls", [])
