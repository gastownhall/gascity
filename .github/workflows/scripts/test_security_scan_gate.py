import datetime as dt
import io
import json
import tempfile
import unittest
from contextlib import redirect_stderr, redirect_stdout
from pathlib import Path

import security_scan_gate as gate

TODAY = dt.date(2026, 11, 5)
LOCK = ".github/requirements/mcp-agent-mail.txt"
OTHER_LOCK = "contrib/other/requirements.txt"

ALLOWLIST = f"""
[[IgnoredVulns]]
id = "CVE-2026-112940"
ignoreUntil = 2027-01-26
source = "{LOCK}"
package = "filelock"
version = "3.29.0"
reason = "filelock ReadWriteLock is unused"

[[IgnoredVulns]]
id = "GHSA-23mf-xhv8-69c2"
ignoreUntil = 2026-11-01
source = "{LOCK}"
package = "gitpython"
version = "3.1.62"
reason = "lapsed waiver"

[[IgnoredVulns]]
id = "GO-2026-6611"
ignoreUntil = 2026-12-07T00:00:00Z
source = "tools/nogo"
package = "golang.org/x/net"
version = "v0.56.0"
reason = "datetime form"
"""

# One filelock 3.29.0 advisory, as osv-scanner groups it: GHSA id first, the
# CVE id the allowlist names among the aliases.
FILELOCK_GROUP = {"ids": ["GHSA-v4x8-3wr5-3wq8"], "aliases": ["CVE-2026-112940", "GHSA-v4x8-3wr5-3wq8"], "max_severity": "7.1"}


def checkout_path(relative: str) -> str:
    # osv-scanner reports absolute paths; the gate runs from the checkout root
    # and shows them relative to it.
    return str(Path.cwd() / relative)


def osv_report(*packages: dict, source: str = LOCK) -> dict:
    return {"results": [{"source": {"path": checkout_path(source), "type": "lockfile"}, "packages": list(packages)}]}


def osv_package(name: str, version: str, groups: list[dict], vulns: list[dict] | None = None, ecosystem: str = "PyPI") -> dict:
    return {
        "package": {"name": name, "version": version, "ecosystem": ecosystem},
        "vulnerabilities": vulns or [],
        "groups": groups,
    }


def sarif_result(rule_id: str, release: str, source: str = LOCK) -> dict:
    # The shape osv-scanner v2.7.0 writes: the rule is the advisory, the
    # message names the release, the location is the lockfile as a file URI.
    return {
        "ruleId": rule_id,
        "level": "warning",
        "message": {"text": f"Package '{release}' is vulnerable to '{rule_id}'."},
        "locations": [{"physicalLocation": {"artifactLocation": {"uri": Path(checkout_path(source)).as_uri()}}}],
    }


def sarif_report(*results: dict) -> dict:
    return {"version": "2.1.0", "runs": [{"tool": {"driver": {"name": "osv-scanner", "rules": []}}, "results": list(results)}]}


def gvc_stream(*messages: dict) -> str:
    # govulncheck writes indented objects back to back, not an array.
    return "\n".join(json.dumps(message, indent=2) for message in messages)


def frame(function: str = "", package: str = "") -> dict:
    return {"module": "golang.org/x/net", "version": "v0.56.0", "package": package, "function": function}


def finding(*ids: str, source: str = LOCK, package: str = "filelock", version: str = "3.29.0", gating: bool = True) -> gate.Finding:
    return gate.Finding(
        scanner=gate.OSV_SCANNER,
        source=source,
        ecosystem="PyPI",
        package=package,
        version=version,
        ids=ids,
        severity="HIGH 7.5" if gating else "MEDIUM 5.0",
        gating=gating,
    )


def waiver(vuln_id: str, until: dt.date = dt.date(2027, 1, 26)) -> gate.Waiver:
    return gate.Waiver(vuln_id=vuln_id, until=until, source=LOCK, package="filelock", version="3.29.0", reason="r")


