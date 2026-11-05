import datetime as dt
import io
import json
import tempfile
import unittest
from contextlib import redirect_stderr, redirect_stdout
from pathlib import Path

import security_scan_gate as gate

TODAY = dt.date(2026, 11, 5)

ALLOWLIST = """
[[IgnoredVulns]]
id = "CVE-2026-112940"
ignoreUntil = 2027-01-26
reason = "filelock ReadWriteLock is unused"

[[IgnoredVulns]]
id = "GHSA-23mf-xhv8-69c2"
ignoreUntil = 2026-11-01
reason = "lapsed waiver"

[[IgnoredVulns]]
id = "GO-2026-6611"
ignoreUntil = 2026-12-07T00:00:00Z
reason = "datetime form"
"""


def osv_report(*packages: dict) -> dict:
    return {"results": [{"source": {"path": "/repo/.github/requirements/x.txt", "type": "lockfile"}, "packages": list(packages)}]}


def osv_package(name: str, version: str, groups: list[dict], vulns: list[dict] | None = None) -> dict:
    return {
        "package": {"name": name, "version": version, "ecosystem": "PyPI"},
        "vulnerabilities": vulns or [],
        "groups": groups,
    }


def gvc_stream(*messages: dict) -> str:
    # govulncheck writes indented objects back to back, not an array.
    return "\n".join(json.dumps(message, indent=2) for message in messages)


def frame(function: str = "", package: str = "") -> dict:
    return {"module": "golang.org/x/net", "version": "v0.56.0", "package": package, "function": function}


