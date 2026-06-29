export const meta = {
  name: 'frontdoor-di-analysis',
  description: 'Classify session/order/nudge call-tree functions for the front-door DI refactor (read-only)',
  phases: [
    { title: 'Classify', detail: 'one read-only agent per call-tree file' },
  ],
}

// Read-only analysis. Each agent reads ONE cmd/gc file and classifies every
// function whose body touches the raw `store` for a SESSION/ORDER/NUDGE object
// op, so the main loop can drive a compiler-first signature refactor.

const FILES = args && args.length ? args : [
  'session_reconciler.go',
  'session_reconcile.go',
  'session_beads.go',
  'session_lifecycle_parallel.go',
  'session_sleep.go',
  'cmd_wait.go',
  'session_name_lookup.go',
  'session_circuit_breaker.go',
  'session_wake.go',
  'session_bead_cycle.go',
  'soft_reload.go',
  'cmd_stop.go',
  'cmd_session_wake.go',
  'cmd_session_pin.go',
  'cmd_session.go',
  'cmd_prime.go',
  'cmd_nudge.go',
  'cmd_handoff.go',
  'adoption_barrier.go',
  'order_dispatch.go',
  'cmd_order.go',
  'nudge_beads.go',
]

const SCHEMA = {
  type: 'object',
  properties: {
    file: { type: 'string' },
    functions: {
      type: 'array',
      items: { type: 'string' },
      description: 'one pipe-delimited record per function (see required format)',
    },
    rootEntries: {
      type: 'array',
      items: { type: 'string' },
      description: 'composition-root entry points in this file that OPEN or RESOLVE the store and could construct the front door once (pipe-delimited: name|startLine|howStoreObtained)',
    },
  },
  required: ['file', 'functions', 'rootEntries'],
}

const PROMPT = (file) => `You are analyzing ONE Go file for a dependency-injection refactor in the gascity repo.
Worktree: /data/projects/gascity/.claude/worktrees/object-front-doors
File: cmd/gc/${file}

BACKGROUND. The repo has typed object front doors that wrap a raw beads.Store:
  - session front door: \`sessionFrontDoor(store beads.Store) *session.InfoStore\` (also reached via setMetaBatch / closeBead / closeFailedCreateBead / session.* patch helpers)
  - order front door: \`orders.NewStore(beads.OrdersStore{Store: store}) *orders.Store\`
  - nudge front door: \`nudgeFrontDoor(store beads.NudgesStore) *nudgequeue.Store\` (also reached via ensureQueuedNudgeBead / findQueuedNudgeBead / markQueuedNudgeTerminal)
  - work-assignment facade (RAW-BY-DESIGN, stays on beads.Store/WorkStore): \`workAssignmentForStore(beads.WorkStore{Store: store})\`
Goal: every deep call-tree function that uses \`store\` ONLY for a session/order/nudge OBJECT op should take the typed front door instead of \`beads.Store\`, so it has no raw store in scope. Functions that ALSO need the raw store for WORK ops, by-id/federation resolution (resolveSessionID*, storeref, Get-by-id), graph (ApplyGraphPlan), or a documented raw-by-design read keep the raw store for that residual.

TASK. Read the WHOLE file. For EVERY top-level function (and notable closures) whose body references a beads.Store value (a param named store/s/cityStore/etc., a local, or a receiver field) AND performs at least one of:
  - a SESSION object op: sessionFrontDoor(...), setMetaBatch(...), closeBead/closeFailedCreateBead(...), store.Create of a Type=="session" bead, store.Get(sessionID) then reading .Status/.Metadata, any session.* MetadataPatch apply, resolveSessionIDAllowClosed-then-mutate, etc.
  - an ORDER object op: orders.NewStore(...), CreateRun/CreateRunClosed/SetOutcome/SetCursor/CloseRun/RecentRuns, raw order-tracking Create/List/Close with order labels.
  - a NUDGE object op: nudgeFrontDoor(...), ensureQueuedNudgeBead/findQueuedNudgeBead*/markQueuedNudgeTerminal, raw nudge bead Create/Close.
emit ONE pipe-delimited record string into \`functions\`:

name | startLine | exactSignature | objectOpsUsed(comma: SESSION/ORDER/NUDGE sub-ops) | otherRawStoreUses(comma from: NONE,WORK_LIST,WORK_UPDATE,WORK_READY,BYID_RESOLVE,GENERIC_GET,GRAPH,FEDERATION,RIG_STORES_MAP,OPENS_STORE) | verdict | storeParamName | calleesGivenStore(comma of in-file helper calls it passes the store to) | notes

verdict is EXACTLY one of:
  - SESSION_ONLY  : store used ONLY for session object ops -> convert the store param to \`sessions *session.InfoStore\`.
  - ORDER_ONLY    : store used ONLY for order object ops -> convert to \`*orders.Store\`.
  - NUDGE_ONLY    : store used ONLY for nudge object ops -> convert to \`*nudgequeue.Store\`.
  - MIXED         : object op(s) PLUS other raw uses -> needs the typed front door AND a residual (name the residual in notes: keep raw store for WORK/by-id/etc., or take workAssignment).
  - ROOT          : an entry point that OPENS/RESOLVES the store (opens via openStore*, receives cityPath and resolves, or is a cobra RunE/command handler) — the allowed composition root that should CONSTRUCT the front door once.
  - RAW_BY_DESIGN : a documented raw exception (full session status/metadata resync e.g. session_reconciler.go ~line 342; session-START workdir/opt reads ~3844/3889; WORK-assignment probes already on the workAssignment facade; cooldown/cursor order runtime helpers). Explain which in notes.

Also populate \`rootEntries\`: any function in THIS file that is a composition root (opens or resolves the store, or is a cobra command handler) where the front door could be constructed once — pipe-delimited: name|startLine|howStoreObtained.

RULES:
  - Be exhaustive: do not skip a function because it "looks trivial". Every function that touches store for an object op must appear.
  - Quote the EXACT current signature (copy it).
  - Do NOT edit anything. Read-only. Use grep/sed/Read freely.
  - If the file has ZERO such functions, return functions: [] and rootEntries: [] with file set.
  - Keep each record on ONE line (no embedded newlines).`

phase('Classify')
const results = await parallel(
  FILES.map((file) => () =>
    agent(PROMPT(file), { label: `classify:${file}`, phase: 'Classify', schema: SCHEMA })
  )
)

const clean = results.filter(Boolean)
return {
  filesAnalyzed: clean.length,
  filesRequested: FILES.length,
  perFile: clean,
}
