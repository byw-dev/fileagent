"""Resource modules for the FileAgent SDK."""
from __future__ import annotations

from fileagent.resources.agents import AgentsResource
from fileagent.resources.file_types import FileTypesResource
from fileagent.resources.files import FilesResource
from fileagent.resources.upload_logs import UploadLogsResource

__all__ = [
    "AgentsResource",
    "FileTypesResource",
    "FilesResource",
    "UploadLogsResource",
]
