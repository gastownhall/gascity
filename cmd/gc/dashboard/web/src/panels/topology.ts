import { api, cityScope } from "../api";
import type { components } from "../generated/schema";
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

function buildAgentTopologyDiagram(
  agents: ConfigAgentResponse[],
  runningByTemplate: Map<string, number>,
): string {
  // Group agents by scope
  const byScope = new Map<string, ConfigAgentResponse[]>();
  for (const agent of agents) {
    const scope = agent.scope ?? "unknown";
    if (!byScope.has(scope)) byScope.set(scope, []);
    byScope.get(scope)!.push(agent);
  }

  // Sort scopes: city first, then alphabetical
  const scopes = Array.from(byScope.keys()).sort((a, b) => {
    if (a === "city") return -1;
    if (b === "city") return 1;
    return a.localeCompare(b);
  });

  const lines: string[] = ["flowchart LR"];

  for (const scope of scopes) {
    const scopeAgents = byScope.get(scope)!;
    const label = scope === "city" ? "City" : `${scope} (rig)`;
    const subId = scope === "city" ? "sg_city" : `sg_${mid(scope)}`;

    lines.push(`  subgraph ${subId}["${mlabel(label)}"]`);
    for (const agent of scopeAgents) {
      const baseName = agent.name.split("/").pop() ?? agent.name;
      const running = (runningByTemplate.get(baseName) ?? 0) > 0;
      const poolSuffix = agent.is_pool ? " (pool)" : "";
      const count = runningByTemplate.get(baseName) ?? 0;
      const countSuffix = count > 1 ? ` ×${count}` : "";
      const dot = running ? "●" : "○";
      const nodeLabel = `${dot} ${baseName}${poolSuffix}${countSuffix}`;
      lines.push(`    ${mid(agent.name)}["${mlabel(nodeLabel)}"]`);
    }
    lines.push("  end");
  }

  return lines.join("\n");
}

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
    // Fall back to sequential ordering from the steps list
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

async function loadAndRenderFormula(
  city: string,
  formulaName: string,
  rigName: string,
  container: HTMLElement,
): Promise<void> {
  const r = await api.GET("/v0/city/{cityName}/formulas/{name}", {
    params: {
      path: { cityName: city, name: formulaName },
      query: { scope_kind: "rig", scope_ref: rigName, target: "_" },
    },
  });
  if (!r.data) {
    container.textContent = "Failed to load formula detail";
    container.classList.remove("topology-loading");
    return;
  }
  await renderMermaid(container, buildFormulaStepDiagram(r.data));
}

function makeFormulaCard(
  city: string,
  formula: FormulaSummaryResponse,
  rigName: string,
): HTMLElement {
  const card = el("div", { class: "topology-formula-card" });

  const header = el("div", { class: "topology-formula-header" });
  const titleEl = el("strong", { class: "topology-formula-name" }, [formula.name]);
  const descEl = formula.description
    ? el("span", { class: "topology-formula-desc" }, [formula.description.split("\n")[0]])
    : null;
  header.append(titleEl);
  if (descEl) header.append(descEl);
  card.append(header);

  const diagramEl = el("div", { class: "topology-diagram topology-loading" }, ["Loading…"]);
  card.append(diagramEl);

  void loadAndRenderFormula(city, formula.name, rigName, diagramEl);
  return card;
}

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

  clear(body);
  body.append(el("div", { class: "topology-loading" }, ["Loading topology…"]));

  // Parallel: config, rigs, sessions
  const [configR, rigsR, sessionsR] = await Promise.all([
    api.GET("/v0/city/{cityName}/config", { params: { path: { cityName: city } } }),
    api.GET("/v0/city/{cityName}/rigs", { params: { path: { cityName: city } } }),
    api.GET("/v0/city/{cityName}/sessions", {
      params: { path: { cityName: city }, query: { limit: 500 } },
    }),
  ]);

  const agents = configR.data?.agents ?? [];
  const rigs = rigsR.data?.items ?? [];
  const sessions = sessionsR.data?.items ?? [];

  // Count running sessions per template name
  const runningByTemplate = new Map<string, number>();
  for (const session of sessions) {
    if (!session.running) continue;
    const name = session.template;
    runningByTemplate.set(name, (runningByTemplate.get(name) ?? 0) + 1);
  }

  clear(body);

  // --- Section 1: Agent topology ---
  const topoSection = el("div", { class: "topology-section" });
  topoSection.append(el("h3", { class: "topology-section-title" }, ["Agent Topology"]));
  const topoDiagram = el("div", { class: "topology-diagram topology-loading" }, ["Rendering…"]);
  topoSection.append(topoDiagram);
  body.append(topoSection);
  void renderMermaid(topoDiagram, buildAgentTopologyDiagram(agents, runningByTemplate));

  // --- Section 2: Formulas per rig ---
  const formulaResults = await Promise.all(
    rigs.map((rig) =>
      api
        .GET("/v0/city/{cityName}/formulas", {
          params: {
            path: { cityName: city },
            query: { scope_kind: "rig", scope_ref: rig.name },
          },
        })
        .then((r) => ({ rigName: rig.name, formulas: r.data?.items ?? [] })),
    ),
  );

  for (const { rigName, formulas } of formulaResults) {
    if (formulas.length === 0) continue;

    const rigSection = el("div", { class: "topology-section" });
    rigSection.append(
      el("h3", { class: "topology-section-title" }, [`${rigName} — Formulas`]),
    );

    for (const formula of formulas) {
      rigSection.append(makeFormulaCard(city, formula, rigName));
    }

    body.append(rigSection);
  }
}
