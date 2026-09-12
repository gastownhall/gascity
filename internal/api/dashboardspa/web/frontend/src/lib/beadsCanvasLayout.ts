import type { SupervisorBead } from '../supervisor/beadReads';

export type CanvasLayoutMode = 'clusters' | 'lanes';

export interface CanvasLayoutItem {
  id: string;
  issueType: string;
  priority: number | undefined;
  width: number;
  height: number;
}

export interface CanvasPoint {
  x: number;
  y: number;
}

export interface CanvasBounds {
  x: number;
  y: number;
  width: number;
  height: number;
}

export interface CanvasTerritory extends CanvasBounds {
  key: string;
  label: string;
  members: readonly string[];
}

export interface CanvasLayout {
  bounds: CanvasBounds;
  territories: readonly CanvasTerritory[];
  positions: ReadonlyMap<string, CanvasPoint>;
}

export interface CanvasEdge {
  from: string;
  to: string;
  relation: string;
}

const TYPE_ORDER = ['bug', 'feature', 'task', 'epic', 'chore', 'decision'];
const PRIORITY_LABELS = [
  'P0 · critical',
  'P1 · high',
  'P2 · medium',
  'P3 · low',
  'P4 · backlog',
] as const;
const GAP = 56;
const TERRITORY_PADDING = 52;
const TERRITORY_HEADER = 54;
const MIN_TERRITORY_WIDTH = 420;
const MIN_TERRITORY_HEIGHT = 320;

export function normalizeCanvasPriority(priority: number | undefined): number {
  return Number.isInteger(priority) && priority !== undefined && priority >= 0 && priority <= 4
    ? priority
    : 3;
}

export function computeCanvasLayout(
  input: readonly CanvasLayoutItem[],
  mode: CanvasLayoutMode,
  aspect = 16 / 9,
): CanvasLayout {
  const items = [...input].sort((a, b) => a.id.localeCompare(b.id));
  const groups = mode === 'lanes' ? priorityGroups(items) : typeGroups(items);
  const safeAspect = Number.isFinite(aspect) && aspect > 0 ? aspect : 16 / 9;

  if (mode === 'lanes') return laneLayout(groups, items);
  return clusterLayout(groups, items, safeAspect);
}

function typeGroups(items: readonly CanvasLayoutItem[]): ReadonlyArray<Group> {
  const values = new Set(items.map((item) => item.issueType));
  const ordered = [...values].sort((a, b) => typeRank(a) - typeRank(b) || a.localeCompare(b));
  return ordered.map((key) => ({
    key,
    label: pluralize(key),
    members: items.filter((item) => item.issueType === key),
  }));
}

function priorityGroups(items: readonly CanvasLayoutItem[]): ReadonlyArray<Group> {
  return PRIORITY_LABELS.map((label, priority) => ({
    key: String(priority),
    label,
    members: items.filter((item) => normalizeCanvasPriority(item.priority) === priority),
  }));
}

interface Group {
  key: string;
  label: string;
  members: readonly CanvasLayoutItem[];
}

function clusterLayout(
  groups: readonly Group[],
  items: readonly CanvasLayoutItem[],
  aspect: number,
): CanvasLayout {
  const territoryCount = Math.max(groups.length, 1);
  const columns = Math.max(1, Math.ceil(Math.sqrt(territoryCount * aspect)));
  const rows = Math.ceil(territoryCount / columns);
  const largest = Math.max(1, ...groups.map((group) => group.members.length));
  const memberColumns = Math.max(1, Math.ceil(Math.sqrt(largest * aspect)));
  const maxWidth = Math.max(220, ...items.map((item) => item.width));
  const maxHeight = Math.max(112, ...items.map((item) => item.height));
  const memberRows = Math.max(1, Math.ceil(largest / memberColumns));
  const territoryWidth = Math.max(
    MIN_TERRITORY_WIDTH,
    TERRITORY_PADDING * 2 + memberColumns * maxWidth + Math.max(0, memberColumns - 1) * GAP,
  );
  const territoryHeight = Math.max(
    MIN_TERRITORY_HEIGHT,
    TERRITORY_HEADER +
      TERRITORY_PADDING * 2 +
      memberRows * maxHeight +
      Math.max(0, memberRows - 1) * GAP,
  );
  const positions = new Map<string, CanvasPoint>();
  const territories: CanvasTerritory[] = [];

  groups.forEach((group, groupIndex) => {
    const column = groupIndex % columns;
    const row = Math.floor(groupIndex / columns);
    const x = column * (territoryWidth + GAP);
    const y = row * (territoryHeight + GAP);
    const members = [...group.members].sort(byStableSlot);
    members.forEach((item, memberIndex) => {
      positions.set(item.id, {
        x:
          x +
          TERRITORY_PADDING +
          (memberIndex % memberColumns) * (maxWidth + GAP) +
          (maxWidth - item.width) / 2,
        y:
          y +
          TERRITORY_HEADER +
          TERRITORY_PADDING +
          Math.floor(memberIndex / memberColumns) * (maxHeight + GAP) +
          (maxHeight - item.height) / 2,
      });
    });
    territories.push({
      key: group.key,
      label: group.label,
      members: members.map((item) => item.id),
      x,
      y,
      width: territoryWidth,
      height: territoryHeight,
    });
  });

  return {
    bounds: {
      x: 0,
      y: 0,
      width: columns * territoryWidth + Math.max(0, columns - 1) * GAP,
      height: rows * territoryHeight + Math.max(0, rows - 1) * GAP,
    },
    territories,
    positions,
  };
}

