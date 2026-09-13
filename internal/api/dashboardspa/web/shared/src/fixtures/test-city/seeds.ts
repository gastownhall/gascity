// Declarative seed data for the static test-city supervisor fixture: the rigs,
// beads (incl. graph.v2 formula-run groups), agents, sessions, mail, and run
// records, expressed as plain spec records. `data.ts` converts these into wire
// shapes. Kept separate so the data table and the assembly logic each stay
// focused and small.

// The dashboard's operator mailbox filters on the operator WIRE alias, so mail
// to/from the operator must use it (not a display name like "mayor") to land in
// the default Inbox/Sent views. This fixture simulates a gc city, so it uses
// gc's wire convention (`human`) directly — fixture data is not production
// source and is exempt from the runtime-config identity rule.
const OP = 'human';

export type BeadStatus = 'open' | 'in_progress' | 'blocked' | 'closed';
export type IssueType = 'feature' | 'bug' | 'task' | 'epic' | 'chore' | 'decision';

/**
 * Declarative bead seed. `agedH`/`updatedH` are hours-ago offsets resolved
 * against `nowMs` at build time; `dependsOn` becomes typed `Dep[]`.
 */
export interface BeadSpec {
  id: string;
  title: string;
  status: BeadStatus;
  issue_type: IssueType;
  agedH: number;
  updatedH?: number;
  assignee?: string;
  priority?: number;
  labels?: string[];
  parent?: string;
  description?: string;
  dependsOn?: string[];
  ephemeral?: boolean;
  metadata?: Record<string, string>;
}

