#!/usr/bin/env python3
"""Apply a narrow audience-validation compatibility fix to pinned mcp-agent-mail."""

from __future__ import annotations

import sys
import sysconfig
from pathlib import Path


OLD = """                claims = jwt.decode(token, key)
                if audience:
                    claims.validate_aud(audience)
"""

NEW = """                claims_options = (
                    {"aud": {"essential": True, "values": [audience]}}
                    if audience
                    else None
                )
                claims = jwt.decode(token, key, claims_options=claims_options)
"""


def main() -> None:
    if len(sys.argv) > 2:
        raise SystemExit("usage: patch-mcp-mail-jwt-audience.py [http.py]")

    source = (
        Path(sys.argv[1])
        if len(sys.argv) == 2
        else Path(sysconfig.get_paths()["purelib"]) / "mcp_agent_mail" / "http.py"
    )
    text = source.read_text(encoding="utf-8")
    matches = text.count(OLD)
    if matches != 1:
        raise SystemExit(
            f"expected exactly one pinned JWT audience block in {source}, found {matches}"
        )

    source.write_text(text.replace(OLD, NEW, 1), encoding="utf-8")
    print(f"patched JWT audience validation in {source}")


if __name__ == "__main__":
    main()
