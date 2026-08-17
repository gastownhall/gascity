import { useMemo } from 'react';
import { NavLink, useLocation } from 'react-router-dom';
import { api } from '../api/client';
import { getActiveCity } from '../api/cityBase';
import { NavAttentionIndicator } from '../attention/NavAttentionIndicator';
import { useAttentionModel } from '../attention/context';
import type { AttentionDomain } from '../attention/compose';
import { useTheme } from '../contexts/ThemeContext';
import { useOperatorConfig } from '../contexts/OperatorConfigContext';
import { READ_ONLY_CONTROL_TITLE, useReadOnly } from '../contexts/ReadOnlyContext';
import { useViewingAs } from '../contexts/ViewingAsContext';
import { displayLabel } from '../hooks/aliasPriority';
import { useCachedData } from '../hooks/useCachedData';
import { supervisorApi } from '../supervisor/client';
import { ALL_VIEWS } from '../views/registry';
import { filterEnabledViews } from '../views/resolve';

interface NavRoute {
  to: string;
  label: string;
  end?: boolean;
  order: number;
}

// Hand-maintained routes for the views that PR-A has NOT yet ported to
// the modular registry. Each entry carries an explicit `order` so the
// registry-driven entries (currently /health at 60) interleave cleanly
// here without a separate "where does Health go" decision. PR-B+ will
// move entries out of this list as they land in ALL_VIEWS.
const EXPLICIT_ROUTES: ReadonlyArray<NavRoute> = [
  // gascity-dashboard-kb3: Home is the L0 ambient page at `/`.
  // `end: true` so the NavLink active-state matches `/` exactly
  // (otherwise every nested route would also be 'active').
  { to: '/', label: 'Home', end: true, order: 10 },
  { to: '/agents', label: 'Agents', order: 20 },
  { to: '/beads', label: 'Beads', order: 30 },
  { to: '/runs', label: 'Runs', order: 40 },
  { to: '/mail', label: 'Mail', order: 50 },
];

const NAV_ATTENTION_DOMAINS: Readonly<Record<string, AttentionDomain>> = {
  '/agents': 'agents',
  '/beads': 'beads',
  '/runs': 'runs',
  '/mail': 'mail',
  '/activity': 'activity',
  '/health': 'health',
};

