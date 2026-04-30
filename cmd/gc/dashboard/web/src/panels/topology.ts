import { api, cityScope } from "../api";
import type { components } from "../generated/schema";
import { logWarn } from "../logger";
import { byId, clear, el } from "../util/dom";

type ConfigAgentResponse = components["schemas"]["ConfigAgentResponse"];
type FormulaSummaryResponse = components["schemas"]["FormulaSummaryResponse"];
type FormulaDetailResponse = components["schemas"]["FormulaDetailResponse"];
type FormulaPreviewNodeResponse = components["schemas"]["FormulaPreviewNodeResponse"];
type FormulaPreviewEdgeResponse = components["schemas"]["FormulaPreviewEdgeResponse"];

// Safe Mermaid node ID: letters/digits/underscores only
function mid(s: string): string {
  return `n_${s.replace(/[^a-zA-Z0-9]/g, "_")}`;
}

// Escape a label for use inside Mermaid quotes
function mlabel(s: string): string {
  return s.replace(/"/g, "'").replace(/[<>]/g, "").trim();
}

let mermaidReady = false;

async function getMermaid() {
  const m = (await import("mermaid")).default;
  if (!mermaidReady) {
    m.initialize({ startOnLoad: false, theme: "dark", securityLevel: "loose" });
    mermaidReady = true;
  }
  return m;
}

async function renderMermaid(container: HTMLElement, diagram: string): Promise<void> {
  try {
    const m = await getMermaid();
    const id = `mm_${Math.random().toString(36).slice(2, 9)}`;
    const { svg } = await m.render(id, diagram);
    container.innerHTML = svg;
    container.classList.remove("topology-loading");
  } catch (err) {
    container.textContent = `Diagram error: ${err instanceof Error ? err.message : String(err)}`;
    container.classList.remove("topology-loading");
  }
}

// --- Caches (invalidated on rig/service events, NOT on session/agent events) ---

const formulaSummaryCache = new Map<string, FormulaSummaryResponse[]>();
const formulaDetailCache = new Map<string, FormulaDetailResponse>();
const formulaDetailErrors = new Map<string, string>();

/** Invalidate cached formula data. Called from main.ts before re-render on rig/config events. */
export function invalidateTopologyCache(): void {
  formulaSummaryCache.clear();
  formulaDetailCache.clear();
  formulaDetailErrors.clear();
}

// --- Formula browser persistent state ---
let browserCity = "";
let browserRig: string | null = null;
let browserFormula: string | null = null;

function resetBrowserStateIfCityChanged(city: string): void {
  if (browserCity !== city) {
    browserCity = city;
    browserRig = null;
    browserFormula = null;
  }
}

// --- Data loaders ---

async function loadFormulaSummaries(
  city: string,
  rigName: string,
): Promise<FormulaSummaryResponse[]> {
  const key = `${city}/${rigName}`;
  const cached = formulaSummaryCache.get(key);
  if (cached !== undefined) return cached;
  try {
    const r = await api.GET("/v0/city/{cityName}/formulas", {
      params: {
        path: { cityName: city },
        query: { scope_kind: "rig", scope_ref: rigName },
      },
    });
    const items = r.data?.items ?? [];
    formulaSummaryCache.set(key, items);
    return items;
  } catch (err) {
    logWarn("topology", `Failed to load formula summaries for ${rigName}`, err);
    formulaSummaryCache.set(key, []);
    return [];
  }
}

async function loadFormulaDetail(
  city: string,
  rigName: string,
  formulaName: string,
): Promise<FormulaDetailResponse | null> {
  const key = `${city}/${rigName}/${formulaName}`;
  if (formulaDetailCache.has(key)) return formulaDetailCache.get(key)!;
  if (formulaDetailErrors.has(key)) return null;
  try {
    const r = await api.GET("/v0/city/{cityName}/formulas/{name}", {
      params: {
        path: { cityName: city, name: formulaName },
        query: { scope_kind: "rig", scope_ref: rigName, target: "_" },
      },
    });
    if (r.data) {
      formulaDetailCache.set(key, r.data);
      return r.data;
    }
    formulaDetailErrors.set(key, "Empty response");
    return null;
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err);
    formulaDetailErrors.set(key, msg);
    logWarn("topology", `Failed to load formula detail for ${formulaName} in ${rigName}`, err);
    return null;
  }
}

