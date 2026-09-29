"""Deprecated session-name role helper.

Agent roles are not inferred from provider session names. Use the exact GC
session ``template`` imported by :mod:`agent_observatory.gc_enrichment` instead.
This no-op remains temporarily for downstream import compatibility.
"""

from __future__ import annotations


def role_from_session_name(session_name: str | None) -> None:
    """Return no role: session names are not authoritative role metadata."""

    return None


__all__ = ["role_from_session_name"]
