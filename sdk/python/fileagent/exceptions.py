"""Exception classes for the FileAgent SDK.

All SDK exceptions derive from FileAgentError, allowing callers to catch
either the base class or specific subclasses as appropriate.
"""
from __future__ import annotations


class FileAgentError(Exception):
    """Base exception for all FileAgent SDK errors.

    Attributes:
        message: Human-readable description of the error.
    """

    def __init__(self, message: str = "") -> None:
        """Initialize FileAgentError.

        Args:
            message: Human-readable description of the error.
        """
        self.message = message
        super().__init__(message)


class AuthenticationError(FileAgentError):
    """Raised when the server returns HTTP 401 Unauthorized.

    This typically means the access token is missing, expired, or invalid.
    The TokenManager will attempt to refresh automatically before raising
    this exception to the caller.
    """


class PermissionError(FileAgentError):
    """Raised when the server returns HTTP 403 Forbidden.

    The authenticated user does not have sufficient permission for the
    requested operation.
    """


class NotFoundError(FileAgentError):
    """Raised when the server returns HTTP 404 Not Found.

    The requested resource does not exist or has been deleted.
    """


class RateLimitError(FileAgentError):
    """Raised when the server returns HTTP 429 Too Many Requests.

    The client has exceeded the server-side rate limit. Callers should
    implement back-off before retrying.
    """


class ServerError(FileAgentError):
    """Raised when the server returns an HTTP 5xx status code.

    Indicates an unexpected error on the server side. The HTTP layer
    will automatically retry on these errors up to *max_retries* times
    before raising this exception.

    Attributes:
        status_code: The HTTP status code returned by the server.
    """

    def __init__(self, message: str = "", status_code: int = 500) -> None:
        """Initialize ServerError.

        Args:
            message: Human-readable description of the error.
            status_code: HTTP status code (5xx).
        """
        self.status_code = status_code
        super().__init__(message)


class NetworkError(FileAgentError):
    """Raised when a network-level error occurs.

    This includes connection refused, DNS resolution failure, TLS errors,
    and other transport-layer problems. The HTTP layer will automatically
    retry on these errors up to *max_retries* times before raising.
    """