// ~38 beads: two epics with children, a blocked cluster wired by deps, a spread
// of bug/task/feature/chore/decision work across open/in_progress/blocked/
// closed, plus a pair of ephemeral convoy beads.
export const BEAD_SPECS: readonly BeadSpec[] = [
  // --- Epic: checkout (open) + children -----------------------------------
  {
    id: 'tc-epic-checkout',
    title: 'EPIC: Guest checkout overhaul',
    status: 'in_progress',
    issue_type: 'epic',
    agedH: 30 * 24,
    updatedH: 3,
    assignee: 'web/builder',
    priority: 1,
    labels: ['rig:web', 'epic'],
    description: 'Umbrella for the guest-checkout rework across web + api.',
  },
  {
    id: 'tc-checkout-cart',
    title: 'Cart state survives refresh',
    status: 'closed',
    issue_type: 'feature',
    agedH: 20 * 24,
    updatedH: 9 * 24,
    assignee: 'web/builder',
    priority: 2,
    labels: ['rig:web'],
    parent: 'tc-epic-checkout',
  },
  {
    id: 'tc-checkout-address',
    title: 'Address autocomplete on shipping step',
    status: 'in_progress',
    issue_type: 'feature',
    agedH: 12 * 24,
    updatedH: 2,
    assignee: 'web/builder',
    priority: 2,
    labels: ['rig:web'],
    parent: 'tc-epic-checkout',
  },
  {
    id: 'tc-checkout-tax',
    title: 'Tax calc wrong for split shipments',
    status: 'blocked',
    issue_type: 'bug',
    agedH: 8 * 24,
    updatedH: 5,
    assignee: 'api/worker',
    priority: 0,
    labels: ['rig:api', 'regression'],
    parent: 'tc-epic-checkout',
    dependsOn: ['tc-tax-service'],
  },
  {
    id: 'tc-checkout-guest',
    title: 'Allow checkout without an account',
    status: 'open',
    issue_type: 'feature',
    agedH: 6 * 24,
    assignee: 'web/reviewer',
    priority: 1,
    labels: ['rig:web'],
    parent: 'tc-epic-checkout',
  },
  {
    id: 'tc-checkout-analytics',
    title: 'Funnel analytics for the new flow',
    status: 'open',
    issue_type: 'chore',
    agedH: 5 * 24,
    assignee: 'data/etl',
    priority: 3,
    labels: ['rig:data'],
    parent: 'tc-epic-checkout',
  },

  // --- Epic: billing (in_progress) + children -----------------------------
  {
    id: 'tc-epic-billing',
    title: 'EPIC: Usage-based billing',
    status: 'in_progress',
    issue_type: 'epic',
    agedH: 25 * 24,
    updatedH: 6,
    assignee: 'api/worker',
    priority: 1,
    labels: ['rig:api', 'epic'],
    description: 'Metered billing: meter ingestion, rating, invoices.',
  },
  {
    id: 'tc-tax-service',
    title: 'Stand up tax-rating service',
    status: 'in_progress',
    issue_type: 'task',
    agedH: 10 * 24,
    updatedH: 4,
    assignee: 'api/worker',
    priority: 0,
    labels: ['rig:api'],
    parent: 'tc-epic-billing',
  },
  {
    id: 'tc-billing-meter',
    title: 'Meter ingestion pipeline',
    status: 'blocked',
    issue_type: 'task',
    agedH: 9 * 24,
    updatedH: 7,
    assignee: 'data/etl',
    priority: 1,
    labels: ['rig:data'],
    parent: 'tc-epic-billing',
    dependsOn: ['tc-tax-service', 'tc-data-warehouse'],
  },
  {
    id: 'tc-billing-invoice',
    title: 'Generate monthly invoices',
    status: 'open',
    issue_type: 'feature',
    agedH: 7 * 24,
    assignee: 'api/tester',
    priority: 2,
    labels: ['rig:api'],
    parent: 'tc-epic-billing',
  },
  {
    id: 'tc-billing-decision',
    title: 'Decision: proration policy for mid-cycle plan changes',
    status: 'open',
    issue_type: 'decision',
    agedH: 4 * 24,
    assignee: 'mayor',
    priority: 1,
    labels: ['rig:api', 'needs-decision'],
    parent: 'tc-epic-billing',
  },

  // --- Blocked cluster (data warehouse) -----------------------------------
  {
    id: 'tc-data-warehouse',
    title: 'Provision analytics warehouse',
    status: 'in_progress',
    issue_type: 'task',
    agedH: 14 * 24,
    updatedH: 11,
    assignee: 'ops/deployer',
    priority: 1,
    labels: ['rig:ops'],
  },
  {
    id: 'tc-data-dbt',
    title: 'dbt models for revenue rollups',
    status: 'blocked',
    issue_type: 'task',
    agedH: 11 * 24,
    updatedH: 8,
    assignee: 'data/etl',
    priority: 2,
    labels: ['rig:data'],
    dependsOn: ['tc-data-warehouse'],
  },
  {
    id: 'tc-data-backfill',
    title: 'Backfill 18 months of events',
    status: 'blocked',
    issue_type: 'chore',
    agedH: 11 * 24,
    updatedH: 8,
    assignee: 'data/etl',
    priority: 3,
    labels: ['rig:data'],
    dependsOn: ['tc-data-dbt'],
  },

  // --- Standalone bugs ----------------------------------------------------
  {
    id: 'tc-bug-login-loop',
    title: 'SSO login redirect loop on Safari',
    status: 'in_progress',
    issue_type: 'bug',
    agedH: 3 * 24,
    updatedH: 1,
    assignee: 'web/builder',
    priority: 0,
    labels: ['rig:web', 'regression', 'p0'],
  },
  {
    id: 'tc-bug-rate-limit',
    title: 'API rate-limiter off-by-one at window edge',
    status: 'open',
    issue_type: 'bug',
    agedH: 2 * 24,
    assignee: 'api/tester',
    priority: 1,
    labels: ['rig:api'],
  },
  {
    id: 'tc-bug-mail-dupe',
    title: 'Duplicate notification emails on retry',
    status: 'closed',
    issue_type: 'bug',
    agedH: 16 * 24,
    updatedH: 12 * 24,
    assignee: 'ops/oncall',
    priority: 2,
    labels: ['rig:ops'],
  },
  {
    id: 'tc-bug-stale-cache',
    title: 'Stale price cache after catalog publish',
    status: 'blocked',
    issue_type: 'bug',
    agedH: 4 * 24,
    updatedH: 6,
    assignee: 'web/reviewer',
    priority: 1,
    labels: ['rig:web'],
    dependsOn: ['tc-checkout-tax'],
  },

  // --- Tasks / chores -----------------------------------------------------
  {
    id: 'tc-task-upgrade-node',
    title: 'Upgrade backend to Node 22',
    status: 'open',
    issue_type: 'chore',
    agedH: 5 * 24,
    assignee: 'ops/deployer',
    priority: 3,
    labels: ['rig:ops', 'maintenance'],
  },
  {
    id: 'tc-task-flaky-e2e',
    title: 'Quarantine flaky checkout E2E',
    status: 'in_progress',
    issue_type: 'chore',
    agedH: 2 * 24,
    updatedH: 5,
    assignee: 'api/tester',
    priority: 2,
    labels: ['rig:api', 'tests'],
  },
  {
    id: 'tc-task-rotate-keys',
    title: 'Rotate provider API keys',
    status: 'closed',
    issue_type: 'chore',
    agedH: 30 * 24,
    updatedH: 22 * 24,
    assignee: 'ops/oncall',
    priority: 1,
    labels: ['rig:ops', 'security'],
  },
  {
    id: 'tc-task-design-tokens',
    title: 'Extract design tokens from DESIGN.md',
    status: 'open',
    issue_type: 'task',
    agedH: 6 * 24,
    assignee: 'web/reviewer',
    priority: 3,
    labels: ['rig:web', 'design'],
  },

  // --- Docs ----------------------------------------------------------------
  {
    id: 'tc-docs-api-ref',
    title: 'Publish billing API reference',
    status: 'in_progress',
    issue_type: 'task',
    agedH: 4 * 24,
    updatedH: 10,
    assignee: 'docs/writer',
    priority: 2,
    labels: ['rig:docs'],
  },
  {
    id: 'tc-docs-runbook',
    title: 'On-call runbook for billing incidents',
    status: 'open',
    issue_type: 'task',
    agedH: 3 * 24,
    assignee: 'docs/writer',
    priority: 3,
    labels: ['rig:docs'],
  },
  {
    id: 'tc-docs-changelog',
    title: 'Backfill changelog for Q1 releases',
    status: 'closed',
    issue_type: 'chore',
    agedH: 40 * 24,
    updatedH: 28 * 24,
    assignee: 'docs/writer',
    priority: 3,
    labels: ['rig:docs'],
  },

  // --- Features in flight --------------------------------------------------
  {
    id: 'tc-feat-search',
    title: 'Typeahead product search',
    status: 'in_progress',
    issue_type: 'feature',
    agedH: 8 * 24,
    updatedH: 3,
    assignee: 'web/builder',
    priority: 1,
    labels: ['rig:web'],
  },
  {
    id: 'tc-feat-webhooks',
    title: 'Outbound webhooks for order events',
    status: 'open',
    issue_type: 'feature',
    agedH: 6 * 24,
    assignee: 'api/worker',
    priority: 2,
    labels: ['rig:api'],
  },
  {
    id: 'tc-feat-darkmode',
    title: 'Dark mode for the storefront',
    status: 'open',
    issue_type: 'feature',
    agedH: 9 * 24,
    assignee: 'web/reviewer',
    priority: 3,
    labels: ['rig:web', 'design'],
  },
  {
    id: 'tc-feat-export',
    title: 'CSV export of invoices',
    status: 'closed',
    issue_type: 'feature',
    agedH: 18 * 24,
    updatedH: 14 * 24,
    assignee: 'api/tester',
    priority: 2,
    labels: ['rig:api'],
  },

  // --- Decisions -----------------------------------------------------------
  {
    id: 'tc-decision-cdn',
    title: 'Decision: CDN vendor for static assets',
    status: 'closed',
    issue_type: 'decision',
    agedH: 26 * 24,
    updatedH: 20 * 24,
    assignee: 'mayor',
    priority: 2,
    labels: ['rig:ops'],
  },
  {
    id: 'tc-decision-auth',
    title: 'Decision: roll our own sessions vs hosted auth',
    status: 'open',
    issue_type: 'decision',
    agedH: 5 * 24,
    assignee: 'mayor',
    priority: 1,
    labels: ['rig:web', 'needs-decision'],
  },

  // --- Ops / infra ---------------------------------------------------------
  {
    id: 'tc-ops-alerts',
    title: 'Wire Prometheus alerts for billing latency',
    status: 'in_progress',
    issue_type: 'task',
    agedH: 3 * 24,
    updatedH: 4,
    assignee: 'ops/oncall',
    priority: 1,
    labels: ['rig:ops', 'observability'],
  },
  {
    id: 'tc-ops-incident',
    title: 'Postmortem: 04-12 checkout outage',
    status: 'closed',
    issue_type: 'task',
    agedH: 50 * 24,
    updatedH: 44 * 24,
    assignee: 'ops/oncall',
    priority: 0,
    labels: ['rig:ops', 'incident'],
  },

  // --- Ephemeral convoy beads ---------------------------------------------
  {
    id: 'tc-convoy-input-1',
    title: 'input convoy for tc-feat-search',
    status: 'open',
    issue_type: 'task',
    agedH: 6,
    assignee: 'web/builder',
    priority: 1,
    labels: ['convoy'],
    ephemeral: true,
  },
  {
    id: 'tc-convoy-input-2',
    title: 'input convoy for tc-billing-invoice',
    status: 'closed',
    issue_type: 'task',
    agedH: 12,
    updatedH: 2,
    assignee: 'api/tester',
    priority: 1,
    labels: ['convoy'],
    ephemeral: true,
  },
];

