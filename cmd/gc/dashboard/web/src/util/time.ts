// Relative-time formatting shared across panels. Matches the format
// the Go `calculateActivity` helper used ("2m ago", "1h ago",
// "3d ago") so existing CSS that keys on age doesn't surprise.

export function relativeTime(ts: string | undefined | null, now: Date = new Date()): string {
  if (!ts) return "";
  const then = new Date(ts);
  if (isNaN(then.getTime())) return "";
  const delta = Math.max(0, now.getTime() - then.getTime());
  const seconds = Math.floor(delta / 1000);
  if (seconds < 60) return `${seconds}s ago`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.floor(hours / 24);
  return `${days}d ago`;
}

// Active-within-window check for badges like "idle / active".
export function activeWithin(ts: string | undefined | null, windowMs: number, now: Date = new Date()): boolean {
  if (!ts) return false;
  const then = new Date(ts).getTime();
  if (isNaN(then)) return false;
  return now.getTime() - then < windowMs;
}
