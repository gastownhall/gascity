"""Independent-review evidence guards; no provider or runtime is launched."""
import copy
import ast
import json
import os
import pathlib
from pathlib import Path
import tempfile
import time
import unittest
from unittest.mock import patch

from qualify_review import prove_review


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
        for role, target, sid, native in [('implement', 'gc.implementation-worker', 'worker-session', 'native-worker'),
                                          ('inspect', 'gc.implementation-reviewer', 'review-session', 'native-reviewer')]:
            bid = 'owned-'+role
            self.rows['runs/owned-root/steps']['steps'].append({'id': bid, 'status': 'completed', 'assignee': sid})
            self.rows['bead/'+bid] = {'id': bid, 'status': 'closed', 'assignee': sid, 'metadata': {
                'gc.step_id': role, 'gc.ralph_step_id': 'review', 'gc.iteration': '1', 'gc.outcome': 'pass', 'gc.run_target': target}}
            self.rows['session/'+sid+'?exact_id=true'] = {'id': sid, 'provider': 'claude', 'template': 'practice/'+target}
            self.rows['session/'+sid+'/transcript?format=structured'] = {
                'id': sid, 'format': 'structured', 'provider': 'claude',
                'history': {'gc_session_id': sid, 'provider_session_id': native, 'logical_conversation_id': sid},
                'structured_messages': [{'id': 'native-message', 'role': 'assistant', 'blocks': [{'type': 'text', 'text': 'Retained actual content'}]}]}
        self.calls = []

    def run_proof(self, *, head=None, deadline=None):
        def get(url, timeout):
            self.assertGreater(timeout, 0)
            suffix = url.split('/v0/city/owned-city/', 1)[1]
            self.calls.append(suffix)
            return Response(copy.deepcopy(self.rows[suffix]))
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
        with self.assertRaisesRegex(AssertionError, 'same GC session'): self.run_proof()

    def test_distinct_gc_rows_cannot_hide_one_native_conversation(self):
        self.rows['session/review-session/transcript?format=structured']['history']['provider_session_id'] = 'native-worker'
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
