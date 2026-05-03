"""Unit tests for fileagent.resources.files.FilesResource using respx."""
from __future__ import annotations

import os
import tempfile
from unittest.mock import MagicMock, patch

import httpx
import pytest
import respx

from fileagent.exceptions import NotFoundError
from fileagent.http import HTTPClient
from fileagent.models.file_entry import FileEntry
from fileagent.models.pagination import FileQuery, Page
from fileagent.resources.files import FilesResource, StreamResponse

BASE_URL = "https://test.example.com"

# ---------------------------------------------------------------------------
# Sample fixtures
# ---------------------------------------------------------------------------

_ENTRY_1 = {
    "id": "file-uuid-1",
    "file_name": "data_2025-04-01.csv",
    "size_bytes": 1024,
    "storage_path": "data-sensor/agent-a/data_2025-04-01.csv",
    "original_path": "/var/data/data_2025-04-01.csv",
    "sha256": "abc123",
    "content_type": "text/csv",
    "file_type_id": "ft-uuid-1",
    "agent_id": "agent-uuid-a",
    "rule_id": "rule-uuid-1",
    "bucket_id": "bucket-uuid-1",
    "status": "completed",
    "file_mtime": "2025-04-01T00:00:00Z",
    "uploaded_at": "2025-04-01T01:00:00Z",
    "created_at": "2025-04-01T01:00:01Z",
    "updated_at": "2025-04-01T01:00:01Z",
}

_ENTRY_2 = {**_ENTRY_1, "id": "file-uuid-2", "file_name": "data_2025-04-02.csv"}


@pytest.fixture()
def http_client():
    """HTTPClient with no retries and a mock token."""
    c = HTTPClient(base_url=BASE_URL, timeout=5, max_retries=0)
    c.get_access_token = lambda: "test-token"
    yield c
    c.close()


@pytest.fixture()
def resource(http_client):
    """FilesResource backed by the http_client fixture."""
    return FilesResource(http_client, verify_ssl=False)


# ---------------------------------------------------------------------------
# list()
# ---------------------------------------------------------------------------


@respx.mock
def test_list_returns_page(resource):
    """list() should return a Page with parsed FileEntry objects."""
    respx.get(f"{BASE_URL}/api/v1/files").mock(
        return_value=httpx.Response(
            200,
            json={
                "items": [_ENTRY_1],
                "total": 1,
                "next_cursor": None,
                "has_more": False,
            },
        )
    )
    page = resource.list()
    assert isinstance(page, Page)
    assert len(page.items) == 1
    assert page.items[0].id == "file-uuid-1"
    assert page.total == 1
    assert page.has_more is False
    assert page.next_cursor is None


@respx.mock
def test_list_with_query_passes_params(resource):
    """list() should forward FileQuery fields as query parameters."""
    route = respx.get(f"{BASE_URL}/api/v1/files").mock(
        return_value=httpx.Response(
            200,
            json={"items": [], "total": 0, "next_cursor": None, "has_more": False},
        )
    )
    from datetime import datetime, timezone

    q = FileQuery(
        file_type_name="var_hourly",
        agent_ids=["agent-a"],
        start_time=datetime(2025, 1, 1, tzinfo=timezone.utc),
        end_time=datetime(2025, 1, 31, tzinfo=timezone.utc),
        path_prefix="/var/",
        page_size=10,
        cursor="abc",
    )
    resource.list(q)
    sent_params = dict(route.calls[0].request.url.params)
    assert sent_params["file_type_name"] == "var_hourly"
    assert sent_params["path_prefix"] == "/var/"
    assert sent_params["cursor"] == "abc"
    assert sent_params["page_size"] == "10"


@respx.mock
def test_list_not_found_raises(resource):
    """list() on a 404 response should raise NotFoundError."""
    respx.get(f"{BASE_URL}/api/v1/files").mock(
        return_value=httpx.Response(404, text="Not Found")
    )
    with pytest.raises(NotFoundError):
        resource.list()


# ---------------------------------------------------------------------------
# iter()
# ---------------------------------------------------------------------------


@respx.mock
def test_iter_single_page(resource):
    """iter() on a single page should yield all items."""
    respx.get(f"{BASE_URL}/api/v1/files").mock(
        return_value=httpx.Response(
            200,
            json={
                "items": [_ENTRY_1, _ENTRY_2],
                "total": 2,
                "next_cursor": None,
                "has_more": False,
            },
        )
    )
    entries = list(resource.iter())
    assert len(entries) == 2
    assert entries[0].id == "file-uuid-1"
    assert entries[1].id == "file-uuid-2"


@respx.mock
def test_iter_multiple_pages(resource):
    """iter() should transparently follow next_cursor across pages."""
    page1 = {
        "items": [_ENTRY_1],
        "total": 2,
        "next_cursor": "cursor-page2",
        "has_more": True,
    }
    page2 = {
        "items": [_ENTRY_2],
        "total": 2,
        "next_cursor": None,
        "has_more": False,
    }

    call_count = 0

    def side_effect(request, route):
        nonlocal call_count
        call_count += 1
        if call_count == 1:
            return httpx.Response(200, json=page1)
        return httpx.Response(200, json=page2)

    respx.get(f"{BASE_URL}/api/v1/files").mock(side_effect=side_effect)
    entries = list(resource.iter(FileQuery(page_size=1)))
    assert len(entries) == 2
    assert call_count == 2


@respx.mock
def test_iter_empty_result(resource):
    """iter() on empty result should yield nothing."""
    respx.get(f"{BASE_URL}/api/v1/files").mock(
        return_value=httpx.Response(
            200,
            json={"items": [], "total": 0, "next_cursor": None, "has_more": False},
        )
    )
    entries = list(resource.iter())
    assert entries == []


