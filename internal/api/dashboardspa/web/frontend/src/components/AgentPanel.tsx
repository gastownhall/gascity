import { useMemo, useState } from 'react';
import { useOperatorConfig } from '../contexts/OperatorConfigContext';
import { displayLabel, tierLabel, type AliasBucket } from '../hooks/aliasPriority';

// Shared rail shell for both panel states: full-width with a bottom rule on
// mobile, fixed-width with a right-edge rule from sm up; only the sm width
// varies per state.
const ASIDE_BASE_CLASS =
  'border-rule pb-6 border-b sm:shrink-0 sm:pr-6 sm:pb-0 sm:border-b-0 sm:border-r';

export function AgentPanel({
  buckets,
  loading,
  sessionsUnavailable,
  value,
  onChange,
  onReset,
  isOperator,
}: {
  buckets: ReadonlyArray<AliasBucket>;
  loading: boolean;
  sessionsUnavailable: boolean;
  value: string;
  onChange: (v: string) => void;
  onReset: () => void;
  isOperator: boolean;
}) {
  // Left-side identity panel. Starts collapsed to a thin rail showing only
  // who you're reading as; expands to a searchable, tier-grouped agent
  // list so the operator can type to find an inbox.
  //
  // Collapse/search state is local: the panel owns its own UI affordances;
  // the page only owns the load-bearing `value`/`onChange` identity state.
  const [expanded, setExpanded] = useState(false);
  const [query, setQuery] = useState('');
  const { operatorAlias: OPERATOR_ALIAS, operatorWireAlias: OPERATOR_WIRE_ALIAS } =
    useOperatorConfig();
  const valueLabel = displayLabel(value, OPERATOR_ALIAS);

  // Filter the tier buckets by the panel's own search box, and drop the
  // operator's wire alias (`human`) — it's the same inbox as "user" and
  // would otherwise read as a confusing duplicate.
  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase();
    return buckets
      .map((bucket) => ({
        tier: bucket.tier,
        aliases: bucket.aliases.filter((alias) => {
          if (alias.toLowerCase() === OPERATOR_WIRE_ALIAS) return false;
          if (q.length === 0) return true;
          const label = displayLabel(alias, OPERATOR_ALIAS).toLowerCase();
          return label.includes(q) || alias.toLowerCase().includes(q);
        }),
      }))
      .filter((bucket) => bucket.aliases.length > 0);
  }, [buckets, query, OPERATOR_ALIAS, OPERATOR_WIRE_ALIAS]);

  const select = (alias: string) => {
    onChange(alias);
    setExpanded(false);
    setQuery('');
  };

  if (!expanded) {
    return (
      <aside className={`${ASIDE_BASE_CLASS} sm:w-44`}>
        <button
          type="button"
          onClick={() => setExpanded(true)}
          aria-expanded={false}
          className="text-label uppercase tracking-wider text-fg-muted hover:text-fg focus-mark rounded-sm"
        >
          ▸ Agents
        </button>
        <div className="mt-4 text-label uppercase tracking-wider text-fg-faint">
          {isOperator ? 'Reading as' : <span className="text-accent">▲ Reading as</span>}
        </div>
        <div
          className={`mt-1 text-body truncate ${isOperator ? 'text-fg' : 'text-accent font-medium'}`}
          title={valueLabel}
        >
          {valueLabel}
        </div>
        {!isOperator && (
          <>
            <button
              type="button"
              onClick={onReset}
              className="mt-3 block text-label uppercase tracking-wider text-fg-muted hover:text-fg focus-mark underline decoration-dotted underline-offset-2 rounded-sm"
            >
              Back to operator
            </button>
            <p className="mt-2 text-label uppercase tracking-wider text-fg-faint italic">
              Read-only. Sends go from the operator.
            </p>
          </>
        )}
      </aside>
    );
  }

  return (
    <aside className={`${ASIDE_BASE_CLASS} sm:w-64`}>
      <button
        type="button"
        onClick={() => setExpanded(false)}
        aria-expanded
        className="text-label uppercase tracking-wider text-fg-muted hover:text-fg focus-mark rounded-sm"
      >
        ▾ Agents
      </button>

      <div className="mt-2 text-label uppercase tracking-wider text-fg-faint">
        {isOperator ? 'Reading as' : <span className="text-accent">▲ Reading as</span>}{' '}
        <span className={`not-italic ${isOperator ? 'text-fg-muted' : 'text-accent'}`}>
          {valueLabel}
        </span>
      </div>

      <div className="mt-3 border-b border-rule pb-1">
        <input
          type="search"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          placeholder="Find an agent"
          aria-label="Find an agent"
          autoFocus
          className="w-full bg-transparent border-0 text-body text-fg placeholder:text-fg-faint focus:outline-none focus:ring-0 px-0 py-0.5"
        />
      </div>

      <div className="mt-3 max-h-[28rem] overflow-y-auto -mr-2 pr-2 space-y-4">
        {/* Progressive: mail-derived aliases land in ~0.5s while the
            slower supervisor sessions call is still in flight. Render whatever
            is ready and footnote "loading more" rather than hiding the
            whole list behind the slowest fetch. */}
        {filtered.length === 0 ? (
          <p className="text-label uppercase tracking-wider text-fg-faint italic">
            {loading ? 'Loading aliases' : 'No agents match.'}
          </p>
        ) : (
          filtered.map((bucket) => (
            <div key={bucket.tier}>
              <div className="text-label uppercase tracking-wider text-fg-faint mb-1">
                {tierLabel(bucket.tier)}
              </div>
              <ul className="space-y-0.5">
                {bucket.aliases.map((alias) => {
                  const active = alias.toLowerCase() === value.toLowerCase();
                  return (
                    <li key={alias}>
                      <button
                        type="button"
                        onClick={() => select(alias)}
                        aria-current={active}
                        className={`block w-full text-left truncate text-body transition-colors duration-150 ease-out-quart focus-mark rounded-sm py-0.5 ${
                          active ? 'text-fg font-semibold' : 'text-fg-muted hover:text-fg'
                        }`}
                        title={displayLabel(alias, OPERATOR_ALIAS)}
                      >
                        {displayLabel(alias, OPERATOR_ALIAS)}
                      </button>
                    </li>
                  );
                })}
              </ul>
            </div>
          ))
        )}
        {loading && filtered.length > 0 && (
          <p className="text-label uppercase tracking-wider text-fg-faint italic">
            Loading more agents
          </p>
        )}
        {/* gascity-dashboard-xba: when supervisor sessions fails, the panel
            is still functional off
            the mail-derived aliases. Surface the partial state explicitly
            so the operator knows session-only agents (no mail activity)
            won't appear, instead of leaving them to wonder if the list is
            still loading.

            gascity-dashboard-5gg: split the message into two branches.
            When the visible list collapses to ONLY the operator entry
            (both fetches failed, or the mail corpus is genuinely empty),
            the narrower "showing mail-derived aliases only" copy is
            misleading because no mail-derived aliases are actually
            present. Use the broader "agent list and mail history both
            unavailable" copy in that case. */}
        {!loading &&
          sessionsUnavailable &&
          filtered.length > 0 &&
          (isOperatorOnly(filtered) ? (
            <p className="text-label uppercase tracking-wider text-fg-faint italic">
              Agent list and mail history both unavailable.
            </p>
          ) : (
            <p className="text-label uppercase tracking-wider text-fg-faint italic">
              Agent list unavailable; showing mail-derived aliases only.
            </p>
          ))}
      </div>

      {!isOperator && (
        <div className="mt-4 pt-3 border-t border-rule space-y-2">
          <button
            type="button"
            onClick={onReset}
            className="block text-label uppercase tracking-wider text-fg-muted hover:text-fg focus-mark underline decoration-dotted underline-offset-2 rounded-sm"
          >
            Back to operator
          </button>
          <p className="text-label uppercase tracking-wider text-fg-faint italic">
            Read-only. Sends always go from the operator.
          </p>
        </div>
      )}
    </aside>
  );
}

// True iff the visible alias list collapses to the single operator
// entry — i.e. neither sessions nor mail produced any non-operator
// aliases. prioritizeAliases always emits the 'you' tier with the
// operator, so the operator-only case is detectable as a single bucket
// with a single alias after the AgentPanel's own search/wire-alias
// filter has run. (gascity-dashboard-5gg)
function isOperatorOnly(buckets: ReadonlyArray<AliasBucket>): boolean {
  let total = 0;
  for (const bucket of buckets) {
    total += bucket.aliases.length;
    if (total > 1) return false;
  }
  return total <= 1;
}