class AllowlistTests(unittest.TestCase):
    def load(self, text: str) -> list:
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "security-scan-allowlist.toml"
            path.write_text(text, encoding="utf-8")
            return gate.load_allowlist(path)

    def test_reads_dates_datetimes_and_the_release(self) -> None:
        waivers = self.load(ALLOWLIST)
        self.assertEqual([w.vuln_id for w in waivers], ["CVE-2026-112940", "GHSA-23mf-xhv8-69c2", "GO-2026-6611"])
        self.assertEqual(waivers[2].until, dt.date(2026, 12, 7))
        self.assertEqual((waivers[0].source, waivers[0].package, waivers[0].version), (LOCK, "filelock", "3.29.0"))

    def test_empty_file_is_an_empty_allowlist(self) -> None:
        self.assertEqual(self.load(""), [])

    def test_rejects_a_waiver_without_an_expiry(self) -> None:
        with self.assertRaisesRegex(gate.GateInputError, "every waiver expires"):
            self.load('[[IgnoredVulns]]\nid = "X"\nreason = "r"\n')

    def test_rejects_a_waiver_without_a_reason(self) -> None:
        with self.assertRaisesRegex(gate.GateInputError, "reason is required"):
            self.load('[[IgnoredVulns]]\nid = "X"\nignoreUntil = 2027-01-01\n')

    def test_rejects_a_waiver_that_names_no_release(self) -> None:
        entry = {"source": f'"{LOCK}"', "package": '"filelock"', "version": '"3.29.0"'}
        for missing in gate.SCOPE_KEYS:
            with self.subTest(missing=missing):
                scope = "".join(f"{key} = {value}\n" for key, value in entry.items() if key != missing)
                with self.assertRaisesRegex(gate.GateInputError, f"{missing} is required"):
                    self.load(f'[[IgnoredVulns]]\nid = "X"\nignoreUntil = 2027-01-01\nreason = "r"\n{scope}')

    def test_rejects_keys_the_gate_does_not_apply(self) -> None:
        # A misspelled scope key, or an osv-scanner override, would otherwise
        # look like it narrows a waiver while the gate ignores it.
        with self.assertRaisesRegex(gate.GateInputError, r"unknown keys \['ecosystem'\]"):
            self.load(ALLOWLIST.replace('package = "filelock"', 'package = "filelock"\necosystem = "PyPI"'))
        with self.assertRaisesRegex(gate.GateInputError, r"unknown top-level keys \['PackageOverrides'\]"):
            self.load(ALLOWLIST + '\n[[PackageOverrides]]\necosystem = "Go"\n')

    def test_rejects_malformed_toml(self) -> None:
        with self.assertRaises(gate.GateInputError):
            self.load("[[IgnoredVulns]\n")

    def test_rejects_package_overrides(self) -> None:
        # osv-scanner never reads the allowlist, so it would look as if it hid
        # whole packages and hide nothing.
        with self.assertRaisesRegex(gate.GateInputError, "PackageOverrides"):
            self.load('[[PackageOverrides]]\nname = "grpc"\nignore = true\n')

    def test_rejects_any_other_top_level_key(self) -> None:
        with self.assertRaisesRegex(gate.GateInputError, "GoVersionOverride"):
            self.load('GoVersionOverride = "1.26.10"\n' + ALLOWLIST)

    def test_rejects_ignored_vulns_that_is_not_an_array_of_tables(self) -> None:
        for text in ('IgnoredVulns = "CVE-2026-1"\n', 'IgnoredVulns = ["CVE-2026-1"]\n'):
            with self.subTest(text=text), self.assertRaisesRegex(gate.GateInputError, "IgnoredVulns"):
                self.load(text)

    def test_expiry_day_no_longer_accepts(self) -> None:
        # Trivy's expired_at and osv-scanner's ignoreUntil stop at 00:00 on
        # the date, so the gate stops the same day.
        expiring = waiver("X", until=TODAY)
        self.assertTrue(expiring.active(TODAY - dt.timedelta(days=1)))
        self.assertFalse(expiring.active(TODAY))


