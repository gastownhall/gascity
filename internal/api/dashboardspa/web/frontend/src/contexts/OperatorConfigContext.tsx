import { createContext, useContext, type ReactNode } from 'react';
import type { DashboardRuntimeConfig } from 'gas-city-dashboard-shared';

// Operator identity surfaced to the SPA from `/config`
// (gascity-dashboard-bhvn / zero-hardcoded-roles). The dashboard is a SHARED
// tool used by others, so it must NOT bake our operator into source. The
// backend resolves the operator from its env-driven config and projects it onto
// the wire (DashboardRuntimeConfig); the frontend reads it here instead of
// importing a hardcoded operator alias / wire alias / decision-label literal.
//
// This is the frontend's config edge — the ONE place a neutral fallback literal
// is allowed to live (parallel to the backend's config boot edge), for the
// sub-second window before `/config` is decoded. The fallback is intentionally
// non-identifying: it never assumes a specific human. Mutating actions (mail
// send, claim) happen well after config lands, so the fallback only ever backs
// mount-time reads, which self-correct when the real config arrives.

export interface OperatorConfig {
  /** Display + bead-assignee identity (DASHBOARD_OPERATOR_ALIAS). */
  readonly operatorAlias: string;
  /** gc mail-wire identity — mail is addressed to/from this, not the display name. */
  readonly operatorWireAlias: string;
  /** The mayor-decision-queue marker label (DASHBOARD_DECISION_LABEL). */
  readonly decisionLabel: string;
}

const FALLBACK_OPERATOR_CONFIG: OperatorConfig = {
  operatorAlias: 'operator',
  operatorWireAlias: 'human',
  decisionLabel: 'needs/operator',
};

const OperatorConfigContext = createContext<OperatorConfig>(FALLBACK_OPERATOR_CONFIG);

export function OperatorConfigProvider({
  operator,
  children,
}: {
  operator: OperatorConfig;
  children: ReactNode;
}) {
  return (
    <OperatorConfigContext.Provider value={operator}>{children}</OperatorConfigContext.Provider>
  );
}

/** The operator identity resolved from `/config` (or the neutral fallback). */
export function useOperatorConfig(): OperatorConfig {
  return useContext(OperatorConfigContext);
}

/**
 * Resolve the SPA's operator identity from the `/config` fetch state. Until the
 * config is decoded (in flight, or a fetch/decode error) the neutral fallback
 * applies — it never assumes a specific human and self-corrects the moment the
 * real config lands. The backend always emits these fields, so a decoded config
 * is the steady state.
 */
export function resolveOperatorConfig(config: DashboardRuntimeConfig | undefined): OperatorConfig {
  if (config === undefined) return FALLBACK_OPERATOR_CONFIG;
  return {
    operatorAlias: config.operatorAlias,
    operatorWireAlias: config.operatorWireAlias,
    decisionLabel: config.decisionLabel,
  };
}
