"""Conservative role normalization for Gas City runtime session names.

Role is provenance on the session, not the role field on an individual chat
message. Pool instance numbers are removed so the same agent template has one
stable role across workers.
"""

from __future__ import annotations

import re

_INSTANCE_SUFFIX = re.compile(r"-\d+$")


def role_from_session_name(session_name: str | None) -> str | None:
    """Return the normalized agent-template role carried by a GC session name.

    GC runtime names normally use ``<rig>--<agent-template>``. Some imported
    session names retain ``/`` or ``__`` as their rig/template separator, while
    an unqualified pool name still has the explicit ``-pool`` marker. Names
    that do not look like GC names are left unknown rather than guessed from
    transcript message roles or arbitrary session IDs.

    Examples::

        gateway-llm-dell--dsh-luna-1-pool -> dsh-luna pool
        test-city--mayor                 -> mayor
        test-city--fleet-work-review-2-pool -> fleet-work review pool
    """

    if not isinstance(session_name, str) or not session_name.strip():
        return None

    # Session-log identities may append a bead id / continuation epoch. These
    # suffixes are not part of the stable runtime session name.
    name = session_name.strip().split("@", 1)[0].split("#", 1)[0]
    separator = None
    for delimiter in ("--", "__"):
        if delimiter in name:
            name = name.rsplit(delimiter, 1)[-1]
            separator = delimiter
            break
    if separator is None and "/" in name:
        # The route spelling ``rig/template`` is used by some GC prompts. Do
        # not treat arbitrary path-like provider session IDs as role names.
        candidate = name.rsplit("/", 1)[-1]
        if candidate.startswith(("dsh-", "fleet-work")) or candidate == "mayor":
            name = candidate
            separator = "/"

    pool = name.endswith("-pool")
    stem = name[:-5] if pool else name
    looks_like_gc_role = (
        separator in {"--", "__"}
        or pool
        or stem == "mayor"
        or stem.startswith("fleet-work")
        or (separator == "/" and stem.startswith("dsh-"))
    )
    if not looks_like_gc_role:
        return None

    # A pool's trailing number is its slot, not part of the template. The
    # slash-qualified dsh route spelling (e.g. dsh-luna-3) also numbers workers.
    stem = _INSTANCE_SUFFIX.sub("", stem)
    if not stem:
        return None
    if stem == "mayor":
        return "mayor"

    if stem.startswith("fleet-work-"):
        # Preserve a lane suffix when the template names one, e.g.
        # fleet-work-review-2-pool -> "fleet-work review pool".
        lane = stem[len("fleet-work-"):]
        role = f"fleet-work {lane}"
    else:
        role = stem
    if pool:
        role += " pool"
    return role


__all__ = ["role_from_session_name"]
