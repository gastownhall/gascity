import type { AgentResponse } from '../generated/gc-supervisor-client/index.js';

// gascity-dashboard-2j8e.4: the agents "needs you" selector. The Agents nav
// badge and the /agents page both read this one projection, so the badge count
// and the on-page "Needs you" count read the same selector and cannot disagree.
// It mirrors selectBlockedRuns (gascity-dashboard-2j8e.2): a pure projection of
// the live roster into operator-actionable rows, with NO clock and NO age
// threshold, so a borderline read can never flap the count between the nav and
// the page.
//
// "Needs you" is the operator-blocking set ONLY: an agent awaiting an input
// decision, exited in a failure state, throttled by a provider limit, or
// claiming to run with no live session backing it. Actively-running, idle,
// asleep, suspended, and merely-unavailable agents are ambient roster state
// (surfaced in the page synopsis), never a badge number.

/** Why an agent needs the operator. Greyscale-safe words, never color-only. */
export type AgentNeedsYouReason = 'awaiting-input' | 'errored' | 'rate-limited' | 'stalled';

/** The single next operator step for a needs-you reason. */
export type AgentNeedsYouAction = 'respond' | 'reset' | 'nudge';

export interface AgentNeedsYou {
  /** Agent name: stable row key and attention identity (`agents:<name>:...`). */
  name: string;
  reason: AgentNeedsYouReason;
  /** Structural why-phrase derived from the agent's own facts (ZFC). */
  detail: string;
  /** The operator's next step to clear it. */
  action: AgentNeedsYouAction;
}

/** An agent has a pending interaction awaiting an operator decision. */
export interface AgentPendingSignal {
  readonly agentName: string;
  /** Prompt the supervisor carried with the interaction, when present. */
  readonly prompt?: string;
}

// Free-form supervisor `state` strings (no enum upstream), matched
// case-insensitively. Aligned with StatusBadge.stateTone and Agents synopsis
// bucketing so the three readers classify a state the same way.
const FAILURE_STATES: ReadonlySet<string> = new Set(['failed', 'errored', 'stuck', 'crashed']);
const RATE_LIMITED_STATES: ReadonlySet<string> = new Set([
  'rate-limited',
  'rate_limited',
  'waiting',
]);

const ACTION_BY_REASON: Record<AgentNeedsYouReason, AgentNeedsYouAction> = {
  'awaiting-input': 'respond',
  errored: 'reset',
  'rate-limited': 'nudge',
  stalled: 'nudge',
};

/** Project the live roster into the agents that need the operator. */
export function selectAgentsNeedingYou(
  agents: readonly AgentResponse[],
  pending: readonly AgentPendingSignal[],
): AgentNeedsYou[] {
  const promptByAgent = new Map<string, string | undefined>();
  for (const signal of pending) promptByAgent.set(signal.agentName, signal.prompt);

  const rows: AgentNeedsYou[] = [];
  for (const agent of agents) {
    const hasPending = promptByAgent.has(agent.name);
    const reason = needsYouReason(agent, hasPending);
    if (reason === null) continue;
    rows.push({
      name: agent.name,
      reason,
      detail: needsYouDetail(agent, reason, promptByAgent.get(agent.name)),
      action: ACTION_BY_REASON[reason],
    });
  }
  return rows;
}

/** First match wins: an awaiting-input ask outranks a failure/throttle/stall. */
function needsYouReason(agent: AgentResponse, hasPending: boolean): AgentNeedsYouReason | null {
  if (hasPending) return 'awaiting-input';
  const state = agent.state.toLowerCase();
  if (FAILURE_STATES.has(state)) return 'errored';
  if (RATE_LIMITED_STATES.has(state)) return 'rate-limited';
  if (isStalled(agent, state)) return 'stalled';
  return null;
}

/** Detached, or claiming to run with no live session backing the claim. */
function isStalled(agent: AgentResponse, state: string): boolean {
  if (state === 'detached') return true;
  return agent.running && agent.session === undefined;
}

function needsYouDetail(
  agent: AgentResponse,
  reason: AgentNeedsYouReason,
  prompt: string | undefined,
): string {
  switch (reason) {
    case 'awaiting-input':
      return promptLine(prompt);
    case 'errored':
      return `Exited ${agent.state}.`;
    case 'rate-limited':
      return 'Throttled by a provider limit.';
    case 'stalled':
      return agent.state.toLowerCase() === 'detached'
        ? 'Detached from its session.'
        : 'Running with no live session.';
  }
}

/** First non-empty line of the prompt, for a one-line row detail. */
function promptLine(prompt: string | undefined): string {
  if (prompt === undefined) return 'Awaiting your decision.';
  const firstLine = prompt.split('\n', 1)[0]?.trim() ?? '';
  return firstLine.length > 0 ? firstLine : 'Awaiting your decision.';
}
