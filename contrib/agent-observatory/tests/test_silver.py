"""Silver reference builder: sampling, judging, adjudication, kappa, evaluation."""

from __future__ import annotations

import json
import os
import tempfile
import unittest

try:
    from . import support
except ImportError:  # pragma: no cover
    import support

from agent_observatory.annotations import load_gold_set
from agent_observatory.errors import AnnotationError, SilverError
from agent_observatory.evaluation import Prediction, load_predictions
from agent_observatory.silver import (
    JUDGE_DEEPSEEK,
    JUDGE_GEMINI38_FLASH,
    JUDGE_GLM,
    JUDGE_GPT6_LUNA,
    JUDGE_PROMPT_VERSION,
    HTTP_USER_AGENT,
    AgyCLIJudgeClient,
    CodexCLIJudgeClient,
    HTTPJudgeClient,
    JudgeSpec,
    agreement_reference,
    build_judge_client,
    build_judge_prompt,
    build_judge_schema,
    build_silver_episodes,
    build_silver_predictions_document,
    build_silver_result,
    cohen_kappa,
    evaluate_jev_references,
    evaluate_silver_vs_jev,
    fleiss_kappa,
    judge_label_distribution,
    kappa_over_judges,
    load_candidates_csv,
    load_judge_config,
    overlap_meets_floor,
    pairwise_cohen_kappa,
    parse_judge_answer,
    predictions_from_store,
    render_transcript_text,
    resolve_judge,
    resolve_judges,
    sample_stratified,
    session_key_from_group_key,
)
from agent_observatory.store import ObservatoryStore
from agent_observatory.taxonomy import load_taxonomy

HERE = os.path.dirname(os.path.abspath(__file__))
PACKAGE_ROOT = os.path.dirname(HERE)
V2_PATH = os.path.join(PACKAGE_ROOT, "agent_observatory", "taxonomy", "jev_taxonomy_v2.json")
CANDIDATES = os.path.join(HERE, "fixtures", "silver", "candidates.csv")
ANSWERS = os.path.join(HERE, "fixtures", "silver", "recorded_judge_answers.json")
MULTI_ANSWERS = os.path.join(HERE, "fixtures", "silver", "recorded_multi_judge_answers.json")


class RecordedJudgeClient:
    """Replay recorded judge answers keyed by judge id and episode id."""

    def __init__(self, judge_id, recorded):
        self.judge_id = judge_id
        self.recorded = recorded

    def label(self, prompt, *, episode_id):
        try:
            return self.recorded[self.judge_id][episode_id]
        except KeyError as exc:  # pragma: no cover - test bug
            raise AssertionError(f"no recorded answer for {self.judge_id}/{episode_id}") from exc


class ConstantJudgeClient:
    """Return one fixed raw answer for every episode (test helper)."""

    def __init__(self, judge_id, raw):
        self.judge_id = judge_id
        self.raw = raw

    def label(self, prompt, *, episode_id):
        return self.raw


def _recorded():
    with open(ANSWERS, encoding="utf-8") as handle:
        return json.load(handle)


def _episodes(candidates, text_by_id):
    from agent_observatory.silver import SilverEpisode

    return tuple(
        SilverEpisode(
            episode_id=candidate.episode_id,
            group_key=candidate.group_key,
            provider=candidate.provider,
            observed_at=candidate.observed_at,
            text=text_by_id[candidate.episode_id],
        )
        for candidate in candidates
    )


class SilverSamplingTests(unittest.TestCase):
    def setUp(self):
        self.taxonomy = load_taxonomy(V2_PATH)

    def test_load_candidates_and_session_key(self):
        candidates = load_candidates_csv(CANDIDATES)
        self.assertEqual(len(candidates), 6)
        self.assertEqual(candidates[0].provider, "codex")
        key = session_key_from_group_key(candidates[0].group_key)
        self.assertEqual(key, ("city-a", "host-a", "codex", "s-1"))
        self.assertIsNone(session_key_from_group_key("bead:g1"))

    def test_sample_is_deterministic_and_proportional(self):
        candidates = load_candidates_csv(CANDIDATES)
        first = sample_stratified(candidates, 4, seed="s")
        second = sample_stratified(list(reversed(candidates)), 4, seed="s")
        self.assertEqual([ep.episode_id for ep in first], [ep.episode_id for ep in second])
        counts = {}
        for episode in first:
            counts[episode.provider] = counts.get(episode.provider, 0) + 1
        self.assertEqual(sum(counts.values()), 4)
        self.assertIn("codex", counts)

    def test_sample_larger_than_pool_is_rejected(self):
        candidates = load_candidates_csv(CANDIDATES)
        with self.assertRaises(SilverError):
            sample_stratified(candidates, 99)


class SilverPromptTests(unittest.TestCase):
    def setUp(self):
        self.taxonomy = load_taxonomy(V2_PATH)

    def test_prompt_contains_criteria_and_version(self):
        prompt = build_judge_prompt("Please fix the bug", self.taxonomy)
        self.assertIn(JUDGE_PROMPT_VERSION, prompt)
        self.assertIn("bugfix", prompt)
        self.assertIn("Correct an existing defect", prompt)
        self.assertIn("Please fix the bug", prompt)
        self.assertIn("unknown", prompt)
        self.assertIn("ONE JSON object", prompt)

    def test_prompt_bounds_text(self):
        prompt = build_judge_prompt("x" * 5000, self.taxonomy, max_bytes=64)
        self.assertIn("[truncated:", prompt)

    def test_parse_answer_accepts_code_fences_and_alias(self):
        allowed = ["bugfix", "unknown"]
        self.assertEqual(parse_judge_answer('```json\n{"primary_intent": "bugfix"}\n```', allowed), ("bugfix", None))
        self.assertEqual(parse_judge_answer('{"label": "unknown", "confidence": 0.4}', allowed), ("unknown", 0.4))

    def test_parse_answer_rejects_unknown_label_and_bad_confidence(self):
        with self.assertRaises(SilverError):
            parse_judge_answer('{"primary_intent": "nonsense"}', ["bugfix"])
        with self.assertRaises(SilverError):
            parse_judge_answer('{"primary_intent": "bugfix", "confidence": 1.5}', ["bugfix"])
        with self.assertRaises(SilverError):
            parse_judge_answer("not json", ["bugfix"])


