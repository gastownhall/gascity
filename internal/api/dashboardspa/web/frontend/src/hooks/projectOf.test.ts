import { describe, expect, it } from 'vitest';
import { getActiveCity, setActiveCity } from '../api/cityBase';
import {
  MAINTENANCE_PROJECT,
  ORCHESTRATION_PROJECT,
  agentProject,
  beadProject,
  cleanWorkerName,
  isAgentOutsideRig,
  isOrchestrationAgent,
  isOrchestrationSession,
  isPerRigDispatcherAgent,
  isWorkerSession,
  mailProject,
  orchestrationLabel,
  sessionProject,
} from './projectOf';
import type { DashboardSession } from 'gas-city-dashboard-shared';

function gcSession(partial: Partial<DashboardSession>): DashboardSession {
  return {
    id: partial.id ?? 'gc-1',
    template: '',
    session_name: partial.id ?? 'gc-1',
    title: '',
    state: 'active',
    created_at: '2026-06-03T00:00:00Z',
    attached: false,
    running: true,
    provider: 'claude',
    ...partial,
  } as DashboardSession;
}

// The cross-rig orchestration bucket now LABELS with the active city name
// (the operator thinks "the city", not "Orchestration"); the KEY stays the
// stable ORCHESTRATION_PROJECT constant. The global test setup sets the active
// city to 'test-city'. Resolved lazily (a function, not a module-load const)
// so it reads the city AFTER setup has run, not at import time.
const cityLabel = (): string => getActiveCity() ?? ORCHESTRATION_PROJECT;

describe('beadProject', () => {
  it.each([
    ['gc-1920', 'gc'],
    ['agent-diagnostics-503', 'agent-diagnostics'],
    ['code-intel-digest-mp5', 'code-intel-digest'],
    ['codeprobe-gg9f', 'codeprobe'],
    ['codeprobe-4cl6.2', 'codeprobe'],
    ['co-ysv', 'co'],
  ])('parses %s → %s', (id, expected) => {
    expect(beadProject({ id } as never)).toBe(expected);
  });

  it('falls back to the raw id when no suffix is found', () => {
    expect(beadProject({ id: 'noprefix' } as never)).toBe('noprefix');
  });
});

describe('sessionProject', () => {
  it('uses basename of rig path', () => {
    expect(sessionProject({ rig: '/home/ds/gascity' } as never)).toEqual({
      key: 'gascity',
      label: 'gascity',
    });
    expect(sessionProject({ rig: '/home/ds/projects/zeldascension' } as never)).toEqual({
      key: 'zeldascension',
      label: 'zeldascension',
    });
  });

  it('falls back to pool then template when rig is missing', () => {
    expect(sessionProject({ pool: 'codex' } as never)).toEqual({
      key: 'codex',
      label: 'codex',
    });
    expect(sessionProject({ template: '/some/path/foo' } as never)).toEqual({
      key: 'foo',
      label: 'foo',
    });
  });

  it('returns "(no rig)" bucket when no candidate exists', () => {
    expect(sessionProject({} as never)).toEqual({
      key: '(no rig)',
      label: '(no rig)',
    });
  });

  it('normalizes the key for case + separator drift while preserving the display label', () => {
    // The whole point of the {key, label} shape: rig paths with mixed
    // case or underscores must bucket together while the header shows
    // the original form. See useListFilters' bucketer.
    expect(sessionProject({ rig: '/home/ds/projects/GEO' } as never)).toEqual({
      key: 'geo',
      label: 'GEO',
    });
    expect(sessionProject({ rig: 'scix_experiments' } as never)).toEqual({
      key: 'scix-experiments',
      label: 'scix_experiments',
    });
  });
});

describe('mailProject', () => {
  it('uses rig directly', () => {
    expect(mailProject({ rig: 'ds-research' } as never)).toBe('ds-research');
  });

  it('falls back to "(no rig)" when missing', () => {
    expect(mailProject({} as never)).toBe('(no rig)');
    expect(mailProject({ rig: '' } as never)).toBe('(no rig)');
  });
});

