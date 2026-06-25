import { useMemo } from 'react';
import { type RunSummary, type SourceState } from 'gas-city-dashboard-shared';
import { api, formatApiError } from '../api/client';
import { getActiveCity } from '../api/cityBase';
import { type OperatorConfig } from '../contexts/OperatorConfigContext';
import { useCachedData } from '../hooks/useCachedData';
import { listAgentPendingInteractions } from '../supervisor/agentPending';
import { listSupervisorBeads } from '../supervisor/beadReads';
import { supervisorApi, supervisorApiForRequestBudget } from '../supervisor/client';
import { DEFAULT_MAIL_HISTORY_LIMIT, listSupervisorMail } from '../supervisor/mailReads';
import type { AttentionContributor } from './compose';
import {
  createAttentionContributors,
  GC_ESCALATION_LABEL,
  type ActivityAttentionFacts,
  type AgentsAttentionFacts,
  type AttentionContributorFacts,
  type BeadsAttentionFacts,
  type HealthAttentionFacts,
  type MailAttentionFacts,
  type RunsAttentionFacts,
} from './registry';

const ATTENTION_LIST_LIMIT = 1000;
const ACTIVITY_EVENT_FETCH_LIMIT = 100;
const ACTIVITY_EVENT_WINDOW = '24h';
const HEALTH_ATTENTION_SUPERVISOR_TIMEOUT_MS = 2_500;

export function useLiveAttentionContributors(
  operator: OperatorConfig,
  runsSource: SourceState<RunSummary> | undefined,
): readonly AttentionContributor[] {
  const cityName = getActiveCity();
  const cacheSuffix = cityName ?? 'no-city';
  // The decision label + operator wire alias come from /config
  // (gascity-dashboard-bhvn). Encode them into the relevant cache keys so a
  // pre-config fallback → real-config transition refetches with the resolved
  // identity (useCachedData only refetches on key change).
  const { decisionLabel, operatorWireAlias } = operator;
  // gascity-dashboard-2j8e.7: the Runs badge reads the SAME shared run-summary
  // subscription the /runs page renders (passed in as runsSource), not its own
  // fan-out — one fetch, by-construction parity, and SSE refresh reach the badge.
  const runs = useMemo(() => runsFactsFromSource(runsSource), [runsSource]);
  const agents = useCachedData<AgentsAttentionFacts>(`attention:agents:${cacheSuffix}`, () =>
    fetchAgentsAttention(cityName),
  );
  const beads = useCachedData<BeadsAttentionFacts>(
    `attention:beads:${cacheSuffix}:${decisionLabel}`,
    () => fetchBeadsAttention(cityName, decisionLabel),
  );
  const mail = useCachedData<MailAttentionFacts>(
    `attention:mail:${cacheSuffix}:${operatorWireAlias}`,
    () => fetchMailAttention(cityName, operator),
  );
  const activity = useCachedData<ActivityAttentionFacts>(`attention:activity:${cacheSuffix}`, () =>
    fetchActivityAttention(cityName),
  );
  const health = useCachedData<HealthAttentionFacts>(`attention:health:${cacheSuffix}`, () =>
    fetchHealthAttention(cityName),
  );

  return useMemo(
    () =>
      createAttentionContributors(
        compactFacts({
          activity: activity.data,
          agents: agents.data,
          beads: beads.data,
          health: health.data,
          mail: mail.data,
          runs,
        }),
      ),
    [activity.data, agents.data, beads.data, health.data, mail.data, runs],
  );
}

/**
 * Project the shared run-summary subscription's source onto the Runs badge
 * facts. The badge counts genuinely-blocked runs from `summary.blockedLanes` —
 * the same selectBlockedRuns the /runs page renders — so reading the page's exact
 * source object makes the badge and the page agree by construction, not merely by
 * shared selector (gascity-dashboard-2j8e.7). The source's own status flows
 * through as provenance and is carried onto `unavailable`-tier items so a
 * degraded read can be aged rather than rendered as current truth; `fetchedAt`
 * carries the exact read time for any age comparison.
 */
export function runsFactsFromSource(
  source: SourceState<RunSummary> | undefined,
): RunsAttentionFacts | undefined {
  if (source === undefined) return undefined;
  if (source.status === 'error') {
    return { error: source.error, provenance: 'error' };
  }
  return { summary: source.data, provenance: source.status, fetchedAt: source.fetchedAt };
}

async function fetchAgentsAttention(cityName: string | null): Promise<AgentsAttentionFacts> {
  if (cityName === null) return {};
  try {
    const list = await supervisorApi().listAgents(cityName);
    const facts: AgentsAttentionFacts = {
      items: list.items ?? [],
      partial: list.partial === true,
    };
    try {
      const sessions = await supervisorApi().listSessions(cityName);
      facts.pendingInteractions = await listAgentPendingInteractions(
        list.items ?? [],
        sessions.items ?? [],
      );
    } catch (err) {
      facts.pendingError = formatApiError(err, 'agent pending state unavailable');
    }
    return facts;
  } catch (err) {
    return { error: formatApiError(err, 'agent list unavailable') };
  }
}

