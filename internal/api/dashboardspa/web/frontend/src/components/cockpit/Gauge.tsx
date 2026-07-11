import { Link } from 'react-router-dom';

import {
  GAUGE_END_DEG,
  GAUGE_START_DEG,
  arcPath,
  gaugeNeedleDeg,
  polarPoint,
} from './arc';

// Dial geometry from the cockpit mockup: a 160×104 viewBox with a 240°
// sweep centered low so the dial face fills the box.
const W = 160;
const H = 104;
const CX = 80;
const CY = 78;
const R = 62;
const TICKS = 11;

export interface GaugeProps {
  /** Instrument caption, e.g. "events / min". */
  label: string;
  /** Current reading (raw units; the needle shows value/max). */
  value: number;
  /** Full-scale reading at the end of the sweep. */
  max: number;
  /** Start of the ochre warn zone (raw units). */
  warnFrom: number;
  /** Renders the big numeric readout under the dial. */
  format: (value: number) => string;
  /** Renders major-tick labels; defaults to the raw number. */
  tickFormat?: (value: number) => string;
  /** Deep link for the whole instrument (every cockpit element is clickable). */
  href: string;
}

/**
 * Gauge is a cockpit needle dial: track arc, ochre warn zone, ticks, and a
 * CSS-transitioned needle. Status is carried by needle position against the
 * printed scale — the warn zone is a region of the scale, not a signal lamp,
 * so it never uses accent (maroon is reserved for needs-you).
 */
export function Gauge({ label, value, max, warnFrom, format, tickFormat, href }: GaugeProps) {
  const needleDeg = gaugeNeedleDeg(value / max);
  const warnStartDeg =
    GAUGE_START_DEG + Math.min(Math.max(warnFrom / max, 0), 1) * (GAUGE_END_DEG - GAUGE_START_DEG);

  return (
    <Link
      to={href}
      className="focus-mark inline-block no-underline"
      aria-label={`${label}: ${format(value)}`}
    >
      <svg
        viewBox={`0 0 ${W} ${H}`}
        width={W}
        height={H}
        role="img"
        aria-hidden
        data-testid="gauge-dial"
        className="overflow-visible"
      >
        <path
          d={arcPath(CX, CY, R, GAUGE_START_DEG, GAUGE_END_DEG)}
          className="stroke-rule"
          strokeWidth={1.5}
          fill="none"
          data-testid="gauge-track"
        />
        {/* The warn zone rides just outside the dial (R+3) so it reads as a
            painted region of the scale, not a second needle track. */}
        <path
          d={arcPath(CX, CY, R + 3, warnStartDeg, GAUGE_END_DEG)}
          className="stroke-warn"
          strokeWidth={2.5}
          fill="none"
          data-testid="gauge-warn-zone"
        />
        {Array.from({ length: TICKS }, (_, i) => {
          const frac = i / (TICKS - 1);
          const deg = GAUGE_START_DEG + frac * (GAUGE_END_DEG - GAUGE_START_DEG);
          const major = i % 5 === 0;
          const outer = polarPoint(CX, CY, R, deg);
          const inner = polarPoint(CX, CY, R - (major ? 8 : 4), deg);
          const labelAt = polarPoint(CX, CY, R - 16, deg);
          return (
            <g key={i}>
              <line
                x1={outer.x}
                y1={outer.y}
                x2={inner.x}
                y2={inner.y}
                className={major ? 'stroke-fg-muted' : 'stroke-rule'}
                strokeWidth={major ? 1.5 : 1}
              />
              {major && tickFormat && (
                <text
                  x={labelAt.x}
                  y={labelAt.y}
                  textAnchor="middle"
                  dominantBaseline="middle"
                  className="fill-fg-faint tnum"
                  fontSize={7}
                >
                  {tickFormat(frac * max)}
                </text>
              )}
            </g>
          );
        })}
        <g
          data-testid="gauge-needle"
          className="transition-transform duration-[400ms] ease-out-quart motion-reduce:transition-none"
          style={{
            transform: `rotate(${needleDeg}deg)`,
            transformOrigin: `${CX}px ${CY}px`,
            transformBox: 'view-box',
          }}
        >
          {/* Drawn pointing straight up; the group rotation is the reading. */}
          <line
            x1={CX}
            y1={CY}
            x2={CX}
            y2={CY - (R - 10)}
            className="stroke-fg"
            strokeWidth={2}
            strokeLinecap="round"
          />
        </g>
        <circle cx={CX} cy={CY} r={3.5} className="fill-fg" />
      </svg>
      <div className="text-center">
        <div className="text-title text-fg tnum" data-testid="gauge-value">
          {format(value)}
        </div>
        <div className="text-label uppercase tracking-wider text-fg-faint">{label}</div>
      </div>
    </Link>
  );
}