/**
 * A formula run is a `graph.v2` workflow-root bead plus its step beads (linked
 * by `gc.root_bead_id`). The Runs tab only renders a bead group as a lane when
 * the group root carries `gc.formula_contract: graph.v2`, so a plain bead seed
 * yields zero run lanes — these groups are what make the Runs view light up.
 */
function workflowGroup(opts: {
  rootId: string;
  rig: string;
  title: string;
  assignee: string;
  startedH: number;
  updatedH: number;
  rootStatus: BeadStatus;
  steps: { suffix: string; title: string; status: BeadStatus; agedH: number; kind?: string }[];
}): BeadSpec[] {
  const rootMeta: Record<string, string> = {
    'gc.formula_contract': 'graph.v2',
    'gc.kind': 'run',
    'gc.root_store_ref': `rig:${opts.rig}`,
  };
  const root: BeadSpec = {
    id: opts.rootId,
    title: opts.title,
    status: opts.rootStatus,
    issue_type: 'task',
    agedH: opts.startedH,
    updatedH: opts.updatedH,
    assignee: opts.assignee,
    priority: 1,
    labels: [`rig:${opts.rig}`, 'run'],
    metadata: rootMeta,
  };
  const steps: BeadSpec[] = opts.steps.map((step) => {
    const meta: Record<string, string> = { 'gc.root_bead_id': opts.rootId };
    if (step.kind !== undefined) meta['gc.kind'] = step.kind;
    return {
      id: `${opts.rootId}-${step.suffix}`,
      title: step.title,
      status: step.status,
      issue_type: 'task',
      agedH: step.agedH,
      updatedH: Math.max(0, step.agedH - 1),
      assignee: opts.assignee,
      labels: [`rig:${opts.rig}`],
      parent: opts.rootId,
      metadata: meta,
    };
  });
  return [root, ...steps];
}

