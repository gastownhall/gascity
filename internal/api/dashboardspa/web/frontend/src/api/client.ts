import type {
  GitCommitList,
  GitView,
  DeployList,
  SystemHealth,
  LocalToolVersions,
  DoltNomsTrend,
  RigStoreHealthReport,
  SupervisorStatusReport,
  ApiError,
  DashboardRuntimeConfig,
  RunSummary,
  FormulaRunDetail,
} from 'gas-city-dashboard-shared';
import { cityPath } from './cityBase';

// Typed fetch client for the admin backend's API. Shares types with the
// backend via the workspace 'gas-city-dashboard-shared' import so wire-shape
// drift produces compile errors instead of runtime undefined.
//
// gascity-dashboard-ucc: the request plane is split. City-scoped reads/writes
// address `/api/city/:cityName/*` via `cityPath()` (the active city is set by
// the router from the URL segment). Non-city dashboard-service endpoints —
// health, csrf, client-error telemetry, git, builds — address `/api/*`
// directly because they are dashboard-local, not GC-owned supervisor resources.

async function performRequest<T>(
  method: 'GET' | 'POST',
  url: string,
  decode: ResponseDecoder<T>,
  body?: object,
): Promise<T> {
  const headers: Record<string, string> = {
    Accept: 'application/json',
  };
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  if (method !== 'GET') {
    // Same-origin custom-header CSRF. The supervisor (and its host-side /api
    // plane) accept any non-empty X-GC-Request on mutations: a cross-site form
    // cannot set a custom header without a CORS preflight, so its presence
    // proves the request came from our own JS. This mirrors the supervisor
    // client's GC_MUTATION_HEADERS, so the SPA uses ONE CSRF model across the
    // typed /v0 surface and the /api plane (no cookie double-submit).
    headers['X-GC-Request'] = 'dashboard';
  }
  const init: RequestInit = {
    method,
    headers,
    credentials: 'same-origin',
  };
  if (body !== undefined) init.body = JSON.stringify(body);
  const res = await fetch(url, init);
  if (!res.ok) {
    const bodyText = await res.text();
    const payload = parseApiErrorBody(bodyText);
    const message = payload?.error ?? (bodyText.trim() || res.statusText || `HTTP ${res.status}`);
    throw new ApiClientError(res.status, message, payload?.kind, payload?.reason);
  }
  let json: unknown;
  try {
    json = await res.json();
  } catch (err) {
    throw new ApiResponseDecodeError(url, `body must be valid JSON: ${unknownMessage(err)}`);
  }
  return decode(json, url);
}

function parseApiErrorBody(bodyText: string): ApiError | undefined {
  if (bodyText.trim().length === 0) return undefined;
  try {
    const parsed = JSON.parse(bodyText) as unknown;
    return isApiError(parsed) ? parsed : undefined;
  } catch {
    return undefined;
  }
}

function isApiError(value: unknown): value is ApiError {
  if (typeof value !== 'object' || value === null) return false;
  const record = value as Record<string, unknown>;
  if (typeof record.error !== 'string') return false;
  if (record.kind !== undefined && typeof record.kind !== 'string') return false;
  return record.reason === undefined || typeof record.reason === 'string';
}

// The /api plane uses the same-origin custom-header CSRF model (X-GC-Request),
// so there is no cookie bootToken to rotate and no /api/csrf self-heal: every
// mutation simply carries the header set in performRequest.
async function request<T>(
  method: 'GET' | 'POST',
  url: string,
  decode: ResponseDecoder<T>,
  body?: object,
): Promise<T> {
  return performRequest<T>(method, url, decode, body);
}

type JsonRecord = Record<string, unknown>;
type ResponseDecoder<T> = (value: unknown, url: string) => T;

export class ApiClientError extends Error {
  constructor(
    public readonly status: number,
    message: string,
    public readonly kind?: string,
    // The BFF run-detail discriminator (see ApiError.reason): 'not_run_view'
    // vs 'invalid_snapshot' on a 422. Absent on every other endpoint.
    public readonly reason?: string,
  ) {
    super(message);
    this.name = 'ApiClientError';
  }
}

export class ApiResponseDecodeError extends Error {
  constructor(
    public readonly url: string,
    public readonly detail: string,
  ) {
    super(`Invalid API response for ${url}: ${detail}`);
    this.name = 'ApiResponseDecodeError';
  }
}

function unknownMessage(err: unknown): string {
  if (err instanceof Error) return err.message;
  if (typeof err === 'string') return err;
  return 'unknown error';
}

function failDecode(url: string, detail: string): never {
  throw new ApiResponseDecodeError(url, detail);
}

