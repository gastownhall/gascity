import {
  Suspense,
  lazy,
  useMemo,
  type ComponentType,
  type LazyExoticComponent,
  type ReactNode,
} from 'react';
import { Routes, Route, Navigate, useLocation } from 'react-router-dom';
import { api } from './api/client';
import { AttentionProvider } from './attention/context';
import { useLiveAttentionContributors } from './attention/liveContributors';
import { ErrorBoundary } from './components/ErrorBoundary';
import { Layout } from './components/Layout';
import { NowProvider } from './contexts/NowContext';
import {
  OperatorConfigProvider,
  resolveOperatorConfig,
  type OperatorConfig,
} from './contexts/OperatorConfigContext';
import { ReadOnlyProvider, resolveReadOnly } from './contexts/ReadOnlyContext';
import { ViewingAsProvider } from './contexts/ViewingAsContext';
import { useCachedData } from './hooks/useCachedData';
import { RunSummaryProvider, useRunSummary } from './runs/runSummarySubscription';
import { ALL_VIEWS } from './views/registry';
import { filterEnabledViews, resolveDefaultViewWithLogging } from './views/resolve';
import type { FrontendViewDescriptor } from './views/types';

const AgentsPage = lazy(() => import('./routes/Agents').then((m) => ({ default: m.AgentsPage })));
const AgentDetailPage = lazy(() =>
  import('./routes/AgentDetail').then((m) => ({ default: m.AgentDetailPage })),
);
const CockpitHomePage = lazy(() =>
  import('./routes/CockpitHome').then((m) => ({ default: m.CockpitHomePage })),
);
const BeadsPage = lazy(() => import('./routes/Beads').then((m) => ({ default: m.BeadsPage })));
const MailPage = lazy(() => import('./routes/Mail').then((m) => ({ default: m.MailPage })));
const FormulaRunDetailPage = lazy(() =>
  import('./routes/FormulaRunDetail').then((m) => ({ default: m.FormulaRunDetailPage })),
);
const RunsPage = lazy(() => import('./routes/Runs').then((m) => ({ default: m.RunsPage })));

export function App() {
  // NowProvider lives at the App root because useFaviconSignal (R8) is
  // mounted inside the L0 ambient home but the favicon swap must persist
  // across routes — a future refactor that mounts the signal on every
  // route stays straightforward because the 1s tick is already global.

  // PR-C: the backend's /api/config carries the operator's MODULES_ENABLED
  // intersection and the DEFAULT_VIEW env value. While the request is in
  // flight `data` is undefined; we treat that as core-only, matching the
  // steady-state default install and preventing disabled first-party modules
  // from flashing or fetching before config lands.
  const { data: config, error: configError } = useCachedData('config', () => api.config());
  const enabledModules = config?.enabledModules ?? null;
  const defaultViewEnv = config?.defaultView ?? null;
  // Read-only posture is fail-closed on a config-fetch error and writable only
  // while the first fetch is in flight — see resolveReadOnly. The server proxy
  // gate stays the real enforcement throughout.
  const readOnly = resolveReadOnly(config, configError);
  // Operator identity from /config (gascity-dashboard-bhvn). Resolved once and
  // shared by the AttentionRoot (which computes contributors under the
  // RunSummaryProvider) and the OperatorConfigProvider below.
  const operator = resolveOperatorConfig(config);

  const enabledViews = useMemo(
    () => filterEnabledViews(ALL_VIEWS, enabledModules),
    [enabledModules],
  );
  const defaultResolution = useMemo(
    () => resolveDefaultViewWithLogging(enabledViews, defaultViewEnv),
    [enabledViews, defaultViewEnv],
  );
  const DefaultViewElement = defaultResolution.view?.element ?? null;
  // dw8 — when `DEFAULT_VIEW` resolves to a view alias (e.g. `needs-you`),
  // the resolver returns no `view` but a `redirectTo` path. Render
  // `<Navigate replace>` at `/` so the URL bar shows the parametrised
  // path the operator is actually viewing; `replace` keeps the back
  // button pointing at wherever the operator came from, not at the
  // resolved alias hop.
  const defaultRedirectTo = defaultResolution.redirectTo ?? null;

  return (
    <OperatorConfigProvider operator={operator}>
      <ViewingAsProvider>
        <NowProvider>
          <ReadOnlyProvider readOnly={readOnly}>
            <RunSummaryProvider>
              <AttentionRoot operator={operator}>
                <Layout>
                  {configError !== null && <ConfigErrorBanner message={configError} />}
                  <RoutedMain
                    defaultRedirectTo={defaultRedirectTo}
                    DefaultViewElement={DefaultViewElement}
                    enabledViews={enabledViews}
                  />
                </Layout>
              </AttentionRoot>
            </RunSummaryProvider>
          </ReadOnlyProvider>
        </NowProvider>
      </ViewingAsProvider>
    </OperatorConfigProvider>
  );
}

