# FileAgent Python SDK

Python SDK for the [FileAgent](https://github.com/byw-dev/fileagent) file collection,
synchronization, and distribution platform.

## Installation

```bash
pip install fileagent-sdk
```

## Quick Start

```python
from fileagent import FileAgentClient

client = FileAgentClient(
    base_url="https://control.example.com",
    username="api-user",
    password="secret",
    timeout=30,
    max_retries=3,
    verify_ssl=True,
)

# Token management is transparent — the client automatically logs in,
# refreshes the access token before it expires, and falls back to
# re-login if the refresh token has itself expired.
```

## Development

```bash
# Install Poetry
pip install poetry

# Install dependencies
poetry install

# Run tests
poetry run pytest tests/ -v
```

## Architecture

| Module | Purpose |
|--------|---------|
| `fileagent.client` | Top-level `FileAgentClient` entry point |
| `fileagent.http` | `HTTPClient` wrapping httpx with retry + error mapping |
| `fileagent.auth` | `TokenManager` — thread-safe token lifecycle |
| `fileagent.exceptions` | Typed SDK exception hierarchy |
| `fileagent.resources` | Per-domain resource accessors (Phase 2) |
| `fileagent.models` | Typed response models (Phase 2) |
