#!/usr/bin/env python3
"""Fail-closed changed-path selection and Bazel BEP target correlation.

The resolver is deliberately limited to the three bounded config pilot labels.
It treats missing diff data, storage/config changes outside the mapped files,
and build-graph changes as conservative all-target selections.
"""

from __future__ import annotations

import argparse
import ast
import dataclasses
import json
import os
import posixpath
import re
import sys
from pathlib import Path
from typing import Any, Iterable

CONFIG_LABELS = (
    "//internal/config:config_diagnostic_locations_test",
    "//internal/config:config_envname_test",
    "//internal/config:config_storage_endpoint_test",
)

_MAPPED_PATHS = {
    "internal/config/envname.go": "//internal/config:config_envname_test",
    "internal/config/config_envname_bazel_test.go": "//internal/config:config_envname_test",
    "internal/config/diagnostic_locations.go": "//internal/config:config_diagnostic_locations_test",
    "internal/config/diagnostic_locations_test.go": "//internal/config:config_diagnostic_locations_test",
    "internal/config/storage_endpoint.go": "//internal/config:config_storage_endpoint_test",
    "internal/config/storage_endpoint_bazel_test.go": "//internal/config:config_storage_endpoint_test",
}
_STATUS = re.compile(r"^[A-Z][0-9]*$")


@dataclasses.dataclass(frozen=True)
class Selection:
    labels: tuple[str, ...]
    conservative: bool
    reason: str
    error: str | None = None

    def as_dict(self) -> dict[str, Any]:
        return dataclasses.asdict(self) | {"labels": list(self.labels)}

    def as_tsv(self) -> str:
        return "\t".join((",".join(self.labels), str(self.conservative).lower(), self.reason, self.error or ""))


@dataclasses.dataclass(frozen=True)
class BepTargets:
    configured: tuple[str, ...]
    completed: tuple[str, ...]
    configured_count: int | None
    completed_count: int | None
    action_summary: dict[str, Any] | None
    error: str | None = None

    def as_dict(self) -> dict[str, Any]:
        return dataclasses.asdict(self) | {
            "configured": list(self.configured),
            "completed": list(self.completed),
        }

    def as_tsv(self) -> str:
        return "\t".join((
            ",".join(self.configured),
            ",".join(self.completed),
            "unknown" if self.configured_count is None else str(self.configured_count),
            "unknown" if self.completed_count is None else str(self.completed_count),
            self.error or "",
        ))


def _fail_closed(error: str) -> Selection:
    return Selection(CONFIG_LABELS, True, "unavailable", error)


def _normalize_path(path: str) -> str:
    if len(path) >= 2 and path.startswith('"') and path.endswith('"'):
        try:
            decoded = ast.literal_eval(path)
            if isinstance(decoded, str):
                path = decoded
        except (SyntaxError, ValueError):
            pass
    if not path or "\x00" in path:
        raise ValueError("empty or NUL path")
    normalized = posixpath.normpath(path)
    if normalized == "." or normalized.startswith("../") or normalized == ".." or normalized.startswith("/"):
        raise ValueError(f"not repo-relative: {path!r}")
    return normalized


def _parse_name_status_z(raw: bytes) -> list[tuple[str, tuple[str, ...]]]:
    if not raw:
        raise ValueError("empty name-status input")
    if b"\0" not in raw:
        records: list[tuple[str, tuple[str, ...]]] = []
        for line in raw.splitlines():
            fields = line.decode(errors="surrogateescape").split("\t")
            if len(fields) < 2 or not _STATUS.fullmatch(fields[0]):
                raise ValueError("malformed tab-delimited name-status input")
            status = fields[0]
            count = 2 if status[0] in {"R", "C"} else 1
            paths = fields[1:]
            if len(paths) != count:
                raise ValueError(f"{status} needs {count} path(s)")
            records.append((status, tuple(_normalize_path(path) for path in paths)))
        if not records:
            raise ValueError("empty name-status input")
        return records
    fields = raw.split(b"\0")
    if fields[-1] != b"":
        raise ValueError("unterminated name-status input")
    fields.pop()
    records: list[tuple[str, tuple[str, ...]]] = []
    i = 0
    while i < len(fields):
        header = os.fsdecode(fields[i])
        i += 1
        tab = header.split("\t")
        status = tab[0]
        if not _STATUS.fullmatch(status):
            raise ValueError(f"invalid status {status!r}")
        needs_two = status[0] in {"R", "C"}
        if len(tab) > 1:
            paths = tuple(tab[1:])
            if needs_two and len(paths) != 2:
                raise ValueError(f"{status} needs old and new paths")
            if not needs_two and len(paths) != 1:
                # A tab is legal in a path; only split the status delimiter.
                paths = (header[len(status) + 1 :],)
        else:
            count = 2 if needs_two else 1
            if i + count > len(fields):
                raise ValueError(f"{status} missing path")
            paths = tuple(os.fsdecode(part) for part in fields[i : i + count])
            i += count
        records.append((status, tuple(_normalize_path(path) for path in paths)))
    return records