// ── Agent grouping (gascity-dashboard-ay6 + Phase-4 H3/M3 follow-up) ──
describe('agentProject', () => {
  it('routes cross-rig orchestration agents to the pinned group, labelled with the city name', () => {
    expect(agentProject({ name: 'mayor' } as never)).toEqual({
      key: ORCHESTRATION_PROJECT,
      label: cityLabel(),
    });
    expect(agentProject({ name: 'control-dispatcher' } as never)).toEqual({
      key: ORCHESTRATION_PROJECT,
      label: cityLabel(),
    });
    expect(agentProject({ name: 'oversight-rig.chief-of-staff' } as never)).toEqual({
      key: ORCHESTRATION_PROJECT,
      label: cityLabel(),
    });
  });

  it('uses the rig basename when rig is set (mirrors sessionProject)', () => {
    expect(agentProject({ name: 'a1', rig: '/home/ds/gascity' } as never)).toEqual({
      key: 'gascity',
      label: 'gascity',
    });
    expect(agentProject({ name: 'a2', rig: '/home/ds/projects/GEO' } as never)).toEqual({
      key: 'geo',
      label: 'GEO',
    });
  });

  it('falls back to pool when rig is absent', () => {
    expect(agentProject({ name: 'a1', pool: 'research' } as never)).toEqual({
      key: 'research',
      label: 'research',
    });
  });

  it('treats empty-string rig as absent (the supervisor uses "" for cross-rig agents)', () => {
    expect(agentProject({ name: 'a1', rig: '', pool: 'research' } as never)).toEqual({
      key: 'research',
      label: 'research',
    });
  });

  it('falls back to "(no rig)" when neither rig nor pool nor orchestration name matches', () => {
    expect(agentProject({ name: 'a1' } as never)).toEqual({
      key: '(no rig)',
      label: '(no rig)',
    });
  });

  it('does NOT classify a rig-scoped agent as orchestration even with an orchestration name (rig guard wins)', () => {
    // Guarantees the rig-guard semantics: an agent named 'mayor' that
    // somehow got scoped to a rig is treated as a rig agent, not
    // accidentally lifted into the cross-rig Orchestration group.
    expect(agentProject({ name: 'mayor', rig: '/home/ds/gascity' } as never)).toEqual({
      key: 'gascity',
      label: 'gascity',
    });
  });

  // gascity-dashboard-jj2z: a gascity builtin MAINTENANCE pool (e.g. `dog`)
  // runs cross-rig agents whose `rig` is empty. The pool name is NOT a rig, so
  // it must bucket into the pinned Maintenance group, never fall through to
  // pool-as-rig (which mislabelled `dog` as a rig in the Rig column/filter).
  it('buckets a maintenance-builtin pool (dog) into Maintenance, NOT a rig named "dog"', () => {
    const bucket = agentProject({ name: 'dog-1', pool: 'dog' } as never);
    expect(bucket).toEqual({ key: MAINTENANCE_PROJECT, label: MAINTENANCE_PROJECT });
    // The exact symptom from the bug report: no rig bucket named 'dog'.
    expect(bucket.key).not.toBe('dog');
    expect(bucket.label).not.toBe('dog');
  });

  it('buckets a maintenance pool into Maintenance even with empty-string rig (cross-rig dog agents)', () => {
    expect(agentProject({ name: 'dog-2', rig: '', pool: 'dog' } as never)).toEqual({
      key: MAINTENANCE_PROJECT,
      label: MAINTENANCE_PROJECT,
    });
  });

  it('does NOT bucket a rig-scoped agent into Maintenance even if its pool is a maintenance pool (rig wins)', () => {
    // Mirrors the orchestration rig-guard: a real rig association always wins
    // over the pool-derived bucket.
    expect(agentProject({ name: 'x', rig: '/home/ds/gascity', pool: 'dog' } as never)).toEqual({
      key: 'gascity',
      label: 'gascity',
    });
  });

  it('leaves a non-maintenance pool as a rig synonym (only the named builtin pools are special-cased)', () => {
    // Guards against over-broad matching: `research` is a legitimate
    // pool-as-rig and must be unaffected by the maintenance carve-out.
    expect(agentProject({ name: 'a1', pool: 'research' } as never)).toEqual({
      key: 'research',
      label: 'research',
    });
  });
});

describe('isPerRigDispatcherAgent', () => {
  it('matches a rig-scoped agent whose alias ends in /control-dispatcher', () => {
    expect(
      isPerRigDispatcherAgent({
        name: 'thriva/control-dispatcher',
        rig: '/home/ds/thriva',
      } as never),
    ).toBe(true);
  });
  it('is false for the bare cross-rig control-dispatcher (no rig)', () => {
    expect(isPerRigDispatcherAgent({ name: 'control-dispatcher' } as never)).toBe(false);
  });
  it('is false for any agent without a rig', () => {
    expect(isPerRigDispatcherAgent({ name: 'thriva/control-dispatcher', rig: '' } as never)).toBe(
      false,
    );
  });
  it('is false for a rig-scoped agent without the dispatcher suffix', () => {
    expect(
      isPerRigDispatcherAgent({ name: 'thriva/architect', rig: '/home/ds/thriva' } as never),
    ).toBe(false);
  });
});