class KappaTests(unittest.TestCase):
    def test_kappa_perfect_and_chance(self):
        self.assertEqual(cohen_kappa([("a", "a"), ("b", "b")]), 1.0)
        self.assertEqual(cohen_kappa([("a", "a"), ("a", "b"), ("b", "a"), ("b", "b")]), 0.0)
        self.assertIsNone(cohen_kappa([(None, "a")]))

    def test_kappa_known_value(self):
        # 8 agreements out of 10 with balanced marginals: po=0.8, pe=0.5 -> 0.6
        pairs = [("a", "a")] * 4 + [("b", "b")] * 4 + [("a", "b"), ("b", "a")]
        self.assertAlmostEqual(cohen_kappa(pairs), 0.6, places=6)


class KappaOverJudgesTests(unittest.TestCase):
    def test_two_judges_matches_the_single_pair(self):
        labels = [
            {"a": "x", "b": "x"},
            {"a": "x", "b": "y"},
            {"a": "y", "b": "y"},
            {"a": "y", "b": "x"},
        ]
        self.assertEqual(
            kappa_over_judges(labels, ["a", "b"]),
            cohen_kappa([("x", "x"), ("x", "y"), ("y", "y"), ("y", "x")]),
        )

    def test_more_than_two_judges_returns_the_minimum_pair(self):
        # a/b agree perfectly; c is inverted against both, so the minimum pair
        # (-1.0) must drive the gate rather than the first pair (1.0).
        labels = [
            {"a": "x", "b": "x", "c": "y"},
            {"a": "x", "b": "x", "c": "y"},
            {"a": "y", "b": "y", "c": "x"},
            {"a": "y", "b": "y", "c": "x"},
        ]
        pairwise = [
            cohen_kappa([(row[left], row[right]) for row in labels])
            for left, right in (("a", "b"), ("a", "c"), ("b", "c"))
        ]
        self.assertEqual(pairwise[0], 1.0)
        self.assertEqual(kappa_over_judges(labels, ["a", "b", "c"]), min(pairwise))
        self.assertLess(kappa_over_judges(labels, ["a", "b", "c"]), 1.0)

    def test_fewer_than_two_judges_or_no_overlap_is_none(self):
        self.assertIsNone(kappa_over_judges([{"a": "x"}], ["a"]))
        self.assertIsNone(
            kappa_over_judges([{"a": None, "b": "x"}], ["a", "b"])
        )

    def test_missing_pair_in_a_three_judge_set_fails_closed(self):
        labels = [
            {"a": "x", "b": "x", "c": None},
            {"a": "y", "b": "y", "c": None},
        ]
        self.assertIsNone(kappa_over_judges(labels, ["a", "b", "c"]))


class SilverBuildTests(unittest.TestCase):
    def setUp(self):
        self.taxonomy = load_taxonomy(V2_PATH)
        self.candidates = load_candidates_csv(CANDIDATES)
        self.recorded = _recorded()

    def _judges(self):
        return [
            (JUDGE_GLM, RecordedJudgeClient(JUDGE_GLM.judge_id, self.recorded)),
            (JUDGE_DEEPSEEK, RecordedJudgeClient(JUDGE_DEEPSEEK.judge_id, self.recorded)),
        ]

    def _sample(self):
        sample = sample_stratified(self.candidates, 4, seed="silver-v1")
        text = {episode.episode_id: f"Work for {episode.episode_id}." for episode in sample}
        return _episodes(sample, text)

    def test_build_adjudicates_and_reports_kappa(self):
        episodes = self._sample()
        result = build_silver_result(episodes, self.taxonomy, self._judges())
        by_id = result.gold_set.by_id()
        # ep-bugfix-1 / ep-impl-2 / ep-review-3 / ep-plan-4 agree; the others may
        # be sampled or not depending on the deterministic draw.
        agreed = [ep for ep in result.gold_set.episodes if ep.adjudication == "adjudicated"]
        self.assertGreaterEqual(len(agreed), 1)
        for episode in agreed:
            self.assertEqual(episode.annotator, "silver-judges")
            self.assertEqual(len(episode.label_set("primary_intent")), 1)
            self.assertIsNotNone(episode.metadata["judge_labels"])
        self.assertEqual(result.report["judge_calls"]["total"], 2 * len(episodes))
        self.assertIn("cohen_kappa", result.report["agreement"])
        self.assertTrue(by_id)

    def test_gold_set_round_trips_through_loader(self):
        episodes = self._sample()
        result = build_silver_result(episodes, self.taxonomy, self._judges())
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "silver.json")
            with open(path, "w", encoding="utf-8") as handle:
                handle.write(result.gold_set.to_json())
            loaded = load_gold_set(path, self.taxonomy)
        self.assertEqual(loaded.gold_set_hash(), result.gold_set.gold_set_hash())
        self.assertEqual(loaded.taxonomy_version, "2.0.0")

    def test_result_requires_two_judges(self):
        episodes = self._sample()
        with self.assertRaises(SilverError):
            build_silver_result(episodes, self.taxonomy, self._judges()[:1])

    def test_build_kappa_uses_every_judge_pair(self):
        episodes = self._sample()
        third = JudgeSpec(judge_id="judge-c", model="test/judge-c", api_model="test/judge-c")
        judges = self._judges() + [
            (third, ConstantJudgeClient("judge-c", '{"primary_intent":"unknown","confidence":0.5}'))
        ]
        result = build_silver_result(episodes, self.taxonomy, judges)
        report = result.report
        judge_ids = [entry["judge_id"] for entry in report["judges"]]
        self.assertEqual(judge_ids, ["glm-5p3-flash", "deepseek-v4-flash", "judge-c"])
        pairwise = [
            cohen_kappa(
                [
                    (row["judge_labels"][judge_ids[i]], row["judge_labels"][judge_ids[j]])
                    for row in report["episodes"]
                ]
            )
            for i in range(len(judge_ids))
            for j in range(i + 1, len(judge_ids))
        ]
        self.assertEqual(report["agreement"]["cohen_kappa"], min(pairwise))

    def test_evaluate_silver_vs_jev_scores_agreed_items(self):
        episodes = self._sample()
        result = build_silver_result(episodes, self.taxonomy, self._judges())
        agreed = [ep for ep in result.gold_set.episodes if ep.adjudication == "adjudicated"]
        self.assertGreaterEqual(len(agreed), 1)
        predictions = []
        for index, episode in enumerate(agreed):
            label = episode.primary("primary_intent")
            # Miss the second item so accuracy is not trivially 1.0 when possible.
            if index == 1:
                label = "unknown"
            predictions.append(
                Prediction(episode_id=episode.episode_id, predictor="jev", labels={"primary_intent": (label,)})
            )
        report = evaluate_silver_vs_jev(result.gold_set, predictions, self.taxonomy)
        self.assertEqual(report["jev"]["predicted"], len(agreed))
        self.assertEqual(report["jev"]["missing"], [])
        self.assertGreater(report["jev"]["accuracy"], 0.0)
        self.assertIsNotNone(report["agreement"]["cohen_kappa"])
        self.assertIn("silver_trustworthy", report["gate"])

    def test_full_evaluator_runs_on_agreed_items(self):
        episodes = self._sample()
        result = build_silver_result(episodes, self.taxonomy, self._judges())
        agreed = [ep for ep in result.gold_set.episodes if ep.adjudication == "adjudicated"]
        predictions = [
            Prediction(
                episode_id=episode.episode_id,
                predictor="jev",
                labels={"primary_intent": (episode.primary("primary_intent"),)},
            )
            for episode in agreed
        ]
        report = evaluate_silver_vs_jev(result.gold_set, predictions, self.taxonomy, run_full_evaluator=True)
        self.assertIn("evaluation", report)
        self.assertIn("jev", report["evaluation"]["evaluations"])

    def test_predictions_document_round_trips(self):
        predictions = [
            Prediction(episode_id="e1", predictor="jev", labels={"primary_intent": ("bugfix",)}),
            Prediction(episode_id="e2", predictor="jev", labels={"primary_intent": ("implementation",)}, confidence=0.5),
        ]
        document = build_silver_predictions_document(predictions)
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "preds.json")
            with open(path, "w", encoding="utf-8") as handle:
                json.dump(document, handle)
            loaded = load_predictions(path, self.taxonomy)
        self.assertEqual([p.episode_id for p in loaded], ["e1", "e2"])


