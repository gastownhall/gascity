const MISSING_MARK = '·';

export const KB = 1024;
export const MB = KB * 1024;
export const GB = MB * 1024;

// Decimal (SI, 1000-based) byte boundaries backing formatBytesSI, kept distinct
// from the binary KB/MB/GB above (which back formatHumanSize).
const KB_SI = 1_000;
const MB_SI = KB_SI * 1_000;
const GB_SI = MB_SI * 1_000;

export function formatDate(value: Date | string): string {
  const ms = timestampMs(value);
  if (!Number.isFinite(ms)) return MISSING_MARK;
  const date = new Date(ms);
  return `${date.getFullYear()}-${pad2(date.getMonth() + 1)}-${pad2(date.getDate())}`;
}

export function formatDateTime(value: Date | string): string {
  const ms = timestampMs(value);
  if (!Number.isFinite(ms)) return MISSING_MARK;
  const date = new Date(ms);
  return `${formatDate(date)} ${pad2(date.getHours())}:${pad2(date.getMinutes())}`;
}

export function formatHumanSize(value: number, unit: 'bytes' | 'chars' = 'bytes'): string {
  if (!Number.isFinite(value) || value < 0) return MISSING_MARK;
  if (value < KB) return unit === 'chars' ? `${value} chars` : `${value} B`;
  if (value < MB) return `${(value / KB).toFixed(1)} KB`;
  if (value < GB) return `${(value / MB).toFixed(1)} MB`;
  return `${(value / GB).toFixed(2)} GB`;
}

/**
 * formatBytesSI renders a byte count with decimal (SI, 1000-based) unit scaling:
 * B, KB, MB, GB — one decimal at the GB tier, whole units below. This is the
 * counterpart to {@link formatHumanSize}, which uses binary (1024-based) units;
 * choose this for surfaces that report storage in SI terms (the cockpit dolt
 * store lamp, the attention host-process memory summaries).
 */
export function formatBytesSI(bytes: number): string {
  if (bytes >= GB_SI) return `${(bytes / GB_SI).toFixed(1)} GB`;
  if (bytes >= MB_SI) return `${Math.round(bytes / MB_SI)} MB`;
  if (bytes >= KB_SI) return `${Math.round(bytes / KB_SI)} KB`;
  return `${bytes} B`;
}

function timestampMs(value: Date | string): number {
  return value instanceof Date ? value.getTime() : Date.parse(value);
}

function pad2(value: number): string {
  return String(value).padStart(2, '0');
}
