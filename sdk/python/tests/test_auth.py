"""Unit tests for fileagent.auth.TokenManager."""
from __future__ import annotations

import threading
from datetime import datetime, timedelta, timezone
from unittest.mock import MagicMock, call, patch

import httpx
import pytest
import respx

from fileagent.auth import TokenManager
from fileagent.exceptions import AuthenticationError
from fileagent.http import HTTPClient

BASE_URL = "https://test.example.com"

# Helpers for building fake token responses
_NOW = datetime.now(tz=timezone.utc)

VALID_RESPONSE = {
    "access_token": "access-tok-1",
    "refresh_token": "refresh-tok-1",
    "expires_in": 7200,  # 2 hours
}

REFRESHED_RESPONSE = {
    "access_token": "access-tok-2",
    "refresh_token": "refresh-tok-2",
    "expires_in": 7200,
}

LOGIN_RESPONSE = {
    "access_token": "access-tok-login",
    "refresh_token": "refresh-tok-login",
    "expires_in": 7200,
}


@pytest.fixture()
def http_client():
    """Return an HTTPClient with no retries."""
    c = HTTPClient(base_url=BASE_URL, timeout=5, max_retries=0)
    yield c
    c.close()


@pytest.fixture()
def token_manager(http_client):
    """Return a TokenManager backed by the http_client fixture."""
    return TokenManager(
        http_client=http_client,
        username="testuser",
        password="testpass",
    )


# ---------------------------------------------------------------------------
# Happy-path: get_access_token returns valid token
# ---------------------------------------------------------------------------


@respx.mock
def test_get_access_token_returns_valid_token(token_manager):
    """First call should login and return the access token."""
    respx.post(f"{BASE_URL}/api/auth/login").mock(
        return_value=httpx.Response(200, json=VALID_RESPONSE)
    )
    token = token_manager.get_access_token()
    assert token == "access-tok-1"


# ---------------------------------------------------------------------------
# Auto-refresh when token is expiring soon
# ---------------------------------------------------------------------------


@respx.mock
def test_get_access_token_auto_refreshes_expiring_soon(token_manager):
    """When token expires within 5 minutes, get_access_token should refresh."""
    # Bootstrap with a near-expired token
    token_manager._access_token = "old-token"
    token_manager._refresh_token = "refresh-tok-1"
    token_manager._expires_at = datetime.now(tz=timezone.utc) + timedelta(minutes=2)

    respx.post(f"{BASE_URL}/api/auth/refresh").mock(
        return_value=httpx.Response(200, json=REFRESHED_RESPONSE)
    )
    token = token_manager.get_access_token()
    assert token == "access-tok-2"


# ---------------------------------------------------------------------------
# Token not refreshed when still valid
# ---------------------------------------------------------------------------


@respx.mock
def test_get_access_token_no_refresh_when_valid(token_manager):
    """When token is valid (> 5 min remaining) no network call should be made."""
    token_manager._access_token = "still-valid"
    token_manager._refresh_token = "refresh-tok"
    token_manager._expires_at = datetime.now(tz=timezone.utc) + timedelta(hours=1)

    # No routes registered — any HTTP call would raise an error.
    token = token_manager.get_access_token()
    assert token == "still-valid"


# ---------------------------------------------------------------------------
# Refresh failure falls back to re-login
# ---------------------------------------------------------------------------


@respx.mock
def test_refresh_failure_falls_back_to_login(token_manager):
    """If refresh returns 401, TokenManager should fall back to re-login."""
    token_manager._access_token = "old-token"
    token_manager._refresh_token = "expired-refresh"
    token_manager._expires_at = datetime.now(tz=timezone.utc) + timedelta(minutes=1)

    respx.post(f"{BASE_URL}/api/auth/refresh").mock(
        return_value=httpx.Response(401, text="Unauthorized")
    )
    respx.post(f"{BASE_URL}/api/auth/login").mock(
        return_value=httpx.Response(200, json=LOGIN_RESPONSE)
    )
    token = token_manager.get_access_token()
    assert token == "access-tok-login"


# ---------------------------------------------------------------------------
# Thread safety: 5 concurrent calls
# ---------------------------------------------------------------------------


@respx.mock
def test_concurrent_access_thread_safe(token_manager):
    """Concurrent calls to get_access_token must all receive the same token."""
    login_count = 0
    original_refresh = token_manager._refresh

    def counting_refresh():
        """Count the number of times _refresh is actually called."""
        nonlocal login_count
        login_count += 1
        original_refresh()

    # Seed a near-expired token to force a refresh on first call.
    token_manager._access_token = "old-token"
    token_manager._refresh_token = "refresh-tok"
    token_manager._expires_at = datetime.now(tz=timezone.utc) + timedelta(minutes=1)

    respx.post(f"{BASE_URL}/api/auth/refresh").mock(
        return_value=httpx.Response(200, json=REFRESHED_RESPONSE)
    )

    token_manager._refresh = counting_refresh

    results: list[str] = []
    errors: list[Exception] = []

    def worker():
        try:
            tok = token_manager.get_access_token()
            results.append(tok)
        except Exception as exc:
            errors.append(exc)

    threads = [threading.Thread(target=worker) for _ in range(5)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()

    assert not errors, f"Thread errors: {errors}"
    # All threads should see the refreshed token.
    assert all(tok == "access-tok-2" for tok in results)
    # The lock should have serialised calls — _refresh called exactly once.
    assert login_count == 1


# ---------------------------------------------------------------------------
# _is_expiring_soon
# ---------------------------------------------------------------------------


def test_is_expiring_soon_when_no_token(token_manager):
    """_is_expiring_soon should return True when access_token is None."""
    token_manager._access_token = None
    token_manager._expires_at = None
    assert token_manager._is_expiring_soon() is True


def test_is_expiring_soon_within_threshold(token_manager):
    """_is_expiring_soon should return True when < 5 min remain."""
    token_manager._access_token = "tok"
    token_manager._expires_at = datetime.now(tz=timezone.utc) + timedelta(minutes=3)
    assert token_manager._is_expiring_soon() is True


def test_is_expiring_soon_outside_threshold(token_manager):
    """_is_expiring_soon should return False when > 5 min remain."""
    token_manager._access_token = "tok"
    token_manager._expires_at = datetime.now(tz=timezone.utc) + timedelta(hours=1)
    assert token_manager._is_expiring_soon() is False


# ---------------------------------------------------------------------------
# _update_tokens stores correct values
# ---------------------------------------------------------------------------


def test_update_tokens_stores_correct_values(token_manager):
    """_update_tokens should parse and store all token fields."""
    before = datetime.now(tz=timezone.utc)
    token_manager._update_tokens(VALID_RESPONSE)
    after = datetime.now(tz=timezone.utc)

    assert token_manager._access_token == "access-tok-1"
    assert token_manager._refresh_token == "refresh-tok-1"
    assert token_manager._expires_at is not None
    # expires_in = 7200 seconds → ~2 hours from now
    expected_min = before + timedelta(seconds=7200)
    expected_max = after + timedelta(seconds=7200)
    assert expected_min <= token_manager._expires_at <= expected_max
