#!/usr/bin/env python3
"""Gate security-scan.yml on HIGH/CRITICAL findings nobody has accepted.

The weekly dependency scan runs two scanners, and this script turns their
reports into one verdict:

* osv-scanner (every lockfile and go.mod). A finding blocks when its CVSS
  score is 7.0 or higher, or, when it carries no score, when its advisory
  labels itself HIGH or CRITICAL. A known-malicious package (an OSV MAL-*
  record from the OpenSSF malicious-packages feed) always blocks: those
  records carry no score and no label, so the CVSS rule alone would pass them.
* govulncheck (each Go module). The Go vulnerability database carries no
  severity at all, so the gate uses reachability instead: a finding blocks
  when the module calls the vulnerable symbol. Imported-only and
  required-only findings are reported but do not block. That is the same
  rule govulncheck.yml applies through govulncheck's own exit code.

Either kind of finding is accepted when one of its ids or aliases has an
unexpired [[IgnoredVulns]] entry in the allowlist (osv-scanner.toml, which
osv-scanner also reads natively). An expired entry accepts nothing, so a
waiver whose date passes turns the scheduled run red instead of silently
staying accepted.

The allowlist may hold nothing but [[IgnoredVulns]] entries: osv-scanner
would apply any other setting (such as [[PackageOverrides]]) on its own and
hide findings with no expiry, so the gate refuses it.

Exit status: 0 when nothing blocks, 1 when something does, 2 when a report
or the allowlist cannot be read or is malformed.
"""

from __future__ import annotations

import argparse
import datetime as dt
import json
import os
import sys
import tomllib
from dataclasses import dataclass
from pathlib import Path

HIGH_CVSS = 7.0
CRITICAL_CVSS = 9.0
MEDIUM_CVSS = 4.0
MALICIOUS = "MALICIOUS"
BLOCKING_LABELS = frozenset({"HIGH", "CRITICAL", MALICIOUS})
REACHABLE = "REACHABLE"
ALLOWLIST_KEY = "IgnoredVulns"


class GateInputError(Exception):
    """A report or the allowlist is missing or malformed."""


@dataclass(frozen=True)
class Waiver:
    """One accepted risk from the allowlist."""

    vuln_id: str
    until: dt.date
    reason: str

    def active(self, today: dt.date) -> bool:
        """Report whether the waiver still accepts its finding on today."""
        return today <= self.until


@dataclass(frozen=True)
class Finding:
    """One advisory against one package, from either scanner."""

    scanner: str
    source: str
    package: str
    version: str
    ids: tuple[str, ...]
    severity: str
    gating: bool

    @property
    def primary_id(self) -> str:
        """The id the scanner reported the finding under."""
        return self.ids[0]


def load_allowlist(path: Path) -> list[Waiver]:
    """Read the [[IgnoredVulns]] entries, each of which must be time-boxed and explained."""
    try:
        data = tomllib.loads(path.read_text(encoding="utf-8"))
    except (OSError, tomllib.TOMLDecodeError) as err:
        raise GateInputError(f"reading allowlist {path}: {err}") from err
    # osv-scanner applies every key in this file itself, so any other setting (PackageOverrides
    # in particular) would hide findings from the gate with no expiry and no reason.
    unexpected = sorted(set(data) - {ALLOWLIST_KEY})
    if unexpected:
        raise GateInputError(
            f"{path}: only [[{ALLOWLIST_KEY}]] waivers are allowed, found {', '.join(unexpected)}; "
            "other osv-scanner settings hide findings without an expiry"
        )
    entries = data.get(ALLOWLIST_KEY, [])
    if not isinstance(entries, list) or not all(isinstance(entry, dict) for entry in entries):
        raise GateInputError(f"{path}: {ALLOWLIST_KEY} must be an array of tables ([[{ALLOWLIST_KEY}]])")
    waivers = []
    for index, entry in enumerate(entries):
        where = f"{path} {ALLOWLIST_KEY}[{index}]"
        vuln_id = entry.get("id")
        until = entry.get("ignoreUntil")
        reason = entry.get("reason")
        if not isinstance(vuln_id, str) or not vuln_id.strip():
            raise GateInputError(f"{where}: id is required")
        if isinstance(until, dt.datetime):
            until = until.date()
        if not isinstance(until, dt.date):
            raise GateInputError(f"{where} ({vuln_id}): ignoreUntil must be a date; every waiver expires")
        if not isinstance(reason, str) or not reason.strip():
            raise GateInputError(f"{where} ({vuln_id}): reason is required")
        waivers.append(Waiver(vuln_id.strip(), until, reason.strip()))
    return waivers


