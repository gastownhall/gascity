// Crew panel: every running agent session with its state badge.
// The Go `/api/crew` endpoint fetched the session list, then issued
// a pending-interaction check per session to decide whether to
// badge a session as "awaiting-input". The SPA does the same: one
// fetch for the list, N parallel fetches for pending state.

import { api, cityScope } from "../api";
import { byId, clear, el } from "../util/dom";
import { relativeTime } from "../util/time";

type CrewState = "spinning" | "questions" | "finished" | "idle";

export async function renderCrew(): Promise<void> {
  const container = byId("crew-panel");
  if (!container) return;
  const city = cityScope();
  if (!city) {
    clear(container);
    return;
  }

  const { data, error } = await api.GET("/v0/city/{cityName}/sessions", {
    params: { path: { cityName: city }, query: { peek: "true" } },
  });
  if (error || !data?.items) {
    clear(container);
    container.append(el("div", { class: "panel-error" }, ["Could not load sessions."]));
    return;
  }

  const sessions = data.items.filter((s) => s.running === true);

  // Parallel pending check per session. If any request fails the
  // session defaults to non-pending (the badge is an enhancement,
  // not a hard requirement).
  const pending = await Promise.all(
    sessions.map(async (s): Promise<boolean> => {
      if (!s.id) return false;
      try {
        const r = await api.GET("/v0/city/{cityName}/session/{id}/pending", {
          params: { path: { cityName: city, id: s.id } },
        });
        return Boolean(r.data?.pending);
      } catch {
        return false;
      }
    }),
  );

  clear(container);
  const list = el("div", { class: "crew-list" });
  sessions.forEach((s, i) => {
    const state = classify(s, pending[i] ?? false);
    list.append(el(
      "div",
      { class: `crew-card crew-${state}` },
      [
        el("div", { class: "crew-name" }, [s.template ?? s.session_name ?? s.id ?? ""]),
        el("div", { class: "crew-badges" }, [
          el("span", { class: `badge badge-${state}` }, [state]),
          s.rig ? el("span", { class: "badge badge-muted" }, [`rig:${s.rig}`]) : null,
          s.pool ? el("span", { class: "badge badge-muted" }, [`pool:${s.pool}`]) : null,
        ]),
        el("div", { class: "crew-activity" }, [relativeTime(s.last_active)]),
      ],
    ));
  });
  if (sessions.length === 0) {
    list.append(el("div", { class: "panel-muted" }, ["No running sessions."]));
  }
  container.append(list);
}

function classify(s: { active_bead?: string; running?: boolean }, pendingInteraction: boolean): CrewState {
  if (pendingInteraction) return "questions";
  if (s.active_bead && s.active_bead.length > 0) return "spinning";
  if (s.running === false) return "finished";
  return "idle";
}