class OSVTests(unittest.TestCase):
    def test_scores_map_to_labels_and_gate_at_seven(self) -> None:
        findings = gate.osv_findings(
            osv_report(
                osv_package(
                    "gitpython",
                    "3.1.62",
                    [
                        {"ids": ["GHSA-a"], "aliases": ["CVE-1", "GHSA-a"], "max_severity": "9.4"},
                        {"ids": ["GHSA-b"], "max_severity": "7.0"},
                        {"ids": ["GHSA-c"], "max_severity": "6.9"},
                    ],
                )
            )
        )
        self.assertEqual([f.severity for f in findings], ["CRITICAL 9.4", "HIGH 7.0", "MEDIUM 6.9"])
        self.assertEqual([f.gating for f in findings], [True, True, False])
        self.assertEqual(findings[0].ids, ("GHSA-a", "CVE-1"))
        self.assertEqual(findings[0].source, LOCK)

    def test_unscored_group_falls_back_to_the_advisory_label(self) -> None:
        findings = gate.osv_findings(
            osv_report(
                osv_package(
                    "pkg",
                    "1.0.0",
                    [{"ids": ["GHSA-high"], "max_severity": ""}, {"ids": ["GHSA-none"], "max_severity": ""}],
                    [{"id": "GHSA-high", "database_specific": {"severity": "HIGH"}}, {"id": "GHSA-none"}],
                )
            )
        )
        self.assertEqual([(f.severity, f.gating) for f in findings], [("HIGH", True), ("UNSCORED", False)])

    def test_malicious_package_blocks_without_a_score(self) -> None:
        # OpenSSF malicious-packages records (MAL-*) carry no CVSS score and no
        # label; a MAL alias also outranks a low score on a grouped advisory.
        findings = gate.osv_findings(
            osv_report(
                osv_package(
                    "flatmap-stream",
                    "0.1.1",
                    [
                        {"ids": ["MAL-2025-20690"], "aliases": ["MAL-2025-20690"], "max_severity": ""},
                        {"ids": ["GHSA-low"], "aliases": ["GHSA-low", "MAL-2025-1"], "max_severity": "3.1"},
                    ],
                    [{"id": "MAL-2025-20690"}, {"id": "GHSA-low", "database_specific": {"severity": "LOW"}}],
                )
            )
        )
        self.assertEqual([(f.severity, f.gating) for f in findings], [(gate.MALICIOUS, True), (gate.MALICIOUS, True)])
        verdict = gate.decide(findings, [], TODAY)
        self.assertEqual(len(verdict.blocking), 2)

    def test_go_rows_are_listed_but_never_gate(self) -> None:
        # Required is not called: a scored Go module row says nothing about
        # reachability, which govulncheck judges.
        findings = gate.osv_findings(
            osv_report(
                osv_package("golang.org/x/crypto", "0.58.0", [{"ids": ["GO-2026-5932"], "aliases": ["CVE-9"], "max_severity": "9.8"}], ecosystem="Go"),
                source="tools/nogo/go.mod",
            )
        )
        self.assertEqual([(f.severity, f.gating, f.left_to_govulncheck) for f in findings], [("CRITICAL 9.8", False, True)])
        verdict = gate.decide(findings, [], TODAY)
        self.assertEqual((verdict.blocking, verdict.informational), ([], findings))
        self.assertIn(gate.GO_ROW_NOTE, gate.render(verdict))

    def test_malicious_go_module_still_blocks(self) -> None:
        # govulncheck reads only the Go vulnerability database and never sees a
        # MAL-* record, so a known-malicious module stays the gate's to block.
        findings = gate.osv_findings(
            osv_report(
                osv_package("example.com/typosquat", "1.3.1", [{"ids": ["MAL-2026-1"], "aliases": ["MAL-2026-1"], "max_severity": ""}], ecosystem="Go"),
                source="go.mod",
            )
        )
        self.assertEqual([(f.severity, f.gating, f.left_to_govulncheck) for f in findings], [(gate.MALICIOUS, True, False)])
        verdict = gate.decide(findings, [], TODAY)
        self.assertEqual(verdict.blocking, findings)
        row = sarif_result("MAL-2026-1", "example.com/typosquat@1.3.1", source="go.mod")
        self.assertEqual(gate.code_scanning_sarif(sarif_report(row), verdict)["runs"][0]["results"], [row])

    def test_empty_report_has_no_findings(self) -> None:
        self.assertEqual(gate.osv_findings({"results": []}), [])
        self.assertEqual(gate.osv_findings({}), [])

    def test_non_numeric_score_is_an_input_error(self) -> None:
        with self.assertRaises(gate.GateInputError):
            gate.osv_findings(osv_report(osv_package("p", "1", [{"ids": ["X"], "max_severity": "high"}])))


