"""Unit tests for FileTypesResource, AgentsResource, and UploadLogsResource."""
from __future__ import annotations

import httpx
import pytest
import respx

from fileagent.exceptions import NotFoundError
from fileagent.http import HTTPClient
from fileagent.models.agent import Agent
from fileagent.models.file_type import FileType
from fileagent.models.upload_log import UploadLog
from fileagent.resources.agents import AgentsResource
from fileagent.resources.file_types import FileTypesResource
from fileagent.resources.upload_logs import UploadLogsResource

BASE_URL = "https://test.example.com"

# ---------------------------------------------------------------------------
# Shared fixtures
# ---------------------------------------------------------------------------


@pytest.fixture()
def http_client():
    """HTTPClient with no retries and a mock token."""
    c = HTTPClient(base_url=BASE_URL, timeout=5, max_retries=0)
    c.get_access_token = lambda: "test-token"
    yield c
    c.close()


# ---------------------------------------------------------------------------
# Sample data
# ---------------------------------------------------------------------------

_FILE_TYPE = {
    "id": "ft-uuid-1",
    "name": "var_hourly",
    "description": "Hourly variance data",
    "created_by": "user-uuid-1",
    "created_at": "2025-01-01T00:00:00Z",
}

_AGENT = {
    "id": "agent-uuid-a",
    "name": "edge-sensor-01",
    "fingerprint": "sha256:abcdef",
    "status": "running",
    "ip_address": "192.168.1.100",
    "last_seen_at": "2025-04-01T12:00:00Z",
    "approved_at": "2025-01-15T10:00:00Z",
    "created_at": "2025-01-10T09:00:00Z",
    "updated_at": "2025-04-01T12:00:00Z",
}

_UPLOAD_LOG = {
    "id": "log-uuid-1",
    "agent_id": "agent-uuid-a",
    "file_entry_id": "file-uuid-1",
    "rule_id": "rule-uuid-1",
    "original_path": "/var/data/file.csv",
    "storage_path": "data-sensor/agent-a/file.csv",
    "size_bytes": 2048,
    "bytes_transferred": 2048,
    "status": "success",
    "error_message": None,
    "retry_count": 0,
    "started_at": "2025-04-01T00:00:00Z",
    "finished_at": "2025-04-01T00:00:05Z",
    "created_at": "2025-04-01T00:00:05Z",
}


# ===========================================================================
# FileTypesResource tests
# ===========================================================================


@pytest.fixture()
def file_types(http_client):
    """FileTypesResource instance."""
    return FileTypesResource(http_client)


@respx.mock
def test_file_types_list_with_items_key(file_types):
    """list() should handle a dict response with 'items' key."""
    respx.get(f"{BASE_URL}/api/v1/file-types").mock(
        return_value=httpx.Response(200, json={"items": [_FILE_TYPE]})
    )
    result = file_types.list()
    assert len(result) == 1
    assert isinstance(result[0], FileType)
    assert result[0].name == "var_hourly"


@respx.mock
def test_file_types_list_with_list_body(file_types):
    """list() should handle a bare list response."""
    respx.get(f"{BASE_URL}/api/v1/file-types").mock(
        return_value=httpx.Response(200, json=[_FILE_TYPE])
    )
    result = file_types.list()
    assert len(result) == 1
    assert result[0].id == "ft-uuid-1"


@respx.mock
def test_file_types_list_empty(file_types):
    """list() on an empty response should return an empty list."""
    respx.get(f"{BASE_URL}/api/v1/file-types").mock(
        return_value=httpx.Response(200, json={"items": []})
    )
    assert file_types.list() == []


@respx.mock
def test_file_types_get(file_types):
    """get() should return a FileType for the given ID."""
    respx.get(f"{BASE_URL}/api/v1/file-types/ft-uuid-1").mock(
        return_value=httpx.Response(200, json=_FILE_TYPE)
    )
    ft = file_types.get("ft-uuid-1")
    assert isinstance(ft, FileType)
    assert ft.id == "ft-uuid-1"
    assert ft.description == "Hourly variance data"


