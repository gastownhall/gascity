// Expected strings for the Playwright render smoke, mirroring the shared corpus
// seeded by test/dashport/corpus (the Go loader) into
// test/dashport/testdata/dashport. This is the Layer B copy of the corpus
// ids/values; the Go side asserts against test/dashport/corpus's exported
// constants directly.
//
// There is NO automated parity check between this file and corpus.go —
// alignment is maintained MANUALLY. When you change a value below, change the
// matching exported constant in test/dashport/corpus/corpus.go (and vice
// versa), or the browser will assert against content the seeded server no
// longer serves. The constant mapping is:
//
//   CITY_NAME             <-> corpus.CityName
//   RIG_NAME              <-> corpus.RigName
//   ANCHOR_RUN_ID         <-> corpus.AnchorRunID
//   ANCHOR_FORMULA        <-> corpus.AnchorFormula
//   COMPLETED_RUN_ID      <-> corpus.CompletedRunID
//   COMPLETED_FORMULA     <-> corpus.CompletedFormula
//   COMPLETED_STEP_APPROVE <-> corpus.CompletedStepApproveID
//   WORK_BEAD_ID          <-> corpus.WorkBeadID
//   WORK_BEAD_TITLE       <-> corpus.WorkBeadTitle
//   MAIL_SUBJECT          <-> corpus.MailSubject
//   AGENT_NAME            <-> corpus.AgentName

export const CITY_NAME = 'dashport-city';
export const RIG_NAME = 'demo';

/** The seeded run root's bead id and workflow id. */
export const ANCHOR_RUN_ID = 'run-anchor';

/** The seeded run's formula name — the run-detail title and the runs-list label. */
export const ANCHOR_FORMULA = 'mol-adopt-pr-v2';

/**
 * The SECOND seeded run — a fully closed molecule (root + both steps closed,
 * capped by molecule.resolved). It projects as a terminal "completed" run: a
 * historical lane in the runs list, a phase-`complete` lane label, a terminal
 * run detail, close-edge rows in the activity feed, and closed rows in the beads
 * view. It exercises the close-side data the happy-path ANCHOR run never reaches.
 */
export const COMPLETED_RUN_ID = 'run-done';

/**
 * The completed run's formula name — its run-detail h1 title and its historical
 * run-list lane title. Deliberately DISTINCT from ANCHOR_FORMULA so the open and
 * completed runs are individually assertable.
 */
export const COMPLETED_FORMULA = 'mol-review-pr-v2';

/**
 * A CLOSED task-type step bead of the completed run. The beads board keeps only
 * engineering issue types (task/bug/feature/…) and filters out molecule roots,
 * so the completed run surfaces in the beads view via this closed STEP, not its
 * molecule root — assert this id after revealing closed beads.
 */
export const COMPLETED_STEP_APPROVE = 'run-done.approve';

/**
 * The lane phase label the runs list renders for the completed run (RunLane.phase
 * === 'complete' → LaneCard renders the lowercase word). It is the terminal-status
 * text on the historical lane; there is no separate "Completed" badge and NO
 * duration/elapsed text is rendered anywhere for a run (verified against the SPA).
 */
export const COMPLETED_PHASE_LABEL = 'complete';

/** The seeded standalone work bead the beads view lists. */
export const WORK_BEAD_ID = 'work-1';
export const WORK_BEAD_TITLE = 'Wire the seeded dashboard corpus';

/** The seeded mail message the mail view lists. */
export const MAIL_SUBJECT = 'seeded handoff';

/** The seeded agent name (from the corpus config). */
export const AGENT_NAME = 'builder';

/**
 * The seeded live agent's slug — the {slug} segment on /agents/:slug. It equals
 * AGENT_NAME (the session's alias === session_name === agent name), so the agent
 * detail route resolves the seeded session bead. Mirrors corpus.AgentSessionSlug.
 */
export const AGENT_SESSION_SLUG = AGENT_NAME;

/**
 * The runtime state the seeded (non-live) session projects — the StatusBadge label
 * on the agent-detail header. The corpus persists state "active", but the fake
 * runtime provider backs no live process, so the sessions read overlays it to
 * "asleep"; that is the deterministic rendered badge.
 */
export const AGENT_SESSION_STATE = 'asleep';

/**
 * The seeded session's template ("<rig>/<agent>"). The agent-detail AgentMetadata
 * rig is parsed from it, and it renders verbatim as the header template code.
 * Mirrors corpus.AgentSessionTemplate.
 */
export const AGENT_SESSION_TEMPLATE = 'demo/builder';

/**
 * The in-progress bead assigned to the seeded agent (assignee === AGENT_NAME). It
 * is the real in-flight assignment the agent-detail AgentBeadsAssigned panel
 * renders. Its id is the run anchor's preflight step; its title is the button
 * label. Mirrors corpus.AnchorStepID / corpus.AnchorStepTitle.
 */
export const AGENT_ASSIGNED_BEAD_ID = 'run-anchor.preflight';
export const AGENT_ASSIGNED_BEAD_TITLE = 'preflight';

/**
 * The two-message operator↔agent thread. It backs BOTH the mail thread-detail
 * render (both bodies in one thread) and the agent-detail Chat pane (messages
 * between the operator alias and the seeded agent). Mirror corpus.OperatorMailSubject
 * / corpus.OperatorMailBody / corpus.AgentReplyBody.
 */
export const OPERATOR_MAIL_SUBJECT = 'adopt PR #42';
export const OPERATOR_MAIL_BODY =
  'Please take the seeded adopt-pr run to completion and report back here.';
export const AGENT_REPLY_BODY =
  'On it. The preflight step is running now; I will report when the review step opens.';

/**
 * The seeded bead whose detail modal shows a populated BeadDependencies section:
 * REVIEW_BEAD_ID "needs" REVIEW_DEP_TARGET_ID, so the modal renders a single
 * upstream dependency line. Mirror corpus.AnchorReviewStepID / corpus.AnchorStepID.
 */
export const REVIEW_BEAD_ID = 'run-anchor.review';
export const REVIEW_BEAD_TITLE = 'review';
export const REVIEW_DEP_TARGET_ID = 'run-anchor.preflight';

/** Base path for the seeded city's client routes (BrowserRouter basename). */
export const CITY_BASE = `/city/${CITY_NAME}`;

/**
 * The endpoint the SPA POSTs client errors to (lib/clientErrorReporting.ts). A
 * spec fails if the browser hits this while rendering a seeded view — it means a
 * render threw and the error boundary caught it.
 */
export const CLIENT_ERROR_ENDPOINT = '/api/client-errors';

/**
 * Text rendered by components/ErrorBoundary.tsx's crash fallback. A spec asserts
 * this is NOT present on any seeded route.
 */
export const ERROR_BOUNDARY_TEXT = 'Dashboard view failed.';