// --- Routing diagram (shows inter-agent work routing via formula sling steps) ---

async function buildRoutingDiagram(
  city: string,
  agents: ConfigAgentResponse[],
  rigNames: string[],
): Promise<string> {
  // Load formula summaries for all rigs in parallel
  const rigFormulaMap = new Map<string, FormulaSummaryResponse[]>();
  await Promise.all(
    rigNames.map(async (rig) => {
      rigFormulaMap.set(rig, await loadFormulaSummaries(city, rig));
    }),
  );

  // Load formula details in batches to extract sling routing steps
  const allFormulas: Array<{ rig: string; name: string }> = [];
  for (const [rig, formulas] of rigFormulaMap) {
    for (const f of formulas) allFormulas.push({ rig, name: f.name });
  }
  const BATCH = 5;
  for (let i = 0; i < allFormulas.length; i += BATCH) {
    await Promise.all(
      allFormulas.slice(i, i + BATCH).map(({ rig, name }) => loadFormulaDetail(city, rig, name)),
    );
  }

  // Group agents by scope for subgraphs
  const agentsByScope = new Map<string, ConfigAgentResponse[]>();
  for (const agent of agents) {
    const scope = agent.scope ?? "unknown";
    if (!agentsByScope.has(scope)) agentsByScope.set(scope, []);
    agentsByScope.get(scope)!.push(agent);
  }
  const scopes = Array.from(agentsByScope.keys()).sort((a, b) => {
    if (a === "city") return -1;
    if (b === "city") return 1;
    return a.localeCompare(b);
  });

  // Collect sling routing edges: formula → assignee
  interface SlingEdge {
    formulaKey: string;
    formulaName: string;
    assignee: string;
    label: string;
  }
  const slingEdges: SlingEdge[] = [];
  const formulasWithSlings = new Set<string>();
  for (const [rig, formulas] of rigFormulaMap) {
    for (const formula of formulas) {
      const detail = formulaDetailCache.get(`${city}/${rig}/${formula.name}`);
      if (!detail) continue;
      for (const step of detail.steps ?? []) {
        if (step.kind === "sling" && step.assignee) {
          const key = `${rig}/${formula.name}`;
          formulasWithSlings.add(key);
          slingEdges.push({
            formulaKey: key,
            formulaName: formula.name,
            assignee: step.assignee,
            label: step.title || "sling",
          });
        }
      }
    }
  }

  // Agent lookup maps for resolving sling targets
  const agentByName = new Map<string, ConfigAgentResponse>();
  const agentByBareName = new Map<string, ConfigAgentResponse>();
  for (const agent of agents) {
    agentByName.set(agent.name, agent);
    const bare = agent.name.split("/").pop() ?? agent.name;
    agentByBareName.set(bare, agent);
  }

  function resolveAgentNode(assignee: string): string | null {
    if (agentByName.has(assignee)) return mid(assignee);
    const bare = assignee.split("/").pop() ?? assignee;
    const found = agentByBareName.get(bare);
    if (found) return mid(found.name);
    return null;
  }

  const lines: string[] = ["flowchart LR"];

  // Agent subgraphs (one per scope/rig)
  for (const scope of scopes) {
    const scopeAgents = agentsByScope.get(scope)!;
    const label = scope === "city" ? "City" : `${scope} (rig)`;
    lines.push(`  subgraph ${mid("sg_" + scope)}["${mlabel(label)}"]`);
    for (const agent of scopeAgents) {
      const baseName = agent.name.split("/").pop() ?? agent.name;
      const poolSuffix = agent.is_pool ? " (pool)" : "";
      const dot = agent.suspended ? "○" : "●";
      lines.push(`    ${mid(agent.name)}["${mlabel(dot + " " + baseName + poolSuffix)}"]`);
    }
    lines.push("  end");
  }

  // Formula nodes (only for formulas that have sling steps)
  for (const key of formulasWithSlings) {
    const formulaName = key.split("/").slice(1).join("/");
    lines.push(`  ${mid("f_" + key)}["📋 ${mlabel(formulaName)}"]`);
  }

  // Routing edges: formula → target agent
  const seenEdges = new Set<string>();
  const externalNodes = new Set<string>();
  for (const edge of slingEdges) {
    const sourceNode = mid(`f_${edge.formulaKey}`);
    let targetNode = resolveAgentNode(edge.assignee);
    if (!targetNode) {
      const extKey = `ext_${edge.assignee}`;
      if (!externalNodes.has(extKey)) {
        externalNodes.add(extKey);
        const label = edge.assignee.split("/").pop() ?? edge.assignee;
        lines.push(`  ${mid(extKey)}(["${mlabel(label)}"])`);
      }
      targetNode = mid(extKey);
    }
    const edgeKey = `${sourceNode}::${targetNode}::${edge.label}`;
    if (!seenEdges.has(edgeKey)) {
      seenEdges.add(edgeKey);
      lines.push(`  ${sourceNode} -->|"${mlabel(edge.label)}"| ${targetNode}`);
    }
  }

  return lines.join("\n");
}

