import type { ReactNode } from 'react';
import { Link } from 'react-router-dom';

// Lamps report a binary system health: healthy or cautionary. Stuck/accent is
// deliberately out of range — the needs-you mark lives on the attention strip,
// not in a systems readout.
export type LampTone = 'ok' | 'warn';

const TONE_COLOR: Record<LampTone, string> = {
  ok: 'text-ok',
  warn: 'text-warn',
};

export interface StatusLamp {
  /** Stable identity / React key. */
  id: string;
  /** System name, shown uppercase. */
  label: string;
  /** Current reading (already formatted for display). */
  value: string;
  /** Health tone; caps at ok|warn (never accent). */
  tone: LampTone;
  /** Dims the bulb (e.g. an asleep or not-applicable system). */
  dim?: boolean;
  /** Optional deep link; the row becomes a link only when present. */
  href?: string;
}

export interface StatusLampsProps {
  lamps: StatusLamp[];
}

function LampRow({ lamp }: { lamp: StatusLamp }) {
  const row: ReactNode = (
    <span className="flex items-baseline gap-2" data-id={lamp.id}>
      <span
        aria-hidden
        data-testid="lamp-bulb"
        className={`text-[0.85em] leading-none ${TONE_COLOR[lamp.tone]}`}
        style={lamp.dim ? { opacity: 0.35 } : undefined}
      >
        ●
      </span>
      <span
        className="text-label uppercase tracking-wider text-fg-faint"
        data-testid="lamp-label"
      >
        {lamp.label}
      </span>
      <span className="tnum text-fg-muted" data-testid="lamp-value">
        {lamp.value}
      </span>
    </span>
  );

  if (lamp.href) {
    return (
      <Link
        to={lamp.href}
        className="focus-mark block no-underline"
        aria-label={`${lamp.label}: ${lamp.value}`}
      >
        {row}
      </Link>
    );
  }
  return row;
}

/**
 * StatusLamps is a systems column: one bulb per subsystem, colored by health
 * tone (healthy-sage or caution-ochre — never accent), with an uppercase label
 * and a tabular value. A dim lamp fades its bulb; a lamp with an href becomes a
 * deep link.
 */
export function StatusLamps({ lamps }: StatusLampsProps) {
  return (
    <div className="flex flex-col gap-1" data-testid="status-lamps">
      {lamps.map((lamp) => (
        <LampRow key={lamp.id} lamp={lamp} />
      ))}
    </div>
  );
}