// Three formula runs: one in-flight, one blocked, one finished (historical).
export const WORKFLOW_GROUPS: readonly BeadSpec[] = [
  ...workflowGroup({
    rootId: 'tc-wf-checkout',
    rig: 'web',
    title: 'Formula run: guest-checkout address autocomplete',
    assignee: 'web/builder',
    startedH: 5,
    updatedH: 1,
    rootStatus: 'in_progress',
    steps: [
      { suffix: 'spec', title: 'draft spec', status: 'closed', agedH: 5, kind: 'spec' },
      { suffix: 'impl', title: 'implement step', status: 'in_progress', agedH: 4 },
    ],
  }),
  ...workflowGroup({
    rootId: 'tc-wf-tax',
    rig: 'api',
    title: 'Formula run: tax-rating service',
    assignee: 'api/worker',
    startedH: 6,
    updatedH: 5,
    rootStatus: 'in_progress',
    steps: [
      { suffix: 'spec', title: 'draft spec', status: 'closed', agedH: 6, kind: 'spec' },
      { suffix: 'impl', title: 'implement step', status: 'blocked', agedH: 5 },
    ],
  }),
  ...workflowGroup({
    rootId: 'tc-wf-cart',
    rig: 'web',
    title: 'Formula run: cart survives refresh',
    assignee: 'web/builder',
    startedH: 9 * 24,
    updatedH: 9 * 24,
    rootStatus: 'closed',
    steps: [
      { suffix: 'spec', title: 'draft spec', status: 'closed', agedH: 9 * 24 + 2, kind: 'spec' },
      { suffix: 'impl', title: 'implement step', status: 'closed', agedH: 9 * 24 + 1 },
    ],
  }),
];