/**
 * Compute the live attention contributors under the RunSummaryProvider so the
 * Runs badge reads the same shared run-summary source the /runs page renders
 * (gascity-dashboard-2j8e.7), then expose the composed model to the tree.
 */
function AttentionRoot({ operator, children }: { operator: OperatorConfig; children: ReactNode }) {
  const { source } = useRunSummary();
  const contributors = useLiveAttentionContributors(operator, source);
  return <AttentionProvider contributors={contributors}>{children}</AttentionProvider>;
}

/**
 * The routed main column, wrapped in an inner ErrorBoundary so a crashed route
 * degrades in place while the Header, nav, and city-switcher stay live
 * (gascity-dashboard-ux-no-per-route-error-boundary). The root boundary in
 * main.tsx remains the last-resort catch-all. The boundary's `key` is tied to
 * the location pathname so navigating away from a crashed route remounts a
 * fresh boundary instead of leaving the operator stuck on the fallback.
 */
function RoutedMain({
  defaultRedirectTo,
  DefaultViewElement,
  enabledViews,
}: {
  defaultRedirectTo: string | null;
  DefaultViewElement: LazyExoticComponent<ComponentType> | null;
  enabledViews: ReadonlyArray<FrontendViewDescriptor>;
}) {
  const { pathname } = useLocation();
  return (
    <ErrorBoundary key={pathname}>
      <Suspense fallback={null}>
        <Routes>
          {/* `/` resolution (PRD §6 / bead 9yj.5):
              DEFAULT_VIEW env → descriptor `defaultRoute: true` →
              live cockpit home fallback. The resolver runs once per
              enabled-set / env change; warnings surface in the
              browser console for premortem #5 visibility. */}
          <Route
            path="/"
            element={
              defaultRedirectTo !== null ? (
                <Navigate to={defaultRedirectTo} replace />
              ) : DefaultViewElement !== null ? (
                <DefaultViewElement />
              ) : (
                <CockpitHomePage />
              )
            }
          />
          <Route path="/agents" element={<AgentsPage />} />
          <Route path="/agents/:slug" element={<AgentDetailPage />} />
          <Route path="/beads" element={<BeadsPage />} />
          <Route path="/runs" element={<RunsPage />} />
          <Route path="/runs/:runId" element={<FormulaRunDetailPage />} />
          <Route path="/mail" element={<MailPage />} />
          {/* Modular-dashboard registry routes, filtered by the
              backend's enabledModules set. A disabled module's path
              is absent so deep-link bookmarks surface the operator's
              MODULES_ENABLED change as the explicit catch-all route. */}
          {enabledViews.map((v) => {
            const Element = v.element;
            return <Route key={v.id} path={v.path} element={<Element />} />;
          })}
          <Route path="*" element={<NotFoundPage />} />
        </Routes>
      </Suspense>
    </ErrorBoundary>
  );
}

/**
 * Top-of-app banner for a failed `/config` fetch. Read-only posture already
 * fails closed on this error (resolveReadOnly), but the operator should also
 * see WHY the dashboard is degraded rather than guessing from disabled
 * controls. Rendered inside the main column so the Header and nav stay live.
 */
function ConfigErrorBanner({ message }: { message: string }) {
  return (
    <section
      role="alert"
      className="mb-8 border border-warn/40 rounded-sm px-4 py-3 text-body text-fg-muted"
    >
      <span className="text-warn">config unavailable:</span> {message} · some controls may be
      disabled until it loads.
    </section>
  );
}

function NotFoundPage() {
  return (
    <section aria-labelledby="not-found-title" className="space-y-3">
      <h1 id="not-found-title" className="text-5xl font-semibold tracking-tight text-fg">
        Page not found
      </h1>
      <p className="text-title text-fg-muted">No dashboard route matches this path.</p>
    </section>
  );
}
