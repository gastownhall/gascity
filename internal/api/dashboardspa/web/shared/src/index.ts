// Package root barrel for dashboard-owned /api DTOs and shared helpers.
// Keep runtime helper exports as value exports; type-only domain leaves use
// `export type *` so importing the package root does not pull in dead JS.

export type * from './snapshot/types.js';
export { resolveSessionForTarget, matchesSessionTarget, lastSegment } from './session-resolve.js';
export * from './run-detail.js';
export type * from './run-snapshot.js';
export * from './run-scope.js';
export * from './strip-non-printable.js';
export * from './session-id.js';
export * from './work-in-flight.js';
export type * from './viewing-as.js';
export * from './agents/needsYou.js';
// The run-fold/graph-layout pipeline moved to Go (internal/runproj); the SPA is
// a pure renderer of the RunSummary / FormulaRunDetail DTOs. Only these run
// presentation helpers stay client-side: the blocked-runs selector, the
// needs-operator accessor, and the active-lane window size.
export * from './runs/blocked.js';
export * from './runs/health.js';
export * from './runs/summary.js';
export * from './bead-id.js';
export * from './links.js';
export * from './links/build-link-view.js';
export * from './links/instrumentation.js';
export * from './links/node-ref.js';
export * from './links/relation-index.js';
export * from './city.js';
export * from './operator.js';
export * from './operator-mail.js';
export * from './alert.js';
export * from './pending.js';
export * from './structured-transcript.js';
export * from './structured-render.js';
export * from './context-window.js';
export type * from './lists.js';
export type * from './transcript.js';
export type * from './dashboard-beads.js';
export type * from './activity.js';
export type * from './dashboard-health.js';
export type * from './rig-store-health.js';
export type * from './supervisor-status.js';
export type * from './api-error.js';
export type * from './views.js';
export type * from './dashboard-sessions.js';
