#!/usr/bin/env python3
"""Gate security-scan.yml on HIGH/CRITICAL findings nobody has accepted.

The weekly dependency scan runs two scanners, and this script turns their
reports into one verdict:

* osv-scanner (every lockfile and go.mod). A finding blocks when its CVSS
  score is 7.0 or higher, or, when it carries no score, when its advisory
  labels itself HIGH or CRITICAL. A known-malicious package (an OSV MAL-*
  record from the OpenSSF malicious-packages feed) always blocks: those
  records carry no score and no label, so the CVSS rule alone would pass them.
  Go module rows block only then. Otherwise osv-scanner matches them by
  module version alone, and whether the vulnerable code is reached is
  govulncheck's verdict, below; they are listed with the findings below the
  gate.
* govulncheck (each Go module). The Go vulnerability database carries no
  severity at all, so the gate uses reachability instead: a finding blocks
  when the module calls the vulnerable symbol. Imported-only and
  required-only findings are reported but do not block. That is the same
  rule govulncheck.yml applies through govulncheck's own exit code.

A finding is accepted when an unexpired entry in the allowlist
(.github/security-scan-allowlist.toml) names one of its ids or aliases and
the exact source, package and version it was found in: the lockfile, or the
Go module directory for govulncheck. The same advisory at another release, or
in another source, still blocks. An entry stops accepting on its ignoreUntil
date, the day Trivy stops honoring the .trivyignore.yaml waiver it mirrors, so
a waiver whose date passes turns the scheduled run red instead of silently
staying accepted.

The allowlist may hold nothing but [[IgnoredVulns]] entries. osv-scanner
never reads it, so any other setting there (such as osv-scanner's
[[PackageOverrides]]) would change nothing; the gate refuses it rather than
ignore it.

Given osv-scanner's SARIF report of the same scan, the gate also writes the
SARIF code scanning gets: that report without the results of the findings it
accepted and of the Go module rows, which govulncheck reports.

Exit status: 0 when nothing blocks, 1 when something does, 2 when a report
or the allowlist cannot be read or is malformed.
"""

from __future__ import annotations

import argparse
import datetime as dt
import json
import os
import re
import sys
import tomllib
from dataclasses import dataclass
from pathlib import Path
from urllib.parse import unquote, urlparse

HIGH_CVSS = 7.0
CRITICAL_CVSS = 9.0
MEDIUM_CVSS = 4.0
MALICIOUS = "MALICIOUS"
BLOCKING_LABELS = frozenset({"HIGH", "CRITICAL", MALICIOUS})
REACHABLE = "REACHABLE"
ALLOWLIST_KEY = "IgnoredVulns"
OSV_SCANNER = "osv-scanner"
GOVULNCHECK = "govulncheck"
GO_ECOSYSTEM = "Go"
GO_ROW_NOTE = "Go module: govulncheck decides"
# The release a waiver accepts its advisory in; every entry names all three.
SCOPE_KEYS = ("source", "package", "version")
WAIVER_KEYS = frozenset({"id", "ignoreUntil", "reason", *SCOPE_KEYS})
# osv-scanner v2 names the release in each SARIF result's message:
# "Package 'name@version' is vulnerable to 'ID' (also known as ...)."
SARIF_RELEASE = re.compile(r"^Package '(?P<package>.+)@(?P<version>[^@']+)' is vulnerable to '")


class GateInputError(Exception):
    """A report or the allowlist is missing or malformed."""


def _govulncheck_judges(ecosystem: str, severity: str) -> bool:
    """Report whether govulncheck, rather than an osv-scanner row, decides a finding.

    Go module rows are govulncheck's, which checks reachability. A
    known-malicious module is the exception: it blocks whether or not its code
    is reached, and govulncheck, which reads only the Go vulnerability
    database, never sees a MAL-* record.
    """
    return ecosystem == GO_ECOSYSTEM and severity != MALICIOUS


@dataclass(frozen=True)
class Finding:
    """One advisory against one package, from either scanner."""

    scanner: str
    source: str
    ecosystem: str
    package: str
    version: str
    ids: tuple[str, ...]
    severity: str
    gating: bool

    @property
    def primary_id(self) -> str:
        """The id the scanner reported the finding under."""
        return self.ids[0]

    @property
    def left_to_govulncheck(self) -> bool:
        """Report whether this is an osv-scanner Go module row, which govulncheck judges."""
        return self.scanner == OSV_SCANNER and _govulncheck_judges(self.ecosystem, self.severity)