// H3 follow-up: ORCHESTRATION_AGENT_NAMES and ORCHESTRATION_TEMPLATES are
// two separate sets in projectOf.ts (session-shape keys on `template`,
// agent-shape keys on `name`). They must stay in sync — if a new
// orchestration role is added to one and forgotten on the other, sessions
// and agents for that role end up in different buckets and the operator
// sees a half-empty group. Lock the invariant via a behavioral test that
// asserts every "orchestration" name produces the Orchestration bucket
// on BOTH `agentProject` and `isOrchestrationSession`.
describe('orchestration name sets stay in sync between sessions and agents', () => {
  const orchestrationIdentifiers = ['mayor', 'control-dispatcher', 'oversight-rig.chief-of-staff'];

  it.each(orchestrationIdentifiers)(
    '"%s" is classified as orchestration on both the session AND agent shapes',
    (id) => {
      // Session-shape: template field carries the identifier.
      expect(isOrchestrationSession({ template: id } as never)).toBe(true);
      // Agent-shape: name field carries the identifier.
      expect(isOrchestrationAgent({ name: id } as never)).toBe(true);
    },
  );
});

describe('isAgentOutsideRig', () => {
  it('is true for cross-rig Orchestration agents (mayor, control-dispatcher, chief-of-staff)', () => {
    expect(isAgentOutsideRig({ name: 'mayor' } as never)).toBe(true);
    expect(isAgentOutsideRig({ name: 'control-dispatcher' } as never)).toBe(true);
    expect(isAgentOutsideRig({ name: 'oversight-rig.chief-of-staff' } as never)).toBe(true);
  });

  it('is true for the residual (no rig) bucket — no rig, no pool, not orchestration', () => {
    expect(isAgentOutsideRig({ name: 'a1' } as never)).toBe(true);
    expect(isAgentOutsideRig({ name: 'a1', rig: '' } as never)).toBe(true);
  });

  it('is false for an agent in a real rig', () => {
    expect(isAgentOutsideRig({ name: 'a1', rig: '/home/ds/gascity' } as never)).toBe(false);
  });

  it('is false for a pool-scoped agent (a pool stands in as the rig label)', () => {
    expect(isAgentOutsideRig({ name: 'a1', pool: 'research' } as never)).toBe(false);
  });

  it('is true for a maintenance-builtin pool agent (dog) — a maintenance pool is not a rig', () => {
    // gascity-dashboard-jj2z: the Maintenance bucket is outside-rig, so the Rig
    // column shows the clean alias (no `dog ·` prefix) and the agent is excluded
    // from the rig filter dropdown.
    expect(isAgentOutsideRig({ name: 'dog-1', pool: 'dog' } as never)).toBe(true);
  });
});

describe('agentProject -main canonicalization', () => {
  it('strips a -main worktree/build suffix to the base rig', () => {
    expect(agentProject({ name: 'x', rig: '/home/ds/gascity-main' } as never).label).toBe(
      'gascity',
    );
    expect(agentProject({ name: 'y', rig: '/home/ds/gascity-packs-main' } as never).label).toBe(
      'gascity-packs',
    );
  });
  it('leaves a normal rig path unchanged', () => {
    expect(agentProject({ name: 'z', rig: '/home/ds/gascity-packs' } as never).label).toBe(
      'gascity-packs',
    );
  });
});

describe('orchestrationLabel', () => {
  it('returns the active city name (operator thinks "the city", not "Orchestration")', () => {
    const prior = getActiveCity();
    try {
      setActiveCity('ds-research');
      expect(orchestrationLabel()).toBe('ds-research');
      // The KEY is unaffected — only the display label tracks the city.
      expect(agentProject({ name: 'mayor' } as never).key).toBe(ORCHESTRATION_PROJECT);
      expect(agentProject({ name: 'mayor' } as never).label).toBe('ds-research');
      // sessionProject mirrors the same label change for orchestration sessions.
      expect(sessionProject({ template: 'mayor' } as never)).toEqual({
        key: ORCHESTRATION_PROJECT,
        label: 'ds-research',
      });
    } finally {
      if (prior !== null) setActiveCity(prior);
    }
  });
});

