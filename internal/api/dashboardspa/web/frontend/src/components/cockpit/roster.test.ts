import { describe, expect, it } from 'vitest';
import type {
  SessionResponse,
  UsageBody,
  UsageSessionRecent,
} from 'gas-city-dashboard-shared/gc-supervisor';
import {
  buildVuMeters,
  meterValue,
  sessionToRoster,
  uniqueStrings,
  usageToRoster,
  type RosterRow,
} from './roster';

function session(over: Partial<SessionResponse> = {}): SessionResponse {
  return {
    attached: true,
    created_at: '2026-07-10T20:00:00.000Z',
    id: 'gc-abc',
    provider: 'claude',
    running: true,
    rig: 'gascity',
    session_name: 'gascity/polecat',
    state: 'active',
    template: 'polecat',
    title: 'polecat',
    ...over,
  };
}

function usageSession(over: Partial<UsageSessionRecent> = {}): UsageSessionRecent {
  return {
    session: 'polecat',
    session_id: 'gc-abc',
    input_tokens: 0,
    output_tokens: 0,
    cache_read_tokens: 0,
    cache_creation_tokens: 0,
    cost_usd_estimate: 0,
    ...over,
  };
}

function usage(over: Partial<UsageBody> = {}): UsageBody {
  const totals = {
    input_tokens: 0,
    output_tokens: 0,
    cache_read_tokens: 0,
    cache_creation_tokens: 0,
    cost_usd_estimate: 0,
    compute_facts: 0,
    invocations: 0,
    unpriced: 0,
    wall_seconds: 0,
  };
  return {
    totals,
    today: totals,
    recent: totals,
    recent_window_secs: 120,
    recent_by_session: [],
    warnings: [],
    ...over,
  };
}

const row: RosterRow = { id: 'gc-abc', name: 'polecat', rig: 'gascity', keys: ['gc-abc', 'polecat'] };

describe('uniqueStrings', () => {
  it('keeps distinct non-empty values in first-seen order', () => {
    expect(uniqueStrings(['a', undefined, '', 'b', 'a'])).toEqual(['a', 'b']);
  });
});

describe('sessionToRoster', () => {
  it('splits rig/name and gathers candidate keys', () => {
    const r = sessionToRoster(session({ id: 'gc-1', session_name: 'gascity/polecat', alias: 'pc' }));
    expect(r).toMatchObject({ id: 'gc-1', rig: 'gascity', name: 'polecat' });
    expect(r.keys).toContain('gc-1');
    expect(r.keys).toContain('polecat');
    expect(r.keys).toContain('pc');
  });

  it('falls back to the session-name prefix for rig when none is set', () => {
    const noRig = session({ session_name: 'infra/worker' });
    delete noRig.rig;
    expect(sessionToRoster(noRig).rig).toBe('infra');
  });
});

describe('usageToRoster', () => {
  it('builds a rig-less row from a usage-recent record', () => {
    const r = usageToRoster(usageSession({ session: 'polecat', session_id: 'gc-9' }));
    expect(r).toMatchObject({ id: 'gc-9', rig: '', name: 'polecat' });
    expect(r.keys).toEqual(expect.arrayContaining(['gc-9', 'polecat']));
  });
});

describe('meterValue (cross-id-space join)', () => {
  it('prefers a live-window value keyed by name', () => {
    expect(meterValue(row, usage(), { polecat: 5000 })).toBe(5000);
  });

  it('prefers a live-window value keyed by session id', () => {
    expect(meterValue(row, usage(), { 'gc-abc': 7000 })).toBe(7000);
  });

  it('falls back to usage tokens matched by session id, normalized to per-minute', () => {
    const u = usage({
      recent_window_secs: 120,
      recent_by_session: [usageSession({ session: 'unrelated', session_id: 'gc-abc', input_tokens: 120 })],
    });
    expect(meterValue(row, u, {})).toBe((120 * 60) / 120); // 60 tok/min
  });

  it('falls back to usage tokens matched by cleaned session name', () => {
    const entry = usageSession({ session: 'polecat', output_tokens: 30 });
    delete entry.session_id;
    const u = usage({ recent_window_secs: 60, recent_by_session: [entry] });
    expect(meterValue(row, u, {})).toBe((30 * 60) / 60); // 30 tok/min
  });

  it('returns zero when neither a live sample nor a usage row matches', () => {
    expect(meterValue(row, usage({ recent_by_session: [] }), {})).toBe(0);
  });
});

describe('buildVuMeters', () => {
  it('emits running sessions sorted by rig then name, all linking to /agents', () => {
    const sessions = [
      session({ id: 's1', rig: 'alpha', session_name: 'alpha/mm' }),
      session({ id: 's2', rig: 'alpha', session_name: 'alpha/aa' }),
      session({ id: 's3', rig: 'bravo', session_name: 'bravo/zz' }),
      session({ id: 's4', rig: 'alpha', session_name: 'alpha/dead', running: false }),
    ];
    const meters = buildVuMeters(sessions, usage(), {});
    expect(meters.map((m) => m.name)).toEqual(['aa', 'mm', 'zz']);
    expect(meters.every((m) => m.href === '/agents')).toBe(true);
  });

  it('caps the bank at eight meters', () => {
    const sessions = Array.from({ length: 12 }, (_, i) =>
      session({ id: `s${i}`, rig: 'r', session_name: `r/w${String(i).padStart(2, '0')}` }),
    );
    expect(buildVuMeters(sessions, usage(), {})).toHaveLength(8);
  });

  it('falls back to the usage roster when the sessions read is unavailable', () => {
    const u = usage({
      recent_by_session: [usageSession({ session: 'gizmo', session_id: 'gc-z', input_tokens: 60 })],
      recent_window_secs: 60,
    });
    const meters = buildVuMeters(null, u, {});
    expect(meters).toHaveLength(1);
    expect(meters[0]).toMatchObject({ id: 'gc-z', name: 'gizmo', value: 60 });
  });
});
