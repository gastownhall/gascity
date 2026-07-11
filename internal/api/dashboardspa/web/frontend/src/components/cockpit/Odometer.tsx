import { Link } from 'react-router-dom';

// One 0-9 stack cell is DIGIT_EM tall; translating the stack by
// (digit × DIGIT_EM) rolls the requested digit into the clipped window.
const DIGIT_EM = 1.1;

/**
 * odometerColumns decomposes a reading into left-to-right digit columns,
 * zero-padded to at least `digits`. It widens past that width when the value
 * itself is longer so a large reading is never silently truncated. Negative
 * or fractional input is floored and clamped to zero (a count has no sign).
 */
export function odometerColumns(value: number, digits: number): number[] {
  const safe = Math.max(0, Math.floor(value));
  const str = String(safe);
  const width = Math.max(digits, str.length);
  return str
    .padStart(width, '0')
    .split('')
    .map((c) => Number(c));
}

export interface OdometerProps {
  /** Current reading; floored and clamped to a non-negative integer. */
  value: number;
  /** Minimum column count; the odometer widens when the value is longer. Defaults to 4. */
  digits?: number;
  /** Instrument caption under the digits. */
  label: string;
  /** Optional secondary caption (e.g. "since boot"). */
  sub?: string;
  /**
   * Optional deep link for the whole instrument. When omitted the odometer
   * renders as an unlinked labelled block (the beads-closed-today readout is
   * not a link per the cockpit spec).
   */
  href?: string;
}

/**
 * Odometer is a mechanical digit-roll readout: each column clips a 0-9 stack
 * and translates it so the current digit shows through the window, animating
 * the roll on value changes. Status is carried by the number itself, so it
 * never uses accent (maroon is reserved for needs-you).
 */
export function Odometer({ value, digits = 4, label, sub, href }: OdometerProps) {
  const columns = odometerColumns(value, digits);
  const reading = Math.max(0, Math.floor(value));
  const ariaLabel = `${label}: ${reading}`;

  const body = (
    <>
      <div className="flex justify-center gap-0.5" data-testid="odometer" aria-hidden>
        {columns.map((digit, i) => (
          <span
            // Position is the identity here — a fixed-width numeric display,
            // not a reorderable list — so the column index is a stable key.
            key={i}
            data-testid="odometer-column"
            data-digit={digit}
            className="relative block overflow-hidden text-display leading-none tnum text-fg"
            style={{ height: `${DIGIT_EM}em` }}
          >
            <span
              data-testid="odometer-stack"
              className="flex flex-col transition-transform duration-[450ms] ease-out-quart motion-reduce:transition-none"
              style={{ transform: `translateY(-${Math.round(digit * DIGIT_EM * 100) / 100}em)` }}
            >
              {Array.from({ length: 10 }, (_, d) => (
                <span
                  key={d}
                  className="flex items-center justify-center"
                  style={{ height: `${DIGIT_EM}em` }}
                >
                  {d}
                </span>
              ))}
            </span>
          </span>
        ))}
      </div>
      <div className="mt-1 text-center">
        <div className="text-label uppercase tracking-wider text-fg-faint">{label}</div>
        {sub && <div className="text-label uppercase tracking-wider text-fg-faint">{sub}</div>}
      </div>
    </>
  );

  if (href === undefined) {
    return (
      <div className="inline-block" aria-label={ariaLabel} data-testid="odometer-block">
        {body}
      </div>
    );
  }

  return (
    <Link to={href} className="focus-mark inline-block no-underline" aria-label={ariaLabel}>
      {body}
    </Link>
  );
}