@dataclass(frozen=True)
class Waiver:
    """One accepted risk from the allowlist: one advisory in one package release."""

    vuln_id: str
    until: dt.date
    source: str
    package: str
    version: str
    reason: str

    def active(self, today: dt.date) -> bool:
        """Report whether the waiver still accepts its finding on today.

        The ignoreUntil date is the first day it no longer does, as it is for
        Trivy's expired_at and osv-scanner's own ignoreUntil.
        """
        return today < self.until

    def covers(self, finding: Finding) -> bool:
        """Report whether the waiver names the finding's advisory, source and release."""
        return (
            self.vuln_id in finding.ids
            and self.source == finding.source
            and self.package == finding.package
            and self.version == finding.version
        )


def load_allowlist(path: Path) -> list[Waiver]:
    """Read the [[IgnoredVulns]] entries, each time-boxed, explained and scoped to one release."""
    try:
        data = tomllib.loads(path.read_text(encoding="utf-8"))
    except (OSError, tomllib.TOMLDecodeError) as err:
        raise GateInputError(f"reading allowlist {path}: {err}") from err
    if unknown := sorted(set(data) - {ALLOWLIST_KEY}):
        raise GateInputError(f"{path}: unknown top-level keys {unknown}; the gate reads only [[{ALLOWLIST_KEY}]]")
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
        if unknown := sorted(set(entry) - WAIVER_KEYS):
            raise GateInputError(f"{where} ({vuln_id}): unknown keys {unknown}")
        if isinstance(until, dt.datetime):
            until = until.date()
        if not isinstance(until, dt.date):
            raise GateInputError(f"{where} ({vuln_id}): ignoreUntil must be a date; every waiver expires")
        if not isinstance(reason, str) or not reason.strip():
            raise GateInputError(f"{where} ({vuln_id}): reason is required")
        scope = {key: entry.get(key) for key in SCOPE_KEYS}
        for key, value in scope.items():
            if not isinstance(value, str) or not value.strip():
                raise GateInputError(f"{where} ({vuln_id}): {key} is required; a waiver accepts one release in one source")
        waivers.append(
            Waiver(
                vuln_id=vuln_id.strip(),
                until=until,
                source=scope["source"].strip(),
                package=scope["package"].strip(),
                version=scope["version"].strip(),
                reason=reason.strip(),
            )
        )
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
            ecosystem = info.get("ecosystem", "?")
            vulns_by_id = {vuln.get("id"): vuln for vuln in package.get("vulnerabilities") or []}
            for group in package.get("groups") or []:
                ids = tuple(dict.fromkeys([*group.get("ids", []), *group.get("aliases", [])]))
                if not ids:
                    raise GateInputError(f"osv-scanner group without ids in {source}")
                severity = _group_severity(group, vulns_by_id)
                findings.append(
                    Finding(
                        scanner=OSV_SCANNER,
                        source=source,
                        ecosystem=ecosystem,
                        package=info.get("name", "?"),
                        version=info.get("version", "?"),
                        ids=ids,
                        severity=severity,
                        gating=not _govulncheck_judges(ecosystem, severity) and severity.split(" ")[0] in BLOCKING_LABELS,
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
                scanner=GOVULNCHECK,
                source=source,
                ecosystem=GO_ECOSYSTEM,
                package=frame.get("module", "?"),
                version=frame.get("version", "?"),
                ids=(osv_id, *aliases.get(osv_id, ())),
                severity=REACHABLE if called else ("imported" if imported else "required"),
                gating=called,
            )
        )
    return findings


def waiver_for(finding: Finding, waivers: list[Waiver]) -> Waiver | None:
    """Return the allowlist entry covering the finding's advisory, source and release, if one exists."""
    return next((waiver for waiver in waivers if waiver.covers(finding)), None)


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


def _sarif_result_key(result: dict) -> tuple[str, str, str, str] | None:
    """Key an osv-scanner SARIF result by lockfile, package, version and rule id, when it names them."""
    release = SARIF_RELEASE.match(str((result.get("message") or {}).get("text") or ""))
    locations = result.get("locations") or [{}]
    uri = ((locations[0].get("physicalLocation") or {}).get("artifactLocation") or {}).get("uri")
    if release is None or not isinstance(uri, str):
        return None
    path = unquote(urlparse(uri).path) if uri.startswith("file:") else uri
    return (_display_path(path), release["package"], release["version"], str(result.get("ruleId")))


def code_scanning_sarif(sarif: dict, verdict: Verdict) -> dict:
    """Return osv-scanner's SARIF report as code scanning should get it.

    The results of the findings the gate accepted, and of the Go module rows,
    are dropped: a result goes only when its rule id, lockfile, package and
    version match one of them. Every other result stays, including one whose
    message names no package release, so code scanning never loses a finding
    this script cannot identify.
    """
    omitted = [finding for finding, _ in verdict.accepted]
    omitted += [finding for finding in verdict.informational if finding.left_to_govulncheck]
    keys = {(f.source, f.package, f.version, vuln_id) for f in omitted for vuln_id in f.ids}
    runs = [
        {**run, "results": [result for result in run.get("results") or [] if _sarif_result_key(result) not in keys]}
        for run in sarif.get("runs") or []
    ]
    return {**sarif, "runs": runs}


