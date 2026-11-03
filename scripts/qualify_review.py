"""Read-only proof of native implementation/reviewer separation for quick-start.

No provider files, terminal controls, writes or role-name-only attribution. The
supervisor's actual step rows bind the assignees; its structured histories bind
each durable GC session to its native provider conversation. Unknown evidence
fails qualification. The caller supplies its existing lesson deadline.
"""
from __future__ import annotations
import json
from pathlib import Path
import re
import subprocess
import time
import urllib.parse
import urllib.request


def remaining(deadline):
    left = deadline - time.monotonic()
    if left <= 0:
        raise TimeoutError("Independent review proof exceeded the original lesson deadline")
    return min(15, left)


def prove_review(*, base, city, city_root, source, root, record, evidence, deadline):
    """Save GET receipts and reject a shared or unknown native reviewer."""
    evidence = Path(evidence)
    evidence.mkdir(exist_ok=False)

    def read(label, suffix):
        url = base + '/v0/city/' + urllib.parse.quote(city, safe='') + '/' + suffix
        with urllib.request.urlopen(url, timeout=remaining(deadline)) as response:
            row = json.load(response)
        (evidence / (urllib.parse.quote(label, safe='') + '.json')).write_text(json.dumps(row, indent=2) + '\n')
        return row

    def item(kind, identity, query=''):
        row = read(kind + '-' + identity, kind + '/' + urllib.parse.quote(identity, safe='') + query)
        assert row.get('id') == identity, ("Exact supervisor identity mismatch", kind, identity)
        return row

    workflow = item('bead', root)
    source_row = item('bead', source)
    metadata = workflow.get('metadata', {})
    assert metadata.get('gc.formula') == 'quick-start', "Reviewer proof supports the actual quick-start graph only"
    assert metadata.get('gc.formula_contract') == 'graph.v2', "Unexpected source-delivery graph contract"
    actual = json.loads(metadata['gc.delivery.record'])
    assert actual == record, "Delivery record changed while proving its review"
    assert actual['root'] == root and actual['source'] == source, "Delivery names another root/input"
    assert source_row.get('metadata', {}).get('gc.delivery.workflow_root') == root, "Input belongs to another workflow"
    city_root = Path(city_root).resolve()
    assert city_root.name == city, "Owned city path/name differs"
    assert Path(actual['repo']).resolve() == city_root/'projects/practice', "Delivery repository is outside the exact owned tutorial rig"
    workspace_root = city_root/'.gc/worktrees/practice'
    assert Path(actual['worktree_root']).resolve() == workspace_root, "Delivery names another worktree root"
    assert Path(actual['worktree']).resolve() == workspace_root/root, "Delivery worktree is not owned by this exact workflow"
    assert source_row['status'] == 'closed' and workflow['status'] == 'closed', "Delivery is not terminal"
    assert workflow.get('metadata', {}).get('gc.outcome') == 'pass', "Workflow did not naturally pass"
    accepted = actual['integrated_sha']
    assert re.fullmatch('[0-9a-f]{40}|[0-9a-f]{64}', accepted), "Full accepted SHA missing"
    assert actual['review_status'] == 'approved' and all(actual.get(key) == accepted for key in ('output_sha', 'reviewed_sha', 'tested_sha')), "Review/test/integration SHA differs"
    assert actual['cleanup_complete'] is True and not Path(actual['worktree']).exists(), "Owned worktree cleanup is unproven"
    head = subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=actual['repo'], text=True,
                                   timeout=remaining(deadline)).strip()
    assert head == accepted, "Actual repository HEAD differs from accepted integration"

    steps = read('steps', 'runs/' + urllib.parse.quote(root, safe='') + '/steps')
    assert steps.get('run_id') == root and isinstance(steps.get('steps'), list), "Run steps belong to another root"
    assert not steps.get('partial'), "Run step projection is incomplete"
    # These are the exact authored quick-start check children. The compiler
    # preserves gc.step_id and gc.ralph_step_id across native loop iterations;
    # the distinct gc.run_target declarations route implementation and review.
    roles = {'implement': 'gc.implementation-worker', 'inspect': 'gc.implementation-reviewer'}
    groups = {}
    for step in steps['steps']:
        bead = item('bead', step['id'])
        meta = bead.get('metadata', {})
        role = meta.get('gc.step_id')
        if role not in roles or meta.get('gc.ralph_step_id') != 'review':
            continue
        # Scope-check controls inherit logical step ids, but are run by GC's
        # control dispatcher, not by the actual implementation/review worker.
        if meta.get('gc.scope_role') == 'control' and meta.get('gc.kind') in ('scope-check', 'fanout'):
            continue
        assert meta.get('gc.run_target') == roles[role], "Native step target differs from quick-start's declared role"
        iteration = meta.get('gc.iteration')
        assert isinstance(iteration, str) and re.fullmatch('[1-3]', iteration), "Unknown native review iteration"
        assert step.get('assignee') == bead.get('assignee'), "Projected/native step assignee differs"
        assert step.get('status') == 'completed', "Native implementation/review step is not completed"
        assert bead.get('status') == 'closed' and meta.get('gc.outcome') == 'pass', "Step completion was not a native pass"
        group = groups.setdefault(int(iteration), {})
        assert role not in group, "Ambiguous implementation/review occurrence"
        group[role] = step
    assert groups, "No actual implementation/review step assignees observed"
    final_iteration = max(groups)
    pair = groups[final_iteration]
    assert set(pair) == set(roles), "Final native iteration lacks completed implementation and review"
    identities = {}
    for role, step in pair.items():
        sid = step.get('assignee')
        assert isinstance(sid, str) and sid, "Native step has no durable assignee"
        session = item('session', sid, '?exact_id=true')
        transcript = read('transcript-' + sid, 'session/' + urllib.parse.quote(sid, safe='') + '/transcript?format=structured')
        assert transcript.get('id') == sid and transcript.get('format') == 'structured', "Missing authoritative structured transcript"
        history = transcript.get('history') or {}
        assert history.get('gc_session_id') == sid, "Transcript belongs to another GC session"
        native = history.get('provider_session_id')
        assert isinstance(native, str) and native, "Native conversation identity is unavailable"
        assert transcript.get('provider') == session.get('provider') and session.get('provider'), "Provider identity differs"
        assert transcript.get('structured_messages'), "Native conversation has no retained evidence"
        identities[role] = {'step': step['id'], 'gc_session_id': sid,
                            'provider': session['provider'], 'provider_session_id': native,
                            'logical_conversation_id': history.get('logical_conversation_id'),
                            'template': session.get('template')}
    implement, reviewer = identities['implement'], identities['inspect']
    assert implement['gc_session_id'] != reviewer['gc_session_id'], "Implementer reviewed its own work in the same GC session"
    assert (implement['provider'], implement['provider_session_id']) != (reviewer['provider'], reviewer['provider_session_id']), "Implementer and reviewer share one native conversation"
    proof = {'status': 'PASS', 'root': root, 'source': source, 'iteration': final_iteration,
             'accepted_sha': accepted, 'actual_repository_head': head, 'identities': identities,
             'basis': 'Actual quick-start child metadata and assignees, retained supervisor session/structured-history identities, exact reviewed/tested/integrated delivery SHA. This is execution separation; semantic review quality remains a native acceptance observation.'}
    (evidence / 'proof.json').write_text(json.dumps(proof, indent=2) + '\n')
    return proof
