import type {
  EntityLinkView,
  LinkNode,
  LinkNodeRef,
  LinkNodeType,
  LinkProvenance,
  LinkResolutionStat,
} from '../links.js';
import { recordResolution, type ResolutionRecorder } from './instrumentation.js';
import type { ParsedRef } from './node-ref.js';
import { nodeKey, sanitiseUrl } from './node-ref.js';
import type { IndexBead, RelationIndex } from './relation-index.js';

export interface BuildLinkViewOptions {
  githubFetchedAt?: string | null;
  supervisorFetchedAt?: string | null;
  partial?: boolean;
  now?: () => Date;
  recorder?: ResolutionRecorder;
}

interface Accumulator {
  view: EntityLinkView;
  nodesByKey: Map<string, LinkNode>;
  stats: Map<string, LinkResolutionStat>;
  recorder: ResolutionRecorder;
}

function beadRef(bead: IndexBead): LinkNodeRef {
  return { key: nodeKey('bead', bead.id, bead.scope), type: 'bead', ref: bead.id };
}

function statFor(acc: Accumulator, relation: string): LinkResolutionStat {
  const existing = acc.stats.get(relation);
  if (existing) return existing;
  const fresh: LinkResolutionStat = {
    relation,
    resolved: 0,
    unresolved: 0,
    nCandidates: 0,
  };
  acc.stats.set(relation, fresh);
  return fresh;
}

function addNode(acc: Accumulator, node: LinkNode): void {
  if (!acc.nodesByKey.has(node.key)) {
    acc.nodesByKey.set(node.key, node);
    acc.view.nodes.push(node);
  }
}

function addEdge(
  acc: Accumulator,
  from: string,
  to: string,
  relation: string,
  provenance: LinkProvenance,
  resolved: boolean,
): void {
  acc.view.edges.push({ from, to, relation, provenance, resolved });
}

function linkBead(
  acc: Accumulator,
  fromKey: string,
  target: IndexBead,
  relation: string,
  supervisorFetchedAt: string | null,
): void {
  const ref = beadRef(target);
  addNode(acc, {
    ...ref,
    title: target.title,
    status: target.status,
    url: null,
    fetchedAt: supervisorFetchedAt,
    unresolved: false,
  });
  addEdge(acc, fromKey, ref.key, relation, 'supervisor', true);
  statFor(acc, relation).resolved += 1;
  recordResolution(acc.recorder, relation, 'resolved');
}

function emptyView(focus: LinkNodeRef, generatedAt: string): EntityLinkView {
  return {
    focus,
    nodes: [],
    edges: [],
    stats: [],
    partial: false,
    generatedAt,
    asOf: null,
  };
}

function olderOf(a: string | null, b: string | null): string | null {
  if (a === null) return b;
  if (b === null) return a;
  return Date.parse(a) <= Date.parse(b) ? a : b;
}

export function buildLinkView(
  index: RelationIndex,
  parsed: Extract<ParsedRef, { ok: true }>,
  opts: BuildLinkViewOptions = {},
): EntityLinkView {
  const now = opts.now ?? (() => new Date());
  const generatedAt = now().toISOString();
  const supervisorFetchedAt = opts.supervisorFetchedAt ?? null;
  const githubFetchedAt = opts.githubFetchedAt ?? null;

  const focusResult = resolveFocus(index, parsed);
  const view = emptyView(focusResult.focus, generatedAt);
  view.partial = opts.partial ?? false;

  const acc: Accumulator = {
    view,
    nodesByKey: new Map(),
    stats: new Map(),
    recorder: opts.recorder ?? (() => {}),
  };

  addNode(acc, focusResult.focusNode);

  if (!focusResult.focusResolved) {
    view.partial = true;
    finalize(acc, supervisorFetchedAt, githubFetchedAt);
    return view;
  }

  const fromKey = focusResult.focusNode.key;

  if (focusResult.beadFocus) {
    for (const bead of focusResult.beads) {
      addBeadEdges(acc, bead, fromKey, index, supervisorFetchedAt, githubFetchedAt);
    }
  } else {
    for (const bead of focusResult.beads) {
      linkBead(acc, fromKey, bead, 'bead', supervisorFetchedAt);
    }
  }

  finalize(acc, supervisorFetchedAt, githubFetchedAt);
  return view;
}

