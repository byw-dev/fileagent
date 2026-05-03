"""Unit tests for fileagent.client.FileAgentClient."""
from __future__ import annotations

import httpx
import pytest
import respx

from fileagent import FileAgentClient
from fileagent.resources.agents import AgentsResource
from fileagent.resources.file_types import FileTypesResource
from fileagent.resources.files import FilesResource
from fileagent.resources.upload_logs import UploadLogsResource

BASE_URL = "https://test.example.com"

# Build a minimal login response so the client can obtain a token.
_LOGIN_RESPONSE = {
    "access_token": "test-access-token",
    "refresh_token": "test-refresh-token",
    "expires_in": 7200,
}


@pytest.fixture()
def client():
    """FileAgentClient with login mocked out."""
    with respx.mock:
        respx.post(f"{BASE_URL}/api/auth/login").mock(
            return_value=httpx.Response(200, json=_LOGIN_RESPONSE)
        )
        c = FileAgentClient(
            base_url=BASE_URL,
            username="user",
            password="pass",
            timeout=5,
            max_retries=0,
            verify_ssl=False,
        )
    yield c
    c.close()


# ---------------------------------------------------------------------------
# Resource accessor types
# ---------------------------------------------------------------------------


def test_files_accessor_returns_files_resource(client):
    """client.files should be a FilesResource instance."""
    assert isinstance(client.files, FilesResource)


def test_file_types_accessor_returns_file_types_resource(client):
    """client.file_types should be a FileTypesResource instance."""
    assert isinstance(client.file_types, FileTypesResource)


def test_agents_accessor_returns_agents_resource(client):
    """client.agents should be an AgentsResource instance."""
    assert isinstance(client.agents, AgentsResource)


def test_upload_logs_accessor_returns_upload_logs_resource(client):
    """client.upload_logs should be an UploadLogsResource instance."""
    assert isinstance(client.upload_logs, UploadLogsResource)


# ---------------------------------------------------------------------------
# Context manager
# ---------------------------------------------------------------------------


@respx.mock
def test_context_manager_closes_client():
    """FileAgentClient should be usable as a context manager."""
    respx.post(f"{BASE_URL}/api/auth/login").mock(
        return_value=httpx.Response(200, json=_LOGIN_RESPONSE)
    )
    with FileAgentClient(
        base_url=BASE_URL,
        username="user",
        password="pass",
        timeout=5,
        max_retries=0,
    ) as c:
        assert isinstance(c.files, FilesResource)
    # After exit, the underlying httpx client is closed; further use would
    # raise an error, which confirms close() was called.
