// Top-of-page city selector: renders a horizontal tab bar of every
// city the supervisor knows about, with the current `?city=...`
// highlighted. Clicking a tab reloads with the new query param so
// every panel re-fetches against the new scope.

import { api, cityScope } from "../api";
import { byId, clear, el } from "../util/dom";

export async function renderCityTabs(): Promise<void> {
  const container = byId("city-tabs");
  if (!container) return;

  const { data, error } = await api.GET("/v0/cities");
  if (error || !data?.items) {
    clear(container);
    return;
  }

  const selected = cityScope();
  clear(container);

  const nav = el("nav", { class: "city-tabs" });
  for (const city of data.items) {
    const running = city.running === true;
    const current = city.name === selected;
    const tab = el(
      "a",
      {
        href: `/?city=${encodeURIComponent(city.name ?? "")}`,
        class: `city-tab${current ? " active" : ""}${running ? "" : " stopped"}`,
      },
      [
        el("span", { class: `city-dot${running ? " running" : ""}` }),
        ` ${city.name ?? ""}`,
      ],
    );
    nav.append(tab);
  }
  container.append(nav);
}
