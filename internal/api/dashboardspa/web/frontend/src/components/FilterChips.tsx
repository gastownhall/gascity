// FilterChips: typeset toggle controls. Active state carried by
// font-weight + underline-offset, NOT by background fill, so the
// Greyscale Test passes (color is emphasis, not signal) and the
// One Mark Rule is not violated by stacking maroons.

interface ChipDef {
  id: string;
  label: string;
}

interface FilterChipsProps {
  chips: ReadonlyArray<ChipDef>;
  activeIds: ReadonlySet<string>;
  onToggle: (id: string) => void;
  /** Optional leading label, e.g. "Status". */
  legend?: string;
}

export function FilterChips({ chips, activeIds, onToggle, legend }: FilterChipsProps) {
  if (chips.length === 0) return null;
  return (
    <div className="flex items-baseline gap-4 flex-wrap">
      {legend && (
        <span className="text-label uppercase tracking-wider text-fg-muted">{legend}</span>
      )}
      {chips.map((chip) => {
        const active = activeIds.has(chip.id);
        return (
          <button
            key={chip.id}
            type="button"
            onClick={() => onToggle(chip.id)}
            aria-pressed={active}
            className={`text-label uppercase tracking-wider transition-colors duration-150 ease-out-quart focus-mark rounded-sm ${
              active
                ? 'text-fg font-semibold underline decoration-fg underline-offset-4'
                : 'text-fg-muted hover:text-fg'
            }`}
          >
            {chip.label}
          </button>
        );
      })}
    </div>
  );
}