// --- Formula step DAG for drill-down ---

function buildFormulaStepDiagram(detail: FormulaDetailResponse): string {
  const nodes: FormulaPreviewNodeResponse[] = detail.preview?.nodes ?? [];
  const edges: FormulaPreviewEdgeResponse[] = detail.preview?.edges ?? [];
  const steps = detail.steps ?? [];

  const lines: string[] = ["flowchart TD"];

  if (nodes.length > 0) {
    for (const node of nodes) {
      const shapeOpen = node.kind === "sling" ? "[/" : node.kind === "converge" ? "{{" : "[";
      const shapeClose = node.kind === "sling" ? "/]" : node.kind === "converge" ? "}}" : "]";
      lines.push(`  ${mid(node.id)}${shapeOpen}"${mlabel(node.title)}"${shapeClose}`);
    }
    for (const edge of edges) {
      const edgeLabel = edge.kind ? `|"${mlabel(edge.kind)}"| ` : "";
      lines.push(`  ${mid(edge.from)} -->${edgeLabel}${mid(edge.to)}`);
    }
  } else if (steps.length > 0) {
    for (const step of steps) {
      lines.push(`  ${mid(step.id)}["${mlabel(step.title)}"]`);
    }
    for (let i = 0; i < steps.length - 1; i++) {
      lines.push(`  ${mid(steps[i].id)} --> ${mid(steps[i + 1].id)}`);
    }
  } else {
    lines.push('  empty["(no steps)"]');
  }

  return lines.join("\n");
}

// --- Formula detail renderer (on-demand) ---

async function renderFormulaDetail(
  city: string,
  rigName: string,
  formulaName: string,
  container: HTMLElement,
): Promise<void> {
  clear(container);
  container.append(
    el("div", { class: "topology-diagram topology-loading" }, [`Loading ${formulaName}…`]),
  );

  const detail = await loadFormulaDetail(city, rigName, formulaName);
  clear(container);

  if (!detail) {
    const errMsg =
      formulaDetailErrors.get(`${city}/${rigName}/${formulaName}`) ?? "unknown error";
    container.append(
      el("div", { class: "topology-formula-error" }, [
        `Could not load ${formulaName}: ${errMsg}`,
      ]),
    );
    return;
  }

  const diagramEl = el("div", { class: "topology-diagram topology-loading" }, ["Rendering…"]);
  container.append(diagramEl);
  await renderMermaid(diagramEl, buildFormulaStepDiagram(detail));
}

// --- Formula browser ---

