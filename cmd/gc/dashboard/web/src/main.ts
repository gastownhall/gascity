// Dashboard SPA entry point.
//
// Each panel is a standalone module with one `render*` function.
// Refresh happens two ways:
//   1. A 30-second timer re-runs every panel's render (matches the
//      Go dashboard's old htmx full-page morph cadence).
//   2. The supervisor-wide SSE stream invalidates panels when a
//      state-changing event arrives (bead/mail/session/convoy).
//
// No htmx, no framework; only typed fetch + imperative DOM updates.

import { renderCityTabs } from "./panels/cities";
import { renderStatus } from "./panels/status";
import { renderCrew } from "./panels/crew";
import { renderReady } from "./panels/ready";
import { renderIssues } from "./panels/issues";
import { renderMail } from "./panels/mail";
import { renderConvoys } from "./panels/convoys";
import { startActivityStream, renderActivity } from "./panels/activity";
import { invalidateOptions } from "./panels/options";
import { connectEvents } from "./sse";

const REFRESH_MS = 30_000;

type RefreshFn = () => Promise<void> | void;

const dataPanels: RefreshFn[] = [
  renderStatus,
  renderCrew,
  renderReady,
  renderIssues,
  renderMail,
  renderConvoys,
];

async function refreshAll(): Promise<void> {
  invalidateOptions();
  await Promise.allSettled([renderCityTabs(), ...dataPanels.map((fn) => Promise.resolve(fn()))]);
}

function wireSSE(): void {
  // Activity panel owns its own SSE connection.
  startActivityStream();
  renderActivity();

  // Second connection: invalidate data panels on state-changing events.
  // EventSource multiplexes cheaply enough that two streams are fine;
  // keeping activity separate lets its render stay append-only.
  connectEvents((msg) => {
    if (msg.type === "message") return; // only react to named events
    const stateChanging = [
      "session.started", "session.ended", "session.crashed",
      "bead.created", "bead.updated", "bead.closed",
      "mail.delivered", "mail.read",
      "convoy.created", "convoy.closed",
    ];
    if (stateChanging.includes(msg.type)) {
      void Promise.all(dataPanels.map((fn) => Promise.resolve(fn())));
    }
  });
}

async function boot(): Promise<void> {
  await refreshAll();
  wireSSE();
  window.setInterval(() => {
    void refreshAll();
  }, REFRESH_MS);
}

void boot();