/** Declarative agent seed; resolved against `nowMs` at build time. */
export interface AgentSpec {
  name: string;
  rig: string;
  state: string;
  available: boolean;
  running: boolean;
  suspended: boolean;
  provider: string;
  model: string;
  activeBead?: string;
  contextPct?: number;
  activity?: string;
  unavailableReason?: string;
  sessionName?: string;
  sessionAttached?: boolean;
  lastActivityMin?: number;
}

export const AGENT_SPECS: readonly AgentSpec[] = [
  {
    name: 'web/builder',
    rig: 'web',
    state: 'running',
    available: true,
    running: true,
    suspended: false,
    provider: 'claude',
    model: 'claude-opus-4-8',
    activeBead: 'tc-bug-login-loop',
    contextPct: 42,
    activity: 'editing frontend/src/routes/Checkout.tsx',
    sessionName: 'gc-web-1',
    sessionAttached: true,
    lastActivityMin: 1,
  },
  {
    name: 'web/reviewer',
    rig: 'web',
    state: 'waiting',
    available: true,
    running: false,
    suspended: false,
    provider: 'claude',
    model: 'claude-sonnet-4-6',
    activity: 'idle — awaiting review work',
    sessionName: 'gc-web-2',
    sessionAttached: false,
    lastActivityMin: 18,
  },
  {
    name: 'api/worker',
    rig: 'api',
    state: 'stuck',
    available: false,
    running: false,
    suspended: false,
    provider: 'claude',
    model: 'claude-opus-4-8',
    activeBead: 'tc-tax-service',
    contextPct: 88,
    activity: 'blocked on missing tax-rating credentials',
    unavailableReason: 'needs you: missing TAX_API_KEY',
    sessionName: 'gc-api-1',
    sessionAttached: true,
    lastActivityMin: 6,
  },
  {
    name: 'api/tester',
    rig: 'api',
    state: 'running',
    available: true,
    running: true,
    suspended: false,
    provider: 'codex',
    model: 'gpt-5',
    activeBead: 'tc-task-flaky-e2e',
    contextPct: 55,
    activity: 'running billing integration suite',
    sessionName: 'gc-api-2',
    sessionAttached: true,
    lastActivityMin: 2,
  },
  {
    name: 'data/etl',
    rig: 'data',
    state: 'suspended',
    available: false,
    running: false,
    suspended: true,
    provider: 'claude',
    model: 'claude-sonnet-4-6',
    activeBead: 'tc-billing-meter',
    activity: 'suspended — upstream warehouse not ready',
    unavailableReason: 'suspended pending tc-data-warehouse',
    lastActivityMin: 95,
  },
  {
    name: 'ops/deployer',
    rig: 'ops',
    state: 'detached',
    available: true,
    running: true,
    suspended: false,
    provider: 'claude',
    model: 'claude-opus-4-8',
    activeBead: 'tc-data-warehouse',
    contextPct: 33,
    activity: 'provisioning warehouse cluster',
    sessionName: 'gc-ops-1',
    sessionAttached: false,
    lastActivityMin: 9,
  },
  {
    name: 'ops/oncall',
    rig: 'ops',
    state: 'rate-limited',
    available: false,
    running: false,
    suspended: false,
    provider: 'claude',
    model: 'claude-opus-4-8',
    activity: 'paused — provider rate limit',
    unavailableReason: 'rate-limited until window resets',
    sessionName: 'gc-ops-2',
    sessionAttached: false,
    lastActivityMin: 14,
  },
  {
    name: 'docs/writer',
    rig: 'docs',
    state: 'failed',
    available: false,
    running: false,
    suspended: false,
    provider: 'gemini',
    model: 'gemini-2.5-pro',
    activeBead: 'tc-docs-api-ref',
    activity: 'crashed mid-generation',
    unavailableReason: 'session crashed (exit 1)',
    sessionName: 'gc-docs-1',
    sessionAttached: false,
    lastActivityMin: 40,
  },
];

