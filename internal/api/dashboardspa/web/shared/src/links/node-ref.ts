import type { LinkNodeType } from '../links.js';
import { makeNodeKey } from '../links.js';
import { BEAD_ID_RE } from '../bead-id.js';

// Ref parsing, validation, and URL sanitisation for the linked view.

export type ParsedRef =
  | { ok: true; type: ParsedRefType; value: string }
  | { ok: false; error: string };

export type ParsedRefType = 'bead' | 'github_pr' | 'github_issue' | 'session' | 'formula_run';

const PR_RE = /^pr\/(\d{1,9})$/;
const ISSUE_RE = /^issue\/(\d{1,9})$/;

export function parseRef(raw: string): ParsedRef {
  const value = raw.trim();
  if (value.length === 0) return { ok: false, error: 'empty ref' };

  const prMatch = PR_RE.exec(value);
  if (prMatch?.[1]) return { ok: true, type: 'github_pr', value: prMatch[1] };

  const issueMatch = ISSUE_RE.exec(value);
  if (issueMatch?.[1]) {
    return { ok: true, type: 'github_issue', value: issueMatch[1] };
  }

  if (BEAD_ID_RE.test(value)) return { ok: true, type: 'bead', value };

  return { ok: false, error: 'unrecognised ref' };
}

export function sanitiseUrl(raw: unknown): string | null {
  if (typeof raw !== 'string') return null;
  const trimmed = raw.trim();
  return /^https?:\/\//i.test(trimmed) ? trimmed : null;
}

export function nodeKey(type: LinkNodeType, ref: string, scope: string): string {
  return makeNodeKey(type, ref, scope);
}
