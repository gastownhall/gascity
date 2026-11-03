"""Read-only proof of native implementation/reviewer separation for quick-start.

No provider files, terminal controls, writes or role-name-only attribution. The
supervisor's actual step rows bind the assignees; its structured histories bind
each durable GC session to its native provider conversation. Unknown evidence
fails qualification. The caller supplies its existing lesson deadline.
"""
from __future__ import annotations
import base64
import json
from pathlib import Path
import re
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request


def remaining(deadline):
    left = deadline - time.monotonic()
    if left <= 0:
        raise TimeoutError("Independent review proof exceeded the original lesson deadline")
    return min(15, left)


NATIVE_UUID = r"[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}"


def canonical_native_identity(provider, history):
    """A supported continuous native conversation, independent of path aliases."""
    assert provider in ('claude', 'codex'), "Native provider identity is unsupported"
    assert isinstance(history, dict) and (history.get('continuity') or {}).get('status') == 'continuous' and not (history.get('continuity') or {}).get('has_branches'), "Native history continuity is not qualified"
    native = history.get('provider_session_id')
    assert isinstance(native, str) and native, "Native conversation identity is unavailable"
    name = native.replace('\\', '/').rsplit('/', 1)[-1]
    if name.endswith('.jsonl'):
        name = name[:-6]
    if provider == 'codex' and name.startswith('rollout-'):
        match = re.fullmatch(r"rollout-\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2}-(" + NATIVE_UUID + r")", name)
    else:
        match = re.fullmatch('(' + NATIVE_UUID + ')', name)
    assert match, "Native conversation identity is not an actual supported UUID"
    canonical = match.group(1).lower()
    assert canonical.replace('-', '').strip('0'), "Native conversation identity is the nil UUID"
    return canonical


