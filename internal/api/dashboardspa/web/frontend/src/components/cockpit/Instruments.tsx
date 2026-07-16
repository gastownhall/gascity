import type { ReactNode } from 'react';
import { Link } from 'react-router-dom';
import { pipelineWidths, type PipelineSegment, type RunRingModel } from './model';

export function InstrumentNote({ children }: { children: ReactNode }) {
  return <p className="mt-1 text-label italic text-fg-faint">{children}</p>;
}

export function Odometer({
  label,
  value,
  note,
}: {
  label: string;
  value: number | null;
  note?: string | undefined;
}) {
  const reading = value === null ? null : Math.max(0, Math.floor(value));
  const digits = reading === null ? '—' : String(reading).padStart(4, '0');
  return (
    <div
      role="status"
      aria-label={`${label}: ${reading === null ? 'unavailable' : reading}`}
      className="min-w-36 text-center"
    >
      <div aria-hidden className="text-display leading-none tracking-[0.08em] text-fg tnum">
        {digits}
      </div>
      <div className="mt-2 text-label uppercase tracking-wider text-fg-faint">{label}</div>
      {note && <InstrumentNote>{note}</InstrumentNote>}
    </div>
  );
}

export function Gauge({
  label,
  value,
  max,
  formatted,
  href,
  note,
}: {
  label: string;
  value: number | null;
  max: number;
  formatted: string;
  href: string;
  note?: string | undefined;
}) {
  const safe = value === null || !Number.isFinite(value) ? 0 : Math.max(0, value);
  const ratio = max > 0 ? Math.min(safe / max, 1) : 0;
  const angle = -120 + ratio * 240;
  return (
    <div className="min-w-36 text-center">
      <Link
        to={href}
        className="focus-mark inline-flex min-h-6 flex-col items-center no-underline"
        aria-label={`${label}: ${value === null ? 'unavailable' : formatted}`}
      >
        <svg viewBox="0 0 160 112" width="160" height="112" aria-hidden>
          <path
            d="M 26.306 109 A 62 62 0 1 1 133.694 109"
            fill="none"
            className="stroke-rule"
            strokeWidth="2"
          />
          {Array.from({ length: 7 }, (_, index) => {
            const tickAngle = ((-120 + index * 40) * Math.PI) / 180;
            const x1 = 80 + Math.sin(tickAngle) * 62;
            const y1 = 78 - Math.cos(tickAngle) * 62;
            const x2 = 80 + Math.sin(tickAngle) * 54;
            const y2 = 78 - Math.cos(tickAngle) * 54;
            return <line key={index} x1={x1} y1={y1} x2={x2} y2={y2} className="stroke-fg-muted" />;
          })}
          <g
            className="transition-transform duration-300 motion-reduce:transition-none"
            style={{ transform: `rotate(${angle}deg)`, transformOrigin: '80px 78px' }}
          >
            <line
              x1="80"
              y1="78"
              x2="80"
              y2="30"
              className="stroke-fg"
              strokeWidth="2"
              strokeLinecap="round"
            />
          </g>
          <circle cx="80" cy="78" r="4" className="fill-fg" />
        </svg>
        <span className="text-title text-fg tnum">{value === null ? '—' : formatted}</span>
        <span className="text-label uppercase tracking-wider text-fg-faint">{label}</span>
      </Link>
      {note && <InstrumentNote>{note}</InstrumentNote>}
    </div>
  );
}

export function ActivityTrace({
  samples,
  available = true,
  note,
}: {
  samples: readonly number[];
  available?: boolean | undefined;
  note?: string | undefined;
}) {
  const values = samples.length > 0 ? samples : [0];
  const max = Math.max(1, ...values);
  const points = values
    .map((value, index) => {
      const x = values.length === 1 ? 0 : (index / (values.length - 1)) * 100;
      const y = 28 - (Math.max(0, value) / max) * 24;
      return `${x},${y}`;
    })
    .join(' ');
  const last = values.at(-1) ?? 0;
  const readingLabel = available
    ? `recent model activity: ${last} invocation${last === 1 ? '' : 's'} in the current window`
    : 'recent model activity: unavailable';
  return (
    <figure className="m-0" aria-label={`${readingLabel}${note ? `; ${note}` : ''}`}>
      <div className="mb-2 flex items-baseline justify-between gap-4">
        <figcaption className="text-label uppercase tracking-wider text-fg-faint">
          recent model activity
        </figcaption>
        <span className="text-label text-fg-muted tnum">
          {samples.length > 1 ? `${samples.length} samples` : 'collecting samples'}
        </span>
      </div>
      <svg
        viewBox="0 0 100 32"
        preserveAspectRatio="none"
        className="h-24 w-full border-y border-rule"
        aria-hidden
      >
        <line x1="0" y1="28" x2="100" y2="28" className="stroke-rule" strokeWidth="0.4" />
        <polyline
          points={points}
          fill="none"
          className="stroke-fg"
          strokeWidth="1.2"
          vectorEffect="non-scaling-stroke"
          strokeLinejoin="round"
        />
      </svg>
      {note && <InstrumentNote>{note}</InstrumentNote>}
    </figure>
  );
}

