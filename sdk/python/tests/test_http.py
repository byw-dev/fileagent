"""Unit tests for fileagent.http.HTTPClient using respx mock transport."""
from __future__ import annotations

import httpx
import pytest
import respx

from fileagent.exceptions import (
    AuthenticationError,
    NetworkError,
    NotFoundError,
    PermissionError,
    RateLimitError,
    ServerError,
)
from fileagent.http import HTTPClient

BASE_URL = "https://test.example.com"


@pytest.fixture()
def client():
    """Return an HTTPClient with retries disabled for most tests."""
    c = HTTPClient(base_url=BASE_URL, timeout=5, max_retries=0)
    yield c
    c.close()


@pytest.fixture()
def retry_client():
    """Return an HTTPClient with 2 retries for retry-logic tests."""
    c = HTTPClient(base_url=BASE_URL, timeout=5, max_retries=2)
    yield c
    c.close()


# ---------------------------------------------------------------------------
# Successful requests
# ---------------------------------------------------------------------------


@respx.mock
def test_get_success(client):
    """Successful GET returns parsed JSON body."""
    respx.get(f"{BASE_URL}/api/v1/files").mock(
        return_value=httpx.Response(200, json={"items": []})
    )
    result = client.get("/api/v1/files")
    assert result == {"items": []}


@respx.mock
def test_post_success(client):
    """Successful POST returns parsed JSON body."""
    respx.post(f"{BASE_URL}/api/auth/login").mock(
        return_value=httpx.Response(
            200,
            json={
                "access_token": "tok",
                "refresh_token": "ref",
                "expires_in": 7200,
            },
        )
    )
    result = client.post("/api/auth/login", json={"username": "u", "password": "p"})
    assert result["access_token"] == "tok"


# ---------------------------------------------------------------------------
# Error status code mapping
# ---------------------------------------------------------------------------


@respx.mock
def test_401_raises_authentication_error(client):
    """HTTP 401 should raise AuthenticationError."""
    respx.get(f"{BASE_URL}/api/v1/files").mock(
        return_value=httpx.Response(401, text="Unauthorized")
    )
    with pytest.raises(AuthenticationError):
        client.get("/api/v1/files")


@respx.mock
def test_403_raises_permission_error(client):
    """HTTP 403 should raise PermissionError."""
    respx.get(f"{BASE_URL}/api/v1/files").mock(
        return_value=httpx.Response(403, text="Forbidden")
    )
    with pytest.raises(PermissionError):
        client.get("/api/v1/files")


@respx.mock
def test_404_raises_not_found_error(client):
    """HTTP 404 should raise NotFoundError."""
    respx.get(f"{BASE_URL}/api/v1/files/missing").mock(
        return_value=httpx.Response(404, text="Not Found")
    )
    with pytest.raises(NotFoundError):
        client.get("/api/v1/files/missing")


@respx.mock
def test_429_raises_rate_limit_error(client):
    """HTTP 429 should raise RateLimitError."""
    respx.get(f"{BASE_URL}/api/v1/files").mock(
        return_value=httpx.Response(429, text="Too Many Requests")
    )
    with pytest.raises(RateLimitError):
        client.get("/api/v1/files")


@respx.mock
def test_500_raises_server_error(client):
    """HTTP 500 should raise ServerError."""
    respx.get(f"{BASE_URL}/api/v1/files").mock(
        return_value=httpx.Response(500, text="Internal Server Error")
    )
    with pytest.raises(ServerError):
        client.get("/api/v1/files")


# ---------------------------------------------------------------------------
# Retry logic
# ---------------------------------------------------------------------------


@respx.mock
def test_retry_on_server_error_eventually_fails(retry_client):
    """Server errors should be retried; raises after max_retries exhausted."""
    route = respx.get(f"{BASE_URL}/api/v1/files").mock(
        return_value=httpx.Response(503, text="Service Unavailable")
    )
    with pytest.raises(ServerError):
        retry_client.get("/api/v1/files")
    # 1 initial attempt + 2 retries = 3 total calls
    assert route.call_count == 3


@respx.mock
def test_retry_on_server_error_succeeds_on_second_attempt(retry_client):
    """A transient 500 followed by 200 should succeed after one retry."""
    route = respx.get(f"{BASE_URL}/api/v1/files").mock(
        side_effect=[
            httpx.Response(500, text="oops"),
            httpx.Response(200, json={"items": []}),
        ]
    )
    # Patch sleep to avoid actual delay during tests
    import fileagent.http as http_module

    original_sleep = http_module.time.sleep
    http_module.time.sleep = lambda _: None
    try:
        result = retry_client.get("/api/v1/files")
    finally:
        http_module.time.sleep = original_sleep

    assert result == {"items": []}
    assert route.call_count == 2


# ---------------------------------------------------------------------------
# Network error
# ---------------------------------------------------------------------------


@respx.mock
def test_network_error_raises_network_error(client):
    """A transport-level error should raise NetworkError."""
    respx.get(f"{BASE_URL}/api/v1/files").mock(
        side_effect=httpx.ConnectError("connection refused")
    )
    with pytest.raises(NetworkError):
        client.get("/api/v1/files")


@respx.mock
def test_retry_on_network_error_eventually_fails(retry_client):
    """Network errors should be retried; raises NetworkError after exhausting."""
    import fileagent.http as http_module

    original_sleep = http_module.time.sleep
    http_module.time.sleep = lambda _: None
    try:
        route = respx.get(f"{BASE_URL}/api/v1/files").mock(
            side_effect=httpx.ConnectError("connection refused")
        )
        with pytest.raises(NetworkError):
            retry_client.get("/api/v1/files")
        assert route.call_count == 3  # 1 + 2 retries
    finally:
        http_module.time.sleep = original_sleep


# ---------------------------------------------------------------------------
# Authorization header injection
# ---------------------------------------------------------------------------


@respx.mock
def test_auth_header_injected_when_callback_set(client):
    """Authorization header should be injected from get_access_token callback."""
    client.get_access_token = lambda: "my-token"
    route = respx.get(f"{BASE_URL}/api/v1/files").mock(
        return_value=httpx.Response(200, json={})
    )
    client.get("/api/v1/files")
    assert route.calls[0].request.headers["authorization"] == "Bearer my-token"


@respx.mock
def test_no_auth_header_when_callback_not_set(client):
    """No Authorization header should be sent when callback is None."""
    client.get_access_token = None
    route = respx.get(f"{BASE_URL}/api/v1/files").mock(
        return_value=httpx.Response(200, json={})
    )
    client.get("/api/v1/files")
    assert "authorization" not in route.calls[0].request.headers
