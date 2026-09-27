# Workbench

The Workbench is a Gas City Control Centre tab that projects durable work
state from Beads. The first slice is read-only: a Work Queue whose entries
open the corresponding real Bead.

## What it does

- The **Workbench** tab (`/workbench`) lists open Beads as a Work Queue.
- Selecting a Work Queue entry opens that real Bead in the shared bead
  detail view (title, description, status, dependencies, related entities).
- Displayed work state comes from Beads through the typed supervisor REST
  client (`listSupervisorBeads` / `fetchSupervisorBead`) and refreshes
  through the established SSE event path (the `bead.*` prefix via
  `useGcEventRefresh`).
- Deep links work: `/workbench?bead=<id>` opens that Bead directly.

## Read-only guarantee

The first slice cannot create a second durable task record:

- It imports no bead write helpers (`beadWrites`) and renders no
  create/close/claim controls.
- Vibe's native task database, scheduler, session/worktree ownership, and
  message queue are not authorities here — Beads are the only durable
  work state, reached through Gas City's typed API.

## States

- **Loading** — the queue fetch is in flight ("Loading work queue.").
- **Unavailable** — the bead store cannot be reached; the page names the
  failure and offers a Retry.
- **Unknown bead** — a deep link to a bead that no longer exists renders
  the calm "resolved or removed" state, not a hard failure.

All three are covered by automated tests in `Workbench.test.tsx`.