export function PipelineBar({
  segments,
  available = true,
}: {
  segments: readonly PipelineSegment[];
  available?: boolean | undefined;
}) {
  const widths = pipelineWidths(segments.map((segment) => segment.count));
  return (
    <div
      aria-label={`runs in flight: ${available ? 'current' : 'unavailable'}`}
      data-testid="pipeline"
    >
      <div className="flex h-3 gap-px overflow-hidden rounded-sm" aria-hidden>
        {segments.map((segment, index) => (
          <span
            key={segment.key}
            data-testid="pipeline-track-segment"
            className="block bg-fg transition-[width] duration-300 motion-reduce:transition-none"
            style={{ width: `${widths[index] ?? 0}%`, opacity: 0.2 + index * 0.2 }}
          />
        ))}
      </div>
      <div className="mt-2 flex flex-wrap gap-x-5 gap-y-1">
        {segments.map((segment) => (
          <Link
            key={segment.key}
            to={segment.href}
            aria-label={`${segment.label}: ${available ? segment.count : 'unavailable'}`}
            className="focus-mark inline-flex min-h-6 items-center gap-2 no-underline"
          >
            <span className="text-label uppercase tracking-wider text-fg-faint">
              {segment.label}
            </span>
            <span className="text-label text-fg tnum">{available ? segment.count : '—'}</span>
          </Link>
        ))}
      </div>
    </div>
  );
}

export interface ContextMeter {
  id: string;
  label: string;
  value: number;
  href: string;
}

export function ContextMeters({ meters }: { meters: readonly ContextMeter[] }) {
  return (
    <div className="flex min-h-40 flex-wrap items-end gap-3" data-testid="context-meters">
      {meters.map((meter) => {
        const value = Math.min(Math.max(meter.value, 0), 100);
        return (
          <Link
            key={meter.id}
            to={meter.href}
            className="focus-mark inline-flex min-h-6 w-14 flex-col items-center no-underline"
            aria-label={`${meter.label}: ${Math.round(value)}% context used`}
          >
            <span
              className="relative block h-28 w-10 overflow-hidden rounded-sm border border-rule"
              aria-hidden
            >
              <span
                className="absolute inset-x-0 bottom-0 bg-ok/60 transition-[height] duration-300 motion-reduce:transition-none"
                style={{ height: `${value}%` }}
              />
            </span>
            <span className="mt-1 w-14 truncate text-center text-label uppercase tracking-wider text-fg-faint">
              {meter.label}
            </span>
            <span className="text-label text-fg-muted tnum">{Math.round(value)}%</span>
          </Link>
        );
      })}
    </div>
  );
}

export function RunRings({ runs }: { runs: readonly RunRingModel[] }) {
  return (
    <div className="flex min-h-24 flex-wrap content-start gap-3" data-testid="run-rings">
      {runs.map((run) => {
        const circumference = 2 * Math.PI * 28;
        const progress = Math.min(Math.max(run.stage / Math.max(run.totalStages, 1), 0), 1);
        const retry = run.attempt !== undefined && run.attempt > 1;
        const retryLabel = retry ? `, retry attempt ${run.attempt}` : '';
        return (
          <Link
            key={run.id}
            to={run.href}
            className="focus-mark inline-flex min-h-6 w-20 flex-col items-center no-underline"
            aria-label={`${run.label}: stage ${run.stage} of ${run.totalStages}${retryLabel}`}
          >
            <span className="relative block h-20 w-20" aria-hidden>
              <svg viewBox="0 0 72 72" width="80" height="80">
                <circle
                  cx="36"
                  cy="36"
                  r="28"
                  fill="none"
                  className="stroke-rule"
                  strokeWidth="3"
                />
                <circle
                  cx="36"
                  cy="36"
                  r="28"
                  fill="none"
                  className="stroke-ok transition-[stroke-dashoffset] duration-300 motion-reduce:transition-none"
                  strokeWidth="3"
                  strokeDasharray={circumference}
                  strokeDashoffset={circumference * (1 - progress)}
                  transform="rotate(-90 36 36)"
                />
              </svg>
              <span className="absolute inset-0 flex flex-col items-center justify-center text-label text-fg tnum">
                {run.stage}/{run.totalStages}
                <span className={retry ? 'text-warn' : 'text-fg-faint'}>
                  {retry ? `retry ${run.attempt}` : run.stageWord}
                </span>
              </span>
            </span>
            <span className="w-20 truncate text-center text-label text-fg-muted">{run.label}</span>
          </Link>
        );
      })}
    </div>
  );
}

export type LampState = 'healthy' | 'warning' | 'unknown';
export interface StatusLamp {
  key: string;
  label: string;
  value: string;
  state: LampState;
  href: string;
}

export function StatusLamps({ lamps }: { lamps: readonly StatusLamp[] }) {
  return (
    <div className="space-y-2">
      {lamps.map((lamp) => (
        <Link
          key={lamp.key}
          to={lamp.href}
          className="focus-mark grid min-h-6 grid-cols-[12px_1fr] items-center gap-x-2 no-underline"
          aria-label={`${lamp.label}: ${lamp.state}, ${lamp.value}`}
        >
          <span
            aria-hidden
            className={`h-2.5 w-2.5 rounded-full border ${
              lamp.state === 'healthy'
                ? 'border-ok bg-ok/70'
                : lamp.state === 'warning'
                  ? 'border-warn bg-warn/70'
                  : 'border-rule bg-transparent'
            }`}
          />
          <span className="flex flex-wrap items-baseline justify-between gap-x-3">
            <span className="text-label uppercase tracking-wider text-fg-faint">{lamp.label}</span>
            <span className="text-label text-fg-muted">{lamp.value}</span>
          </span>
        </Link>
      ))}
    </div>
  );
}