function isRecord(value: unknown): value is JsonRecord {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function requireRecord(value: unknown, url: string, label: string): JsonRecord {
  if (!isRecord(value)) failDecode(url, `${label} must be an object`);
  return value;
}

function requireStringField(record: JsonRecord, url: string, label: string, field: string): void {
  if (typeof record[field] !== 'string') failDecode(url, `${label}.${field} must be a string`);
}

function requireNullableStringField(
  record: JsonRecord,
  url: string,
  label: string,
  field: string,
): void {
  const value = record[field];
  if (value !== null && typeof value !== 'string') {
    failDecode(url, `${label}.${field} must be a string or null`);
  }
}

function requireBooleanField(record: JsonRecord, url: string, label: string, field: string): void {
  if (typeof record[field] !== 'boolean') failDecode(url, `${label}.${field} must be a boolean`);
}

function requireNumberField(record: JsonRecord, url: string, label: string, field: string): void {
  if (typeof record[field] !== 'number') failDecode(url, `${label}.${field} must be a number`);
}

function requireArrayField(record: JsonRecord, url: string, label: string, field: string): void {
  if (!Array.isArray(record[field])) failDecode(url, `${label}.${field} must be an array`);
}

function requireObjectField(record: JsonRecord, url: string, label: string, field: string): void {
  requireRecord(record[field], url, `${label}.${field}`);
}

function requireStringArrayOrNullField(
  record: JsonRecord,
  url: string,
  label: string,
  field: string,
): void {
  const value = record[field];
  if (value === null) return;
  if (!Array.isArray(value) || value.some((item) => typeof item !== 'string')) {
    failDecode(url, `${label}.${field} must be an array of strings or null`);
  }
}

function objectDecoder<T>(
  label: string,
  validate?: (record: JsonRecord, url: string) => void,
): ResponseDecoder<T> {
  return (value, url) => {
    const record = requireRecord(value, url, label);
    validate?.(record, url);
    return record as T;
  };
}

function itemsDecoder<T>(
  label: string,
  validate?: (record: JsonRecord, url: string) => void,
): ResponseDecoder<T> {
  return objectDecoder<T>(label, (record, url) => {
    requireArrayField(record, url, label, 'items');
    validate?.(record, url);
  });
}

const decodeHealth = objectDecoder<{ ok: boolean; ts: string }>('health', (record, url) => {
  requireBooleanField(record, url, 'health', 'ok');
  requireStringField(record, url, 'health', 'ts');
});

const decodeCommitList = itemsDecoder<GitCommitList>('commits', (record, url) => {
  requireStringField(record, url, 'commits', 'view');
});
const decodeBuildList = itemsDecoder<DeployList>('builds', (record, url) => {
  requireNullableStringField(record, url, 'builds', 'source');
  requireBooleanField(record, url, 'builds', 'failed_marker');
});
const decodeRuntimeConfig = objectDecoder<DashboardRuntimeConfig>('config', (record, url) => {
  requireStringField(record, url, 'config', 'cityName');
  requireStringField(record, url, 'config', 'cityRoot');
  requireBooleanField(record, url, 'config', 'useFixtures');
  requireBooleanField(record, url, 'config', 'readOnly');
  requireStringField(record, url, 'config', 'operatorAlias');
  requireStringField(record, url, 'config', 'operatorWireAlias');
  requireStringField(record, url, 'config', 'decisionLabel');
  requireStringArrayOrNullField(record, url, 'config', 'enabledModules');
  requireNullableStringField(record, url, 'config', 'defaultView');
});
const healthMetricUnavailableReasons = new Set([
  'sample_failed',
  'invalid_sample',
  'value_overflow',
]);

function requireHealthMetricField(
  record: JsonRecord,
  url: string,
  label: string,
  field: string,
  validateValue: (value: unknown, url: string, label: string) => void,
): void {
  const metric = requireRecord(record[field], url, `${label}.${field}`);
  requireStringField(metric, url, `${label}.${field}`, 'status');
  if (metric.status === 'available') {
    validateValue(metric.value, url, `${label}.${field}.value`);
    return;
  }
  if (metric.status !== 'unavailable') {
    failDecode(url, `${label}.${field}.status must be available or unavailable`);
  }
  requireStringField(metric, url, `${label}.${field}`, 'reason');
  if (!healthMetricUnavailableReasons.has(metric.reason as string)) {
    failDecode(url, `${label}.${field}.reason is not recognized`);
  }
}

function requireNumberValue(value: unknown, url: string, label: string): void {
  if (typeof value !== 'number') failDecode(url, `${label} must be a number`);
}

const decodeSystemHealth = objectDecoder<SystemHealth>('system health', (record, url) => {
  const admin = requireRecord(record.admin, url, 'system health.admin');
  const host = requireRecord(record.host, url, 'system health.host');
  requireNumberField(admin, url, 'system health.admin', 'pid');
  requireNumberField(admin, url, 'system health.admin', 'uptime_sec');
  requireNumberField(admin, url, 'system health.admin', 'heap_used_bytes');
  requireStringField(admin, url, 'system health.admin', 'node_version');
  requireHealthMetricField(admin, url, 'system health.admin', 'rss', requireNumberValue);

  requireNumberField(host, url, 'system health.host', 'cpu_count');
  requireHealthMetricField(host, url, 'system health.host', 'uptime', requireNumberValue);
  requireHealthMetricField(host, url, 'system health.host', 'load', (value, metricURL, label) => {
    const load = requireRecord(value, metricURL, label);
    requireNumberField(load, metricURL, label, 'load_avg_1');
    requireNumberField(load, metricURL, label, 'load_avg_5');
    requireNumberField(load, metricURL, label, 'load_avg_15');
  });
  requireHealthMetricField(host, url, 'system health.host', 'memory', (value, metricURL, label) => {
    const memory = requireRecord(value, metricURL, label);
    requireNumberField(memory, metricURL, label, 'total_mem_bytes');
    requireNumberField(memory, metricURL, label, 'free_mem_bytes');
  });
});
function requireLocalToolVersionField(
  record: JsonRecord,
  url: string,
  label: string,
  field: string,
): void {
  requireObjectField(record, url, label, field);
  // The Health renderer branches on each tool's `status`, so validate it here at
  // the edge rather than letting a malformed wire value mis-render.
  const tool = record[field] as JsonRecord;
  const toolLabel = `${label}.${field}`;
  requireStringField(tool, url, toolLabel, 'status');
}

const decodeLocalToolVersions = objectDecoder<LocalToolVersions>(
  'local tool versions',
  (record, url) => {
    requireLocalToolVersionField(record, url, 'local tool versions', 'dolt');
    requireLocalToolVersionField(record, url, 'local tool versions', 'beads');
    requireLocalToolVersionField(record, url, 'local tool versions', 'gc');
  },
);
const decodeDoltTrend = objectDecoder<DoltNomsTrend>('dolt trend', (record, url) => {
  requireBooleanField(record, url, 'dolt trend', 'available');
  requireArrayField(record, url, 'dolt trend', 'samples');
});
const decodeRigStoreHealth = objectDecoder<RigStoreHealthReport>(
  'rig store health',
  (record, url) => {
    requireBooleanField(record, url, 'rig store health', 'available');
    requireArrayField(record, url, 'rig store health', 'rigs');
  },
);
// The Health Dolt/Beads/threshold widgets dereference status.work.{open,ready,
// in_progress} unconditionally (store_health is optional and the widget guards
// its absence), so validate the nested shape the widgets actually read at the
// edge, not at render. Both the fresh and the degraded-with-last-good paths
// surface status to those widgets, so both validate it.
function requireStatusBody(value: unknown, url: string): void {
  const status = requireRecord(value, url, 'supervisor status.status');
  requireObjectField(status, url, 'supervisor status.status', 'work');
}
const decodeSupervisorStatus = objectDecoder<SupervisorStatusReport>(
  'supervisor status',
  (record, url) => {
    requireBooleanField(record, url, 'supervisor status', 'available');
    if (record['available'] === true) {
      requireStringField(record, url, 'supervisor status', 'sampledAt');
      requireStatusBody(record['status'], url);
    } else {
      requireStringField(record, url, 'supervisor status', 'reason');
      if (record['status'] !== null) {
        requireStatusBody(record['status'], url);
      }
    }
  },
);
// The run summary/detail DTOs are produced by the Go run projection
// (internal/runproj), which is golden-gated byte-for-byte against these exact
// shapes. Validate the structural arrays/objects the renderers iterate at the
// edge so a wire-shape regression fails here rather than mis-rendering deep in a
// lane or diagram component.
const decodeRunSummary = objectDecoder<RunSummary>('run summary', (record, url) => {
  // Validate every field a renderer dereferences: RunMap reads the counts,
  // the lane arrays, and totalActive/totalHistorical. The DTO is golden-gated
  // against the Go projection, so this edge check is defensive — but it is now
  // the ONLY backstop (the client-side fold that used to rebuild this is gone).
  requireNumberField(record, url, 'run summary', 'totalActive');
  requireNumberField(record, url, 'run summary', 'totalHistorical');
  requireArrayField(record, url, 'run summary', 'lanes');
  requireArrayField(record, url, 'run summary', 'historicalLanes');
  requireArrayField(record, url, 'run summary', 'blockedLanes');
  requireArrayField(record, url, 'run summary', 'recentChanges');
  requireObjectField(record, url, 'run summary', 'runCounts');
  requireObjectField(record, url, 'run summary', 'census');
});
export const decodeFormulaRunDetail = objectDecoder<FormulaRunDetail>(
  'formula run detail',
  (record, url) => {
    requireStringField(record, url, 'formula run detail', 'runId');
    requireObjectField(record, url, 'formula run detail', 'formula');
    requireObjectField(record, url, 'formula run detail', 'formulaDetail');
    requireObjectField(record, url, 'formula run detail', 'executionPath');
    // The detail renderer hard-derefs these union/nested fields (snapshotLabel
    // reads snapshotEventSeq.kind, the partial notice reads completeness.kind,
    // the status summary reads progress.statusCounts[...]), so validate them at
    // the edge rather than let a malformed wire value throw deep in the diagram.
    requireObjectField(record, url, 'formula run detail', 'snapshotEventSeq');
    requireObjectField(record, url, 'formula run detail', 'completeness');
    const progress = requireRecord(record['progress'], url, 'formula run detail.progress');
    requireObjectField(progress, url, 'formula run detail.progress', 'statusCounts');
    requireArrayField(record, url, 'formula run detail', 'stages');
    requireArrayField(record, url, 'formula run detail', 'nodes');
    requireArrayField(record, url, 'formula run detail', 'edges');
    requireArrayField(record, url, 'formula run detail', 'lanes');
  },
);
export interface ApiErrorParts {
  message: string;
  status?: number;
  kind?: string;
}

export function apiErrorParts(err: unknown, fallback = 'request failed'): ApiErrorParts {
  if (err instanceof ApiClientError) {
    const parts: ApiErrorParts = { message: err.message, status: err.status };
    if (err.kind !== undefined) parts.kind = err.kind;
    return parts;
  }
  if (err instanceof Error) return { message: err.message };
  return { message: fallback };
}

export function formatApiError(err: unknown, fallback = 'request failed'): string {
  const parts = apiErrorParts(err, fallback);
  return parts.status === undefined ? parts.message : `${parts.status} ${parts.message}`;
}

export const api = {
  // ── Non-city (supervisor / host-global) endpoints ──────────────────────
  health(): Promise<{ ok: boolean; ts: string }> {
    return request('GET', '/api/health', decodeHealth);
  },
  listCommits(view: GitView): Promise<GitCommitList> {
    return request('GET', `/api/git/commits?view=${encodeURIComponent(view)}`, decodeCommitList);
  },
  listBuilds(): Promise<DeployList> {
    return request('GET', '/api/builds', decodeBuildList);
  },

  // ── City-scoped endpoints (ride /api/city/:cityName/*) ─────────────────
  config(): Promise<DashboardRuntimeConfig> {
    return request('GET', cityPath('/config'), decodeRuntimeConfig);
  },
  systemHealth(): Promise<SystemHealth> {
    return request('GET', '/api/health/system', decodeSystemHealth);
  },
  localToolVersions(): Promise<LocalToolVersions> {
    return request('GET', '/api/health/local-tools', decodeLocalToolVersions);
  },
  doltTrend(): Promise<DoltNomsTrend> {
    return request('GET', cityPath('/dolt-noms/trend'), decodeDoltTrend);
  },
  rigStoreHealth(): Promise<RigStoreHealthReport> {
    return request('GET', cityPath('/rig-store-health'), decodeRigStoreHealth);
  },
  supervisorStatus(): Promise<SupervisorStatusReport> {
    return request('GET', cityPath('/supervisor-status'), decodeSupervisorStatus);
  },
  // The run view reads its summary and per-run detail from the BFF run
  // projection (internal/api/dashboardbff/runtailer.go), a sub-second warm
  // fold of the city event log that already layers session health/census.
  // Both DTOs are the same shapes the SPA used to reconstruct client-side.
  runSummary(): Promise<RunSummary> {
    return request('GET', cityPath('/runs/summary'), decodeRunSummary);
  },
  // 200 → FormulaRunDetail. The endpoint rejects a non-graph.v2 run with
  // 422 + reason 'not_run_view' (list-only) or 'invalid_snapshot' (load
  // failure), an unknown run with 404, and a still-warming projection with
  // 503 — surfaced to callers as ApiClientError (status + reason).
  runDetail(runId: string): Promise<FormulaRunDetail> {
    return request(
      'GET',
      cityPath(`/runs/${encodeURIComponent(runId)}/detail`),
      decodeFormulaRunDetail,
    );
  },
  // The per-run SSE detail stream (BFF plane). It pushes the whole
  // FormulaRunDetail as a snapshot frame on connect and again whenever the
  // fold changes this run's bytes, so the client renders each frame with zero
  // refetch. Same-origin path off cityPath — the browser resolves it against
  // the current origin, matching the credentials the GET uses.
  runDetailStreamUrl(runId: string): string {
    return cityPath(`/runs/${encodeURIComponent(runId)}/detail/stream`);
  },
};