@respx.mock
def test_file_types_get_not_found(file_types):
    """get() for a missing type should raise NotFoundError."""
    respx.get(f"{BASE_URL}/api/v1/file-types/missing").mock(
        return_value=httpx.Response(404, text="Not Found")
    )
    with pytest.raises(NotFoundError):
        file_types.get("missing")


@respx.mock
def test_file_types_create(file_types):
    """create() should POST and return the new FileType."""
    created = {**_FILE_TYPE, "id": "ft-uuid-new"}
    respx.post(f"{BASE_URL}/api/v1/file-types").mock(
        return_value=httpx.Response(201, json=created)
    )
    ft = file_types.create("new_type", description="New type description")
    assert ft.id == "ft-uuid-new"
    assert ft.name == "var_hourly"


@respx.mock
def test_file_types_update(file_types):
    """update() should PUT and return the updated FileType."""
    updated = {**_FILE_TYPE, "description": "Updated description"}
    respx.put(f"{BASE_URL}/api/v1/file-types/ft-uuid-1").mock(
        return_value=httpx.Response(200, json=updated)
    )
    ft = file_types.update("ft-uuid-1", description="Updated description")
    assert ft.description == "Updated description"


@respx.mock
def test_file_types_delete(file_types):
    """delete() should issue a DELETE request without error."""
    respx.delete(f"{BASE_URL}/api/v1/file-types/ft-uuid-1").mock(
        return_value=httpx.Response(200, json={})
    )
    file_types.delete("ft-uuid-1")  # should not raise


@respx.mock
def test_file_types_delete_not_found(file_types):
    """delete() for a missing ID should raise NotFoundError."""
    respx.delete(f"{BASE_URL}/api/v1/file-types/missing").mock(
        return_value=httpx.Response(404, text="Not Found")
    )
    with pytest.raises(NotFoundError):
        file_types.delete("missing")


# ===========================================================================
# AgentsResource tests
# ===========================================================================


@pytest.fixture()
def agents(http_client):
    """AgentsResource instance."""
    return AgentsResource(http_client)


@respx.mock
def test_agents_list_with_items_key(agents):
    """list() should handle a dict response with 'items' key."""
    respx.get(f"{BASE_URL}/api/v1/agents").mock(
        return_value=httpx.Response(200, json={"items": [_AGENT]})
    )
    result = agents.list()
    assert len(result) == 1
    assert isinstance(result[0], Agent)
    assert result[0].name == "edge-sensor-01"


@respx.mock
def test_agents_list_with_list_body(agents):
    """list() should handle a bare list response."""
    respx.get(f"{BASE_URL}/api/v1/agents").mock(
        return_value=httpx.Response(200, json=[_AGENT])
    )
    result = agents.list()
    assert len(result) == 1
    assert result[0].status == "running"


@respx.mock
def test_agents_list_empty(agents):
    """list() on an empty response should return an empty list."""
    respx.get(f"{BASE_URL}/api/v1/agents").mock(
        return_value=httpx.Response(200, json={"items": []})
    )
    assert agents.list() == []


@respx.mock
def test_agents_get(agents):
    """get() should return an Agent for the given ID."""
    respx.get(f"{BASE_URL}/api/v1/agents/agent-uuid-a").mock(
        return_value=httpx.Response(200, json=_AGENT)
    )
    agent = agents.get("agent-uuid-a")
    assert isinstance(agent, Agent)
    assert agent.id == "agent-uuid-a"
    assert agent.fingerprint == "sha256:abcdef"
    assert agent.ip_address == "192.168.1.100"


@respx.mock
def test_agents_get_not_found(agents):
    """get() for a missing agent should raise NotFoundError."""
    respx.get(f"{BASE_URL}/api/v1/agents/missing").mock(
        return_value=httpx.Response(404, text="Not Found")
    )
    with pytest.raises(NotFoundError):
        agents.get("missing")


