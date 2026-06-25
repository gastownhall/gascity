// SortToggle: editorial-typographic segmented control. Two labels
// separated by an interpunct, current mode in fg, others in fg-faint.
// No box, no pill — whitespace and weight carry the state (Flat Page Rule).

interface SortToggleOption<M extends string> {
  id: M;
  label: string;
}

interface SortToggleProps<M extends string> {
  value: M;
  options: ReadonlyArray<SortToggleOption<M>>;
  onChange: (next: M) => void;
  /** Visible label preceding the options (e.g. "Sort"). */
  legend: string;
  /** Aria-label for the group; falls back to the visible legend. */
  ariaLabel?: string;
}

export function SortToggle<M extends string>({
  value,
  options,
  onChange,
  legend,
  ariaLabel,
}: SortToggleProps<M>) {
  return (
    <div
      role="radiogroup"
      aria-label={ariaLabel ?? legend}
      className="flex items-baseline gap-2 text-label"
    >
      <span className="uppercase tracking-wider text-fg-faint">{legend}</span>
      <div className="flex items-baseline gap-1">
        {options.map((opt, idx) => {
          const active = opt.id === value;
          return (
            <span key={opt.id} className="flex items-baseline gap-1">
              {idx > 0 && (
                <span aria-hidden className="text-fg-faint">
                  ·
                </span>
              )}
              <button
                type="button"
                role="radio"
                aria-checked={active}
                onClick={() => onChange(opt.id)}
                className={`focus-mark rounded-sm px-0.5 ${
                  active ? 'text-fg font-medium' : 'text-fg-faint hover:text-fg-muted'
                }`}
              >
                {opt.label}
              </button>
            </span>
          );
        })}
      </div>
    </div>
  );
}