class GovulncheckTests(unittest.TestCase):
    def test_collapses_levels_and_gates_only_on_called_symbols(self) -> None:
        messages = gate.parse_json_stream(
            gvc_stream(
                {"config": {"scan_level": "symbol"}},
                {"osv": {"id": "GO-2026-6611", "aliases": ["CVE-2026-78660"]}},
                {"osv": {"id": "GO-2026-6860", "aliases": []}},
                {"osv": {"id": "GO-2026-6629"}},
                {"finding": {"osv": "GO-2026-6611", "trace": [frame()]}},
                {"finding": {"osv": "GO-2026-6611", "trace": [frame(package="golang.org/x/net/http2")]}},
                {"finding": {"osv": "GO-2026-6611", "trace": [frame("Error", "golang.org/x/net/http2"), frame("main", "example.com/m")]}},
                {"finding": {"osv": "GO-2026-6860", "trace": [frame()]}},
                {"finding": {"osv": "GO-2026-6629", "trace": [frame(package="golang.org/x/text/secure/precis")]}},
            )
        )
        findings = {f.primary_id: f for f in gate.govulncheck_findings(messages, "deploy/tools/pss-check")}
        self.assertEqual(set(findings), {"GO-2026-6611", "GO-2026-6860", "GO-2026-6629"})
        self.assertEqual(findings["GO-2026-6611"].ids, ("GO-2026-6611", "CVE-2026-78660"))
        self.assertEqual((findings["GO-2026-6611"].severity, findings["GO-2026-6611"].gating), (gate.REACHABLE, True))
        self.assertEqual((findings["GO-2026-6860"].severity, findings["GO-2026-6860"].gating), ("required", False))
        self.assertEqual((findings["GO-2026-6629"].severity, findings["GO-2026-6629"].gating), ("imported", False))
        self.assertFalse(findings["GO-2026-6611"].left_to_govulncheck)

    def test_clean_module_has_no_findings(self) -> None:
        self.assertEqual(gate.govulncheck_findings(gate.parse_json_stream(gvc_stream({"config": {}})), "."), [])
        self.assertEqual(gate.parse_json_stream(""), [])

    def test_truncated_stream_is_an_input_error(self) -> None:
        with self.assertRaises(gate.GateInputError):
            gate.parse_json_stream('{"config": {}}\n{"finding": ')


class DecideTests(unittest.TestCase):
    def test_waiver_accepts_its_release_via_an_alias(self) -> None:
        verdict = gate.decide([finding("GHSA-v4x8-3wr5-3wq8", "CVE-2026-112940")], [waiver("CVE-2026-112940")], TODAY)
        self.assertEqual(verdict.blocking, [])
        self.assertEqual(len(verdict.accepted), 1)

    def test_waiver_accepts_nothing_outside_its_release(self) -> None:
        elsewhere = {
            "another lock": finding("CVE-2026-112940", source=OTHER_LOCK),
            "another package": finding("CVE-2026-112940", package="filelock-ng"),
            "another version": finding("CVE-2026-112940", version="3.29.1"),
        }
        for name, outside in elsewhere.items():
            with self.subTest(name):
                verdict = gate.decide([outside], [waiver("CVE-2026-112940")], TODAY)
                self.assertEqual((verdict.blocking, verdict.accepted, verdict.expired), ([outside], [], []))

    def test_expired_waiver_blocks_and_says_so(self) -> None:
        verdict = gate.decide([finding("GHSA-23mf-xhv8-69c2")], [waiver("GHSA-23mf-xhv8-69c2", until=dt.date(2026, 11, 1))], TODAY)
        self.assertEqual(len(verdict.blocking), 1)
        self.assertEqual(len(verdict.expired), 1)
        self.assertIn("expired 2026-11-01", gate.render(verdict))

    def test_waiver_stops_accepting_on_its_expiry_date(self) -> None:
        verdict = gate.decide([finding("CVE-2026-112940")], [waiver("CVE-2026-112940", until=TODAY)], TODAY)
        self.assertEqual((len(verdict.blocking), len(verdict.expired), verdict.accepted), (1, 1, []))

    def test_expired_note_stays_on_the_waived_release(self) -> None:
        waived, elsewhere = finding("CVE-2026-112940"), finding("CVE-2026-112940", source=OTHER_LOCK)
        report = gate.render(gate.decide([waived, elsewhere], [waiver("CVE-2026-112940", until=TODAY)], TODAY))
        self.assertIn(f"`{LOCK}` | filelock@3.29.0 | CVE-2026-112940 | HIGH 7.5 | waiver CVE-2026-112940 expired {TODAY} |", report)
        self.assertIn(f"`{OTHER_LOCK}` | filelock@3.29.0 | CVE-2026-112940 | HIGH 7.5 | blocking |", report)

    def test_unwaived_high_blocks_and_medium_does_not(self) -> None:
        verdict = gate.decide([finding("GHSA-new"), finding("GHSA-med", gating=False)], [], TODAY)
        self.assertEqual([f.primary_id for f in verdict.blocking], ["GHSA-new"])
        self.assertEqual([f.primary_id for f in verdict.informational], ["GHSA-med"])


