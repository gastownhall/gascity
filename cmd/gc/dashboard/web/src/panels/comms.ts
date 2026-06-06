// Agent Comms panel.
//
// Renders a force-free node/edge graph of inter-agent mail traffic:
// nodes are agents, edges are sender->recipient mail links (weighted
// by volume), and each new message animates a packet pulse along its
// edge. History is seeded from the typed events API (server-side
// `type` filter); live updates are fed from the shared city event
// stream via ingestCommsEvent() — no second SSE connection.
//
// The render loop is demand-driven: it only runs while pulses are in
// flight and never while the tab is hidden, so an idle dashboard
// costs zero animation frames.

import { cityAPI, cityScope } from "../api";
import { byId, clear, el } from "../util/dom";
import type { DashboardEventMessage } from "../sse";

interface MailEdge {
  from: string;
  id: string;
  subject: string;
  to: string;
  ts: string;
}
interface Pulse {
  from: string;
  t0: number;
  to: string;
}

const MAIL_TYPES = new Set(["mail.sent", "mail.replied"]);
const PULSE_MS = 900;
const HOT_MS = 600;
const MAX_TICKER = 50;

const nodes = new Map<string, { hot: number; x: number; y: number }>();
const edges = new Map<string, { count: number; from: string; to: string }>();
const pulses: Pulse[] = [];
const seen = new Set<string>();
let msgs = 0;

let canvas: HTMLCanvasElement | null = null;
let ctx: CanvasRenderingContext2D | null = null;
let rafId = 0;

// --- graph model ---------------------------------------------------

function node(name: string) {
  let n = nodes.get(name);
  if (!n) {
    n = { hot: 0, x: 0, y: 0 };
    nodes.set(name, n);
  }
  return n;
}

function addMessage(e: MailEdge, live: boolean): boolean {
  if (e.id && seen.has(e.id)) return false;
  if (e.id) seen.add(e.id);
  node(e.from);
  node(e.to);
  const key = `${e.from}\u0000${e.to}`;
  const edge = edges.get(key) ?? { count: 0, from: e.from, to: e.to };
  edge.count += 1;
  edges.set(key, edge);
  msgs += 1;
  if (live && e.from !== e.to) {
    pulses.push({ from: e.from, t0: performance.now(), to: e.to });
    node(e.from).hot = node(e.to).hot = performance.now();
  }
  layout();
  return true;
}

function tier(name: string): number {
  const n = name.toLowerCase();
  if (n === "human" || n === "controller") return 0;
  if (n === "mayor") return 1;
  if (n.includes("deacon") || n.includes("boot")) return 2;
  if (n === "witness") return 3;
  return 4;
}

function layout(): void {
  const w = canvas?.clientWidth || 600;
  const h = canvas?.clientHeight || 340;
  const margin = 56;
  const byTier = new Map<number, { hot: number; x: number; y: number }[]>();
  let maxTier = 0;
  nodes.forEach((nd, name) => {
    const t = tier(name);
    if (t > maxTier) maxTier = t;
    const row = byTier.get(t) ?? [];
    row.push(nd);
    byTier.set(t, row);
  });
  const rowGap = maxTier > 0 ? (h - margin * 2) / maxTier : 0;
  byTier.forEach((row, t) => {
    const y = margin + t * rowGap;
    row.forEach((nd, i) => {
      nd.x = margin + ((i + 0.5) / row.length) * (w - margin * 2);
      nd.y = y;
    });
  });
}

function extractMail(env: { payload?: unknown; ts?: string; type?: string }): MailEdge | null {
  if (!env.type || !MAIL_TYPES.has(env.type)) return null;
  const payload = env.payload;
  if (typeof payload !== "object" || payload === null) return null;
  const message = (payload as { message?: unknown }).message;
  if (typeof message !== "object" || message === null) return null;
  const m = message as Record<string, unknown>;
  if (typeof m.from !== "string" || typeof m.to !== "string") return null;
  return {
    from: m.from,
    id: typeof m.id === "string" ? m.id : "",
    subject: typeof m.subject === "string" ? m.subject : "",
    to: m.to,
    ts: typeof m.created_at === "string" ? m.created_at : env.ts ?? "",
  };
}

