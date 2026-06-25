import { describe, it, expect } from 'vitest';
import type { DashboardSession } from 'gas-city-dashboard-shared';
import { sessionSlug } from './sessionSlug';

// gascity-dashboard-6bv7.1 — slug resolution for AgentDetail routing.
// session_name is required per OpenAPI (F10), but the wire schema uses
// z.string() (not z.string().min(1)), so '' is still possible. The
// fallback chain uses `||` so an empty session_name falls through to
// alias/id rather than producing an unroutable /agents/ URL.

function session(overrides: Partial<DashboardSession> & { id: string }): DashboardSession {
  return {
    template: 't',
    session_name: 'default-name',
    title: 't',
    state: 'active',
    created_at: '2026-05-24T00:00:00Z',
    attached: false,
    running: true,
    provider: 'claude',
    ...overrides,
  } as DashboardSession;
}

describe('sessionSlug', () => {
  it('returns session_name when present and non-empty', () => {
    const s = session({
      id: 'gc-1',
      session_name: 'my-session',
      alias: 'my-alias',
    });
    expect(sessionSlug(s)).toBe('my-session');
  });

  it('falls back to alias when session_name is empty string', () => {
    const s = session({
      id: 'gc-1',
      session_name: '',
      alias: 'my-alias',
    });
    expect(sessionSlug(s)).toBe('my-alias');
  });

  it('falls back to id when session_name and alias are both empty/absent', () => {
    const s = session({
      id: 'gc-1',
      session_name: '',
    });
    expect(sessionSlug(s)).toBe('gc-1');
  });
});