async function fetchBeadsAttention(
  cityName: string | null,
  decisionLabel: string,
): Promise<BeadsAttentionFacts> {
  if (cityName === null) return { decisionLabel };
  // Three independent reads: the general bead list (capped) and two dedicated
  // label+status-filtered queues — the mayor-decision queue and the escalation
  // queue. Both queues bypass the general list's gc:-label filter and are always
  // complete. Settled separately so one failing does not blank the others — the
  // decision queue, the escalation queue, and generic bead alerts are distinct
  // signals (gascity-dashboard-2j8e.3).
  const [list, decisions, escalations] = await Promise.allSettled([
    listSupervisorBeads({ limit: ATTENTION_LIST_LIMIT }),
    listDecisionBeads(cityName, decisionLabel),
    listEscalationBeads(cityName),
  ]);
  const facts: BeadsAttentionFacts = { nowMs: Date.now(), decisionLabel };
  if (list.status === 'fulfilled') {
    facts.items = list.value.items;
    facts.partial = list.value.partial === true;
  } else {
    facts.error = formatApiError(list.reason, 'bead list unavailable');
  }
  if (decisions.status === 'fulfilled') {
    facts.decisions = decisions.value.items ?? [];
  } else {
    facts.decisionsError = formatApiError(decisions.reason, 'decision queue unavailable');
  }
  if (escalations.status === 'fulfilled') {
    facts.escalations = escalations.value.items ?? [];
  } else {
    facts.escalationsError = formatApiError(escalations.reason, 'escalation queue unavailable');
  }
  return facts;
}

async function listDecisionBeads(cityName: string, decisionLabel: string) {
  return supervisorApi().listBeads(cityName, {
    label: decisionLabel,
    status: 'open',
  });
}

async function listEscalationBeads(cityName: string) {
  return supervisorApi().listBeads(cityName, {
    label: GC_ESCALATION_LABEL,
    status: 'open',
  });
}

async function fetchMailAttention(
  cityName: string | null,
  operator: OperatorConfig,
): Promise<MailAttentionFacts> {
  if (cityName === null) return {};
  try {
    // The operator inbox (to:operator). selectOperatorActionableUnread then
    // folds the pool-worker firehose, so the badge counts only needs-you mail
    // (gascity-dashboard-2j8e.5).
    const list = await listSupervisorMail(
      'inbox',
      operator.operatorAlias,
      operator,
      DEFAULT_MAIL_HISTORY_LIMIT,
    );
    return {
      items: list.items ?? [],
      nowMs: Date.now(),
      partial: list.partial === true,
    };
  } catch (err) {
    return {
      error: formatApiError(err, 'mail list unavailable'),
    };
  }
}

async function fetchActivityAttention(cityName: string | null): Promise<ActivityAttentionFacts> {
  const [deploys, events] = await Promise.allSettled([
    api.listBuilds(),
    cityName === null
      ? Promise.resolve(null)
      : supervisorApi().listEvents(cityName, {
          limit: ACTIVITY_EVENT_FETCH_LIMIT,
          since: ACTIVITY_EVENT_WINDOW,
        }),
  ]);
  const facts: ActivityAttentionFacts = {};

  if (deploys.status === 'fulfilled') {
    facts.deploys = deploys.value;
  } else {
    facts.deploysError = formatApiError(deploys.reason, 'deploy activity unavailable');
  }

  if (events.status === 'fulfilled') {
    if (events.value !== null) {
      facts.events = events.value.items ?? [];
      facts.eventsPartial = events.value.partial === true;
      if (events.value.partial_errors !== null && events.value.partial_errors !== undefined) {
        facts.eventsDegraded = events.value.partial_errors.join('; ');
      }
    }
  } else {
    facts.eventsError = formatApiError(events.reason, 'event history unavailable');
  }

  return facts;
}

async function fetchHealthAttention(cityName: string | null): Promise<HealthAttentionFacts> {
  if (cityName === null) return {};
  const [system, supervisor, trend] = await Promise.allSettled([
    api.systemHealth(),
    supervisorApiForRequestBudget(HEALTH_ATTENTION_SUPERVISOR_TIMEOUT_MS).cityHealth(cityName),
    api.doltTrend(),
  ]);
  const facts: HealthAttentionFacts = {};
  const dashboardErrors: string[] = [];

  if (system.status === 'fulfilled') {
    facts.system = system.value;
  } else {
    dashboardErrors.push(formatApiError(system.reason, 'dashboard health unavailable'));
  }

  if (supervisor.status === 'fulfilled') {
    facts.supervisor = { status: 'available', data: supervisor.value };
  } else {
    facts.supervisor = {
      status: 'unavailable',
      error: formatApiError(supervisor.reason, 'supervisor health unavailable'),
    };
  }

  if (trend.status === 'fulfilled') {
    facts.trend = trend.value;
  } else {
    dashboardErrors.push(formatApiError(trend.reason, 'dolt-noms trend unavailable'));
  }

  if (dashboardErrors.length > 0) {
    facts.dashboardError = dashboardErrors.join('; ');
  }
  return facts;
}

function compactFacts(facts: {
  [K in keyof AttentionContributorFacts]: AttentionContributorFacts[K] | undefined;
}): AttentionContributorFacts {
  const out: AttentionContributorFacts = {};
  for (const [key, value] of Object.entries(facts)) {
    if (value !== undefined) {
      (out as Record<string, unknown>)[key] = value;
    }
  }
  return out;
}
