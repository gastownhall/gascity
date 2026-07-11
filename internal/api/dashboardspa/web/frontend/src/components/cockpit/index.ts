// The cockpit surface consumed by the home route (routes/CockpitHome.tsx): the
// DOM/SVG instruments, the shared numeric formatters, the pure source→instrument
// derivations, and the live-telemetry hook. This barrel exports exactly what the
// page imports — instrument-internal helpers (odometerColumns, fillPct,
// pipelineWidths, ringDashoffset, the arc/scopeBuffer/ink modules) are imported
// from their own module files by the components and their tests, not re-exported
// here. Every instrument deep-links and carries status by shape and position,
// never by the maroon accent (reserved for the needs-you strip).

export { Gauge } from './Gauge';
export { Odometer } from './Odometer';
export { PipelineBar, type PipelineSegment } from './PipelineBar';
export { VUBank, type VUMeter } from './VUBank';
export { RunRings, type RunRing } from './RunRings';
export { StatusLamps, type StatusLamp } from './StatusLamps';
export { Oscilloscope } from './Oscilloscope';

export { LiveChip } from './LiveChip';
export { NeedsYouStrip } from './NeedsYouStrip';
export { ColumnHeading, DialCell, Microcopy } from './layout';

export { agoStr, connLabel, fmtCompact, fmtCost, fmtCount, fmtRate } from './format';

export { computeUsageMetrics, laneToRing, pipelineSegments, statusLamps } from './derive';
export { buildVuMeters } from './roster';
export { useCockpitTelemetry, type FeedStats } from './useCockpitTelemetry';