def _is_shared_graph_path(path: str) -> bool:
    name = posixpath.basename(path)
    return (
        path in {"MODULE.bazel", "MODULE.bazel.lock", ".bazelrc", ".bazelversion", "go.mod", "go.sum", "BUILD.bazel"}
        or name in {"BUILD", "BUILD.bazel"}
        or path.endswith(".bzl")
    )


def resolve_name_status_z(raw: bytes) -> Selection:
    """Resolve NUL-delimited `git diff --name-status -z` output fail-closed."""
    try:
        records = _parse_name_status_z(raw)
    except ValueError as exc:
        return _fail_closed(str(exc))

    mapped: set[str] = set()
    shared = False
    config_unmapped = False
    for status, paths in records:
        for path in paths:
            if _is_shared_graph_path(path):
                shared = True
            if path.startswith("internal/config/"):
                # Deletes/renames/copies need complete re-analysis: either side
                # may remove or introduce a package-level dependency.
                if status[0] in {"D", "R", "C"} or path not in _MAPPED_PATHS:
                    config_unmapped = True
                else:
                    mapped.add(_MAPPED_PATHS[path])
    if shared:
        return Selection(CONFIG_LABELS, True, "shared-build-graph")
    if config_unmapped:
        return Selection(CONFIG_LABELS, True, "config-unmapped")
    if mapped:
        return Selection(tuple(sorted(mapped)), False, "mapped")
    return Selection((), False, "unrelated")


def _normalize_label(value: Any) -> str | None:
    if not isinstance(value, str):
        return None
    value = value.strip()
    if not value.startswith("//"):
        return None
    package, separator, target = value[2:].partition(":")
    if not separator or not package or not target:
        return None
    return f"//{package}:{target}"


def _event_label(event: dict[str, Any], kind: str) -> str | None:
    identifier = event.get("id", {})
    if isinstance(identifier, dict):
        node = identifier.get(kind, {})
        if isinstance(node, dict):
            label = _normalize_label(node.get("label"))
            if label:
                return label
    payload = event.get("configured" if kind == "targetConfigured" else "completed", {})
    if isinstance(payload, dict):
        label = _normalize_label(payload.get("label"))
        if label:
            return label
        node = payload.get(kind, {})
        if isinstance(node, dict):
            return _normalize_label(node.get("label"))
    return None


def parse_bep_jsonl(path: str | Path, requested_labels: Iterable[str]) -> BepTargets:
    """Read Bazel 9.2 JSONL BEP and correlate requested target events only."""
    requested = {_normalize_label(label) for label in requested_labels}
    requested.discard(None)
    configured: set[str] = set()
    completed: set[str] = set()
    action_summary: dict[str, Any] | None = None
    try:
        with Path(path).open(encoding="utf-8") as source:
            for number, line in enumerate(source, 1):
                if not line.strip():
                    continue
                try:
                    event = json.loads(line)
                except json.JSONDecodeError as exc:
                    raise ValueError(f"malformed BEP line {number}: {exc.msg}") from exc
                if not isinstance(event, dict):
                    raise ValueError(f"malformed BEP line {number}: object required")
                configured_label = _event_label(event, "targetConfigured")
                completed_label = _event_label(event, "targetCompleted")
                if configured_label in requested:
                    configured.add(configured_label)
                if completed_label in requested:
                    completed.add(completed_label)
                metrics = event.get("buildMetrics", {})
                if isinstance(metrics, dict) and isinstance(metrics.get("actionSummary"), dict):
                    action_summary = metrics["actionSummary"]
    except (OSError, ValueError) as exc:
        return BepTargets((), (), None, None, action_summary, str(exc))

    sorted_configured = tuple(sorted(configured))
    sorted_completed = tuple(sorted(completed))
    if not sorted_configured or not sorted_completed:
        return BepTargets((), (), None, None, action_summary, "missing requested target events")
    if sorted_configured != sorted_completed:
        return BepTargets((), (), None, None, action_summary, "configured/completed requested targets differ")
    return BepTargets(sorted_configured, sorted_completed, len(sorted_configured), len(sorted_completed), action_summary)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=("resolve", "bep"))
    parser.add_argument("input", type=Path)
    parser.add_argument("--format", choices=("json", "tsv"), default="json")
    parser.add_argument("--label", action="append", default=list(CONFIG_LABELS))
    args = parser.parse_args()
    if args.mode == "resolve":
        result: Selection | BepTargets = resolve_name_status_z(args.input.read_bytes())
    else:
        result = parse_bep_jsonl(args.input, args.label)
    print(json.dumps(result.as_dict(), sort_keys=True) if args.format == "json" else result.as_tsv())
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