/** Declarative mail seed; `agedMin` resolved against `nowMs`. */
export interface MailSpec {
  id: string;
  from: string;
  to: string;
  subject: string;
  body: string;
  read: boolean;
  agedMin: number;
  threadId?: string;
  rig?: string;
  priority?: number;
}

export const MAIL_SPECS: readonly MailSpec[] = [
  {
    id: 'tc-mail-1',
    from: 'api/worker',
    to: OP,
    subject: 'BLOCKED: tax-rating service missing credentials',
    body: 'tc-tax-service is blocked — TAX_API_KEY is not set in the api rig env.',
    read: false,
    agedMin: 6,
    threadId: 'tc-thread-tax',
    rig: 'api',
    priority: 0,
  },
  {
    id: 'tc-mail-2',
    from: OP,
    to: 'api/worker',
    subject: 'Re: BLOCKED: tax-rating service missing credentials',
    body: 'Acknowledged — rotating a key now, will land in the rig env within the hour.',
    read: true,
    agedMin: 4,
    threadId: 'tc-thread-tax',
    rig: 'api',
    priority: 0,
  },
  {
    id: 'tc-mail-3',
    from: 'web/builder',
    to: OP,
    subject: 'Checkout cart-refresh fix landed',
    body: 'tc-checkout-cart closed and merged. Moving to address autocomplete next.',
    read: true,
    agedMin: 3 * 60,
    rig: 'web',
    priority: 2,
  },
  {
    id: 'tc-mail-4',
    from: 'data/etl',
    to: OP,
    subject: 'Warehouse still not provisioned',
    body: 'tc-billing-meter and the dbt models are blocked until tc-data-warehouse is up.',
    read: false,
    agedMin: 95,
    threadId: 'tc-thread-warehouse',
    rig: 'data',
    priority: 1,
  },
  {
    id: 'tc-mail-5',
    from: 'ops/deployer',
    to: 'data/etl',
    subject: 'Re: Warehouse still not provisioned',
    body: 'Cluster is spinning up — ETA ~30 min. Will ping when ready.',
    read: false,
    agedMin: 60,
    threadId: 'tc-thread-warehouse',
    rig: 'ops',
    priority: 1,
  },
  {
    id: 'tc-mail-6',
    from: 'docs/writer',
    to: OP,
    subject: 'Session crashed during API-ref generation',
    body: 'gc-docs-1 exited 1 partway through tc-docs-api-ref. Restarting.',
    read: false,
    agedMin: 40,
    rig: 'docs',
    priority: 1,
  },
  {
    id: 'tc-mail-7',
    from: OP,
    to: 'all',
    subject: 'Proration policy decision needed',
    body: 'tc-billing-decision is open — need a call on mid-cycle proration before invoicing ships.',
    read: true,
    agedMin: 5 * 60,
    rig: 'api',
    priority: 1,
  },
  {
    id: 'tc-mail-8',
    from: 'api/tester',
    to: OP,
    subject: 'Flaky checkout E2E quarantined',
    body: 'Pulled the flaky spec into quarantine; tracking the real fix under tc-task-flaky-e2e.',
    read: true,
    agedMin: 5 * 60,
    rig: 'api',
    priority: 3,
  },
  {
    id: 'tc-mail-9',
    from: 'ops/oncall',
    to: OP,
    subject: 'Rate-limited on provider',
    body: 'Hitting provider rate limits — backing off until the window resets.',
    read: false,
    agedMin: 14,
    rig: 'ops',
    priority: 2,
  },
  {
    id: 'tc-mail-10',
    from: 'web/reviewer',
    to: 'web/builder',
    subject: 'Review queue is empty',
    body: 'No open review work — grabbing the design-tokens task in the meantime.',
    read: true,
    agedMin: 18,
    rig: 'web',
    priority: 3,
  },
];