def prove_review(*, base, city, city_root, source, root, record, evidence, deadline):
    """Save GET receipts and reject a shared or unknown native reviewer."""
    evidence = Path(evidence)
    evidence.mkdir(exist_ok=False)

    def read(label, suffix):
        url = base + '/v0/city/' + urllib.parse.quote(city, safe='') + '/' + suffix
        try:
            with urllib.request.urlopen(url, timeout=remaining(deadline)) as response:
                row = json.load(response)
        except urllib.error.HTTPError as error:
            try:
                body = error.read()
            finally:
                error.close()
            receipt = {'url': url, 'status': error.code, 'body_base64': base64.b64encode(body).decode('ascii'),
                       'body_text': body.decode('utf-8', errors='replace')}
            (evidence / (urllib.parse.quote(label, safe='') + '.http-error.json')).write_text(json.dumps(receipt, indent=2) + '\n')
            raise
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
        assert meta.get('gc.root_bead_id') == root, "Native actor step belongs to another workflow root"
        if bead.get('assignee'):
            assert meta.get('gc.session_id') == bead['assignee'], "Native actor claim is not bound to its assignee"
        else:
            assert step.get('status') in ('skipped', 'canceled'), "Unclaimed actor cannot establish execution proof"
        iteration = meta.get('gc.iteration')
        assert isinstance(iteration, str) and re.fullmatch('[1-3]', iteration), "Unknown native review iteration"
        assert step.get('assignee') == bead.get('assignee'), "Projected/native step assignee differs"
        assert step.get('status') in ('completed', 'failed', 'skipped', 'canceled') and bead.get('status') == 'closed', "Actor step is not terminal"
        group = groups.setdefault(int(iteration), {})
        assert role not in group, "Ambiguous implementation/review occurrence"
        group[role] = {'step': step, 'metadata': meta}
    assert groups, "No actual implementation/review step assignees observed"
    final_iteration = max(groups)
    pair = groups[final_iteration]
    assert set(pair) == set(roles), "Final native iteration lacks completed implementation and review"
    for entry in pair.values():
        assert entry['step'].get('status') == 'completed' and entry['metadata'].get('gc.outcome') == 'pass', "Final implementation/review pair did not complete with native pass"
    observed = {}
    cached = {}
    for iteration, group in sorted(groups.items()):
        observed[iteration] = {}
        for role, entry in group.items():
            step = entry['step']
            sid = step.get('assignee')
            identity = {'step': step['id'], 'status': step['status'], 'outcome': entry['metadata'].get('gc.outcome'), 'gc_session_id': sid}
            if not sid:
                identity['native_unavailable'] = 'unclaimed terminal row; not execution or reviewer evidence'
                observed[iteration][role] = identity
                continue
            assert isinstance(sid, str), "Native step has no durable assignee"
            if sid not in cached:
                session = item('session', sid, '?exact_id=true')
                transcript = read('transcript-' + sid, 'session/' + urllib.parse.quote(sid, safe='') + '/transcript?format=structured')
                assert transcript.get('id') == sid and transcript.get('format') == 'structured', "Missing authoritative structured transcript"
                cached[sid] = (session, transcript)
            session, transcript = cached[sid]
            history = transcript.get('history') or {}
            if not transcript.get('structured_messages') and iteration != final_iteration and step['status'] != 'completed':
                identity['native_unavailable'] = 'historical nonpassing actor has no retained native messages; not reviewer evidence'
                observed[iteration][role] = identity
                continue
            assert history.get('gc_session_id') == sid, "Transcript belongs to another GC session"
            assert transcript.get('provider') == session.get('provider') and session.get('provider'), "Provider identity differs"
            native = canonical_native_identity(session['provider'], history)
            assert transcript.get('structured_messages'), "Native conversation has no retained evidence"
            identity.update(provider=session['provider'], provider_session_id=history['provider_session_id'], native_uuid=native,
                            logical_conversation_id=history.get('logical_conversation_id'), template=session.get('template'))
            observed[iteration][role] = identity
    implementations = [row['implement'] for row in observed.values() if 'implement' in row]
    reviewers = [row['inspect'] for row in observed.values() if 'inspect' in row]
    implement_sessions = {row['gc_session_id'] for row in implementations if row.get('gc_session_id')}
    review_sessions = {row['gc_session_id'] for row in reviewers if row.get('gc_session_id')}
    assert not implement_sessions & review_sessions, "Implementer reviewed its own work in the same GC session across iterations"
    implement_native = {(row['provider'], row['native_uuid']) for row in implementations if row.get('native_uuid')}
    review_native = {(row['provider'], row['native_uuid']) for row in reviewers if row.get('native_uuid')}
    assert not implement_native & review_native, "Implementer and reviewer share one native conversation across iterations"
    identities = observed[final_iteration]
    assert all(row.get('native_uuid') for row in identities.values()), "Final pair lacks proven native identities"
    remaining(deadline)  # Last read, parsing and retained receipts must fit the original bound.
    proof = {'status': 'PASS', 'root': root, 'source': source, 'iteration': final_iteration,
             'accepted_sha': accepted, 'actual_repository_head': head, 'identities': identities, 'observed_iterations': observed,
             'causality_limit': 'The delivery record binds the accepted SHA to this root/source/repository/worktree, and actor claims bind the observed native sessions to its steps. GC history has no first-class per-review actor/SHA attestation; this proof does not infer one from transcript text.',
             'basis': 'Actual quick-start child metadata and assignees, retained supervisor session/structured-history identities, exact reviewed/tested/integrated delivery SHA. This is execution separation; semantic review quality remains a native acceptance observation.'}
    receipt = evidence / 'proof.json'
    receipt.write_text(json.dumps(proof, indent=2) + '\n')
    try:
        remaining(deadline)
    except TimeoutError:
        proof.update(status='FAILED', error='Original lesson deadline exceeded while writing the proof receipt')
        receipt.write_text(json.dumps(proof, indent=2) + '\n')
        raise
    return proof