class SilverTrustGateTests(unittest.TestCase):
    """The kappa floor is enforced on load, not merely reported."""

    def setUp(self):
        self.taxonomy = load_taxonomy(V2_PATH)
        self.candidates = load_candidates_csv(CANDIDATES)
        self.recorded = _recorded()

    def _judges(self):
        return [
            (JUDGE_GLM, RecordedJudgeClient(JUDGE_GLM.judge_id, self.recorded)),
            (JUDGE_DEEPSEEK, RecordedJudgeClient(JUDGE_DEEPSEEK.judge_id, self.recorded)),
        ]

    def _build(self, sample_size):
        sample = sample_stratified(self.candidates, sample_size, seed="silver-v1")
        text = {episode.episode_id: f"Work for {episode.episode_id}." for episode in sample}
        return build_silver_result(_episodes(sample, text), self.taxonomy, self._judges())

    def _write(self, result, directory):
        path = os.path.join(directory, "silver.json")
        with open(path, "w", encoding="utf-8") as handle:
            handle.write(result.gold_set.to_json())
        return path

    def test_untrusted_silver_is_refused_unless_opted_in(self):
        result = self._build(2)  # deterministic draw: kappa 0.333 < 0.6 floor
        self.assertFalse(result.report["agreement"]["trustworthy"])
        with tempfile.TemporaryDirectory() as tmp:
            path = self._write(result, tmp)
            with self.assertRaises(AnnotationError):
                load_gold_set(path, self.taxonomy)
            loaded = load_gold_set(path, self.taxonomy, allow_untrusted=True)
        self.assertTrue(loaded.is_silver)
        self.assertFalse(loaded.silver_trustworthy)

    def test_trusted_silver_loads_without_an_override(self):
        result = self._build(4)  # deterministic draw: kappa 0.692 >= 0.6 floor
        self.assertTrue(result.report["agreement"]["trustworthy"])
        with tempfile.TemporaryDirectory() as tmp:
            path = self._write(result, tmp)
            loaded = load_gold_set(path, self.taxonomy)
        self.assertTrue(loaded.is_silver)
        self.assertTrue(loaded.silver_trustworthy)
        self.assertEqual(loaded.gold_set_hash(), result.gold_set.gold_set_hash())

    # F4: a single agreeing episode yields kappa 1.0 by construction (the
    # degenerate branch in ``cohen_kappa``), so a minimum sample floor is needed
    # for the gate to mean anything.
    def test_tiny_sample_is_untrusted_even_with_perfect_agreement(self):
        candidates = load_candidates_csv(CANDIDATES)
        bugfix = [c for c in candidates if c.episode_id == "ep-bugfix-1"]
        episodes = _episodes(bugfix, {"ep-bugfix-1": "fix the flaky test"})
        result = build_silver_result(episodes, self.taxonomy, self._judges())
        agreement = result.report["agreement"]
        self.assertEqual(len(episodes), 1)
        self.assertEqual(agreement["cohen_kappa"], 1.0)
        self.assertIn("min_sample_size", agreement)
        self.assertGreaterEqual(agreement["min_sample_size"], 2)
        self.assertFalse(agreement["trustworthy"])
        self.assertFalse(result.gold_set.silver_trustworthy)

    def test_sample_at_or_above_the_floor_with_good_kappa_is_trusted(self):
        result = self._build(4)  # deterministic draw: kappa 0.692 >= 0.6 floor
        agreement = result.report["agreement"]
        self.assertGreaterEqual(result.report["sample"]["episodes"], agreement["min_sample_size"])
        self.assertTrue(agreement["trustworthy"])

    def test_evaluate_keeps_a_tiny_gold_set_untrusted(self):
        candidates = load_candidates_csv(CANDIDATES)
        bugfix = [c for c in candidates if c.episode_id == "ep-bugfix-1"]
        episodes = _episodes(bugfix, {"ep-bugfix-1": "fix the flaky test"})
        result = build_silver_result(episodes, self.taxonomy, self._judges())
        report = evaluate_silver_vs_jev(result.gold_set, [], self.taxonomy)
        self.assertEqual(report["agreement"]["sample_size"], 1)
        self.assertFalse(report["agreement"]["trustworthy"])
        self.assertFalse(report["gate"]["silver_trustworthy"])

    def test_silver_marker_without_a_trust_verdict_is_untrusted(self):
        fixture = os.path.join(HERE, "fixtures", "gold", "gold_episodes_v1.json")
        with open(fixture, encoding="utf-8") as handle:
            document = json.load(handle)
        document["silver"] = True  # an older silver set with no trust verdict
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "silver.json")
            with open(path, "w", encoding="utf-8") as handle:
                json.dump(document, handle)
            with self.assertRaises(AnnotationError):
                load_gold_set(path, self.taxonomy)
            loaded = load_gold_set(path, self.taxonomy, allow_untrusted=True)
        self.assertTrue(loaded.is_silver)
        self.assertIsNone(loaded.silver_trustworthy)

    def test_human_gold_set_is_not_affected(self):
        fixture = os.path.join(HERE, "fixtures", "gold", "gold_episodes_v1.json")
        loaded = load_gold_set(fixture, self.taxonomy)
        self.assertFalse(loaded.is_silver)
        self.assertIsNone(loaded.silver_trustworthy)