// --- rendering (demand-driven) -------------------------------------

function cssVar(name: string, fallback: string): string {
  const v = getComputedStyle(document.documentElement).getPropertyValue(name).trim();
  return v || fallback;
}

function drawFrame(now: number): void {
  if (!canvas || !ctx) return;
  const w = canvas.clientWidth;
  const h = canvas.clientHeight;
  ctx.clearRect(0, 0, w, h);

  const edgeColor = cssVar("--text-secondary", "#6c7680");
  const nodeFill = cssVar("--bg-card", "#1a1f26");
  const nodeLine = cssVar("--text-primary", "#e6e1cf");
  const accent = cssVar("--cyan", "#95e6cb");

  edges.forEach((edge) => {
    const a = nodes.get(edge.from);
    const b = nodes.get(edge.to);
    if (!a || !b || a === b) return;
    ctx!.globalAlpha = Math.min(0.7, 0.18 + edge.count * 0.08);
    ctx!.strokeStyle = edgeColor;
    ctx!.fillStyle = edgeColor;
    ctx!.lineWidth = 1;
    ctx!.beginPath();
    ctx!.moveTo(a.x, a.y);
    ctx!.lineTo(b.x, b.y);
    ctx!.stroke();
    const ang = Math.atan2(b.y - a.y, b.x - a.x);
    const tipX = b.x - Math.cos(ang) * 10;
    const tipY = b.y - Math.sin(ang) * 10;
    ctx!.beginPath();
    ctx!.moveTo(tipX, tipY);
    ctx!.lineTo(tipX - Math.cos(ang - 0.4) * 6, tipY - Math.sin(ang - 0.4) * 6);
    ctx!.lineTo(tipX - Math.cos(ang + 0.4) * 6, tipY - Math.sin(ang + 0.4) * 6);
    ctx!.closePath();
    ctx!.fill();
  });
  ctx.globalAlpha = 1;

  for (let i = pulses.length - 1; i >= 0; i--) {
    const p = pulses[i];
    const a = nodes.get(p.from);
    const b = nodes.get(p.to);
    if (!a || !b) {
      pulses.splice(i, 1);
      continue;
    }
    const k = (now - p.t0) / PULSE_MS;
    if (k >= 1) {
      pulses.splice(i, 1);
      continue;
    }
    const x = a.x + (b.x - a.x) * k;
    const y = a.y + (b.y - a.y) * k;
    ctx.fillStyle = accent;
    ctx.fillRect(x - 3, y - 3, 6, 6);
    ctx.globalAlpha = 1 - k;
    ctx.strokeStyle = accent;
    ctx.lineWidth = 1;
    ctx.strokeRect(x - 6, y - 6, 12, 12);
    ctx.globalAlpha = 1;
  }

  ctx.font = "11px system-ui, sans-serif";
  ctx.textBaseline = "middle";
  nodes.forEach((nd, name) => {
    const hot = now - nd.hot < HOT_MS;
    ctx!.fillStyle = hot ? accent : nodeFill;
    ctx!.strokeStyle = nodeLine;
    ctx!.lineWidth = 1.4;
    ctx!.fillRect(nd.x - 7, nd.y - 7, 14, 14);
    ctx!.strokeRect(nd.x - 7, nd.y - 7, 14, 14);
    ctx!.fillStyle = nodeLine;
    ctx!.textAlign = nd.x < w / 2 ? "right" : "left";
    ctx!.fillText(name, nd.x < w / 2 ? nd.x - 12 : nd.x + 12, nd.y);
  });
}

function loop(now: number): void {
  rafId = 0;
  drawFrame(now);
  if (pulses.length && !document.hidden) rafId = requestAnimationFrame(loop);
}

function kick(): void {
  if (document.hidden) {
    drawFrame(performance.now());
    return;
  }
  if (!rafId) rafId = requestAnimationFrame(loop);
}