/** Declarative formula-run seed (the Runs tab's monitor feed). */
export interface RunSpec {
  id: string;
  workflowId: string;
  rootBeadId: string;
  rig: string;
  title: string;
  target: string;
  type: string;
  status: string;
  startedH: number;
  updatedH: number;
}

// The monitor feed enriches run lanes with rig scope. Items must be `type:
// 'formula'` or the Runs view's feed discovery skips them. `rootBeadId` points
// at the graph.v2 workflow roots in WORKFLOW_GROUPS so the scopes line up.
export const RUN_SPECS: readonly RunSpec[] = [
  {
    id: 'tc-run-checkout',
    workflowId: 'tc-wf-checkout',
    rootBeadId: 'tc-wf-checkout',
    rig: 'web',
    title: 'Formula run: guest-checkout address autocomplete',
    target: 'tc-wf-checkout',
    type: 'formula',
    status: 'running',
    startedH: 5,
    updatedH: 1,
  },
  {
    id: 'tc-run-tax',
    workflowId: 'tc-wf-tax',
    rootBeadId: 'tc-wf-tax',
    rig: 'api',
    title: 'Formula run: tax-rating service',
    target: 'tc-wf-tax',
    type: 'formula',
    status: 'blocked',
    startedH: 6,
    updatedH: 5,
  },
  {
    id: 'tc-run-cart',
    workflowId: 'tc-wf-cart',
    rootBeadId: 'tc-wf-cart',
    rig: 'web',
    title: 'Formula run: cart survives refresh',
    target: 'tc-wf-cart',
    type: 'formula',
    status: 'done',
    startedH: 9 * 24,
    updatedH: 9 * 24,
  },
  {
    id: 'tc-run-docs',
    workflowId: 'tc-docs-api-ref',
    rootBeadId: 'tc-docs-api-ref',
    rig: 'docs',
    title: 'Formula run: publish billing API reference',
    target: 'tc-docs-api-ref',
    type: 'formula',
    status: 'failed',
    startedH: 10,
    updatedH: 9,
  },
];
