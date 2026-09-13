export type Avail<T> =
  | ({ status: 'available' } & T)
  | {
      status: 'unavailable';
      error: string;
    };

export interface PartialAwareListMeta {
  /** True when the supervisor reports the list is incomplete. */
  partial?: boolean;
  /** Human-readable errors from upstream sources that failed during aggregation. */
  partial_errors?: readonly string[];
}

export interface PartialAwareList<T> extends PartialAwareListMeta {
  /** Normalized list items. Degraded `items: null` becomes `[]` at the edge. */
  items: T[];
}

export interface CountedList<T> extends PartialAwareList<T> {
  /** Supervisor's own total count for the requested scope. */
  total: number;
}

export interface RequiredPartialList<T> extends Omit<PartialAwareList<T>, 'partial'> {
  /** Required on supervisor feeds whose OpenAPI declares `partial: boolean`. */
  partial: boolean;
}