class CodeScanningSarifTests(unittest.TestCase):
    def test_drops_only_accepted_and_go_rows(self) -> None:
        npm_lock = "web/package-lock.json"
        reports = [
            osv_report(osv_package("filelock", "3.29.0", [FILELOCK_GROUP])),
            osv_report(osv_package("filelock", "3.29.0", [FILELOCK_GROUP]), source=OTHER_LOCK),
            osv_report(osv_package("golang.org/x/crypto", "0.58.0", [{"ids": ["GO-2026-5932"], "aliases": ["GO-2026-5932"]}], ecosystem="Go"), source="go.mod"),
            osv_report(osv_package("minimatch", "10.2.5", [{"ids": ["GHSA-g3hv-75wg-m249"], "max_severity": "6.9"}], ecosystem="npm"), source=npm_lock),
        ]
        findings = gate.osv_findings({"results": [result for report in reports for result in report["results"]]})
        verdict = gate.decide(findings, [waiver("CVE-2026-112940")], TODAY)
        accepted = sarif_result("CVE-2026-112940", "filelock@3.29.0")
        elsewhere = sarif_result("CVE-2026-112940", "filelock@3.29.0", source=OTHER_LOCK)
        go_row = sarif_result("GO-2026-5932", "golang.org/x/crypto@0.58.0", source="go.mod")
        npm_row = sarif_result("GHSA-g3hv-75wg-m249", "minimatch@10.2.5", source=npm_lock)
        # A commit-identified package has no name@version to match on.
        unnamed = {**npm_row, "message": {"text": "Package '1234567890abcdef' is vulnerable to 'GHSA-g3hv-75wg-m249'."}}
        sarif = sarif_report(accepted, elsewhere, go_row, npm_row, unnamed)

        out = gate.code_scanning_sarif(sarif, verdict)
        self.assertEqual(out["runs"][0]["results"], [elsewhere, npm_row, unnamed])
        self.assertEqual(out["runs"][0]["tool"], sarif["runs"][0]["tool"])
        self.assertEqual(len(sarif["runs"][0]["results"]), 5, "the input report is left as it was")


