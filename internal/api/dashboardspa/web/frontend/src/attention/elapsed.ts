// Shared age helpers for the attention layer. Used by the registry's
// agent/mail emitters and the beads selector to derive and phrase how long ago
// something happened, so the two read one implementation.

/** Milliseconds since `rawTimestamp`, or null if it is absent, unparseable, or in the future. */
export function elapsedSince(rawTimestamp: string | undefined, nowMs: number): number | null {
  if (rawTimestamp === undefined || rawTimestamp.length === 0) return null;
  const timestampMs = Date.parse(rawTimestamp);
  if (!Number.isFinite(timestampMs)) return null;
  const ageMs = nowMs - timestampMs;
  return ageMs >= 0 ? ageMs : null;
}

/** A coarse, operator-readable age phrase ("3h", "5d"). */
export function formatElapsed(ageMs: number): string {
  const hours = Math.max(1, Math.round(ageMs / (60 * 60 * 1000)));
  if (hours < 48) return `${hours}h`;
  return `${Math.round(hours / 24)}d`;
}

/** Like formatElapsed but with minute resolution under an hour ("8m", "3h"). */
export function formatElapsedFine(ageMs: number): string {
  const minutes = Math.round(ageMs / (60 * 1000));
  if (minutes < 60) return `${Math.max(1, minutes)}m`;
  return formatElapsed(ageMs);
}
