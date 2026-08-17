// Dashboard-owned session projection used by pure selectors and view-model
// helpers. Supervisor `SessionResponse` values are normalized into this shape
// at frontend edges; shared modules do not import or own the generated
// supervisor type.
//
// 9yj.1.1: extracted from `shared/src/index.ts` to break a type-only cycle
// (`views.ts` imports from `index.ts`, `index.ts` re-exports `views.ts`).
// TypeScript and bundlers tolerate type-only cycles, but the architecture
// is fragile — every new export added to the barrel risks a new edge.
// This file holds timestamp/session primitives views.ts depends on; index.ts
// re-exports them so external consumers (`gas-city-dashboard-shared`) see
// no API change.
//
// This file MUST NOT import from `./index.js`. Doing so would re-introduce
// the cycle this file exists to prevent. Keep it as a leaf node in the
// module graph.

import type { CountedList } from './lists.js';

export type IsoTimestamp = string;
export type SessionId = string;

export type DashboardSessionState =
  | 'creating'
  | 'active'
  | 'asleep'
  | 'detached'
  | 'failed'
  | 'closed'
  | string;

export interface DashboardSession {
  id: SessionId;
  template: string;
  /** Supervisor's tmux/screen session name on disk. */
  session_name: string;
  title: string;
  alias?: string;
  state: DashboardSessionState;
  /** Set when state transition has a structured reason (e.g. "city-stop"). */
  reason?: string;
  /** Human-readable display name from the provider (e.g. "Claude Code"). */
  display_name?: string;
  created_at: IsoTimestamp;
  /** Last time the session emitted activity; only set after first activity. */
  last_active?: IsoTimestamp;
  /** Whether a human is currently attached to the tmux session. */
  attached: boolean;
  rig?: string;
  pool?: string;
  agent_kind?: 'pool' | 'role' | string;
  /** Process-running state independent of session.state. */
  running: boolean;
  model?: string;
  context_pct?: number;
  context_window?: number;
  /** Coarse activity hint: 'idle' | 'thinking' | 'tool_use' | ... */
  activity?: string;
  /** Session provider (e.g. 'codex', 'claude', 'gemini'). */
  provider: string;
}

export type DashboardSessionList = CountedList<DashboardSession>;
