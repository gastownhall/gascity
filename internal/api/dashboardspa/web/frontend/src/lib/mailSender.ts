// Mail senders are usually clean aliases ("mayor"), but a worker in a
// worktree can report its sender as a filesystem path, e.g.
// "/home/ds/gascity-packs/gascity-packs-polecat-1". Format that for display
// as "rig · agent" (e.g. "gascity-packs · polecat-1"); pass clean aliases
// through unchanged.
import { canonicalRigLabel } from '../hooks/projectOf';

export function formatMailSender(from: string): string {
  const raw = from.trim();
  if (raw.length === 0) return raw;
  if (!raw.includes('/') && !raw.includes('\\')) return raw;

  const parts = raw.split(/[\\/]/).filter((p) => p.length > 0);
  const agentSeg = parts[parts.length - 1];
  if (agentSeg === undefined) return raw;
  const rawRig = parts[parts.length - 2];
  if (rawRig === undefined) return agentSeg;

  // Drop a redundant "<rig>-" prefix the worktree dir often carries
  // ("gascity-packs-polecat-1" under ".../gascity-packs" → "polecat-1"),
  // then canonicalize the rig ("gascity-main" → "gascity").
  const agent = agentSeg.startsWith(`${rawRig}-`) ? agentSeg.slice(rawRig.length + 1) : agentSeg;
  return `${canonicalRigLabel(rawRig)} · ${agent}`;
}
