export const GC_EVENT_PREFIX = {
  bead: 'bead.',
  session: 'session.',
} as const;

export interface ClientErrorReport {
  readonly component: string;
  readonly operation: string;
  readonly message: string;
}

export type SlingIntent = 'review' | 'draft' | 'triage';
export type SlingKind = 'pr' | 'issue';

export function errorMessage(error: unknown): string {
  if (error instanceof Error) return error.message;
  if (typeof error === 'string') return error;
  return 'unknown error';
}
