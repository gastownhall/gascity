"""Independent-review evidence guards; no provider or runtime is launched."""
import copy
import base64
import io
import urllib.error
import ast
import json
import os
import pathlib
from pathlib import Path
import tempfile
import time
import unittest
from unittest.mock import patch

from qualify_review import prove_review, canonical_native_identity


class Response:
    def __init__(self, row): self.row = row
    def __enter__(self): return self
    def __exit__(self, *args): pass
    def read(self): return json.dumps(self.row).encode()


class ReviewProofTests(unittest.TestCase):
    def setUp(self):
        self.folder = tempfile.TemporaryDirectory()
        self.addCleanup(self.folder.cleanup)
        self.path = Path(self.folder.name)
        self.city = self.path/'owned-city'
        self.repo = self.city/'projects/practice'
        self.repo.mkdir(parents=True)
        self.sha = 'a' * 40
        self.record = {'root': 'owned-root', 'source': 'owned-input', 'repo': str(self.repo),
                       'worktree_root': str(self.city/'.gc/worktrees/practice'),
                       'worktree': str(self.city/'.gc/worktrees/practice/owned-root'), 'cleanup_complete': True,
                       'review_status': 'approved', 'integrated_sha': self.sha,
                       'reviewed_sha': self.sha, 'tested_sha': self.sha, 'output_sha': self.sha}
        self.rows = {
            'bead/owned-root': {'id': 'owned-root', 'status': 'closed', 'metadata': {
                'gc.formula': 'quick-start', 'gc.formula_contract': 'graph.v2',
                'gc.outcome': 'pass', 'gc.delivery.record': json.dumps(self.record)}},
            'bead/owned-input': {'id': 'owned-input', 'status': 'closed', 'metadata': {'gc.delivery.workflow_root': 'owned-root'}},
            'runs/owned-root/steps': {'run_id': 'owned-root', 'steps': []},
        }
        for role, target, sid, native in [('implement', 'gc.implementation-worker', 'worker-session', 'a0187eff-1ad6-4755-803c-fd41f1035a93'),
                                          ('inspect', 'gc.implementation-reviewer', 'review-session', '7b32036c-f6d3-4336-b2e2-d631f0f8bbb2')]:
            bid = 'owned-'+role
            self.rows['runs/owned-root/steps']['steps'].append({'id': bid, 'status': 'completed', 'assignee': sid})
            self.rows['bead/'+bid] = {'id': bid, 'status': 'closed', 'assignee': sid, 'metadata': {
                'gc.step_id': role, 'gc.ralph_step_id': 'review', 'gc.root_bead_id': 'owned-root', 'gc.session_id': sid, 'gc.iteration': '1', 'gc.outcome': 'pass', 'gc.run_target': target}}
            self.rows['session/'+sid+'?exact_id=true'] = {'id': sid, 'provider': 'claude', 'template': 'practice/'+target}
            self.rows['session/'+sid+'/transcript?format=structured'] = {
                'id': sid, 'format': 'structured', 'provider': 'claude',
                'history': {'continuity': {'status': 'continuous'}, 'gc_session_id': sid, 'provider_session_id': native, 'logical_conversation_id': sid},
                'structured_messages': [{'id': 'native-message', 'role': 'assistant', 'blocks': [{'type': 'text', 'text': 'Retained actual content'}]}]}
        self.calls = []

    def run_proof(self, *, head=None, deadline=None):
        def get(url, timeout):
            self.assertGreater(timeout, 0)
            suffix = url.split('/v0/city/owned-city/', 1)[1]
            self.calls.append(suffix)
            row = self.rows[suffix]
            if isinstance(row, BaseException): raise row
            return Response(copy.deepcopy(row))
        with patch('qualify_review.urllib.request.urlopen', side_effect=get), \
             patch('qualify_review.subprocess.check_output', return_value=(head or self.sha)+'\n') as git:
            result = prove_review(base='http://127.0.0.1:18599', city='owned-city', city_root=self.city, source='owned-input', root='owned-root',
                                  record=self.record, evidence=self.path/'proof', deadline=deadline or time.monotonic()+30)
            git.assert_called_once()
            return result

    def test_actual_distinct_sessions_and_native_conversations_are_proved(self):
        result = self.run_proof()
        self.assertEqual(result['status'], 'PASS')
        self.assertEqual(result['actual_repository_head'], self.sha)
        self.assertNotEqual(result['identities']['implement']['gc_session_id'], result['identities']['inspect']['gc_session_id'])
        self.assertTrue((self.path/'proof/transcript-review-session.json').is_file())

    def test_scope_check_inheriting_inspect_id_is_not_a_reviewer(self):
        self.rows['runs/owned-root/steps']['steps'].append({'id': 'inspect-control', 'status': 'completed'})
        self.rows['bead/inspect-control'] = {'id': 'inspect-control', 'status': 'closed', 'metadata': {
            'gc.step_id': 'inspect', 'gc.ralph_step_id': 'review', 'gc.scope_role': 'control', 'gc.kind': 'scope-check'}}
        self.assertEqual(self.run_proof()['status'], 'PASS')

    def test_same_gc_session_cannot_self_review(self):
        old = self.rows['runs/owned-root/steps']['steps'][1]
        old['assignee'] = 'worker-session'
        self.rows['bead/owned-inspect']['assignee'] = 'worker-session'
        self.rows['bead/owned-inspect']['metadata']['gc.session_id'] = 'worker-session'
        with self.assertRaisesRegex(AssertionError, 'same GC session'): self.run_proof()

    def test_distinct_gc_rows_cannot_hide_one_native_conversation(self):
        self.rows['session/review-session/transcript?format=structured']['history']['provider_session_id'] = 'a0187eff-1ad6-4755-803c-fd41f1035a93'
        with self.assertRaisesRegex(AssertionError, 'one native conversation'): self.run_proof()

    def test_missing_native_identity_stays_unqualified(self):
        del self.rows['session/review-session/transcript?format=structured']['history']['provider_session_id']
        with self.assertRaisesRegex(AssertionError, 'identity is unavailable'): self.run_proof()

    def test_role_label_without_authored_step_target_is_not_independence(self):
        self.rows['bead/owned-inspect']['metadata']['gc.run_target'] = 'gc.implementation-worker'
        with self.assertRaisesRegex(AssertionError, 'declared role'): self.run_proof()

    def test_changed_accepted_sha_cannot_pass_review_proof(self):
        self.record['tested_sha'] = 'b' * 40
        self.rows['bead/owned-root']['metadata']['gc.delivery.record'] = json.dumps(self.record)
        with self.assertRaisesRegex(AssertionError, 'SHA differs'): self.run_proof()

    def test_repository_moved_after_acceptance_cannot_pass(self):
        with self.assertRaisesRegex(AssertionError, 'repository HEAD'): self.run_proof(head='b'*40)

    def test_foreign_repository_or_worktree_is_never_used_for_git_read(self):
        self.record['repo'] = '/not/this/trial'
        self.rows['bead/owned-root']['metadata']['gc.delivery.record'] = json.dumps(self.record)
        with self.assertRaisesRegex(AssertionError, 'exact owned tutorial rig'): self.run_proof()

    def test_prior_iteration_cannot_mask_missing_final_reviewer(self):
        row = copy.deepcopy(self.rows['bead/owned-implement'])
        row['id'] = 'later-implement'; row['metadata']['gc.iteration'] = '2'
        self.rows['bead/later-implement'] = row
        self.rows['runs/owned-root/steps']['steps'].append({'id': 'later-implement', 'status': 'completed', 'assignee': row['assignee']})
        with self.assertRaisesRegex(AssertionError, 'Final native iteration'): self.run_proof()

    def test_unknown_projected_assignee_cannot_claim_native_assignment(self):
        self.rows['runs/owned-root/steps']['steps'][1]['assignee'] = 'other-session'
        with self.assertRaisesRegex(AssertionError, 'assignee differs'): self.run_proof()

    def test_existing_lesson_deadline_is_enforced_before_first_read(self):
        with self.assertRaises(TimeoutError): self.run_proof(deadline=time.monotonic()-1)
        self.assertEqual(self.calls, [])

    def test_malformed_nil_and_unknown_native_histories_refused(self):
        identity = 'a0187eff-1ad6-4755-803c-fd41f1035a93'
        for history in [
            {'provider_session_id': 'native-worker', 'continuity': {'status': 'continuous'}},
            {'provider_session_id': '00000000-0000-0000-0000-000000000000', 'continuity': {'status': 'continuous'}},
            {'provider_session_id': identity},
            {'provider_session_id': identity, 'continuity': {'status': 'unknown'}},
            {'provider_session_id': identity, 'continuity': {'status': 'branched'}},
            {'provider_session_id': identity, 'continuity': {'status': 'continuous', 'has_branches': True}},
        ]:
            with self.subTest(history=history), self.assertRaises(AssertionError):
                canonical_native_identity('claude', history)

    def test_claude_path_and_case_aliases_are_one_actual_conversation(self):
        native = 'a0187eff-1ad6-4755-803c-fd41f1035a93'
        history = self.rows['session/review-session/transcript?format=structured']['history']
        history['provider_session_id'] = '/owned/provider/' + native.upper() + '.jsonl'
        with self.assertRaisesRegex(AssertionError, 'one native conversation'):
            self.run_proof()

    def test_codex_rollout_path_alias_is_one_actual_conversation(self):
        native = '01a1a105-9ee9-7cd3-a25b-45840b88eb51'
        for sid in ['worker-session', 'review-session']:
            self.rows['session/'+sid+'?exact_id=true']['provider'] = 'codex'
            transcript = self.rows['session/'+sid+'/transcript?format=structured']
            transcript['provider'] = 'codex'
            transcript['history']['provider_session_id'] = native
        self.rows['session/review-session/transcript?format=structured']['history']['provider_session_id'] = '/owned/sessions/rollout-2026-11-03T10-47-42-' + native.upper() + '.jsonl'
        with self.assertRaisesRegex(AssertionError, 'one native conversation'):
            self.run_proof()

    def test_actor_root_and_claim_session_ties_are_required(self):
        self.rows['bead/owned-inspect']['metadata']['gc.root_bead_id'] = 'foreign-root'
        with self.assertRaisesRegex(AssertionError, 'another workflow root'):
            self.run_proof()

    def test_unbound_actor_claim_cannot_establish_reviewer_identity(self):
        self.rows['bead/owned-inspect']['metadata']['gc.session_id'] = 'foreign-session'
        with self.assertRaisesRegex(AssertionError, 'bound to its assignee'):
            self.run_proof()

    def add_prior_failed_implementation(self, native):
        for name in ['owned-implement', 'owned-inspect']:
            self.rows['bead/'+name]['metadata']['gc.iteration'] = '2'
        prior = copy.deepcopy(self.rows['bead/owned-implement'])
        prior.update(id='prior-implement', assignee='prior-worker')
        prior['metadata'].update({'gc.iteration': '1', 'gc.session_id': 'prior-worker', 'gc.outcome': 'fail'})
        self.rows['bead/prior-implement'] = prior
        self.rows['runs/owned-root/steps']['steps'].append({'id': 'prior-implement', 'status': 'failed', 'assignee': 'prior-worker'})
        self.rows['session/prior-worker?exact_id=true'] = {'id': 'prior-worker', 'provider': 'claude'}
        transcript = copy.deepcopy(self.rows['session/worker-session/transcript?format=structured'])
        transcript.update(id='prior-worker')
        transcript['history'].update(gc_session_id='prior-worker', provider_session_id=native)
        self.rows['session/prior-worker/transcript?format=structured'] = transcript

    def test_repair_keeps_prior_failed_actor_truthfully_failed(self):
        self.add_prior_failed_implementation('01a1a105-9ee9-7cd3-a25b-45840b88eb51')
        result = self.run_proof()
        self.assertEqual(result['iteration'], 2)
        self.assertEqual(result['observed_iterations'][1]['implement']['status'], 'failed')
        self.assertEqual(result['observed_iterations'][1]['implement']['outcome'], 'fail')
        self.assertEqual(result['identities']['inspect']['status'], 'completed')

    def test_prior_implementer_cannot_become_later_native_reviewer(self):
        self.add_prior_failed_implementation('7b32036c-f6d3-4336-b2e2-d631f0f8bbb2')
        with self.assertRaisesRegex(AssertionError, 'one native conversation across iterations'):
            self.run_proof()

    def test_unclaimed_skipped_actor_is_not_review_completion_evidence(self):
        for name in ['owned-implement', 'owned-inspect']:
            self.rows['bead/'+name]['metadata']['gc.iteration'] = '2'
        prior = copy.deepcopy(self.rows['bead/owned-inspect'])
        prior.update(id='prior-skipped', assignee=None)
        prior['metadata'].update({'gc.iteration': '1', 'gc.outcome': 'skipped'})
        prior['metadata'].pop('gc.session_id')
        self.rows['bead/prior-skipped'] = prior
        self.rows['runs/owned-root/steps']['steps'].append({'id': 'prior-skipped', 'status': 'skipped'})
        result = self.run_proof()
        earlier = result['observed_iterations'][1]['inspect']
        self.assertEqual(earlier['status'], 'skipped')
        self.assertNotIn('native_uuid', earlier)
        self.assertIn('native_unavailable', earlier)

    def test_final_io_crossing_original_deadline_never_writes_pass(self):
        now = [0.0]
        original = Response.read
        def read(response):
            result = original(response)
            if response.row.get('id') == 'review-session' and response.row.get('format') == 'structured':
                now[0] = 101.0
            return result
        with patch('qualify_review.time.monotonic', side_effect=lambda: now[0]), patch.object(Response, 'read', read):
            with self.assertRaises(TimeoutError):
                self.run_proof(deadline=100.0)
        self.assertFalse((self.path/'proof/proof.json').exists())

    def test_final_receipt_write_crossing_deadline_is_retained_as_failed(self):
        now = [0.0]
        original = Path.write_text
        def write(path, data, *args, **kwargs):
            result = original(path, data, *args, **kwargs)
            if path.name == 'proof.json' and json.loads(data).get('status') == 'PASS':
                now[0] = 101.0
            return result
        with patch('qualify_review.time.monotonic', side_effect=lambda: now[0]), patch.object(Path, 'write_text', write):
            with self.assertRaises(TimeoutError):
                self.run_proof(deadline=100.0)
        receipt = json.loads((self.path/'proof/proof.json').read_text())
        self.assertEqual(receipt['status'], 'FAILED')
        self.assertIn('deadline exceeded', receipt['error'])

    def test_http_failure_receipt_preserves_exact_status_url_and_body(self):
        url = 'http://127.0.0.1:18599/v0/city/owned-city/bead/owned-root'
        body = b'{"error":"snapshot unavailable"}\x00\xff'
        self.rows['bead/owned-root'] = urllib.error.HTTPError(url, 503, 'unavailable', {}, io.BytesIO(body))
        with self.assertRaises(urllib.error.HTTPError):
            self.run_proof()
        receipt = json.loads((self.path/'proof/bead-owned-root.http-error.json').read_text())
        self.assertEqual(receipt['url'], url)
        self.assertEqual(receipt['status'], 503)
        self.assertEqual(base64.b64decode(receipt['body_base64']), body)

    def test_maintained_qualifier_cannot_accept_one_native_conversation_for_both_roles(self):
        qualifier = Path(os.environ.get('RFC_QUALIFIER_UNDER_TEST', str(Path(__file__).with_name('qualify-beginner'))))
        syntax = ast.parse(qualifier.read_text())
        function = next(node for node in syntax.body if isinstance(node, ast.FunctionDef) and node.name == 'wait_delivery')
        code = compile(ast.Module(body=[function], type_ignores=[]), str(qualifier), 'exec')
        source = copy.deepcopy(self.rows['bead/owned-input'])
        workflow = copy.deepcopy(self.rows['bead/owned-root'])
        calls = []
        def required(**kwargs):
            calls.append(kwargs)
            raise AssertionError('Implementer and reviewer share one native conversation')
        scope = {'time': time, 'pathlib': pathlib, 'json': json, 'lessons': [], 'base': 'http://127.0.0.1:18599',
                 'city': self.path/'owned-city', 'out': self.path, 'cmd': lambda *args, **kwargs: '',
                 'bead': lambda label, identity: source if identity == 'owned-input' else workflow,
                 'prove_review': required}
        exec(code, scope)
        with self.assertRaisesRegex(AssertionError, 'one native conversation'):
            scope['wait_delivery']('greeting', 'owned-input', 'owned-root')
        self.assertEqual(len(calls), 1)
        self.assertEqual(scope['lessons'], [], 'Unproven review entered the accepted lesson list')


if __name__ == '__main__': unittest.main()