def test_agent_from_dict_minimal():
    """Agent.from_dict should handle minimal fields gracefully."""
    data = {"id": "a", "name": "n"}
    agent = Agent.from_dict(data)
    assert agent.id == "a"
    assert agent.fingerprint == ""
    assert agent.status == ""
    assert agent.ip_address is None
    assert agent.last_seen_at is None


# ===========================================================================
# UploadLogsResource tests
# ===========================================================================


@pytest.fixture()
def upload_logs(http_client):
    """UploadLogsResource instance."""
    return UploadLogsResource(http_client)


@respx.mock
def test_upload_logs_list_with_items_key(upload_logs):
    """list() should handle a dict response with 'items' key."""
    respx.get(f"{BASE_URL}/api/v1/upload-logs").mock(
        return_value=httpx.Response(200, json={"items": [_UPLOAD_LOG]})
    )
    result = upload_logs.list()
    assert len(result) == 1
    assert isinstance(result[0], UploadLog)
    assert result[0].status == "success"


@respx.mock
def test_upload_logs_list_with_list_body(upload_logs):
    """list() should handle a bare list response."""
    respx.get(f"{BASE_URL}/api/v1/upload-logs").mock(
        return_value=httpx.Response(200, json=[_UPLOAD_LOG])
    )
    result = upload_logs.list()
    assert len(result) == 1


@respx.mock
def test_upload_logs_list_empty(upload_logs):
    """list() on an empty response should return an empty list."""
    respx.get(f"{BASE_URL}/api/v1/upload-logs").mock(
        return_value=httpx.Response(200, json={"items": []})
    )
    assert upload_logs.list() == []


@respx.mock
def test_upload_logs_list_with_filters(upload_logs):
    """list() should forward agent_id, status, and cursor as query params."""
    route = respx.get(f"{BASE_URL}/api/v1/upload-logs").mock(
        return_value=httpx.Response(200, json={"items": []})
    )
    upload_logs.list(agent_id="agent-uuid-a", status="failed", cursor="cur1")
    params = dict(route.calls[0].request.url.params)
    assert params["agent_id"] == "agent-uuid-a"
    assert params["status"] == "failed"
    assert params["cursor"] == "cur1"


@respx.mock
def test_upload_logs_get(upload_logs):
    """get() should return an UploadLog for the given ID."""
    respx.get(f"{BASE_URL}/api/v1/upload-logs/log-uuid-1").mock(
        return_value=httpx.Response(200, json=_UPLOAD_LOG)
    )
    log = upload_logs.get("log-uuid-1")
    assert isinstance(log, UploadLog)
    assert log.id == "log-uuid-1"
    assert log.size_bytes == 2048
    assert log.error_message is None


@respx.mock
def test_upload_logs_get_not_found(upload_logs):
    """get() for a missing log should raise NotFoundError."""
    respx.get(f"{BASE_URL}/api/v1/upload-logs/missing").mock(
        return_value=httpx.Response(404, text="Not Found")
    )
    with pytest.raises(NotFoundError):
        upload_logs.get("missing")


def test_upload_log_from_dict_minimal():
    """UploadLog.from_dict should handle minimal fields gracefully."""
    data = {"id": "x", "original_path": "/tmp/f", "started_at": "2025-01-01T00:00:00Z"}
    log = UploadLog.from_dict(data)
    assert log.id == "x"
    assert log.size_bytes == 0
    assert log.retry_count == 0
    assert log.error_message is None
    assert log.finished_at is None


# ===========================================================================
# FileType.from_dict edge cases
# ===========================================================================


def test_file_type_from_dict_minimal():
    """FileType.from_dict should work with minimal required fields."""
    data = {"id": "x", "name": "mytype"}
    ft = FileType.from_dict(data)
    assert ft.id == "x"
    assert ft.description is None
    assert ft.created_by is None
    assert ft.created_at == ""