interface FocusResult {
  focus: LinkNodeRef;
  focusNode: LinkNode;
  beads: IndexBead[];
  focusResolved: boolean;
  beadFocus: boolean;
}

function resolveFocus(index: RelationIndex, parsed: Extract<ParsedRef, { ok: true }>): FocusResult {
  if (parsed.type === 'github_pr' || parsed.type === 'github_issue') {
    const type: LinkNodeType = parsed.type === 'github_pr' ? 'github_pr' : 'github_issue';
    const ref = parsed.type === 'github_pr' ? `pr/${parsed.value}` : `issue/${parsed.value}`;
    const ids =
      parsed.type === 'github_pr'
        ? (index.beadsForPr.get(parsed.value) ?? [])
        : (index.beadsForIssue.get(parsed.value) ?? []);
    const beads = ids
      .map((id) => index.beads.get(id))
      .filter((b): b is IndexBead => b !== undefined);
    const focus: LinkNodeRef = { key: nodeKey(type, parsed.value, 'github'), type, ref };
    return {
      focus,
      focusNode: {
        ...focus,
        title: null,
        status: null,
        url: null,
        fetchedAt: null,
        unresolved: beads.length === 0,
        ...(beads.length > 1 ? { candidateCount: beads.length } : {}),
      },
      beads,
      focusResolved: beads.length > 0,
      beadFocus: false,
    };
  }

  const bead = index.beads.get(parsed.value) ?? index.allBeads.get(parsed.value);
  if (bead !== undefined) {
    const ref = beadFocusRef(bead);
    return {
      focus: ref,
      focusNode: {
        ...ref,
        title: bead.title,
        status: bead.status,
        url: null,
        fetchedAt: null,
        unresolved: false,
      },
      beads: bead.superseded ? [] : [bead],
      focusResolved: true,
      beadFocus: true,
    };
  }

  const beadsForSession = index.beadsForSession.get(parsed.value) ?? [];
  const sessionPresent = index.sessions.has(parsed.value);
  if (beadsForSession.length > 0 || sessionPresent) {
    const focus: LinkNodeRef = {
      key: nodeKey('session', parsed.value, 'session'),
      type: 'session',
      ref: parsed.value,
    };
    const beads = beadsForSession
      .map((id) => index.beads.get(id))
      .filter((b): b is IndexBead => b !== undefined);
    const focusResolved = sessionPresent || beads.length > 0;
    return {
      focus,
      focusNode: {
        ...focus,
        title: index.sessions.get(parsed.value)?.title ?? null,
        status: index.sessions.get(parsed.value)?.state ?? null,
        url: null,
        fetchedAt: null,
        unresolved: !focusResolved,
      },
      beads,
      focusResolved,
      beadFocus: false,
    };
  }

  const focus: LinkNodeRef = {
    key: nodeKey('bead', parsed.value, 'unknown'),
    type: 'bead',
    ref: parsed.value,
  };
  return {
    focus,
    focusNode: {
      ...focus,
      title: null,
      status: null,
      url: null,
      fetchedAt: null,
      unresolved: true,
    },
    beads: [],
    focusResolved: false,
    beadFocus: true,
  };
}

function beadFocusRef(bead: IndexBead): LinkNodeRef {
  return { key: nodeKey('bead', bead.id, bead.scope), type: 'bead', ref: bead.id };
}

