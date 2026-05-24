import json
import sys
import tempfile
import unittest
from pathlib import Path


SCRIPT_DIR = Path(__file__).resolve().parent
if str(SCRIPT_DIR) not in sys.path:
    sys.path.insert(0, str(SCRIPT_DIR))

import project_dashboard


class ProjectDashboardTests(unittest.TestCase):
    def snapshot(self, date: str, stars: int, ci_rate: float | None) -> dict:
        return {
            "schema_version": project_dashboard.SCHEMA_VERSION,
            "snapshot_date": date,
            "generated_at": f"{date}T06:00:00Z",
            "repository": "gastownhall/gascity",
            "latest_release": {"tag": "v1.2.3", "age_days": 5, "downloads": 10},
            "workflow_latest": [],
            "top_referrers": [],
            "top_paths": [],
            "metrics": {
                "github_stars": {"value": stars, "unit": "count"},
                "traffic_views_14d_uniques": {"value": stars + 10, "unit": "visitors"},
                "traffic_clones_14d_uniques": {"value": stars + 2, "unit": "cloners"},
                "release_downloads_total": {"value": stars * 2, "unit": "downloads"},
                "ci_success_rate_7d": {"value": ci_rate, "unit": "percent"},
                "ttfr_median_hours_30d": {"value": 24, "unit": "hours"},
                "community_profile_score": {"value": 87, "unit": "percent"},
            },
        }

    def test_upsert_replaces_same_snapshot_date(self) -> None:
        old = self.snapshot("2026-05-17", 100, 90)
        replacement = self.snapshot("2026-05-17", 125, 95)
        new = self.snapshot("2026-05-24", 130, 92)

        snapshots = project_dashboard.upsert_snapshot([old, new], replacement)

        self.assertEqual([item["snapshot_date"] for item in snapshots], ["2026-05-17", "2026-05-24"])
        self.assertEqual(project_dashboard.metric_value(snapshots[0], "github_stars"), 125)

    def test_snapshot_file_round_trips_jsonl_sorted(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "snapshots.jsonl"
            snapshots = [
                self.snapshot("2026-05-24", 130, 92),
                self.snapshot("2026-05-17", 125, 95),
            ]

            project_dashboard.write_snapshots(path, snapshots)
            loaded = project_dashboard.load_snapshots(path)

            self.assertEqual([item["snapshot_date"] for item in loaded], ["2026-05-17", "2026-05-24"])
            for line in path.read_text(encoding="utf-8").splitlines():
                self.assertIsInstance(json.loads(line), dict)

    def test_render_dashboard_includes_trends_and_status(self) -> None:
        snapshots = [
            self.snapshot("2026-05-17", 100, 80),
            self.snapshot("2026-05-24", 130, 92),
        ]

        rendered = project_dashboard.render_dashboard(snapshots)

        self.assertIn("# Project Dashboard", rendered)
        self.assertIn("Executive Signals", rendered)
        self.assertIn("CI success rate, 7d", rendered)
        self.assertIn("Healthy", rendered)
        self.assertIn("+12", rendered)

    def test_status_thresholds_handle_missing_values(self) -> None:
        self.assertEqual(project_dashboard.status_for("ci_success_rate_7d", None), "No data")
        self.assertEqual(project_dashboard.status_for("ci_success_rate_7d", 70), "Needs attention")
        self.assertEqual(project_dashboard.status_for("ttfr_median_hours_30d", 24), "Healthy")


if __name__ == "__main__":
    unittest.main()