def label_for_score(score: float) -> str:
    """Map a CVSS base score onto the qualitative rating scale."""
    if score >= CRITICAL_CVSS:
        return "CRITICAL"
    if score >= HIGH_CVSS:
        return "HIGH"
    if score >= MEDIUM_CVSS:
        return "MEDIUM"
    return "LOW"


def _group_severity(group: dict, vulns_by_id: dict[str, dict]) -> str:
    if any(str(vuln_id).startswith("MAL-") for vuln_id in (*group.get("ids", []), *group.get("aliases", []))):
        return MALICIOUS
    raw = str(group.get("max_severity") or "").strip()
    if raw:
        try:
            score = float(raw)
        except ValueError as err:
            raise GateInputError(f"osv-scanner max_severity {raw!r} is not a number") from err
        return f"{label_for_score(score)} {raw}"
    labels = {
        str((vulns_by_id.get(vuln_id, {}).get("database_specific") or {}).get("severity") or "").upper()
        for vuln_id in group.get("ids", [])
    }
    for label in ("CRITICAL", "HIGH", "MODERATE", "MEDIUM", "LOW"):
        if label in labels:
            return label
    return "UNSCORED"


def _display_path(path: str) -> str:
    """Show a scanned path relative to the checkout when it lies inside it."""
    relative = os.path.relpath(path) if os.path.isabs(path) else path
    return path if relative.startswith("..") else relative


def osv_findings(report: dict) -> list[Finding]:
    """Flatten an osv-scanner JSON report into one finding per advisory group."""
    findings = []
    for result in report.get("results") or []:
        source = _display_path((result.get("source") or {}).get("path", "?"))
        for package in result.get("packages") or []:
            info = package.get("package") or {}
            vulns_by_id = {vuln.get("id"): vuln for vuln in package.get("vulnerabilities") or []}
            for group in package.get("groups") or []:
                ids = tuple(dict.fromkeys([*group.get("ids", []), *group.get("aliases", [])]))
                if not ids:
                    raise GateInputError(f"osv-scanner group without ids in {source}")
                severity = _group_severity(group, vulns_by_id)
                findings.append(
                    Finding(
                        scanner="osv-scanner",
                        source=source,
                        package=info.get("name", "?"),
                        version=info.get("version", "?"),
                        ids=ids,
                        severity=severity,
                        gating=severity.split(" ")[0] in BLOCKING_LABELS,
                    )
                )
    return findings


def parse_json_stream(text: str) -> list[dict]:
    """Decode govulncheck -format json output: concatenated JSON objects, not an array."""
    decoder = json.JSONDecoder()
    objects = []
    index = 0
    while True:
        while index < len(text) and text[index].isspace():
            index += 1
        if index >= len(text):
            return objects
        try:
            obj, index = decoder.raw_decode(text, index)
        except json.JSONDecodeError as err:
            raise GateInputError(f"govulncheck output is not a JSON stream: {err}") from err
        objects.append(obj)


def govulncheck_findings(messages: list[dict], source: str) -> list[Finding]:
    """Collapse govulncheck's per-level findings into one finding per advisory."""
    aliases = {}
    traces: dict[str, list[dict]] = {}
    for message in messages:
        if "osv" in message:
            entry = message["osv"]
            aliases[entry["id"]] = tuple(entry.get("aliases") or ())
        elif "finding" in message:
            finding = message["finding"]
            frames = finding.get("trace") or []
            if not frames:
                raise GateInputError(f"govulncheck finding {finding.get('osv')} has no trace")
            traces.setdefault(finding["osv"], []).append(frames[0])
    findings = []
    for osv_id, frames in traces.items():
        called = any(frame.get("function") for frame in frames)
        imported = any(frame.get("package") for frame in frames)
        frame = frames[0]
        findings.append(
            Finding(
                scanner="govulncheck",
                source=source,
                package=frame.get("module", "?"),
                version=frame.get("version", "?"),
                ids=(osv_id, *aliases.get(osv_id, ())),
                severity=REACHABLE if called else ("imported" if imported else "required"),
                gating=called,
            )
        )
    return findings


def waiver_for(finding: Finding, waivers: list[Waiver]) -> Waiver | None:
    """Return the allowlist entry naming any of the finding's ids, if one exists."""
    ids = set(finding.ids)
    for waiver in waivers:
        if waiver.vuln_id in ids:
            return waiver
    return None


