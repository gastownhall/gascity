const MISSING_MARK = '·';

export const KB = 1024;
export const MB = KB * 1024;
export const GB = MB * 1024;

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

function timestampMs(value: Date | string): number {
  return value instanceof Date ? value.getTime() : Date.parse(value);
}

function pad2(value: number): string {
  return String(value).padStart(2, '0');
}