class HTTPJudgeClientTests(unittest.TestCase):
    class _Response:
        def __init__(self, payload):
            self._payload = payload

        def read(self):
            return json.dumps(self._payload).encode("utf-8")

        def __enter__(self):
            return self

        def __exit__(self, *args):
            return False

    def test_sends_user_agent_temperature_and_json_mode(self):
        captured = {}

        def opener(request, timeout=None):
            captured["headers"] = {key.lower(): value for key, value in request.headers.items()}
            captured["url"] = request.full_url
            captured["body"] = json.loads(request.data.decode("utf-8"))
            return self._Response({"choices": [{"message": {"content": '{"primary_intent":"bugfix"}'}}]})

        os.environ["UNIBLOCK_PROD_KEY"] = "test-key-not-real"
        self.addCleanup(os.environ.pop, "UNIBLOCK_PROD_KEY", None)
        client = HTTPJudgeClient(JUDGE_GLM, opener=opener, max_requests=1)
        content = client.label("prompt", episode_id="e1")
        self.assertEqual(content, '{"primary_intent":"bugfix"}')
        self.assertEqual(captured["headers"]["user-agent"], HTTP_USER_AGENT)
        self.assertEqual(captured["headers"]["authorization"], "Bearer test-key-not-real")
        self.assertTrue(captured["url"].endswith("/chat/completions"))
        self.assertEqual(captured["body"]["temperature"], 0)
        self.assertEqual(captured["body"]["response_format"], {"type": "json_object"})
        with self.assertRaises(SilverError):
            client.label("prompt", episode_id="e2")

    def test_missing_credential_is_an_error(self):
        os.environ.pop("UNIBLOCK_PROD_KEY", None)
        client = HTTPJudgeClient(JUDGE_GLM)
        with self.assertRaises(SilverError):
            client.label("prompt", episode_id="e1")

    def test_retries_transient_timeout_then_succeeds(self):
        attempts = {"n": 0}

        def opener(request, timeout=None):
            attempts["n"] += 1
            if attempts["n"] == 1:
                raise TimeoutError("read timed out")
            return self._Response({"choices": [{"message": {"content": "{\"primary_intent\":\"bugfix\"}"}}]})

        os.environ["UNIBLOCK_PROD_KEY"] = "test-key-not-real"
        self.addCleanup(os.environ.pop, "UNIBLOCK_PROD_KEY", None)
        client = HTTPJudgeClient(
            JUDGE_GLM, opener=opener, max_attempts=3, retry_backoff_seconds=0, max_requests=5
        )
        self.assertEqual(client.label("prompt", episode_id="e1"), '{"primary_intent":"bugfix"}')
        self.assertEqual(client.requests_made, 2)

    def test_does_not_retry_validation_status(self):
        import urllib.error

        def opener(request, timeout=None):
            raise urllib.error.HTTPError(request.full_url, 403, "forbidden", {}, None)

        os.environ["UNIBLOCK_PROD_KEY"] = "test-key-not-real"
        self.addCleanup(os.environ.pop, "UNIBLOCK_PROD_KEY", None)
        client = HTTPJudgeClient(JUDGE_GLM, opener=opener, max_attempts=3, retry_backoff_seconds=0)
        with self.assertRaises(SilverError):
            client.label("prompt", episode_id="e1")
        self.assertEqual(client.requests_made, 1)

    def test_checkpoint_replays_without_respending(self):
        from agent_observatory.silver import CheckpointJudgeClient

        class Counting:
            def __init__(self):
                self.calls = 0

            def label(self, prompt, *, episode_id):
                self.calls += 1
                return '{"primary_intent":"bugfix"}'

        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "checkpoint.json")
            inner = Counting()
            first = CheckpointJudgeClient("glm", inner, path)
            self.assertEqual(first.label("p", episode_id="e1"), '{"primary_intent":"bugfix"}')
            self.assertEqual(inner.calls, 1)
            second_inner = Counting()
            second = CheckpointJudgeClient("glm", second_inner, path)
            self.assertEqual(second.label("p", episode_id="e1"), '{"primary_intent":"bugfix"}')
            self.assertEqual(second_inner.calls, 0)
            self.assertEqual(second.cache_hits, 1)

    def test_checkpoint_merges_two_judge_sections(self):
        from agent_observatory.silver import CheckpointJudgeClient

        class Constant:
            def __init__(self, value):
                self.value = value

            def label(self, prompt, *, episode_id):
                return self.value

        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "checkpoint.json")
            glm = CheckpointJudgeClient("glm", Constant('{"primary_intent":"bugfix"}'), path)
            deepseek = CheckpointJudgeClient("deepseek", Constant('{"primary_intent":"implementation"}'), path)
            glm.label("p", episode_id="e1")
            deepseek.label("p", episode_id="e1")
            with open(path, encoding="utf-8") as handle:
                stored = json.load(handle)
        self.assertEqual(set(stored), {"glm", "deepseek"})
        self.assertEqual(stored["glm"]["e1"], '{"primary_intent":"bugfix"}')
        self.assertEqual(stored["deepseek"]["e1"], '{"primary_intent":"implementation"}')


class SilverProjectionTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.db = os.path.join(self.tmp.name, "proj.db")
        self.taxonomy = load_taxonomy(V2_PATH)
        records = [
            support.make_record(
                event_id="e1",
                kind="message",
                timestamp="2026-09-01T00:00:00Z",
                text=(
                    "[city] codex-worker-1 • 2026-09-01T00:00:00\n\n# GC Role Worker\n\n"
                    "You are a role worker. gc hook --claim"
                ),
            ),
            support.make_record(
                event_id="e2",
                kind="message",
                timestamp="2026-09-01T00:00:01Z",
                text="Please fix the flaky scheduler test",
            ),
            support.make_record(
                event_id="e3",
                kind="tool_call",
                timestamp="2026-09-01T00:00:02Z",
                tool_name="Bash",
                command="pytest tests/test_scheduler.py",
            ),
        ]
        path = support.write_jsonl(os.path.join(self.tmp.name, "s.jsonl"), records)
        self.store = ObservatoryStore(self.db)
        self.addCleanup(self.store.close)
        self.store.import_jsonl(path)

    def test_build_silver_episodes_drops_framework_and_keeps_task_text(self):
        candidates = load_candidates_csv(CANDIDATES)[:1]
        group = candidates[0].group_key.replace('"s-1"', '"session-1"')
        from agent_observatory.silver import CandidateEpisode

        candidate = CandidateEpisode(
            episode_id="ep-1",
            group_key=group,
            provider="codex",
            observed_at="2026-09-01T00:00:00Z",
        )
        episodes, skipped = build_silver_episodes(self.store, [candidate])
        self.assertEqual(skipped, [])
        self.assertEqual(len(episodes), 1)
        text = episodes[0].text
        self.assertIn("Please fix the flaky scheduler test", text)
        self.assertIn("pytest tests/test_scheduler.py", text)
        self.assertNotIn("GC Role Worker", text)
        self.assertNotIn("gc hook --claim", text)

    def test_render_transcript_text_is_bounded(self):
        state = {"excerpts": [{"kind": "message", "text": "x" * 5000}]}
        rendered = render_transcript_text(state, max_bytes=100)
        self.assertIn("[truncated:", rendered)

    def test_predictions_from_store_reads_metadata_classification(self):
        key = ("city-a", "host-a", "codex", "session-1")
        snapshot = self.store.session_snapshot(key)
        self.store.save_classification(
            subject_kind="session",
            snapshot_hash=snapshot,
            taxonomy_version="1.1.0",
            question_hash="q" * 64,
            model_version="jev-1.13.0",
            request_hash="r" * 64,
            response_hash="h" * 64,
            answers=[
                {
                    "question_id": "primary_intent",
                    "question_type": "choice",
                    "answer": {"choice": "bugfix", "confidence": 0.9, "probabilities": {"bugfix": 1.0}},
                }
            ],
        )
        from agent_observatory.silver import SilverEpisode

        episode = SilverEpisode(
            episode_id="ep-1",
            group_key='session:["city-a","host-a","codex","session-1"]',
            provider="codex",
            observed_at="2026-09-01T00:00:00Z",
            text="Please fix the flaky scheduler test",
        )
        predictions = predictions_from_store(self.store, [episode])
        self.assertEqual(len(predictions), 1)
        self.assertEqual(predictions[0].primary("primary_intent"), "bugfix")

    def test_predictions_from_store_prefers_text_scope_over_newer_metadata(self):
        # F3: the docstring promises text scope wins when present, but the
        # newest-classification lookup let a newer metadata row shadow an older
        # (text-scope) bugfix row.
        from agent_observatory.collector import _text_snapshot_hash
        from agent_observatory.silver import SilverEpisode

        key = ("city-a", "host-a", "codex", "session-1")
        raw = self.store.session_snapshot(key)
        self.store.save_classification(
            subject_kind="session",
            snapshot_hash=_text_snapshot_hash(raw),
            taxonomy_version="1.1.0",
            question_hash="q" * 64,
            model_version="jev-1.13.0",
            request_hash="t" * 64,
            response_hash="a" * 64,
            answers=[
                {
                    "question_id": "primary_intent",
                    "question_type": "choice",
                    "answer": {"choice": "bugfix", "confidence": 0.9},
                }
            ],
        )
        # The metadata row is newer, so classification_id is larger.
        self.store.save_classification(
            subject_kind="session",
            snapshot_hash=raw,
            taxonomy_version="1.1.0",
            question_hash="q" * 64,
            model_version="jev-1.13.0",
            request_hash="m" * 64,
            response_hash="b" * 64,
            answers=[
                {
                    "question_id": "primary_intent",
                    "question_type": "choice",
                    "answer": {"choice": "unknown", "confidence": 0.5},
                }
            ],
        )
        episode = SilverEpisode(
            episode_id="ep-1",
            group_key='session:["city-a","host-a","codex","session-1"]',
            provider="codex",
            observed_at="2026-09-01T00:00:00Z",
            text="Please fix the flaky scheduler test",
        )
        predictions = predictions_from_store(self.store, [episode])
        self.assertEqual(len(predictions), 1)
        self.assertEqual(predictions[0].primary("primary_intent"), "bugfix")


class _FakeCompleted:
    def __init__(self, returncode=0, stdout="", stderr=""):
        self.returncode = returncode
        self.stdout = stdout
        self.stderr = stderr


def _multi_recorded():
    with open(MULTI_ANSWERS, encoding="utf-8") as handle:
        return json.load(handle)


def _multi_episodes():
    from agent_observatory.silver import SilverEpisode

    return tuple(
        SilverEpisode(
            episode_id=f"e{index}",
            group_key=f'session:["city-a","host-a","codex","s-{index}"]',
            provider="codex",
            observed_at=f"2026-09-0{index}T00:00:00Z",
            text=f"Work for episode e{index}.",
        )
        for index in range(1, 9)
    )


