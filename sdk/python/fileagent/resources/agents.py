"""Agents resource — read-only access to edge agent records.

T2-D2 implementation.  See system-design.md §5.11.2.
"""
from __future__ import annotations

from typing import TYPE_CHECKING, List

from fileagent.models.agent import Agent

if TYPE_CHECKING:
    from fileagent.http import HTTPClient

_AGENTS_PATH = "/api/v1/agents"


class AgentsResource:
    """Access edge agent records on the FileAgent control plane (read-only).

    Args:
        http: The authenticated :class:`~fileagent.http.HTTPClient` instance.

    Example:
        >>> for agent in client.agents.list():
        ...     print(agent.name, agent.status)
    """

    def __init__(self, http: "HTTPClient") -> None:
        """Initialise AgentsResource.

        Args:
            http: Authenticated HTTP client.
        """
        self._http = http

    def list(self) -> List[Agent]:
        """List all agents registered in the organisation.

        Returns:
            A list of :class:`~fileagent.models.agent.Agent` objects.
        """
        data = self._http.get(_AGENTS_PATH)
        items = data if isinstance(data, list) else data.get("items", [])
        return [Agent.from_dict(item) for item in items]

    def get(self, agent_id: str) -> Agent:
        """Retrieve a single agent by its UUID.

        Args:
            agent_id: UUID of the agent.

        Returns:
            An :class:`~fileagent.models.agent.Agent` instance.

        Raises:
            NotFoundError: If no agent with the given ID exists.
        """
        data = self._http.get(f"{_AGENTS_PATH}/{agent_id}")
        return Agent.from_dict(data)
