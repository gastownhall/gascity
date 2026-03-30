// Hand-maintained TypeScript mirrors for the orders-v2 contract artifact.
// Keep these aligned with the JSON Schemas and fixture validation script.

export type ScopeKind = "city" | "rig";
export type WatchResource = "project" | "session" | "workflow";

export interface OrdersFeedRequest {
  scope_kind: ScopeKind;
  scope_ref: string;
}

export interface FormulasListRequest extends OrdersFeedRequest {
  target: string;
}

export interface FormulasDetailRequest extends FormulasListRequest {
  formula: string;
  vars?: Record<string, string>;
  attached_bead_id?: string;
}

export interface WorkflowGetRequest extends OrdersFeedRequest {
  workflow_id: string;
}

export interface ValidationError {
  code: string;
  field?: string;
  message: string;
  details?: unknown;
}

export interface BrokerErrorPayload {
  code?: string;
  error?: string;
  message?: string;
  errors?: ValidationError[];
  [key: string]: unknown;
}

export interface SessionLink {
  project_id: string;
  session_id: string;
  session_name: string;
  assignee: string;
}

export interface VarDef {
  name: string;
  type: string;
  description?: string;
  required?: boolean;
  default?: unknown;
  enum?: string[];
  pattern?: string;
  derived_from?: string;
}

export interface FormulaRecentRun {
  workflow_id: string;
  status: string;
  target: string;
  started_at: string;
  updated_at: string;
}

export interface FormulaSummary {
  name: string;
  description: string;
  version: string;
  var_defs: VarDef[];
  run_count: number;
  recent_runs: FormulaRecentRun[];
}

export interface FormulaPreviewNode {
  id: string;
  title: string;
  kind: string;
  scope_ref?: string;
}

export interface FormulaPreviewEdge {
  from: string;
  to: string;
  kind?: string;
}

export interface FormulaDetail {
  name: string;
  description: string;
  version: string;
  var_defs: VarDef[];
  steps: Array<Record<string, unknown>>;
  deps: FormulaPreviewEdge[];
  preview: {
    nodes: FormulaPreviewNode[];
    edges: FormulaPreviewEdge[];
  };
}

export interface MonitorFeedItem {
  id: string;
  type: "exec" | "formula";
  status: string;
  title: string;
  scope_kind: ScopeKind;
  scope_ref: string;
  target: string;
  started_at: string;
  updated_at: string;
  bead_id?: string;
  detail_available?: boolean;
  workflow_id?: string;
  root_bead_id?: string;
  root_store_ref?: string;
  attached_bead_id?: string;
  logical_bead_id?: string;
  run_detail_available?: boolean;
}

export interface WorkflowBead {
  id: string;
  title: string;
  status: string;
  kind: string;
  step_ref?: string;
  attempt?: number;
  logical_bead_id?: string;
  scope_ref?: string;
  assignee?: string;
  metadata: Record<string, string>;
}

export interface WorkflowDep {
  from: string;
  to: string;
  kind?: string;
}

export interface LogicalAttempt {
  bead_id: string;
  status: string;
  attempt: number;
  started_at?: string;
  updated_at?: string;
  summary?: Record<string, unknown>;
}

export interface LogicalNode {
  id: string;
  title: string;
  kind: string;
  status: string;
  scope_ref?: string;
  current_bead_id?: string;
  attempt_badge?: string;
  attempt_count?: number;
  active_attempt?: number;
  attempts?: LogicalAttempt[];
  // Logical node metadata may include broker-enriched, non-string values.
  metadata: Record<string, unknown>;
  session_link?: SessionLink;
}

export interface LogicalEdge {
  from: string;
  to: string;
  kind?: string;
}

export interface ScopeGroup {
  scope_ref: string;
  label: string;
  member_logical_node_ids: string[];
  layout_hint?: Record<string, unknown>;
}

export interface WorkflowSnapshot {
  workflow_id: string;
  root_bead_id: string;
  root_store_ref: string;
  scope_kind: ScopeKind;
  scope_ref: string;
  beads: WorkflowBead[];
  deps: WorkflowDep[];
  logical_nodes: LogicalNode[];
  logical_edges: LogicalEdge[];
  scope_groups: ScopeGroup[];
  partial: boolean;
  resolved_root_store: string;
  stores_scanned: string[];
  snapshot_version: number;
  snapshot_event_seq?: number;
}

export interface WorkflowEvent {
  workflow_id: string;
  root_bead_id: string;
  root_store_ref: string;
  scope_kind: ScopeKind;
  scope_ref: string;
  watch_generation: string;
  event_seq: number;
  workflow_seq: number;
  event_ts: string;
  event_type: string;
  bead: WorkflowBead;
  changed_fields: string[];
  deps?: WorkflowDep[];
  logical_node_id: string;
  attempt_summary?: Record<string, unknown>;
  scope_group_delta?: Record<string, unknown>;
  requires_resync?: boolean;
}

export interface WorkflowEventFrame extends WorkflowEvent {
  type: "workflow:event";
}

export interface WatchResult {
  type: "watch:result";
  resource: WatchResource;
  id: string;
  ok: boolean;
}

export interface WorkflowWatchReady {
  type: "workflow:watch_ready";
  workflow_id: string;
  watch_generation: string;
  installed_after_workflow_seq: number;
  installed_after_event_seq?: number;
}

export interface WorkflowResyncRequired {
  type: "workflow:resync_required";
  workflow_id: string;
  watch_generation: string;
  reason: string;
}

export interface OrdersFeedRefresh extends OrdersFeedRequest {
  type: "orders:feed:refresh";
}

export interface OrdersV2ContractHandshake {
  contract_version: string;
  artifact_hash: string;
}

export interface OrdersV2Capabilities {
  monitor: boolean;
  launch: boolean;
  workflow_snapshot: boolean;
  workflow_live: boolean;
  pool_targets: boolean;
  bead_picker: boolean;
  orders_v2_contract: OrdersV2ContractHandshake;
}

export interface OrdersV2CapabilitiesPayload {
  capabilities_status: "unknown" | "ready" | "stale";
  capabilities: {
    orders: boolean;
    orders_v2: OrdersV2Capabilities;
    [key: string]: unknown;
  };
}

export interface MutateResult {
  type: "mutate:result";
  id: string;
  ok: boolean;
  payload?: unknown;
  error?: string;
  status?: number;
  confirmToken?: string;
  errorPayload?: BrokerErrorPayload;
}

export interface OrdersLaunchMutateResult extends Omit<MutateResult, "ok" | "payload"> {
  type: "mutate:result";
  id: string;
  ok: true;
  payload: OrdersLaunchResponse;
}

// Deprecated compatibility interfaces. OrdersLaunch* is the canonical
// sling-backed launch shape, but older consumers may still import the
// FormulasExecute* names from the shared contract.
export interface FormulasExecuteMutateResult extends OrdersLaunchMutateResult {}

export interface OrdersLaunchRequest {
  scope_kind: ScopeKind;
  scope_ref: string;
  target: string;
  formula: string;
  vars: Record<string, string>;
  attached_bead_id?: string;
}

export interface FormulasExecuteRequest extends OrdersLaunchRequest {}

export interface OrdersLaunchResponse {
  workflow_id: string;
  root_bead_id: string;
  root_store_ref: string;
  attached_bead_id?: string;
  mode: "standalone" | "attached";
  scope_kind: ScopeKind;
  scope_ref: string;
}

export interface FormulasExecuteResponse extends OrdersLaunchResponse {}