function drawStatic(): void {
  drawFrame(performance.now());
}

function resize(): void {
  if (!canvas || !ctx) return;
  const dpr = window.devicePixelRatio || 1;
  canvas.width = canvas.clientWidth * dpr;
  canvas.height = canvas.clientHeight * dpr;
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  layout();
  drawStatic();
}

function ensureCanvas(): boolean {
  if (ctx) return true;
  canvas = byId<HTMLCanvasElement>("comms-canvas");
  if (!canvas) return false;
  ctx = canvas.getContext("2d");
  if (!ctx) return false;
  new ResizeObserver(() => resize()).observe(canvas);
  document.addEventListener("visibilitychange", () => {
    if (!document.hidden) {
      drawStatic();
      kick();
    }
  });
  resize();
  return true;
}

// --- ticker / counts -----------------------------------------------

function time(ts: string): string {
  const d = new Date(ts);
  return Number.isNaN(d.getTime()) ? "" : d.toLocaleTimeString([], { hour12: false });
}

function tickerRow(e: MailEdge): HTMLElement {
  return el("div", { class: "comms-tick" }, [
    el("span", { class: "t" }, [time(e.ts)]),
    el("span", { class: "m" }, [
      el("b", {}, [e.from]),
      el("span", { class: "arr" }, ["\u2192"]),
      el("b", {}, [e.to]),
      " ",
      el("span", { class: "sub" }, [e.subject]),
    ]),
  ]);
}

function updateCounts(): void {
  const set = (id: string, v: number) => {
    const node2 = byId(id);
    if (node2) node2.textContent = String(v);
  };
  set("comms-count", nodes.size);
  set("comms-agents", nodes.size);
  set("comms-links", edges.size);
  set("comms-msgs", msgs);
}

function resetState(): void {
  nodes.clear();
  edges.clear();
  pulses.length = 0;
  seen.clear();
  msgs = 0;
}

// --- public surface ------------------------------------------------

export function resetComms(): void {
  resetState();
  const ticker = byId("comms-ticker");
  if (ticker) clear(ticker);
  updateCounts();
  if (ctx) drawStatic();
}

export async function renderComms(): Promise<void> {
  if (!ensureCanvas()) return;
  const city = cityScope();
  if (!city) {
    resetComms();
    return;
  }
  resetState();
  const [sent, replied] = await Promise.all([
    cityAPI(city).events({ type: "mail.sent", limit: 1000 }),
    cityAPI(city).events({ type: "mail.replied", limit: 1000 }),
  ]);
  const items = [...(sent.data?.items ?? []), ...(replied.data?.items ?? [])];
  const entries = items
    .map((item) => extractMail(item))
    .filter((e): e is MailEdge => e !== null)
    .sort((a, b) => Date.parse(a.ts) - Date.parse(b.ts));
  entries.forEach((e) => addMessage(e, false));

  const ticker = byId("comms-ticker");
  if (ticker) {
    clear(ticker);
    [...entries]
      .sort((a, b) => Date.parse(b.ts) - Date.parse(a.ts))
      .slice(0, MAX_TICKER)
      .forEach((e) => ticker.append(tickerRow(e)));
  }
  updateCounts();
  drawStatic();
}

export function ingestCommsEvent(msg: DashboardEventMessage): void {
  if (msg.event !== "event") return;
  const e = extractMail(msg.data);
  if (!e) return;
  if (!addMessage(e, true)) return;
  const ticker = byId("comms-ticker");
  if (ticker) {
    ticker.insertBefore(tickerRow(e), ticker.firstChild);
    while (ticker.children.length > MAX_TICKER) ticker.removeChild(ticker.lastChild!);
  }
  updateCounts();
  kick();
}

// Test-only hooks. extractMail is module-private (pure); expose it for
// unit tests of the envelope->edge parsing without a canvas/rAF.
export function extractMailForTest(env: { payload?: unknown; ts?: string; type?: string }): MailEdge | null {
  return extractMail(env);
}