class AllowlistTests(unittest.TestCase):
    def load(self, text: str) -> list:
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "osv-scanner.toml"
            path.write_text(text, encoding="utf-8")
            return gate.load_allowlist(path)

    def test_reads_dates_and_datetimes(self) -> None:
        waivers = self.load(ALLOWLIST)
        self.assertEqual([w.vuln_id for w in waivers], ["CVE-2026-112940", "GHSA-23mf-xhv8-69c2", "GO-2026-6611"])
        self.assertEqual(waivers[2].until, dt.date(2026, 12, 7))

    def test_empty_file_is_an_empty_allowlist(self) -> None:
        self.assertEqual(self.load(""), [])

    def test_rejects_a_waiver_without_an_expiry(self) -> None:
        with self.assertRaisesRegex(gate.GateInputError, "every waiver expires"):
            self.load('[[IgnoredVulns]]\nid = "X"\nreason = "r"\n')

    def test_rejects_a_waiver_without_a_reason(self) -> None:
        with self.assertRaisesRegex(gate.GateInputError, "reason is required"):
            self.load('[[IgnoredVulns]]\nid = "X"\nignoreUntil = 2027-01-01\n')

    def test_rejects_malformed_toml(self) -> None:
        with self.assertRaises(gate.GateInputError):
            self.load("[[IgnoredVulns]\n")

    def test_rejects_package_overrides(self) -> None:
        # osv-scanner would apply it natively and hide whole packages with no expiry or reason.
        with self.assertRaisesRegex(gate.GateInputError, "PackageOverrides"):
            self.load('[[PackageOverrides]]\nname = "grpc"\nignore = true\n')

    def test_rejects_any_other_top_level_key(self) -> None:
        with self.assertRaisesRegex(gate.GateInputError, "GoVersionOverride"):
            self.load('GoVersionOverride = "1.26.10"\n' + ALLOWLIST)

    def test_rejects_ignored_vulns_that_is_not_an_array_of_tables(self) -> None:
        for text in ('IgnoredVulns = "CVE-2026-1"\n', 'IgnoredVulns = ["CVE-2026-1"]\n'):
            with self.subTest(text=text), self.assertRaisesRegex(gate.GateInputError, "IgnoredVulns"):
                self.load(text)

    def test_expiry_day_is_still_active(self) -> None:
        waiver = gate.Waiver("X", TODAY, "r")
        self.assertTrue(waiver.active(TODAY))
        self.assertFalse(waiver.active(TODAY + dt.timedelta(days=1)))


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

    def test_unscored_group_falls_back_to_the_advisory_label(self) -> None:
        findings = gate.osv_findings(
            osv_report(
                osv_package(
                    "pkg",
                    "1.0.0",
                    [{"ids": ["GHSA-high"], "max_severity": ""}, {"ids": ["GO-2026-5932"], "max_severity": ""}],
                    [{"id": "GHSA-high", "database_specific": {"severity": "HIGH"}}, {"id": "GO-2026-5932"}],
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

    def test_clean_module_has_no_findings(self) -> None:
        self.assertEqual(gate.govulncheck_findings(gate.parse_json_stream(gvc_stream({"config": {}})), "."), [])
        self.assertEqual(gate.parse_json_stream(""), [])

    def test_truncated_stream_is_an_input_error(self) -> None:
        with self.assertRaises(gate.GateInputError):
            gate.parse_json_stream('{"config": {}}\n{"finding": ')


class DecideTests(unittest.TestCase):
    def finding(self, *ids: str, gating: bool = True) -> gate.Finding:
        return gate.Finding("osv-scanner", "x", "p", "1", ids, "HIGH 7.5" if gating else "MEDIUM 5.0", gating)

    def test_active_waiver_accepts_via_an_alias(self) -> None:
        waivers = [gate.Waiver("CVE-2026-112940", dt.date(2027, 1, 26), "r")]
        verdict = gate.decide([self.finding("GHSA-v4x8-3wr5-3wq8", "CVE-2026-112940")], waivers, TODAY)
        self.assertEqual(verdict.blocking, [])
        self.assertEqual(len(verdict.accepted), 1)

    def test_expired_waiver_blocks_and_says_so(self) -> None:
        waivers = [gate.Waiver("GHSA-23mf-xhv8-69c2", dt.date(2026, 11, 1), "r")]
        verdict = gate.decide([self.finding("GHSA-23mf-xhv8-69c2")], waivers, TODAY)
        self.assertEqual(len(verdict.blocking), 1)
        self.assertEqual(len(verdict.expired), 1)
        self.assertIn("expired 2026-11-01", gate.render(verdict))

    def test_unwaived_high_blocks_and_medium_does_not(self) -> None:
        verdict = gate.decide([self.finding("GHSA-new"), self.finding("GHSA-med", gating=False)], [], TODAY)
        self.assertEqual([f.primary_id for f in verdict.blocking], ["GHSA-new"])
        self.assertEqual([f.primary_id for f in verdict.informational], ["GHSA-med"])


class MainTests(unittest.TestCase):
    def run_main(self, tmp: Path, *args: str) -> tuple[int, str, str]:
        out, err = io.StringIO(), io.StringIO()
        with redirect_stdout(out), redirect_stderr(err):
            code = gate.main(["--allowlist", str(tmp / "osv-scanner.toml"), "--today", TODAY.isoformat(), *args])
        return code, out.getvalue(), err.getvalue()

    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.tmp = Path(self._tmp.name)
        (self.tmp / "osv-scanner.toml").write_text(ALLOWLIST, encoding="utf-8")

    def tearDown(self) -> None:
        self._tmp.cleanup()

    def test_accepted_high_passes_and_writes_the_summary(self) -> None:
        report = osv_report(osv_package("filelock", "3.29.0", [{"ids": ["GHSA-v4x8-3wr5-3wq8"], "aliases": ["CVE-2026-112940"], "max_severity": "7.1"}]))
        (self.tmp / "osv.json").write_text(json.dumps(report), encoding="utf-8")
        summary = self.tmp / "summary.md"
        code, out, _ = self.run_main(self.tmp, "--osv", str(self.tmp / "osv.json"), "--summary", str(summary))
        self.assertEqual(code, 0)
        self.assertIn("Accepted by the allowlist (1)", out)
        self.assertEqual(summary.read_text(encoding="utf-8"), out)

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
                (self.tmp / "osv-scanner.toml").write_text(text, encoding="utf-8")
                code, out, err = self.run_main(self.tmp, "--osv", str(self.tmp / "osv.json"))
                self.assertEqual(code, 2)
                self.assertEqual(out, "")
                self.assertIn("osv-scanner.toml", err)

    def test_malformed_module_spec_is_exit_two(self) -> None:
        code, _, err = self.run_main(self.tmp, "--govulncheck", "no-equals-sign")
        self.assertEqual(code, 2)
        self.assertIn("MODULE=PATH", err)


if __name__ == "__main__":
    unittest.main()