class MultiJudgeKappaTests(unittest.TestCase):
    """Pairwise, Fleiss and distribution math for the multi-judge path."""

    def setUp(self):
        self.taxonomy = load_taxonomy(V2_PATH)
        self.recorded = _multi_recorded()
        self.judge_ids = ["glm-5p3-flash", "deepseek-v4-flash", "gpt6-luna", "gemini-3p8-flash"]
        self.labels = [
            {judge_id: json.loads(self.recorded[judge_id][f"e{index}"])["primary_intent"] for judge_id in self.judge_ids}
            for index in range(1, 9)
        ]

    def test_pairwise_kappa_has_all_six_pairs(self):
        rows = pairwise_cohen_kappa(self.labels, self.judge_ids)
        self.assertEqual(len(rows), 6)
        pairs = {(row["left"], row["right"]) for row in rows}
        self.assertEqual(len(pairs), 6)
        for row in rows:
            self.assertEqual(row["overlap"], 8)
            self.assertIsInstance(row["cohen_kappa"], float)

    def test_fleiss_perfect_agreement_is_one(self):
        labels = [{"a": "x", "b": "x"}, {"a": "y", "b": "y"}]
        self.assertEqual(fleiss_kappa(labels, ["a", "b"]), 1.0)
        # Fleiss treats raters as exchangeable; with one rater there is no kappa.
        self.assertIsNone(fleiss_kappa([{"a": "x"}], ["a"]))

    def test_fleiss_known_value(self):
        # 3 raters, 4 subjects: two unanimous and two 2-1 splits -> kappa 1/3.
        labels = [
            {"a": "x", "b": "x", "c": "x"},
            {"a": "y", "b": "y", "c": "y"},
            {"a": "x", "b": "x", "c": "y"},
            {"a": "y", "b": "y", "c": "x"},
        ]
        self.assertAlmostEqual(fleiss_kappa(labels, ["a", "b", "c"]), 1.0 / 3.0, places=12)

    def test_fleiss_is_none_without_complete_cases(self):
        labels = [{"a": "x", "b": None, "c": "x"}, {"a": None, "b": "y", "c": "y"}]
        self.assertIsNone(fleiss_kappa(labels, ["a", "b", "c"]))
        self.assertIsNone(fleiss_kappa([{"a": "x"}], ["a"]))

    def test_judge_label_distribution_counts_unknown_and_missing(self):
        labels = [
            {"a": "bugfix", "b": "unknown"},
            {"a": "bugfix", "b": None},
            {"a": "unknown", "b": "unknown"},
        ]
        report = judge_label_distribution(labels, ["a", "b"])
        self.assertEqual(report["a"]["labelled"], 3)
        self.assertEqual(report["a"]["unknown"], 1)
        self.assertEqual(report["b"]["missing"], 1)
        self.assertAlmostEqual(report["b"]["unknown_rate"], 1.0)

    def test_fixture_pins_its_own_kappa_values_not_the_150_episode_run(self):
        # MEASUREMENT-SILVER.md reports the 150-episode production run
        # (two-judge 0.406, minimum pairwise 0.199, Fleiss 0.306). This
        # checked-in fixture is an 8-episode mechanics fixture with different
        # values; pin its own numbers so a fixture edit cannot silently rewrite
        # the measurement note.
        two_judge = pairwise_cohen_kappa(self.labels, self.judge_ids[:2])
        self.assertEqual(len(two_judge), 1)
        self.assertAlmostEqual(two_judge[0]["cohen_kappa"], 0.6, places=9)
        pairwise = pairwise_cohen_kappa(self.labels, self.judge_ids)
        self.assertAlmostEqual(
            min(row["cohen_kappa"] for row in pairwise), 1.0 / 3.0, places=9
        )
        self.assertAlmostEqual(
            fleiss_kappa(self.labels, self.judge_ids), 0.528023598820059, places=9
        )

    def test_agreement_reference_requires_threshold(self):
        episode_labels = [(f"e{index + 1}", row) for index, row in enumerate(self.labels)]
        majority = agreement_reference(episode_labels, self.judge_ids, min_agreement=3)
        unanimous = agreement_reference(episode_labels, self.judge_ids, min_agreement=4)
        self.assertEqual(set(majority), {"e1", "e2", "e3", "e5", "e8"})
        self.assertEqual(set(unanimous), {"e1", "e3", "e5", "e8"})
        self.assertEqual(majority["e2"], "bugfix")


class MultiJudgeBuildTests(unittest.TestCase):
    def setUp(self):
        self.taxonomy = load_taxonomy(V2_PATH)
        self.recorded = _multi_recorded()
        self.episodes = _multi_episodes()

    def _judges(self):
        specs = [JUDGE_GLM, JUDGE_DEEPSEEK, JUDGE_GPT6_LUNA, JUDGE_GEMINI38_FLASH]
        return [(spec, RecordedJudgeClient(spec.judge_id, self.recorded)) for spec in specs]

    def test_four_judge_build_reports_pairwise_fleiss_and_distribution(self):
        result = build_silver_result(self.episodes, self.taxonomy, self._judges())
        report = result.report
        self.assertEqual(len(report["agreement"]["pairwise_cohen_kappa"]), 6)
        self.assertIsInstance(report["agreement"]["fleiss_kappa"], float)
        self.assertEqual(set(report["judge_distribution"]), {
            "glm-5p3-flash", "deepseek-v4-flash", "gpt6-luna", "gemini-3p8-flash",
        })
        backends = {entry["judge_id"]: entry["backend"] for entry in report["judges"]}
        self.assertEqual(backends["gpt6-luna"], "codex-cli")
        self.assertEqual(backends["gemini-3p8-flash"], "agy-cli")
        self.assertEqual(report["judge_calls"]["total"], 4 * len(self.episodes))

    def test_default_two_judge_build_still_adjudicates_as_before(self):
        judges = [
            (JUDGE_GLM, RecordedJudgeClient(JUDGE_GLM.judge_id, self.recorded)),
            (JUDGE_DEEPSEEK, RecordedJudgeClient(JUDGE_DEEPSEEK.judge_id, self.recorded)),
        ]
        result = build_silver_result(self.episodes, self.taxonomy, judges)
        self.assertEqual(len(result.report["agreement"]["pairwise_cohen_kappa"]), 1)
        # The two-judge agreement is exactly the single pair's kappa.
        pair = result.report["agreement"]["pairwise_cohen_kappa"][0]
        self.assertEqual(result.report["agreement"]["cohen_kappa"], pair["cohen_kappa"])

    def test_non_strict_build_records_a_transport_error_without_coercion(self):
        class Broken:
            def label(self, prompt, *, episode_id):
                raise RuntimeError("subscription rate limited")

        judges = self._judges() + [
            (JudgeSpec(judge_id="judge-broken", model="test/judge-broken", api_model="test/judge-broken"),
             Broken())
        ]
        result = build_silver_result(self.episodes, self.taxonomy, judges, strict=False)
        self.assertEqual(result.report["judge_calls"]["transport_errors"]["judge-broken"], len(self.episodes))
        self.assertEqual(result.report["judge_distribution"]["judge-broken"]["missing"], len(self.episodes))
        # No fabricated label: the episode metadata records null for the broken judge.
        for episode in result.gold_set.episodes:
            self.assertIsNone(episode.metadata["judge_labels"]["judge-broken"])

    def test_strict_build_still_raises_on_transport_error(self):
        class Broken:
            def label(self, prompt, *, episode_id):
                raise RuntimeError("boom")

        judges = self._judges() + [
            (JudgeSpec(judge_id="judge-broken", model="test/judge-broken", api_model="test/judge-broken"),
             Broken())
        ]
        with self.assertRaises(RuntimeError):
            build_silver_result(self.episodes, self.taxonomy, judges)


class _ScriptedJudge:
    """Return a fixed label per episode and optionally fail chosen episodes."""

    def __init__(self, labels, *, fail_on=()):
        self._labels = labels
        self._fail_on = set(fail_on)

    def label(self, prompt, *, episode_id):
        if episode_id in self._fail_on:
            raise RuntimeError("judge transport error")
        return json.dumps({"primary_intent": self._labels[episode_id], "confidence": 0.8})


def _scripted_episodes(count=6):
    from agent_observatory.silver import SilverEpisode

    return tuple(
        SilverEpisode(
            episode_id=f"e{index}",
            group_key=f'session:["city-a","host-a","codex","s-{index}"]',
            provider="codex",
            observed_at=f"2026-09-0{index}T00:00:00Z",
            text=f"Work for episode e{index}.",
        )
        for index in range(1, count + 1)
    )


