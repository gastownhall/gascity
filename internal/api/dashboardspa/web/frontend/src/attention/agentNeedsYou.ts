import type { AgentNeedsYouAction, AgentNeedsYouReason } from 'gas-city-dashboard-shared';
import type { StatusTone } from '../components/StatusBadge';

// gascity-dashboard-2j8e.4: UI copy for the agents "needs you" set. The reason
// enum lives in shared (the SSOT selector); the words, action guidance, and
// status tone are presentation, so they live at the frontend edge. The nav
// badge title and the /agents "Needs you" section both read these, so the
// vocabulary stays in one place (DRY). Every reason carries a word, so the
// signal survives the Greyscale Test (DESIGN.md §Status).

const REASON_LABEL: Record<AgentNeedsYouReason, string> = {
  'awaiting-input': 'awaiting input',
  errored: 'errored',
  'rate-limited': 'rate limited',
  stalled: 'stalled',
};

// Imperative next step shown beside the row, mirroring the Runs blocked
// "remedy" line. No em dashes (DESIGN.md §Don'ts).
const ACTION_LABEL: Record<AgentNeedsYouAction, string> = {
  respond: 'Respond to its prompt.',
  reset: 'Reset the agent.',
  nudge: 'Nudge it to resume.',
};

// A direct ask or a hard failure earns the maroon (stuck) mark; a throttle or
// a stall reads as caution (warn). StatusBadge pairs every tone with a glyph
// and the word above, so this never carries meaning by color alone.
const REASON_TONE: Record<AgentNeedsYouReason, StatusTone> = {
  'awaiting-input': 'stuck',
  errored: 'stuck',
  'rate-limited': 'warn',
  stalled: 'warn',
};

export function agentNeedsYouReasonLabel(reason: AgentNeedsYouReason): string {
  return REASON_LABEL[reason];
}

export function agentNeedsYouActionLabel(action: AgentNeedsYouAction): string {
  return ACTION_LABEL[action];
}

export function agentNeedsYouReasonTone(reason: AgentNeedsYouReason): StatusTone {
  return REASON_TONE[reason];
}
