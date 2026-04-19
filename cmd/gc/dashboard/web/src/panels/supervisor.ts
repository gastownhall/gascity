import { cityScope } from "../api";
import { getCachedCities } from "../state";
import { byId, clear, el } from "../util/dom";

export function renderSupervisorOverview(): void {
  const panel = byId("supervisor-overview-panel");
  const body = byId("supervisor-overview-body");
  const count = byId("supervisor-city-count");
  if (!panel || !body || !count) return;

  const inSupervisorMode = cityScope() === "";
  panel.hidden = !inSupervisorMode;
  if (!inSupervisorMode) return;

  const cities = getCachedCities().sort((a, b) => a.name.localeCompare(b.name));
  count.textContent = String(cities.length);
  clear(body);

  if (cities.length === 0) {
    body.append(el("div", { class: "empty-state" }, [
      el("p", {}, ["No managed cities available"]),
    ]));
    return;
  }

  const tbody = el("tbody");
  cities.forEach((city) => {
    const phases = city.phasesCompleted.length > 0 ? city.phasesCompleted.join(", ") : "—";
    const openLink = el(
      "a",
      {
        class: "supervisor-city-link",
        href: `?city=${encodeURIComponent(city.name)}`,
      },
      ["Open"],
    );
    tbody.append(
      el("tr", {}, [
        el("td", {}, [el("strong", {}, [city.name])]),
        el("td", {}, [
          el(
            "span",
            {
              class: `badge ${city.error ? "badge-red" : city.running ? "badge-green" : "badge-muted"}`,
            },
            [city.error ? "Error" : city.running ? "Running" : "Stopped"],
          ),
        ]),
        el("td", {}, [city.status ?? "—"]),
        el("td", { class: "supervisor-city-phases" }, [phases]),
        el("td", { class: "supervisor-city-error" }, [city.error ?? "—"]),
        el("td", { class: "supervisor-city-actions" }, [openLink]),
      ]),
    );
  });

  body.append(
    el("table", { class: "supervisor-city-table" }, [
      el("thead", {}, [
        el("tr", {}, [
          el("th", {}, ["City"]),
          el("th", {}, ["State"]),
          el("th", {}, ["Status"]),
          el("th", {}, ["Phases"]),
          el("th", {}, ["Error"]),
          el("th", {}, [""]),
        ]),
      ]),
      tbody,
    ]),
  );
}