def _row(finding: Finding, note: str = "") -> str:
    ids = ", ".join(finding.ids)
    return f"| {finding.scanner} | `{finding.source}` | {finding.package}@{finding.version} | {ids} | {finding.severity} | {note} |"


def render(verdict: Verdict) -> str:
    """Render the verdict as Markdown for the job summary and the log."""
    header = ["| Scanner | Source | Package | Advisory | Severity | Note |", "|---|---|---|---|---|---|"]
    lines = ["## Dependency security scan", ""]
    if verdict.blocking:
        lines += [f"**{len(verdict.blocking)} unaccepted HIGH/CRITICAL or reachable finding(s).** Fix them, or add a time-boxed waiver to the allowlist in a reviewed change.", ""]
        expired = dict(verdict.expired)
        lines += header
        for finding in verdict.blocking:
            waiver = expired.get(finding)
            lines.append(_row(finding, f"waiver {waiver.vuln_id} expired {waiver.until}" if waiver else "blocking"))
    else:
        lines.append("No unaccepted HIGH/CRITICAL or reachable finding.")
    if verdict.accepted:
        lines += ["", f"### Accepted by the allowlist ({len(verdict.accepted)})", "", *header]
        lines += [_row(finding, f"{waiver.vuln_id}, expires {waiver.until}") for finding, waiver in verdict.accepted]
    if verdict.informational:
        lines += ["", f"### Below the gate ({len(verdict.informational)})", "", *header]
        lines += [_row(finding, GO_ROW_NOTE if finding.left_to_govulncheck else "") for finding in verdict.informational]
    return "\n".join(lines) + "\n"


def _read_json(path: Path, what: str) -> dict:
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as err:
        raise GateInputError(f"reading {what} {path}: {err}") from err


def _read_text(path: Path) -> str:
    try:
        return path.read_text(encoding="utf-8")
    except OSError as err:
        raise GateInputError(f"reading govulncheck report {path}: {err}") from err


def main(argv: list[str] | None = None) -> int:
    """Run the gate over the given reports and print the Markdown verdict."""
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--allowlist", type=Path, required=True, help="security-scan-allowlist.toml with [[IgnoredVulns]] waivers")
    parser.add_argument("--osv", type=Path, action="append", default=[], help="osv-scanner --format json report")
    parser.add_argument(
        "--govulncheck",
        action="append",
        default=[],
        metavar="MODULE=PATH",
        help="govulncheck -format json report for the Go module at MODULE",
    )
    parser.add_argument("--osv-sarif", type=Path, help="osv-scanner --format sarif report of the same scan as --osv")
    parser.add_argument("--sarif-out", type=Path, help="write the --osv-sarif report for code scanning here")
    parser.add_argument("--today", type=dt.date.fromisoformat, default=dt.datetime.now(dt.timezone.utc).date())
    parser.add_argument("--summary", type=Path, help="also append the Markdown verdict here (GITHUB_STEP_SUMMARY)")
    args = parser.parse_args(argv)
    if not args.osv and not args.govulncheck:
        parser.error("give at least one --osv or --govulncheck report")
    if (args.osv_sarif is None) != (args.sarif_out is None):
        parser.error("--osv-sarif and --sarif-out go together")
    if args.osv_sarif is not None and not args.osv:
        parser.error("--osv-sarif needs the --osv report of the same scan")

    try:
        waivers = load_allowlist(args.allowlist)
        findings = []
        for path in args.osv:
            findings += osv_findings(_read_json(path, "osv-scanner report"))
        for spec in args.govulncheck:
            module, sep, path = spec.partition("=")
            if not sep or not module or not path:
                raise GateInputError(f"--govulncheck wants MODULE=PATH, got {spec!r}")
            findings += govulncheck_findings(parse_json_stream(_read_text(Path(path))), module)
        sarif = None if args.osv_sarif is None else _read_json(args.osv_sarif, "osv-scanner SARIF report")
    except GateInputError as err:
        print(f"security-scan gate: {err}", file=sys.stderr)
        return 2

    verdict = decide(findings, waivers, args.today)
    if sarif is not None:
        args.sarif_out.write_text(json.dumps(code_scanning_sarif(sarif, verdict), indent=2) + "\n", encoding="utf-8")
    report = render(verdict)
    sys.stdout.write(report)
    if args.summary is not None:
        with args.summary.open("a", encoding="utf-8") as handle:
            handle.write(report)
    return 1 if verdict.blocking else 0


if __name__ == "__main__":
    sys.exit(main())
