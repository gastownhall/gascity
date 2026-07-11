// A fixed-capacity ring buffer of per-second event counts backing the cockpit
// Oscilloscope. It carries no notion of wall-clock time: callers drive shifts
// on their own cadence, so the buffer stays pure and trivially testable.

/** ScopeBuffer accumulates event counts into a scrolling window of bins. */
export interface ScopeBuffer {
  /** Increment the newest (rightmost) bin by one. */
  bump(): void;
  /** Advance the window one bin, retiring the oldest and opening a fresh zero. */
  shift(): void;
  /** Copy the bins in oldest→newest order. */
  snapshot(): number[];
}

/**
 * createScopeBuffer returns a ring buffer of `bins` counts, all starting at
 * zero. `head` indexes the newest bin; shifting rotates it forward and zeroes
 * the slot it lands on, which simultaneously retires the oldest count.
 */
export function createScopeBuffer(bins: number): ScopeBuffer {
  if (!Number.isInteger(bins) || bins <= 0) {
    throw new Error(`createScopeBuffer: bins must be a positive integer, got ${bins}`);
  }

  const data = new Array<number>(bins).fill(0);
  let head = 0; // index of the newest bin

  return {
    bump() {
      data[head] = (data[head] ?? 0) + 1;
    },
    shift() {
      head = (head + 1) % bins;
      data[head] = 0;
    },
    snapshot() {
      const out = new Array<number>(bins);
      // The oldest bin sits just past head (it is overwritten next); walk
      // forward from there to unroll the ring into chronological order.
      for (let i = 0; i < bins; i++) {
        out[i] = data[(head + 1 + i) % bins] ?? 0;
      }
      return out;
    },
  };
}