# ---------------------------------------------------------------------------
# get()
# ---------------------------------------------------------------------------


@respx.mock
def test_get_returns_entry(resource):
    """get() should return a FileEntry for the given ID."""
    respx.get(f"{BASE_URL}/api/v1/files/file-uuid-1").mock(
        return_value=httpx.Response(200, json=_ENTRY_1)
    )
    entry = resource.get("file-uuid-1")
    assert isinstance(entry, FileEntry)
    assert entry.id == "file-uuid-1"
    assert entry.file_name == "data_2025-04-01.csv"


@respx.mock
def test_get_not_found_raises(resource):
    """get() for a missing file should raise NotFoundError."""
    respx.get(f"{BASE_URL}/api/v1/files/missing").mock(
        return_value=httpx.Response(404, text="Not Found")
    )
    with pytest.raises(NotFoundError):
        resource.get("missing")


# ---------------------------------------------------------------------------
# get_download_url()
# ---------------------------------------------------------------------------


@respx.mock
def test_get_download_url(resource):
    """get_download_url() should return the presigned URL string."""
    respx.get(f"{BASE_URL}/api/v1/files/file-uuid-1/download-url").mock(
        return_value=httpx.Response(
            200, json={"url": "https://minio.example.com/presigned"}
        )
    )
    url = resource.get_download_url("file-uuid-1")
    assert url == "https://minio.example.com/presigned"


# ---------------------------------------------------------------------------
# download()
# ---------------------------------------------------------------------------


@respx.mock
def test_download_writes_file(resource):
    """download() should fetch the presigned URL and write content to disk."""
    presigned_url = "https://minio.example.com/presigned-file"
    respx.get(f"{BASE_URL}/api/v1/files/file-uuid-1/download-url").mock(
        return_value=httpx.Response(200, json={"url": presigned_url})
    )

    file_content = b"hello file content"

    with tempfile.TemporaryDirectory() as tmpdir:
        dest = os.path.join(tmpdir, "downloaded.csv")
        with patch("httpx.Client") as MockClient:
            mock_client_instance = MockClient.return_value.__enter__.return_value
            mock_response = MagicMock()
            mock_response.raise_for_status = MagicMock()
            mock_response.iter_bytes.return_value = iter([file_content])
            mock_client_instance.send.return_value.__enter__.return_value = (
                mock_response
            )
            resource.download("file-uuid-1", dest)

        assert os.path.exists(dest)
        with open(dest, "rb") as f:
            assert f.read() == file_content


# ---------------------------------------------------------------------------
# batch_download_urls()
# ---------------------------------------------------------------------------


@respx.mock
def test_batch_download_urls_list_response(resource):
    """batch_download_urls() should return a list of URL dicts (list body)."""
    batch_result = [
        {"id": "file-uuid-1", "url": "https://minio/signed-1"},
        {"id": "file-uuid-2", "url": "https://minio/signed-2"},
    ]
    respx.post(f"{BASE_URL}/api/v1/files/batch-download-urls").mock(
        return_value=httpx.Response(200, json=batch_result)
    )
    result = resource.batch_download_urls(["file-uuid-1", "file-uuid-2"])
    assert len(result) == 2
    assert result[0]["url"] == "https://minio/signed-1"


@respx.mock
def test_batch_download_urls_dict_response(resource):
    """batch_download_urls() should handle a dict body with 'urls' key."""
    batch_result = {
        "urls": [
            {"id": "file-uuid-1", "url": "https://minio/signed-1"},
        ]
    }
    respx.post(f"{BASE_URL}/api/v1/files/batch-download-urls").mock(
        return_value=httpx.Response(200, json=batch_result)
    )
    result = resource.batch_download_urls(["file-uuid-1"])
    assert len(result) == 1
    assert result[0]["id"] == "file-uuid-1"


# ---------------------------------------------------------------------------
# StreamResponse
# ---------------------------------------------------------------------------


def test_stream_response_context_manager():
    """StreamResponse should open a streaming connection and yield chunks."""
    file_content = b"streaming bytes here"
    chunks = [file_content[:10], file_content[10:]]

    with patch("httpx.Client") as MockClient:
        mock_instance = MockClient.return_value
        mock_instance.__enter__ = lambda s: s
        mock_instance.__exit__ = MagicMock(return_value=False)
        mock_response = MagicMock()
        mock_response.raise_for_status = MagicMock()
        mock_response.iter_bytes.return_value = iter(chunks)
        mock_instance.send.return_value = mock_response

        sr = StreamResponse("https://minio.example.com/file", verify_ssl=False)
        with sr as opened:
            collected = b"".join(opened.iter_content(chunk_size=10))

    assert collected == file_content


def test_stream_response_iter_content_without_context_raises():
    """iter_content() outside a 'with' block should raise RuntimeError."""
    sr = StreamResponse("https://minio.example.com/file")
    with pytest.raises(RuntimeError):
        list(sr.iter_content())


# ---------------------------------------------------------------------------
# FileEntry.from_dict edge cases
# ---------------------------------------------------------------------------


def test_file_entry_from_dict_minimal():
    """FileEntry.from_dict should work with minimal required fields."""
    data = {"id": "x", "file_name": "f.txt", "bucket_id": "b"}
    entry = FileEntry.from_dict(data)
    assert entry.id == "x"
    assert entry.size_bytes == 0
    assert entry.sha256 is None
    assert entry.status == ""