class OverlapFloorTests(unittest.TestCase):
    """A systematically failing judge must not inflate the trust gate (F6).

    ``--tolerate-judge-failures`` drops ``None`` pairs from ``cohen_kappa``'s
    denominator, so a judge that fails on exactly the episodes it would disagree
    on can leave a perfect kappa over a thin overlap. The fail-closed coverage
    floor must refuse that pair even though the reported kappa looks high.
    """

    def setUp(self):
        self.taxonomy = load_taxonomy(V2_PATH)
        self.episodes = _scripted_episodes(6)
        self.a_labels = {
            "e1": "bugfix", "e2": "bugfix", "e3": "bugfix",
            "e4": "pr_review", "e5": "pr_review", "e6": "pr_review",
        }
        # Judge B disagrees on e3 and e6 -- exactly the episodes it fails on.
        self.b_labels = {
            "e1": "bugfix", "e2": "bugfix", "e3": "implementation",
            "e4": "pr_review", "e5": "pr_review", "e6": "unknown",
        }
        self.judge_a = JudgeSpec(judge_id="judge-a", model="test/judge-a", api_model="test/judge-a")
        self.judge_b = JudgeSpec(judge_id="judge-b", model="test/judge-b", api_model="test/judge-b")

    def _build(self, *, b_fail_on=()):
        judges = [
            (self.judge_a, _ScriptedJudge(self.a_labels)),
            (self.judge_b, _ScriptedJudge(self.b_labels, fail_on=b_fail_on)),
        ]
        return build_silver_result(self.episodes, self.taxonomy, judges, strict=False)

    def test_overlap_floor_requires_both_absolute_and_fractional_minimum(self):
        self.assertFalse(overlap_meets_floor(4, 6))  # 4 < 0.9 * 6
        self.assertTrue(overlap_meets_floor(6, 6))
        self.assertFalse(overlap_meets_floor(3, 3))  # below the absolute floor
        self.assertTrue(overlap_meets_floor(4, 4))
        self.assertFalse(overlap_meets_floor(0, 0))

    def test_failing_disagreements_do_not_inflate_the_pair_kappa(self):
        result = self._build(b_fail_on={"e3", "e6"})
        agreement = result.report["agreement"]
        pair = agreement["pairwise_cohen_kappa"][0]
        self.assertEqual(pair["overlap"], 4)
        self.assertEqual(pair["cohen_kappa"], 1.0)
        self.assertFalse(pair["meets_floor"])
        self.assertFalse(pair["trustworthy"])
        self.assertEqual(pair["reason"], "missing_labels_over_floor")
        self.assertFalse(agreement["coverage_ok"])
        self.assertFalse(agreement["trustworthy"])
        self.assertEqual(agreement["trust_reason"], "missing_labels_over_floor")
        self.assertFalse(result.gold_set.silver_trustworthy)

    def test_full_overlap_with_two_disagreements_reports_the_honest_kappa(self):
        result = self._build()
        agreement = result.report["agreement"]
        pair = agreement["pairwise_cohen_kappa"][0]
        self.assertEqual(pair["overlap"], 6)
        self.assertTrue(pair["meets_floor"])
        self.assertAlmostEqual(agreement["cohen_kappa"], 0.5, places=9)
        self.assertFalse(agreement["trustworthy"])
        self.assertEqual(agreement["trust_reason"], "kappa_below_floor")

    def test_full_overlap_and_full_agreement_is_trusted(self):
        judges = [
            (self.judge_a, _ScriptedJudge(self.a_labels)),
            (self.judge_b, _ScriptedJudge(self.a_labels)),
        ]
        result = build_silver_result(self.episodes, self.taxonomy, judges)
        agreement = result.report["agreement"]
        self.assertEqual(agreement["cohen_kappa"], 1.0)
        self.assertTrue(agreement["coverage_ok"])
        self.assertTrue(agreement["trustworthy"])
        self.assertEqual(agreement["trust_reason"], "ok")
        self.assertTrue(result.gold_set.silver_trustworthy)


class JudgeReferenceTests(unittest.TestCase):
    def setUp(self):
        self.taxonomy = load_taxonomy(V2_PATH)
        self.recorded = _multi_recorded()
        self.episodes = _multi_episodes()

    def _result(self):
        specs = [JUDGE_GLM, JUDGE_DEEPSEEK, JUDGE_GPT6_LUNA, JUDGE_GEMINI38_FLASH]
        judges = [(spec, RecordedJudgeClient(spec.judge_id, self.recorded)) for spec in specs]
        return build_silver_result(self.episodes, self.taxonomy, judges)

    def test_references_cover_each_judge_majority_and_unanimous(self):
        result = self._result()
        predictions = [
            Prediction(
                episode_id=episode.episode_id,
                predictor="jev",
                labels={"primary_intent": (label,)},
            )
            for episode in result.gold_set.episodes
            if (label := episode.primary("primary_intent")) is not None
        ]
        report = evaluate_jev_references(result.gold_set, predictions)
        self.assertEqual(set(report["judges"]), {
            "glm-5p3-flash", "deepseek-v4-flash", "gpt6-luna", "gemini-3p8-flash",
        })
        names = set(report["references"])
        for judge_id in report["judges"]:
            self.assertIn(f"judge:{judge_id}", names)
        self.assertIn("majority_3_of_4", names)
        self.assertIn("unanimous_4_of_4", names)
        self.assertEqual(report["references"]["majority_3_of_4"]["episodes"], 5)
        self.assertEqual(report["references"]["unanimous_4_of_4"]["episodes"], 4)
        self.assertEqual(report["references"]["judge:gpt6-luna"]["episodes"], 8)

    def test_evaluate_silver_vs_jev_embeds_the_multi_judge_comparison(self):
        result = self._result()
        predictions = [
            Prediction(
                episode_id=episode.episode_id,
                predictor="jev",
                labels={"primary_intent": (episode.primary("primary_intent"),)},
            )
            for episode in result.gold_set.episodes
            if episode.primary("primary_intent") is not None
        ]
        report = evaluate_silver_vs_jev(result.gold_set, predictions, self.taxonomy)
        self.assertIn("judge_references", report)
        self.assertEqual(len(report["judge_references"]["pairwise_cohen_kappa"]), 6)
        self.assertIn("majority_3_of_4", report["judge_references"]["references"])