// The header is page furniture, not chrome. A small wordmark, the
// five route names typeset as a row, a textual theme toggle. The
// route weight contrast IS the active-state affordance; no underline,
// no background pill.
export function Header() {
  const { resolved, toggle } = useTheme();
  const { viewingAs } = useViewingAs();
  const { operatorAlias } = useOperatorConfig();
  const readOnly = useReadOnly();
  const attention = useAttentionModel();
  const { data: config } = useCachedData('config', () => api.config());
  // City switcher source (gascity-dashboard-ucc). Lists every managed city;
  // selecting one navigates (full reload) to that city's `/city/:name/`
  // basename so the app remounts with the new active city. A bare full
  // navigation — not client-side — keeps the active-city base, router
  // basename, and every city-scoped fetch consistent without a per-fetch
  // city argument.
  const { data: cities } = useCachedData('cities', () => supervisorApi().listCities());
  const activeCity = getActiveCity();
  const cityItems = cities?.items ?? [];
  // The selected option value. Prefer the URL-derived active city, then the
  // backend's reported city, so the switcher never renders blank.
  const selectedCity = activeCity ?? config?.cityName ?? '';
  // True once the list has loaded and the selected city is a member. While the
  // list is empty (pre-load or a transient fetch blip) we do NOT flag the city
  // as unknown — only a loaded list that omits it is a real miss.
  const activeCityKnown =
    selectedCity === '' || cityItems.some((c) => c.name === selectedCity);
  // Show the switcher whenever there's a choice to make, or when the selected
  // city is unknown — in the latter case the disabled "(unknown)" option keeps
  // the stale city visible instead of an empty select.
  const showSwitcher = cityItems.length > 1 || !activeCityKnown;
  const onSwitchCity = (next: string): void => {
    if (next === activeCity) return;
    window.location.assign(`/city/${encodeURIComponent(next)}/`);
  };
  // PR-C: filter the registry views by the backend-advertised
  // enabledModules so a disabled module's nav entry disappears in lockstep
  // with its route. While the config fetch is in flight `enabledModules`
  // is null — meaning every firstParty view appears, matching the
  // first-paint route tree in App.tsx.
  const ROUTES: ReadonlyArray<NavRoute> = useMemo(() => {
    const enabled = filterEnabledViews(ALL_VIEWS, config?.enabledModules ?? null);
    const registry: NavRoute[] = enabled.flatMap((v) =>
      v.nav === null
        ? []
        : [{ to: v.path, label: v.nav.label, end: v.path === '/', order: v.nav.order }],
    );
    // Spread already allocates a new mutable array, so .sort() in-place is
    // safe — no .slice() needed (PR-A Phase-4 TS M4).
    return [...EXPLICIT_ROUTES, ...registry].sort((a, b) => a.order - b.order);
  }, [config?.enabledModules]);
  const { pathname } = useLocation();
  // "Reading as" is a Mail-only concept — the value persists across views
  // (for AgentDetail's chat filter) but the header indicator only makes
  // sense inside the mail surface.
  const showReadingAs = !viewingAs.isOperator && pathname.startsWith('/mail');

  return (
    <header className="border-b border-rule">
      <div className="max-w-dashboard mx-auto px-4 sm:px-6 lg:px-8 py-5 flex items-baseline gap-x-6 lg:gap-x-8 gap-y-2 flex-wrap">
        <div className="flex items-baseline gap-3 min-w-0">
          <span className="text-title font-semibold tracking-tight text-fg">gas city</span>
          <span className="text-fg-muted" aria-hidden="true">
            ·
          </span>
          {showSwitcher ? (
            <label className="sr-only" htmlFor="city-switcher">
              Switch city
            </label>
          ) : null}
          {showSwitcher ? (
            <select
              id="city-switcher"
              value={selectedCity}
              onChange={(e) => onSwitchCity(e.target.value)}
              className="text-label uppercase tracking-wider text-fg-muted bg-transparent border-0 focus-mark cursor-pointer hover:text-fg transition-colors duration-150 ease-out-quart"
            >
              {/* A grammar-valid but unregistered/renamed city in the URL is not
                  a list member; without this the select would render blank and
                  the operator couldn't tell which (stale) city they're on. The
                  disabled option keeps it visible without making it selectable
                  (gascity-dashboard-ux-stale-city-url-no-recovery). */}
              {!activeCityKnown && selectedCity !== '' ? (
                <option value={selectedCity} disabled>
                  {selectedCity} (unknown)
                </option>
              ) : null}
              {cityItems.map((c) => (
                <option key={c.name} value={c.name}>
                  {c.name}
                  {c.running ? '' : ' (stopped)'}
                </option>
              ))}
            </select>
          ) : (
            <span className="text-label uppercase tracking-wider text-fg-muted">
              {selectedCity || 'city'}
            </span>
          )}
          {showReadingAs && (
            <span className="text-label uppercase tracking-wider text-accent ml-3">
              · reading as {displayLabel(viewingAs.alias, operatorAlias)}
            </span>
          )}
          {readOnly && (
            <span
              title={READ_ONLY_CONTROL_TITLE}
              className="text-label uppercase tracking-wider text-warn ml-3"
            >
              · read-only
            </span>
          )}
        </div>

        <nav className="flex-1">
          <ul className="flex items-baseline gap-x-5 lg:gap-x-7 gap-y-1 flex-wrap">
            {ROUTES.map((r) => {
              const domain = NAV_ATTENTION_DOMAINS[r.to];
              return (
                <li key={r.to}>
                  <NavLink
                    to={r.to}
                    end={r.end ?? false}
                    className={({ isActive }) =>
                      [
                        'text-title transition-colors duration-150 ease-out-quart focus-mark',
                        isActive
                          ? 'text-fg font-semibold'
                          : 'text-fg-muted font-medium hover:text-fg',
                      ].join(' ')
                    }
                  >
                    {r.label}
                    {domain !== undefined && (
                      <NavAttentionIndicator label={r.label} summary={attention.byDomain[domain]} />
                    )}
                  </NavLink>
                </li>
              );
            })}
          </ul>
        </nav>

        <button
          type="button"
          onClick={toggle}
          aria-label={`Switch to ${resolved === 'dark' ? 'light' : 'dark'} theme`}
          className="text-label uppercase tracking-wider text-fg-muted hover:text-fg transition-colors duration-150 ease-out-quart focus-mark"
        >
          {resolved === 'dark' ? 'Light' : 'Dark'}
        </button>
      </div>
    </header>
  );
}