@dataclass(frozen=True)
class Verdict:
    """The gate's decision and the rows that explain it."""

    blocking: list[Finding]
    accepted: list[tuple[Finding, Waiver]]
    expired: list[tuple[Finding, Waiver]]
    informational: list[Finding]


def decide(findings: list[Finding], waivers: list[Waiver], today: dt.date) -> Verdict:
    """Split findings into blocking, accepted, expired-waiver and informational rows."""
    verdict = Verdict([], [], [], [])
    for finding in findings:
        waiver = waiver_for(finding, waivers)
        if waiver is not None and waiver.active(today):
            verdict.accepted.append((finding, waiver))
        elif not finding.gating:
            verdict.informational.append(finding)
        elif waiver is not None:
            verdict.expired.append((finding, waiver))
            verdict.blocking.append(finding)
        else:
            verdict.blocking.append(finding)
    return verdict


def _row(finding: Finding, note: str = "") -> str:
    ids = ", ".join(finding.ids)
    return f"| {finding.scanner} | `{finding.source}` | {finding.package}@{finding.version} | {ids} | {finding.severity} | {note} |"


def render(verdict: Verdict) -> str:
    """Render the verdict as Markdown for the job summary and the log."""
    header = ["| Scanner | Source | Package | Advisory | Severity | Note |", "|---|---|---|---|---|---|"]
    lines = ["## Dependency security scan", ""]
    if verdict.blocking:
        lines += [f"**{len(verdict.blocking)} unaccepted HIGH/CRITICAL or reachable finding(s).** Fix them, or add a time-boxed waiver to the allowlist in a reviewed change.", ""]
        expired_ids = {finding.primary_id: waiver for finding, waiver in verdict.expired}
        lines += header
        for finding in verdict.blocking:
            waiver = expired_ids.get(finding.primary_id)
            lines.append(_row(finding, f"waiver {waiver.vuln_id} expired {waiver.until}" if waiver else "blocking"))
    else:
        lines.append("No unaccepted HIGH/CRITICAL or reachable finding.")
    if verdict.accepted:
        lines += ["", f"### Accepted by the allowlist ({len(verdict.accepted)})", "", *header]
        lines += [_row(finding, f"{waiver.vuln_id} until {waiver.until}") for finding, waiver in verdict.accepted]
    if verdict.informational:
        lines += ["", f"### Below the gate ({len(verdict.informational)})", "", *header]
        lines += [_row(finding) for finding in verdict.informational]
    return "\n".join(lines) + "\n"


def _read_json(path: Path) -> dict:
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as err:
        raise GateInputError(f"reading osv-scanner report {path}: {err}") from err


def _read_text(path: Path) -> str:
    try:
        return path.read_text(encoding="utf-8")
    except OSError as err:
        raise GateInputError(f"reading govulncheck report {path}: {err}") from err


def main(argv: list[str] | None = None) -> int:
    """Run the gate over the given reports and print the Markdown verdict."""
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--allowlist", type=Path, required=True, help="osv-scanner.toml with [[IgnoredVulns]] waivers")
    parser.add_argument("--osv", type=Path, action="append", default=[], help="osv-scanner --format json report")
    parser.add_argument(
        "--govulncheck",
        action="append",
        default=[],
        metavar="MODULE=PATH",
        help="govulncheck -format json report for the Go module at MODULE",
    )
    parser.add_argument("--today", type=dt.date.fromisoformat, default=dt.datetime.now(dt.timezone.utc).date())
    parser.add_argument("--summary", type=Path, help="also append the Markdown verdict here (GITHUB_STEP_SUMMARY)")
    args = parser.parse_args(argv)
    if not args.osv and not args.govulncheck:
        parser.error("give at least one --osv or --govulncheck report")

    try:
        waivers = load_allowlist(args.allowlist)
        findings = []
        for path in args.osv:
            findings += osv_findings(_read_json(path))
        for spec in args.govulncheck:
            module, sep, path = spec.partition("=")
            if not sep or not module or not path:
                raise GateInputError(f"--govulncheck wants MODULE=PATH, got {spec!r}")
            findings += govulncheck_findings(parse_json_stream(_read_text(Path(path))), module)
    except GateInputError as err:
        print(f"security-scan gate: {err}", file=sys.stderr)
        return 2

    verdict = decide(findings, waivers, args.today)
    report = render(verdict)
    sys.stdout.write(report)
    if args.summary is not None:
        with args.summary.open("a", encoding="utf-8") as handle:
            handle.write(report)
    return 1 if verdict.blocking else 0


if __name__ == "__main__":
    sys.exit(main())