describe('cleanWorkerName', () => {
  it('strips a trailing -gc-XXXXX live-session suffix to the role', () => {
    expect(cleanWorkerName('polecat-gc-335825')).toBe('polecat');
    expect(cleanWorkerName('scix-worker-gc-335812')).toBe('scix-worker');
    expect(cleanWorkerName('enterprisebench-worker-gc-335808')).toBe('enterprisebench-worker');
  });

  it('strips a leading filesystem path to the basename', () => {
    expect(cleanWorkerName('/home/ds/gas-city/city-infra-polecat')).toBe('city-infra-polecat');
  });

  it('strips both a path and a session suffix together', () => {
    expect(cleanWorkerName('/home/ds/gas-city/polecat-gc-335825')).toBe('polecat');
  });

  it('also strips td-/th-/4-letter-prefixed session handles', () => {
    expect(cleanWorkerName('worker-td-9abc')).toBe('worker');
    expect(cleanWorkerName('worker-fddc-12xy')).toBe('worker');
  });

  it('does NOT strip a hyphenated role whose penultimate segment is 4 letters but carries no session digit', () => {
    // `-scix-worker` matches the `<4-letter-prefix>-<body>` shape, but the body
    // (`worker`) has no digit, so it is a role suffix, not a live-session handle
    // (live ids always carry a numeric handle). Stripping it would truncate the
    // label and mis-classify the worker.
    expect(cleanWorkerName('city-scix-worker')).toBe('city-scix-worker');
    expect(cleanWorkerName('infra-scix-worker')).toBe('infra-scix-worker');
    // The genuine handle (digit body) on the same role still strips cleanly.
    expect(cleanWorkerName('city-scix-worker-gc-335812')).toBe('city-scix-worker');
  });

  it('leaves a clean name unchanged', () => {
    expect(cleanWorkerName('polecat-1')).toBe('polecat-1');
    expect(cleanWorkerName('mayor')).toBe('mayor');
  });

  it('trims surrounding whitespace', () => {
    expect(cleanWorkerName('  polecat-gc-335825  ')).toBe('polecat');
  });
});

describe('isWorkerSession', () => {
  it('counts active polecat / worker sessions', () => {
    expect(isWorkerSession(gcSession({ template: 'polecat', rig: 'gascity' }))).toBe(true);
    expect(isWorkerSession(gcSession({ template: 'scix-worker', rig: 'scix' }))).toBe(true);
    expect(isWorkerSession(gcSession({ template: 'worker-1', rig: 'geo' }))).toBe(true);
  });

  it('counts a dynamically-spawned slot named by its session handle', () => {
    expect(
      isWorkerSession(
        gcSession({
          template: 'pool',
          session_name: 'polecat-gc-335825',
          rig: 'gascity',
        }),
      ),
    ).toBe(true);
  });

  it("counts a worker reported in the 'running' state", () => {
    // isRunningAgent / isSessionStreamable / stateTone all honour 'running';
    // isWorkerSession must too, or a worker in that state is silently dropped
    // from the Workers-active count.
    expect(
      isWorkerSession(gcSession({ template: 'polecat', rig: 'gascity', state: 'running' })),
    ).toBe(true);
  });

  it('excludes a worker session that is not active', () => {
    expect(
      isWorkerSession(gcSession({ template: 'polecat', rig: 'gascity', state: 'asleep' })),
    ).toBe(false);
    expect(
      isWorkerSession(gcSession({ template: 'polecat', rig: 'gascity', state: 'closed' })),
    ).toBe(false);
  });

  it('excludes cross-rig orchestration (mayor, control-dispatcher, chief-of-staff)', () => {
    expect(isWorkerSession(gcSession({ template: 'mayor', rig: '' }))).toBe(false);
    expect(isWorkerSession(gcSession({ template: 'control-dispatcher', rig: '' }))).toBe(false);
    expect(isWorkerSession(gcSession({ template: 'oversight-rig.chief-of-staff', rig: '' }))).toBe(
      false,
    );
  });

  it('excludes per-rig dispatchers and project-leads', () => {
    expect(
      isWorkerSession(
        gcSession({ template: 'worker', rig: 'gascity', alias: 'gascity/control-dispatcher' }),
      ),
    ).toBe(false);
    expect(isWorkerSession(gcSession({ template: 'gascity.project-lead', rig: 'gascity' }))).toBe(
      false,
    );
  });

  it('excludes a non-worker rig session (no worker/pool role)', () => {
    expect(isWorkerSession(gcSession({ template: 'reviewer', rig: 'gascity' }))).toBe(false);
  });
});
