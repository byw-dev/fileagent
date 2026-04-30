"""Unit tests for fileagent.exceptions."""
from __future__ import annotations

import pytest

from fileagent.exceptions import (
    AuthenticationError,
    FileAgentError,
    NetworkError,
    NotFoundError,
    PermissionError,
    RateLimitError,
    ServerError,
)


class TestExceptionHierarchy:
    """Verify that all exceptions are subclasses of FileAgentError."""

    def test_authentication_error_is_fileagent_error(self):
        """AuthenticationError should derive from FileAgentError."""
        assert issubclass(AuthenticationError, FileAgentError)

    def test_permission_error_is_fileagent_error(self):
        """PermissionError should derive from FileAgentError."""
        assert issubclass(PermissionError, FileAgentError)

    def test_not_found_error_is_fileagent_error(self):
        """NotFoundError should derive from FileAgentError."""
        assert issubclass(NotFoundError, FileAgentError)

    def test_rate_limit_error_is_fileagent_error(self):
        """RateLimitError should derive from FileAgentError."""
        assert issubclass(RateLimitError, FileAgentError)

    def test_server_error_is_fileagent_error(self):
        """ServerError should derive from FileAgentError."""
        assert issubclass(ServerError, FileAgentError)

    def test_network_error_is_fileagent_error(self):
        """NetworkError should derive from FileAgentError."""
        assert issubclass(NetworkError, FileAgentError)


class TestServerError:
    """Verify ServerError carries the status_code attribute."""

    def test_default_status_code(self):
        """Default status_code should be 500."""
        err = ServerError("internal server error")
        assert err.status_code == 500

    def test_custom_status_code(self):
        """Custom status_code should be stored on the instance."""
        err = ServerError("gateway timeout", status_code=504)
        assert err.status_code == 504

    def test_message_stored(self):
        """Message should be accessible via .message attribute."""
        err = ServerError("oops", status_code=503)
        assert err.message == "oops"
        assert str(err) == "oops"


class TestBaseExceptionMessage:
    """Verify FileAgentError and subclasses store message correctly."""

    @pytest.mark.parametrize(
        "exc_cls",
        [
            FileAgentError,
            AuthenticationError,
            PermissionError,
            NotFoundError,
            RateLimitError,
            NetworkError,
        ],
    )
    def test_message_attribute(self, exc_cls):
        """All exceptions should expose .message and str() == message."""
        exc = exc_cls("test message")
        assert exc.message == "test message"
        assert str(exc) == "test message"

    def test_empty_message(self):
        """Instantiating with no args should not raise."""
        exc = FileAgentError()
        assert exc.message == ""
