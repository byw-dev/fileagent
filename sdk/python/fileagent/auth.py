"""Token management for the FileAgent SDK.

Implements thread-safe access token lifecycle: initial login, proactive
refresh when the token is about to expire, and fall-back re-login when
the refresh token itself has expired.
"""
from __future__ import annotations

import threading
from datetime import datetime, timedelta, timezone
from typing import TYPE_CHECKING, Any

from fileagent.exceptions import AuthenticationError

if TYPE_CHECKING:
    from fileagent.http import HTTPClient

# Threshold: refresh the access token if it expires within this window.
_REFRESH_THRESHOLD = timedelta(minutes=5)

_LOGIN_PATH = "/api/auth/login"
_REFRESH_PATH = "/api/auth/refresh"


class TokenManager:
    """Thread-safe manager for FileAgent Bearer tokens.

    Maintains a valid access token by transparently refreshing it before
    it expires.  The refresh strategy is:

    1. If a refresh token is available, attempt ``POST /api/auth/refresh``.
    2. If that fails with :class:`~fileagent.exceptions.AuthenticationError`
       (refresh token has expired), fall back to a full re-login via
       ``POST /api/auth/login``.

    All state mutations are protected by an internal :class:`threading.Lock`
    so multiple threads can call :meth:`get_access_token` concurrently.

    Attributes:
        _http: The HTTPClient used for auth endpoints.
        _username: Username used for re-login.
        _password: Password used for re-login.
        _access_token: Current access token string (or None).
        _refresh_token: Current refresh token string (or None).
        _expires_at: UTC datetime when the access token expires (or None).
        _lock: Mutex guarding all token state.

    Example:
        >>> manager = TokenManager(http_client, "user", "secret")
        >>> token = manager.get_access_token()
    """

    def __init__(
        self,
        http_client: "HTTPClient",
        username: str,
        password: str,
    ) -> None:
        """Initialize the TokenManager.

        Args:
            http_client: An :class:`~fileagent.http.HTTPClient` instance.
                The client must *not* have ``get_access_token`` set yet;
                that callback is wired up by :class:`~fileagent.client.FileAgentClient`
                after construction.
            username: Username for ``POST /api/auth/login``.
            password: Password for ``POST /api/auth/login``.
        """
        self._http = http_client
        self._username = username
        self._password = password

        self._access_token: str | None = None
        self._refresh_token: str | None = None
        self._expires_at: datetime | None = None
        self._lock = threading.Lock()

    # ------------------------------------------------------------------
    # Public API
    # ------------------------------------------------------------------

    def get_access_token(self) -> str:
        """Return a valid access token, refreshing first if necessary.

        This method is thread-safe.  If the current token is expiring
        within 5 minutes (or has never been fetched), it will call
        :meth:`_refresh` before returning.

        Returns:
            A valid Bearer token string.

        Raises:
            AuthenticationError: If both the refresh attempt and the
                re-login fallback fail.
        """
        with self._lock:
            if self._is_expiring_soon():
                self._refresh()
            assert self._access_token is not None
            return self._access_token

    # ------------------------------------------------------------------
    # Internal helpers
    # ------------------------------------------------------------------

    def _is_expiring_soon(self) -> bool:
        """Return True if the access token is absent or expires within 5 min.

        Returns:
            True when a refresh is needed, False otherwise.
        """
        if self._access_token is None or self._expires_at is None:
            return True
        now = datetime.now(tz=timezone.utc)
        return self._expires_at - now < _REFRESH_THRESHOLD

    def _refresh(self) -> None:
        """Refresh the access token.

        First tries the refresh token endpoint.  Falls back to a full
        re-login if the refresh token has expired or is unavailable.

        Raises:
            AuthenticationError: If re-login also fails.
        """
        if self._refresh_token:
            try:
                data = self._http.post(
                    _REFRESH_PATH,
                    json={"refresh_token": self._refresh_token},
                )
                self._update_tokens(data)
                return
            except AuthenticationError:
                pass  # fall through to re-login

        data = self._http.post(
            _LOGIN_PATH,
            json={"username": self._username, "password": self._password},
        )
        self._update_tokens(data)

    def _update_tokens(self, response_data: dict[str, Any]) -> None:
        """Persist token data from a login or refresh response.

        Parses ``access_token``, ``refresh_token``, and ``expires_in``
        from the API response and stores them as instance attributes.
        The expiry is stored as a timezone-aware UTC :class:`datetime`.

        Args:
            response_data: Decoded JSON body from the auth endpoint.
                Expected keys: ``access_token`` (str),
                ``refresh_token`` (str), ``expires_in`` (int, seconds).
        """
        self._access_token = response_data["access_token"]
        self._refresh_token = response_data.get("refresh_token")
        expires_in: int = response_data.get("expires_in", 7200)
        self._expires_at = datetime.now(tz=timezone.utc) + timedelta(
            seconds=expires_in
        )