class MainTests(unittest.TestCase):
    def run_main(self, tmp: Path, *args: str) -> tuple[int, str, str]:
        out, err = io.StringIO(), io.StringIO()
        with redirect_stdout(out), redirect_stderr(err):
            code = gate.main(["--allowlist", str(tmp / "security-scan-allowlist.toml"), "--today", TODAY.isoformat(), *args])
        return code, out.getvalue(), err.getvalue()

    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.tmp = Path(self._tmp.name)
        (self.tmp / "security-scan-allowlist.toml").write_text(ALLOWLIST, encoding="utf-8")

    def tearDown(self) -> None:
        self._tmp.cleanup()

    def test_accepted_high_passes_and_writes_the_summary(self) -> None:
        report = osv_report(osv_package("filelock", "3.29.0", [FILELOCK_GROUP]))
        (self.tmp / "osv.json").write_text(json.dumps(report), encoding="utf-8")
        summary = self.tmp / "summary.md"
        code, out, _ = self.run_main(self.tmp, "--osv", str(self.tmp / "osv.json"), "--summary", str(summary))
        self.assertEqual(code, 0)
        self.assertIn("Accepted by the allowlist (1)", out)
        self.assertIn("CVE-2026-112940, expires 2027-01-26", out)
        self.assertEqual(summary.read_text(encoding="utf-8"), out)

    def test_another_release_of_a_waived_package_fails(self) -> None:
        report = osv_report(osv_package("filelock", "3.29.1", [FILELOCK_GROUP]))
        (self.tmp / "osv.json").write_text(json.dumps(report), encoding="utf-8")
        code, out, _ = self.run_main(self.tmp, "--osv", str(self.tmp / "osv.json"))
        self.assertEqual(code, 1)
        self.assertIn("filelock@3.29.1", out)

    def test_writes_the_code_scanning_sarif(self) -> None:
        report = osv_report(osv_package("filelock", "3.29.0", [FILELOCK_GROUP, {"ids": ["GHSA-low"], "max_severity": "2.5"}]))
        accepted, low = sarif_result("CVE-2026-112940", "filelock@3.29.0"), sarif_result("GHSA-low", "filelock@3.29.0")
        (self.tmp / "osv.json").write_text(json.dumps(report), encoding="utf-8")
        (self.tmp / "osv.sarif").write_text(json.dumps(sarif_report(accepted, low)), encoding="utf-8")
        code, _, _ = self.run_main(
            self.tmp, "--osv", str(self.tmp / "osv.json"), "--osv-sarif", str(self.tmp / "osv.sarif"), "--sarif-out", str(self.tmp / "out.sarif")
        )
        self.assertEqual(code, 0)
        self.assertEqual(json.loads((self.tmp / "out.sarif").read_text(encoding="utf-8"))["runs"][0]["results"], [low])

    def test_sarif_flags_go_together(self) -> None:
        with self.assertRaises(SystemExit) as raised:
            self.run_main(self.tmp, "--osv", str(self.tmp / "osv.json"), "--osv-sarif", str(self.tmp / "osv.sarif"))
        self.assertEqual(raised.exception.code, 2)

    def test_reachable_unwaived_go_finding_fails(self) -> None:
        stream = gvc_stream({"osv": {"id": "GO-2026-7000"}}, {"finding": {"osv": "GO-2026-7000", "trace": [frame("Read", "golang.org/x/net/html")]}})
        (self.tmp / "gvc.json").write_text(stream, encoding="utf-8")
        code, out, _ = self.run_main(self.tmp, "--govulncheck", f"tools/nogo={self.tmp / 'gvc.json'}")
        self.assertEqual(code, 1)
        self.assertIn("GO-2026-7000", out)

    def test_missing_report_is_exit_two(self) -> None:
        code, _, err = self.run_main(self.tmp, "--osv", str(self.tmp / "absent.json"))
        self.assertEqual(code, 2)
        self.assertIn("absent.json", err)

    def test_malformed_allowlist_is_exit_two(self) -> None:
        (self.tmp / "osv.json").write_text(json.dumps(osv_report()), encoding="utf-8")
        for text in ("[[IgnoredVulns]\n", "[[PackageOverrides]]\nname = \"grpc\"\nignore = true\n", 'IgnoredVulns = "CVE-2026-1"\n'):
            with self.subTest(text=text):
                (self.tmp / "security-scan-allowlist.toml").write_text(text, encoding="utf-8")
                code, out, err = self.run_main(self.tmp, "--osv", str(self.tmp / "osv.json"))
                self.assertEqual(code, 2)
                self.assertEqual(out, "")
                self.assertIn("security-scan-allowlist.toml", err)

    def test_malformed_module_spec_is_exit_two(self) -> None:
        code, _, err = self.run_main(self.tmp, "--govulncheck", "no-equals-sign")
        self.assertEqual(code, 2)
        self.assertIn("MODULE=PATH", err)


if __name__ == "__main__":
    unittest.main()