function addBeadEdges(
  acc: Accumulator,
  bead: IndexBead,
  fromKey: string,
  index: RelationIndex,
  supervisorFetchedAt: string | null,
  githubFetchedAt: string | null,
): void {
  if (bead.parentBeadId) {
    const parent = index.beads.get(bead.parentBeadId);
    if (parent) linkBead(acc, fromKey, parent, 'parent', supervisorFetchedAt);
    else recordUnresolvedRef(acc, fromKey, bead.parentBeadId, 'parent');
  }

  const children = (index.childrenOf.get(bead.id) ?? []).filter((id) => id !== bead.id);
  for (const id of children) {
    const child = index.beads.get(id);
    if (child) linkBead(acc, fromKey, child, 'child', supervisorFetchedAt);
  }

  if (bead.moleculeId) {
    const members = (index.membersOfMolecule.get(bead.moleculeId) ?? []).filter(
      (id) => id !== bead.id && id !== bead.moleculeId,
    );
    if (bead.moleculeId !== bead.id) {
      const root = index.beads.get(bead.moleculeId);
      if (root) linkBead(acc, fromKey, root, 'molecule', supervisorFetchedAt);
    }
    for (const id of members) {
      const member = index.beads.get(id);
      if (member) linkBead(acc, fromKey, member, 'molecule', supervisorFetchedAt);
    }
  }

  if (bead.prNumber) {
    addGithubNode(
      acc,
      fromKey,
      'github_pr',
      `pr/${bead.prNumber}`,
      bead.prNumber,
      sanitiseUrl(bead.prUrl),
      'pr',
      'supervisor',
      githubFetchedAt,
    );
  }

  if (bead.issueNumber) {
    addGithubNode(
      acc,
      fromKey,
      'github_issue',
      `issue/${bead.issueNumber}`,
      bead.issueNumber,
      sanitiseUrl(bead.issueUrl),
      'issue',
      'supervisor',
      githubFetchedAt,
    );
  }

  if (bead.sessionId) {
    const session = index.sessions.get(bead.sessionId);
    const ref: LinkNodeRef = {
      key: nodeKey('session', bead.sessionId, 'session'),
      type: 'session',
      ref: bead.sessionId,
    };
    if (session) {
      addNode(acc, {
        ...ref,
        title: session.title ?? session.alias ?? bead.sessionName ?? null,
        status: session.state ?? null,
        url: null,
        fetchedAt: supervisorFetchedAt,
        unresolved: false,
      });
      addEdge(acc, fromKey, ref.key, 'session', 'supervisor', true);
      statFor(acc, 'session').resolved += 1;
      recordResolution(acc.recorder, 'session', 'resolved');
    } else {
      addNode(acc, {
        ...ref,
        title: bead.sessionName ?? null,
        status: null,
        url: null,
        fetchedAt: supervisorFetchedAt,
        unresolved: true,
      });
      addEdge(acc, fromKey, ref.key, 'session', 'supervisor', false);
      statFor(acc, 'session').unresolved += 1;
      recordResolution(acc.recorder, 'session', 'unresolved');
    }
  }
}

function addGithubNode(
  acc: Accumulator,
  fromKey: string,
  type: LinkNodeType,
  ref: string,
  value: string,
  url: string | null,
  relation: string,
  provenance: LinkProvenance,
  githubFetchedAt: string | null,
): void {
  const key = nodeKey(type, value, 'github');
  addNode(acc, {
    key,
    type,
    ref,
    title: null,
    status: null,
    url,
    fetchedAt: githubFetchedAt,
    unresolved: true,
  });
  addEdge(acc, fromKey, key, relation, provenance, false);
  statFor(acc, relation).unresolved += 1;
  recordResolution(acc.recorder, relation, 'unresolved');
}

function recordUnresolvedRef(
  acc: Accumulator,
  fromKey: string,
  ref: string,
  relation: string,
): void {
  const key = nodeKey('bead', ref, 'unknown');
  addNode(acc, {
    key,
    type: 'bead',
    ref,
    title: null,
    status: null,
    url: null,
    fetchedAt: null,
    unresolved: true,
  });
  addEdge(acc, fromKey, key, relation, 'supervisor', false);
  statFor(acc, relation).unresolved += 1;
  recordResolution(acc.recorder, relation, 'unresolved');
}

function finalize(
  acc: Accumulator,
  supervisorFetchedAt: string | null,
  githubFetchedAt: string | null,
): void {
  acc.view.stats = [...acc.stats.values()].sort((a, b) => a.relation.localeCompare(b.relation));
  let asOf: string | null = null;
  for (const node of acc.view.nodes) {
    asOf = olderOf(asOf, node.fetchedAt);
  }
  acc.view.asOf = asOf ?? olderOf(supervisorFetchedAt, githubFetchedAt);
}