function laneLayout(groups: readonly Group[], items: readonly CanvasLayoutItem[]): CanvasLayout {
  const maxWidth = Math.max(220, ...items.map((item) => item.width));
  const maxHeight = Math.max(112, ...items.map((item) => item.height));
  const maximumColumns = 8;
  const largest = Math.max(1, ...groups.map((group) => group.members.length));
  const columns = Math.min(maximumColumns, largest);
  const width = TERRITORY_PADDING * 2 + columns * maxWidth + Math.max(0, columns - 1) * GAP;
  const positions = new Map<string, CanvasPoint>();
  const territories: CanvasTerritory[] = [];
  let y = 0;

  for (const group of groups) {
    const members = [...group.members].sort(byStableSlot);
    const memberRows = Math.max(1, Math.ceil(members.length / columns));
    const height = Math.max(
      MIN_TERRITORY_HEIGHT,
      TERRITORY_HEADER +
        TERRITORY_PADDING * 2 +
        memberRows * maxHeight +
        Math.max(0, memberRows - 1) * GAP,
    );
    members.forEach((item, memberIndex) => {
      positions.set(item.id, {
        x:
          TERRITORY_PADDING +
          (memberIndex % columns) * (maxWidth + GAP) +
          (maxWidth - item.width) / 2,
        y:
          y +
          TERRITORY_HEADER +
          TERRITORY_PADDING +
          Math.floor(memberIndex / columns) * (maxHeight + GAP) +
          (maxHeight - item.height) / 2,
      });
    });
    territories.push({
      key: group.key,
      label: group.label,
      members: members.map((item) => item.id),
      x: 0,
      y,
      width,
      height,
    });
    y += height + GAP;
  }

  return {
    bounds: { x: 0, y: 0, width, height: Math.max(0, y - GAP) },
    territories,
    positions,
  };
}

export function buildCanvasEdges(beads: readonly SupervisorBead[]): CanvasEdge[] {
  const visible = new Set(beads.map((bead) => bead.id));
  const seen = new Set<string>();
  const edges: CanvasEdge[] = [];
  const add = (from: string, to: string, relation: string) => {
    if (!visible.has(from) || !visible.has(to) || from === to) return;
    const key = `${from}\u0000${to}\u0000${relation}`;
    if (seen.has(key)) return;
    seen.add(key);
    edges.push({ from, to, relation });
  };

  for (const bead of beads) {
    for (const dependency of bead.needs ?? []) add(dependency, bead.id, 'blocks');
    for (const dependency of bead.dependencies ?? []) {
      add(
        dependency.depends_on_id,
        bead.id,
        dependency.type === 'parent-child' ? 'contains' : dependency.type || 'references',
      );
    }
    if (bead.parent) add(bead.parent, bead.id, 'contains');
  }
  return edges;
}

export function fitCanvas(
  bounds: CanvasBounds,
  viewport: { width: number; height: number },
  padding = 56,
): { zoom: number; pan: CanvasPoint } {
  const usableWidth = Math.max(1, viewport.width - padding * 2);
  const usableHeight = Math.max(1, viewport.height - padding * 2);
  const zoom = Math.max(
    0.08,
    Math.min(
      1.35,
      usableWidth / Math.max(bounds.width, 1),
      usableHeight / Math.max(bounds.height, 1),
    ),
  );
  return {
    zoom,
    pan: {
      x: viewport.width / 2 - (bounds.x + bounds.width / 2) * zoom,
      y: viewport.height / 2 - (bounds.y + bounds.height / 2) * zoom,
    },
  };
}

function typeRank(value: string): number {
  const rank = TYPE_ORDER.indexOf(value);
  return rank === -1 ? TYPE_ORDER.length : rank;
}

function pluralize(value: string): string {
  if (value.endsWith('s')) return value;
  if (value.endsWith('y')) return `${value.slice(0, -1)}ies`;
  return `${value}s`;
}

function byStableSlot(a: CanvasLayoutItem, b: CanvasLayoutItem): number {
  return hash(a.id) - hash(b.id) || a.id.localeCompare(b.id);
}

function hash(value: string): number {
  let result = 0x811c9dc5;
  for (let index = 0; index < value.length; index += 1) {
    result ^= value.charCodeAt(index);
    result = Math.imul(result, 0x01000193);
  }
  return result >>> 0;
}
