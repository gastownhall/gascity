"""Two-stage abstention split: prompts, votes, scoring, live runner, CLI.

No test touches the network or runs a judge CLI: recorded checkpoints and
in-memory replay clients are used throughout.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
import unittest

try:
    from . import support
except ImportError:  # pragma: no cover
    import support

from agent_observatory.collapse import PRIMARY_INTENT_COLLAPSE_V1, load_judge_checkpoint
from agent_observatory.errors import SilverError
from agent_observatory.silver import JudgeSpec
from agent_observatory.taxonomy import load_taxonomy
from agent_observatory.two_stage import (
    EVIDENCE_GATE_PROMPT_VERSION,
    EVIDENCE_KNOWN,
    EVIDENCE_UNKNOWN,
    INTENT_PROMPT_VERSION,
    TWO_STAGE_PROMPT_VERSION,
    TwoStageVote,
    approximate_votes_from_silver_checkpoint,
    build_evidence_gate_prompt,
    build_intent_prompt,
    build_two_stage_result,
    eligible_episode_ids,
    load_episodes_json,
    parse_evidence_answer,
    parse_intent_answer,
    score_two_stage,
    substantive_labels,
    votes_from_checkpoints,
    write_two_stage_report,
)

HERE = os.path.dirname(os.path.abspath(__file__))
FIXTURES = os.path.join(HERE, "fixtures")
COLLAPSE_CHECKPOINT = os.path.join(FIXTURES, "collapse", "recorded_judge_checkpoint.json")
COLLAPSE_JEV = os.path.join(FIXTURES, "collapse", "jev_predictions.json")
PACKAGE_ROOT = support.PACKAGE_ROOT
TAXONOMY = os.path.join(PACKAGE_ROOT, "agent_observatory", "taxonomy", "jev_taxonomy_v2.json")


def _taxonomy():
    return load_taxonomy(TAXONOMY)


def _known_unknown_checkpoint():
    """A tiny checkpoint with all four judges on eight episodes."""

    judges = ("j1", "j2", "j3", "j4")
    labels = [
        # episode, j1, j2, j3, j4
        ("e1", "bugfix", "bugfix", "bugfix", "bugfix"),
        ("e2", "pr_review", "pr_review", "pr_review", "pr_review"),
        ("e3", "dependency_worktree_agent_ops", "dependency_worktree_agent_ops", "unknown", "dependency_worktree_agent_ops"),
        ("e4", "research_docs", "research_docs", "research_docs", "research_docs"),
        ("e5", "unknown", "unknown", "unknown", "unknown"),
        ("e6", "implementation", "implementation", "implementation", "implementation"),
        ("e7", "planning_spec", "planning_spec", "test_lint_build_ci", "planning_spec"),
        ("e8", "unknown", "unknown", "unknown", "unknown"),
    ]
    checkpoint = {judge: {} for judge in judges}
    for row in labels:
        episode_id = row[0]
        for index, judge in enumerate(judges):
            checkpoint[judge][episode_id] = json.dumps(
                {"primary_intent": row[index + 1], "confidence": 0.9}
            )
    return checkpoint


class PromptTests(unittest.TestCase):
    def test_evidence_prompt_names_only_known_and_unknown(self):
        prompt = build_evidence_gate_prompt("Please fix the flaky test")
        self.assertIn(EVIDENCE_GATE_PROMPT_VERSION, prompt)
        self.assertIn('"evidence"', prompt)
        self.assertIn(EVIDENCE_KNOWN, prompt)
        self.assertIn(EVIDENCE_UNKNOWN, prompt)
        self.assertNotIn("dependency_worktree_agent_ops", prompt)
        self.assertIn("Please fix the flaky test", prompt)

    def test_intent_prompt_lists_every_substantive_label_and_no_abstention(self):
        taxonomy = _taxonomy()
        prompt = build_intent_prompt("Please fix the flaky test", taxonomy)
        self.assertIn(INTENT_PROMPT_VERSION, prompt)
        for label in substantive_labels(taxonomy):
            self.assertIn(f"- {label}:", prompt)
        self.assertIn("no unknown option", prompt.lower())
        self.assertNotIn("- unknown:", prompt)

    def test_prompts_bound_the_text(self):
        taxonomy = _taxonomy()
        self.assertIn("[truncated:", build_evidence_gate_prompt("x" * 5000, max_bytes=64))
        self.assertIn("[truncated:", build_intent_prompt("x" * 5000, taxonomy, max_bytes=64))


class ParseTests(unittest.TestCase):
    def test_parse_evidence_accepts_known_and_unknown(self):
        self.assertEqual(parse_evidence_answer('{"evidence": "known", "confidence": 0.5}')[0], "known")
        self.assertEqual(
            parse_evidence_answer('```json\n{"evidence": "unknown"}\n```')[0], "unknown"
        )

    def test_parse_evidence_rejects_other_labels_and_confidence(self):
        with self.assertRaises(SilverError):
            parse_evidence_answer('{"evidence": "dependency_worktree_agent_ops"}')
        with self.assertRaises(SilverError):
            parse_evidence_answer('{"evidence": "known", "confidence": 2}')
        with self.assertRaises(SilverError):
            parse_evidence_answer("not json")

    def test_parse_intent_rejects_unknown(self):
        allowed = substantive_labels(_taxonomy())
        label, confidence = parse_intent_answer('{"primary_intent": "bugfix", "confidence": 0.4}', allowed)
        self.assertEqual(label, "bugfix")
        self.assertEqual(confidence, 0.4)
        with self.assertRaises(SilverError):
            parse_intent_answer('{"primary_intent": "unknown"}', allowed)


class VoteTests(unittest.TestCase):
    def test_approximation_derives_both_stages_and_eligibility(self):
        votes, ids = approximate_votes_from_silver_checkpoint(
            _known_unknown_checkpoint(), majority=3
        )
        self.assertEqual(ids, ("j1", "j2", "j3", "j4"))
        by_id = {vote.episode_id: vote for vote in votes}
        self.assertEqual(by_id["e1"].evidence["j1"], EVIDENCE_KNOWN)
        self.assertEqual(by_id["e5"].evidence["j1"], EVIDENCE_UNKNOWN)
        # e3 has one unknown vote but three known: eligible, and the abstainer
        # has no stage-2 label.
        self.assertIn("e3", eligible_episode_ids(votes, ids, majority=3))
        self.assertIsNone(by_id["e3"].intent["j3"])
        self.assertEqual(by_id["e3"].intent["j1"], "dependency_worktree_agent_ops")
        # e8 has no known votes: not eligible and no stage-2 labels.
        self.assertNotIn("e8", eligible_episode_ids(votes, ids, majority=3))
        self.assertTrue(all(label is None for label in by_id["e8"].intent.values()))

    def test_votes_from_checkpoints_reads_both_stages(self):
        evidence = {
            "j1": {"e1": '{"evidence": "known"}', "e2": '{"evidence": "unknown"}'},
            "j2": {"e1": '{"evidence": "known"}', "e2": '{"evidence": "unknown"}'},
        }
        intent = {
            "j1": {"e1": '{"primary_intent": "bugfix"}'},
            "j2": {"e1": '{"primary_intent": "bugfix"}'},
        }
        votes, ids = votes_from_checkpoints(evidence, intent, intent_labels={"bugfix", "unknown"})
        self.assertEqual(ids, ("j1", "j2"))
        self.assertEqual(votes[0].intent["j1"], "bugfix")
        self.assertIsNone(votes[1].intent["j1"])

    def test_votes_from_checkpoints_treats_unknown_intent_as_missing(self):
        evidence = {
            "j1": {"e1": '{"evidence": "known"}'},
            "j2": {"e1": '{"evidence": "known"}'},
        }
        intent = {
            "j1": {"e1": '{"primary_intent": "unknown"}'},
            "j2": {"e1": '{"primary_intent": "bugfix"}'},
        }
        votes, _ = votes_from_checkpoints(evidence, intent, intent_labels={"bugfix", "unknown"})
        self.assertIsNone(votes[0].intent["j1"])
        self.assertEqual(votes[0].intent["j2"], "bugfix")


def _vote(episode_id, evidence, intent):
    return TwoStageVote(episode_id, evidence, intent)


class ScoringTests(unittest.TestCase):
    def _perfect_votes(self, episodes=6):
        judges = ("j1", "j2", "j3")
        votes = []
        for index in range(episodes):
            episode_id = f"e{index}"
            if index == episodes - 1:
                evidence = {judge: EVIDENCE_UNKNOWN for judge in judges}
                intent = {judge: None for judge in judges}
            else:
                evidence = {judge: EVIDENCE_KNOWN for judge in judges}
                intent = {judge: "bugfix" for judge in judges}
            votes.append(_vote(episode_id, evidence, intent))
        return tuple(votes), judges

    def test_perfect_two_stage_clears_the_gate(self):
        votes, judges = self._perfect_votes()
        report = score_two_stage(votes, judges, {})
        self.assertTrue(report["gate"]["stage1_clears"])
        self.assertTrue(report["gate"]["stage2_clears_original"])
        self.assertTrue(report["gate"]["clears_floor"])
        self.assertEqual(report["stage1"]["trust"]["min_pairwise_cohen_kappa"], 1.0)
        self.assertEqual(report["stage2"]["eligible_episodes"], 5)

    def test_stage1_disagreement_blocks_the_gate(self):
        judges = ("j1", "j2", "j3")
        votes = []
        for index in range(6):
            # Half the panel flips known/unknown on every other episode.
            label = EVIDENCE_KNOWN if index % 2 == 0 else EVIDENCE_UNKNOWN
            evidence = {
                "j1": label,
                "j2": label,
                "j3": EVIDENCE_UNKNOWN if label == EVIDENCE_KNOWN else EVIDENCE_KNOWN,
            }
            votes.append(_vote(f"e{index}", evidence, {judge: "bugfix" for judge in judges}))
        report = score_two_stage(tuple(votes), judges, {})
        self.assertFalse(report["gate"]["stage1_clears"])
        self.assertFalse(report["gate"]["clears_floor"])
        self.assertEqual(report["stage1"]["trust"]["trust_reason"], "kappa_below_floor")

    def test_stage2_coverage_floor_blocks_the_gate(self):
        judges = ("j1", "j2", "j3")
        votes = []
        for index in range(6):
            evidence = {judge: EVIDENCE_KNOWN for judge in judges}
            # j1 only has a stage-2 label on one episode: overlap fails the floor.
            intent = {
                "j1": "bugfix" if index == 0 else None,
                "j2": "bugfix",
                "j3": "bugfix",
            }
            votes.append(_vote(f"e{index}", evidence, intent))
        report = score_two_stage(tuple(votes), judges, {})
        self.assertFalse(report["stage2"]["scorings"]["original"]["trust"]["coverage_ok"])
        self.assertEqual(
            report["stage2"]["scorings"]["original"]["trust"]["trust_reason"],
            "missing_labels_over_floor",
        )
        self.assertFalse(report["gate"]["clears_floor"])

    def test_small_sample_is_untrusted(self):
        judges = ("j1", "j2")
        votes = tuple(
            _vote(f"e{index}", {j: EVIDENCE_KNOWN for j in judges}, {j: "bugfix" for j in judges})
            for index in range(2)
        )
        report = score_two_stage(votes, judges, {}, min_sample_size=4)
        self.assertEqual(report["stage1"]["trust"]["trust_reason"], "sample_too_small")
        self.assertFalse(report["gate"]["clears_floor"])

    def test_approximation_report_is_flagged_and_reports_collapse(self):
        votes, judges = approximate_votes_from_silver_checkpoint(
            _known_unknown_checkpoint(), majority=3
        )
        report = score_two_stage(votes, judges, {}, approximation=True)
        self.assertTrue(report["approximation"])
        self.assertIn(PRIMARY_INTENT_COLLAPSE_V1.collapse_id, report["stage2"]["scorings"])
        for name, scoring in report["stage2"]["scorings"].items():
            self.assertIn("trust", scoring)
            self.assertIn("jev", scoring)

    def test_score_rejects_bad_inputs(self):
        with self.assertRaises(SilverError):
            score_two_stage([], ("j1", "j2"), {})
        with self.assertRaises(SilverError):
            score_two_stage(
                [_vote("e1", {EVIDENCE_KNOWN: "known"}, {})], ("only-one",), {}
            )


class _ReplayClient:
    """A judge client that returns a canned answer per (episode, prompt kind)."""

    def __init__(self, judge_id, *, evidence=None, intent=None):
        self.judge_id = judge_id
        self.evidence = evidence or {}
        self.intent = intent or {}
        self.evidence_calls = []
        self.intent_calls = []

    def label(self, prompt, *, episode_id):
        if "Evidence-gate prompt version" in prompt:
            self.evidence_calls.append(episode_id)
            answer = self.evidence.get(episode_id)
        else:
            self.intent_calls.append(episode_id)
            answer = self.intent.get(episode_id)
        if answer is None:
            raise RuntimeError(f"no replay answer for {episode_id}")
        return json.dumps({"evidence": answer, "primary_intent": answer, "confidence": 0.9})


class _Episode:
    def __init__(self, episode_id, text="do the thing"):
        self.episode_id = episode_id
        self.text = text


class LiveRunnerTests(unittest.TestCase):
    def _judges(self):
        j1 = _ReplayClient(
            "j1",
            evidence={"e1": "known", "e2": "known", "e3": "unknown"},
            intent={"e1": "bugfix", "e2": "bugfix"},
        )
        j2 = _ReplayClient(
            "j2",
            evidence={"e1": "known", "e2": "known", "e3": "unknown"},
            intent={"e1": "bugfix", "e2": "bugfix"},
        )
        return [
            (JudgeSpec("j1", "j1", "j1", backend="gateway"), j1),
            (JudgeSpec("j2", "j2", "j2", backend="gateway"), j2),
        ]

    def test_run_calls_stage2_only_on_eligible_episodes(self):
        episodes = [_Episode("e1"), _Episode("e2"), _Episode("e3")]
        judges = self._judges()
        result = build_two_stage_result(episodes, _taxonomy(), judges, majority=2)
        # Stage 1 asks all three; stage 2 only e1/e2.
        self.assertEqual(sorted(judges[0][1].evidence_calls), ["e1", "e2", "e3"])
        self.assertEqual(sorted(judges[0][1].intent_calls), ["e1", "e2"])
        self.assertEqual(result.report["judge_calls"]["stage1"]["judge_calls"], {"j1": 3, "j2": 3})
        self.assertEqual(result.report["judge_calls"]["stage2"]["judge_calls"], {"j1": 2, "j2": 2})
        self.assertEqual(result.report["sample"]["episodes"], 3)
        self.assertEqual(result.report["stage2"]["eligible_episodes"], 2)
        self.assertIn("pair", result.report["prompt_versions"])
        # 3 episodes is below the minimum sample: no pass.
        self.assertFalse(result.report["gate"]["clears_floor"])

    def test_run_records_missing_labels_when_tolerant(self):
        episodes = [_Episode("e1"), _Episode("e2"), _Episode("e3")]
        judges = self._judges()
        for _spec, client in judges:
            client.evidence.pop("e2")
        result = build_two_stage_result(
            episodes, _taxonomy(), judges, majority=2, strict=False
        )
        self.assertEqual(result.report["judge_calls"]["stage1"]["transport_errors"], {"j1": 1, "j2": 1})
        self.assertIsNone(result.votes[1].evidence["j1"])

    def test_run_requires_matching_judge_sets(self):
        episodes = [_Episode("e1"), _Episode("e2")]
        stage1 = self._judges()
        stage2 = [stage1[0]]
        with self.assertRaises(SilverError):
            build_two_stage_result(episodes, _taxonomy(), stage1, stage2)

    def test_load_episodes_json_round_trips_a_document(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "episodes.json")
            with open(path, "w", encoding="utf-8") as handle:
                json.dump([{"episode_id": "e1", "text": "hello"}], handle)
            episodes = load_episodes_json(path)
        self.assertEqual(len(episodes), 1)
        self.assertEqual(episodes[0].episode_id, "e1")
        self.assertEqual(episodes[0].provider, "unknown")


class ReportWriterTests(unittest.TestCase):
    def test_write_two_stage_report_is_stable_json(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "report.json")
            write_two_stage_report({"b": 1, "a": {"z": 2}}, path)
            with open(path, encoding="utf-8") as handle:
                text = handle.read()
        self.assertTrue(text.endswith("\n"))
        self.assertLess(text.index('"a"'), text.index('"b"'))

    def test_write_two_stage_report_rejects_non_finite_numbers(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "report.json")
            with self.assertRaises(ValueError):
                write_two_stage_report({"value": float("nan")}, path)


def run_cli(args, cwd=PACKAGE_ROOT):
    env = dict(os.environ)
    env["PYTHONPATH"] = PACKAGE_ROOT + os.pathsep + env.get("PYTHONPATH", "")
    env["PYTHONDONTWRITEBYTECODE"] = "1"
    return subprocess.run(
        [sys.executable, "-m", "agent_observatory", *args],
        cwd=cwd,
        env=env,
        capture_output=True,
        text=True,
    )


class TwoStageCliTests(unittest.TestCase):
    def test_cli_approximates_from_a_single_pass_checkpoint(self):
        with tempfile.TemporaryDirectory() as tmp:
            out = os.path.join(tmp, "two_stage.json")
            result = run_cli([
                "silver-two-stage",
                "--checkpoint", COLLAPSE_CHECKPOINT,
                "--jev-predictions", COLLAPSE_JEV,
                "--out", out,
            ])
            self.assertEqual(result.returncode, 0, result.stderr)
            with open(out, encoding="utf-8") as handle:
                report = json.load(handle)
        self.assertEqual(report["kind"], "silver_two_stage_rescore")
        self.assertTrue(report["approximation"])
        self.assertFalse(report["gate"]["clears_floor"])
        self.assertEqual(report["source"]["kind"], "single_pass_checkpoint")

    def test_cli_require_clear_fails_but_still_writes(self):
        with tempfile.TemporaryDirectory() as tmp:
            out = os.path.join(tmp, "two_stage.json")
            result = run_cli([
                "silver-two-stage",
                "--checkpoint", COLLAPSE_CHECKPOINT,
                "--jev-predictions", COLLAPSE_JEV,
                "--out", out,
                "--require-clear",
            ])
            self.assertNotEqual(result.returncode, 0)
            self.assertTrue(os.path.exists(out))

    def test_cli_scores_recorded_two_stage_checkpoints(self):
        with tempfile.TemporaryDirectory() as tmp:
            stage1 = os.path.join(tmp, "stage1.json")
            stage2 = os.path.join(tmp, "stage2.json")
            judges = ("j1", "j2")
            evidence = {judge: {} for judge in judges}
            intent = {judge: {} for judge in judges}
            for index in range(5):
                episode_id = f"e{index}"
                for judge in judges:
                    evidence[judge][episode_id] = '{"evidence": "known"}'
                    intent[judge][episode_id] = '{"primary_intent": "bugfix"}'
            with open(stage1, "w", encoding="utf-8") as handle:
                json.dump(evidence, handle)
            with open(stage2, "w", encoding="utf-8") as handle:
                json.dump(intent, handle)
            jev = os.path.join(tmp, "jev.json")
            with open(jev, "w", encoding="utf-8") as handle:
                json.dump(
                    {
                        "model": "jev-1.13.0",
                        "predictor": "jev",
                        "predictions": [
                            {"episode_id": f"e{index}", "primary_intent": "bugfix"}
                            for index in range(5)
                        ],
                    },
                    handle,
                )
            out = os.path.join(tmp, "two_stage.json")
            result = run_cli([
                "silver-two-stage",
                "--stage1-checkpoint", stage1,
                "--stage2-checkpoint", stage2,
                "--jev-predictions", jev,
                "--majority", "2",
                "--out", out,
            ])
            self.assertEqual(result.returncode, 0, result.stderr)
            with open(out, encoding="utf-8") as handle:
                report = json.load(handle)
        self.assertFalse(report["approximation"])
        self.assertEqual(report["source"]["kind"], "two_stage_checkpoints")
        self.assertTrue(report["gate"]["clears_floor"])
        self.assertEqual(report["stage2"]["scorings"]["original"]["jev"]["majority"]["accuracy"], 1.0)


if __name__ == "__main__":
    unittest.main()