async function renderFormulaBrowser(
  city: string,
  rigNames: string[],
  container: HTMLElement,
): Promise<void> {
  if (rigNames.length === 0) return;

  await Promise.all(rigNames.map((rig) => loadFormulaSummaries(city, rig)));

  if (!browserRig || !rigNames.includes(browserRig)) {
    browserRig = rigNames[0] ?? null;
    browserFormula = null;
  }

  const section = el("div", { class: "topology-section" });
  section.append(el("h3", { class: "topology-section-title" }, ["Formula Browser"]));

  // Rig tab bar
  const tabBar = el("div", { class: "topology-rig-tabs" });
  for (const rig of rigNames) {
    const isActive = rig === browserRig;
    const tab = el(
      "button",
      { class: `topology-rig-tab${isActive ? " active" : ""}`, "data-rig": rig },
      [rig],
    );
    tab.addEventListener("click", () => {
      if (browserRig === rig) return;
      browserRig = rig;
      browserFormula = null;
      clear(container);
      void renderFormulaBrowser(city, rigNames, container);
    });
    tabBar.append(tab);
  }
  section.append(tabBar);

  // Formula list for the selected rig
  const formulas = formulaSummaryCache.get(`${city}/${browserRig}`) ?? [];
  const listEl = el("div", { class: "topology-formula-list" });
  const detailEl = el("div", { class: "topology-formula-detail" });

  if (formulas.length === 0) {
    listEl.append(el("div", { class: "topology-formula-empty" }, ["No formulas found"]));
  } else {
    for (const formula of formulas) {
      const isSelected = formula.name === browserFormula;
      const item = el("div", { class: `topology-formula-item${isSelected ? " active" : ""}` });
      item.append(el("span", { class: "topology-formula-name" }, [formula.name]));
      if (formula.description) {
        item.append(
          el("small", { class: "topology-formula-desc" }, [formula.description.split("\n")[0]]),
        );
      }
      item.addEventListener("click", () => {
        const wasSelected = browserFormula === formula.name;
        browserFormula = wasSelected ? null : formula.name;
        listEl
          .querySelectorAll(".topology-formula-item")
          .forEach((e) => e.classList.remove("active"));
        if (!wasSelected) {
          item.classList.add("active");
          void renderFormulaDetail(city, browserRig!, formula.name, detailEl);
        } else {
          clear(detailEl);
        }
      });
      listEl.append(item);
    }
  }

  section.append(listEl);
  section.append(detailEl);
  container.append(section);

  // Restore detail for previously-selected formula
  if (browserFormula) {
    const idx = formulas.findIndex((f) => f.name === browserFormula);
    if (idx >= 0) {
      const activeItem = listEl.querySelectorAll(".topology-formula-item")[idx];
      activeItem?.classList.add("active");
      void renderFormulaDetail(city, browserRig!, browserFormula, detailEl);
    }
  }
}

// --- Main export ---

export async function renderTopology(): Promise<void> {
  const body = byId("topology-body");
  if (!body) return;

  const city = cityScope();
  if (!city) {
    clear(body);
    body.append(
      el("div", { class: "empty-state" }, [el("p", {}, ["Select a city to view topology"])]),
    );
    return;
  }

  resetBrowserStateIfCityChanged(city);

  if (body.childElementCount === 0) {
    body.append(el("div", { class: "topology-loading" }, ["Loading topology…"]));
  }

  const [configR, rigsR] = await Promise.all([
    api.GET("/v0/city/{cityName}/config", { params: { path: { cityName: city } } }).catch(
      (err) => {
        logWarn("topology", "Failed to load config", err);
        return { data: null };
      },
    ),
    api.GET("/v0/city/{cityName}/rigs", { params: { path: { cityName: city } } }).catch((err) => {
      logWarn("topology", "Failed to load rigs", err);
      return { data: null };
    }),
  ]);

  const agents = configR.data?.agents ?? [];
  const rigNames = (rigsR.data?.items ?? []).map((r) => r.name);

  clear(body);

  // Section 1: Inter-agent routing diagram
  const routingSection = el("div", { class: "topology-section" });
  routingSection.append(el("h3", { class: "topology-section-title" }, ["Agent Routing"]));
  const routingDiagram = el("div", { class: "topology-diagram topology-loading" }, [
    "Building routing graph…",
  ]);
  routingSection.append(routingDiagram);
  body.append(routingSection);

  const diagram = await buildRoutingDiagram(city, agents, rigNames);
  await renderMermaid(routingDiagram, diagram);

  // Section 2: Formula browser
  const browserContainer = el("div", {});
  body.append(browserContainer);
  await renderFormulaBrowser(city, rigNames, browserContainer);
}