class JudgeResolutionTests(unittest.TestCase):
    def test_default_and_known_judges(self):
        self.assertEqual(resolve_judges([]), [JUDGE_GLM, JUDGE_DEEPSEEK])
        self.assertEqual(resolve_judge("gpt6-luna"), JUDGE_GPT6_LUNA)
        self.assertEqual(resolve_judge("gemini-3p8-flash"), JUDGE_GEMINI38_FLASH)
        self.assertEqual(resolve_judge("deepseek/deepseek-flash"), JUDGE_DEEPSEEK)

    def test_explicit_backend_and_bare_slug(self):
        spec = resolve_judge("codex-cli:gpt-6-luna")
        self.assertEqual(spec.backend, "codex-cli")
        self.assertEqual(spec.api_model, "gpt-6-luna")
        gateway = resolve_judge("fireworks-ai/some-model")
        self.assertEqual(gateway.backend, "gateway")
        self.assertEqual(gateway.api_model, "fireworks-ai/some-model")
        with self.assertRaises(SilverError):
            resolve_judge("bogus-backend:model")

    def test_judge_config_file_round_trips(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "judges.json")
            with open(path, "w", encoding="utf-8") as handle:
                json.dump(
                    [
                        "glm-5p3-flash",
                        {"judge_id": "gem", "model": "gemini-3.8-flash-medium", "backend": "agy-cli"},
                    ],
                    handle,
                )
            specs = load_judge_config(path)
        self.assertEqual([spec.judge_id for spec in specs], ["glm-5p3-flash", "gem"])
        self.assertEqual(specs[1].backend, "agy-cli")
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, "empty.json")
            with open(path, "w", encoding="utf-8") as handle:
                json.dump([], handle)
            with self.assertRaises(SilverError):
                load_judge_config(path)

    def test_build_judge_schema_restricts_labels(self):
        schema = build_judge_schema(["bugfix", "unknown"])
        self.assertEqual(schema["properties"]["primary_intent"]["enum"], ["bugfix", "unknown"])
        self.assertFalse(schema["additionalProperties"])

    def test_build_judge_schema_supports_a_custom_answer_key(self):
        schema = build_judge_schema(["known", "unknown"], key="evidence")
        self.assertEqual(schema["properties"]["evidence"]["enum"], ["known", "unknown"])
        self.assertEqual(schema["required"], ["evidence", "confidence"])
        self.assertNotIn("primary_intent", schema["properties"])
        with self.assertRaises(SilverError):
            build_judge_schema(["known"], key="not an identifier")


class CLIJudgeClientTests(unittest.TestCase):
    def test_codex_client_uses_schema_read_only_and_reads_output(self):
        captured = {}

        def runner(argv, *, input_text, timeout):
            if argv[1] == "--version":
                return _FakeCompleted(stdout="codex-cli 9.9.9\n")
            captured["argv"] = argv
            captured["input"] = input_text
            out_path = argv[argv.index("-o") + 1]
            with open(out_path, "w", encoding="utf-8") as handle:
                handle.write('{"primary_intent":"bugfix","confidence":0.5}')
            return _FakeCompleted(stdout="")

        client = CodexCLIJudgeClient(JUDGE_GPT6_LUNA, ["bugfix", "unknown"], runner=runner)
        content = client.label("judge prompt text", episode_id="e1")
        self.assertEqual(json.loads(content), {"primary_intent": "bugfix", "confidence": 0.5})
        self.assertEqual(client.cli_version, "codex-cli 9.9.9")
        self.assertEqual(captured["input"], "judge prompt text")
        self.assertIn("read-only", captured["argv"])
        self.assertIn("--ephemeral", captured["argv"])
        self.assertEqual(captured["argv"][captured["argv"].index("-m") + 1], "gpt-6-luna")

    def test_cli_client_writes_a_custom_schema_key(self):
        client = CodexCLIJudgeClient(
            JUDGE_GPT6_LUNA, ["known", "unknown"], runner=lambda *a, **k: None, schema_key="evidence"
        )
        schema = json.loads(client.schema_path.read_text(encoding="utf-8"))
        self.assertEqual(schema["properties"]["evidence"]["enum"], ["known", "unknown"])
        self.assertEqual(schema["required"], ["evidence", "confidence"])

    def test_agy_client_parses_structured_output(self):
        captured = {}

        def runner(argv, *, input_text, timeout):
            if argv[1] == "--version":
                return _FakeCompleted(stdout="1.2.11\n")
            captured["argv"] = argv
            return _FakeCompleted(
                stdout=json.dumps(
                    {
                        "structured_output": {"primary_intent": "unknown", "confidence": 0.4},
                        "response": "noise",
                    }
                )
            )

        client = AgyCLIJudgeClient(JUDGE_GEMINI38_FLASH, ["bugfix", "unknown"], runner=runner)
        content = client.label("judge prompt text", episode_id="e1")
        self.assertEqual(json.loads(content), {"primary_intent": "unknown", "confidence": 0.4})
        self.assertEqual(client.cli_version, "1.2.11")
        self.assertIn("--json-schema", captured["argv"])
        self.assertTrue(any(arg.startswith("--print=") for arg in captured["argv"]))

    def test_cli_client_retries_transient_failure(self):
        attempts = {"n": 0}

        def runner(argv, *, input_text, timeout):
            if argv[1] == "--version":
                return _FakeCompleted(stdout="1.2.11\n")
            attempts["n"] += 1
            if attempts["n"] == 1:
                return _FakeCompleted(returncode=1, stderr="rate limited")
            return _FakeCompleted(stdout=json.dumps({"structured_output": {"primary_intent": "bugfix"}}))

        client = AgyCLIJudgeClient(
            JUDGE_GEMINI38_FLASH,
            ["bugfix"],
            runner=runner,
            max_attempts=2,
            retry_backoff_seconds=0,
        )
        self.assertEqual(json.loads(client.label("p", episode_id="e1")), {"primary_intent": "bugfix"})
        self.assertEqual(client.calls_made, 2)

    def test_build_judge_client_dispatches_on_backend(self):
        gateway = build_judge_client(JUDGE_GLM, ["bugfix"])
        self.assertIsInstance(gateway, HTTPJudgeClient)
        codex = build_judge_client(JUDGE_GPT6_LUNA, ["bugfix"], runner=lambda *a, **k: _FakeCompleted())
        self.assertIsInstance(codex, CodexCLIJudgeClient)
        agy = build_judge_client(JUDGE_GEMINI38_FLASH, ["bugfix"], runner=lambda *a, **k: _FakeCompleted())
        self.assertIsInstance(agy, AgyCLIJudgeClient)


if __name__ == "__main__":
    unittest.main()
