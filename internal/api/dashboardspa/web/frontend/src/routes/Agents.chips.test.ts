import { describe, expect, it } from 'vitest';
import type { AgentResponse } from 'gas-city-dashboard-shared/gc-supervisor';
import {
  agentRowLabel,
  buildAgentSynopsis,
  isRunningAgent,
  isVisibleUnderRunning,
  stateTone,
} from './Agents';

// gascity-dashboard-fgzf: the Agents view was reverted from the flat
// sortable/filterable table (chips + sort + rig dropdown) to the older,
// simpler view — a single 'running' toggle over a plain list with the
// 'rig · agent' label restored. These tests pin the toggle predicate and
// the label format that drive that simpler view.

function mkAgent(state: string, overrides: Partial<AgentResponse> = {}): AgentResponse {
  return {
    name: `agent-${state}`,
    available: true,
    running: state === 'active' || state === 'running',
    suspended: false,
    state,
    ...overrides,
  };
}

describe('isRunningAgent', () => {
  it('treats active/running agents as running', () => {
    expect(isRunningAgent(mkAgent('active'))).toBe(true);
    expect(isRunningAgent(mkAgent('running'))).toBe(true);
  });

  it('treats idle/asleep/detached agents as not running by default', () => {
    expect(isRunningAgent(mkAgent('asleep'))).toBe(false);
    expect(isRunningAgent(mkAgent('idle'))).toBe(false);
    expect(isRunningAgent(mkAgent('detached'))).toBe(false);
  });

  it('counts a detached-but-live process (running flag set) as running', () => {
    // gc can report state='detached' (tmux disconnected) while the
    // underlying process is still alive. The 'running' toggle keys on the
    // running flag too, so the operator scanning for "what is alive right
    // now" does not lose a detached-but-running agent.
    expect(isRunningAgent(mkAgent('detached', { running: true }))).toBe(true);
  });

  it('never counts a suspended agent as running, even if its state reads alive', () => {
    expect(isRunningAgent(mkAgent('active', { suspended: true }))).toBe(false);
    expect(isRunningAgent(mkAgent('detached', { running: true, suspended: true }))).toBe(false);
  });
});

describe('isVisibleUnderRunning', () => {
  it('shows running agents regardless of attention severity', () => {
    expect(isVisibleUnderRunning(mkAgent('active'), null)).toBe(true);
    expect(isVisibleUnderRunning(mkAgent('running'), 'watch')).toBe(true);
  });

  it('excludes a suspended agent under the running filter even with a watch item', () => {
    // Regression: /home/ds/gas-city/city-infra-polecat was suspended=true,
    // running=false, state=suspended, yet showed under 'running'. The registry
    // raises a passive 'watch' (idle) item for suspended agents; keying the
    // carve-out on any-non-null severity kept it visible. Only 'attention'
    // should bypass the filter.
    const suspended = mkAgent('suspended', { suspended: true, running: false });
    expect(isVisibleUnderRunning(suspended, 'watch')).toBe(false);
    expect(isVisibleUnderRunning(suspended, null)).toBe(false);
  });

  it('still surfaces a non-running agent that has an urgent attention item', () => {
    // A stuck agent blocked on a pending interaction is usually NOT running but
    // must never be hidden by the 'running' default.
    expect(isVisibleUnderRunning(mkAgent('asleep'), 'attention')).toBe(true);
  });
});

describe('agentRowLabel', () => {
  it("formats in-rig agents as 'rig · agent'", () => {
    const agent = mkAgent('active', { name: 'polecat-1', rig: 'gascity-packs' });
    expect(agentRowLabel(agent)).toBe('gascity-packs · polecat-1');
  });

  it('folds the rig path to its basename in the label', () => {
    const agent = mkAgent('active', { name: 'worker', rig: '/home/ds/projects/geo' });
    expect(agentRowLabel(agent)).toBe('geo · worker');
  });

  it('shows just the alias for cross-rig orchestration agents (no rig prefix)', () => {
    const mayor = mkAgent('active', { name: 'mayor', rig: '' });
    expect(agentRowLabel(mayor)).toBe('mayor');
  });

  it('shows just the alias for agents with no rig association', () => {
    const orphan = mkAgent('asleep', { name: 'control-dispatcher', rig: '' });
    expect(agentRowLabel(orphan)).toBe('control-dispatcher');
  });
});

describe('stateTone', () => {
  it('classifies detached agents explicitly (not via default fallthrough)', () => {
    // Detached is paused-alive — same neutral palette as idle/asleep, but the
    // case is explicit so a reviewer sees the intent rather than a silent
    // default. See gascity-dashboard-x4k for context.
    expect(stateTone('detached')).toBe('neutral');
  });

  it('falls through to neutral for unknown states', () => {
    // AgentResponse.state is free `string` for forward-compat. Any state
    // gc emits that the dashboard hasn't seen yet must land on neutral
    // via the default branch — not crash, not lie with a tone we picked
    // at random.
    expect(stateTone('this-state-does-not-exist')).toBe('neutral');
  });
});

describe('buildAgentSynopsis', () => {
  it('reports detached agents as a distinct count, not bucketed under idle', () => {
    const rows: AgentResponse[] = [
      mkAgent('active'),
      mkAgent('asleep'),
      mkAgent('asleep'),
      mkAgent('detached'),
    ];
    const synopsis = buildAgentSynopsis(rows);
    expect(synopsis).toContain('1 active');
    expect(synopsis).toContain('2 idle');
    expect(synopsis).toContain('1 detached');
  });

  it('omits detached from the synopsis when there are no detached agents', () => {
    const rows: AgentResponse[] = [mkAgent('active'), mkAgent('asleep')];
    expect(buildAgentSynopsis(rows)).not.toContain('detached');
  });

  it('breaks suspended out as its own count', () => {
    const rows: AgentResponse[] = [mkAgent('active'), mkAgent('asleep', { suspended: true })];
    expect(buildAgentSynopsis(rows)).toContain('1 suspended');
  });

  it('returns the empty-state sentence when no rows', () => {
    expect(buildAgentSynopsis([])).toBe('No agents configured.');
  });
});
