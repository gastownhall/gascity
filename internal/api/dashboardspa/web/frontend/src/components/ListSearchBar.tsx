import type { ChangeEvent } from 'react';

// A bare typeset search input. Border-b hairline, no box, no card
// (Flat Page Rule). Placeholder reads as instruction, not decoration.

interface ListSearchBarProps {
  value: string;
  onChange: (v: string) => void;
  placeholder?: string;
  /** Visible row count after filtering, for the typeset count to the right. */
  matchCount?: number;
  /** Total row count before filtering, for context. */
  totalCount?: number;
  ariaLabel?: string;
}

export function ListSearchBar({
  value,
  onChange,
  placeholder = 'Search',
  matchCount,
  totalCount,
  ariaLabel = 'Search list',
}: ListSearchBarProps) {
  const showCount =
    value.length > 0 && typeof matchCount === 'number' && typeof totalCount === 'number';

  return (
    <div className="flex items-baseline gap-3 border-b border-rule pb-1">
      <input
        type="search"
        value={value}
        onChange={(e: ChangeEvent<HTMLInputElement>) => onChange(e.target.value)}
        placeholder={placeholder}
        aria-label={ariaLabel}
        className="flex-1 bg-transparent border-0 text-body text-fg placeholder:text-fg-faint focus:outline-none focus:ring-0 px-0 py-0.5"
      />
      {showCount && (
        <span className="text-label uppercase tracking-wider text-fg-faint tnum">
          {matchCount} / {totalCount}
        </span>
      )}
    </div>
  );
}
