import { useEffect, useRef } from 'react';
import type { BeadNode } from '../../lib/beadGraph';
import type { BadgeSeverity } from '../../attention/compose';
import { attentionListItemProps } from '../../attention/routeHighlight';

// One bead on the board, in the editorial register: a typeset row, no card,
// no side-stripe. Selection is carried by weight + a leading "▸" + a warm
// surface tint, never by a colored left edge (DESIGN.md §6). Clicking a row
// opens the bead detail pop-out (gascity-dashboard-14s1); the row carries
// only a compact `needs N · blocks M` annotation, with the full navigable
// dependency tree living in the modal where it has room to read.

interface BeadBoardRowProps {
  node: BeadNode;
  selected: boolean;
  attentionSeverity?: BadgeSeverity | null;
  onSelect: (beadId: string) => void;
}

export function BeadBoardRow({
  node,
  selected,
  attentionSeverity = null,
  onSelect,
}: BeadBoardRowProps) {
  const { bead, deps, blocks, hasUnresolvedDeps } = node;
  const rowRef = useRef<HTMLLIElement | null>(null);
  const depCount = deps.length;
  const blockCount = blocks.length;
  const hasNeighbourhood = depCount > 0 || blockCount > 0;

  const { className: attentionClassName = '', ...attentionProps } =
    attentionListItemProps(attentionSeverity);

  useEffect(() => {
    if (!selected) return;
    rowRef.current?.scrollIntoView?.({
      block: 'center',
      inline: 'nearest',
    });
  }, [selected]);

  return (
    <li
      ref={rowRef}
      {...attentionProps}
      className={`px-2 py-2 -mx-2 rounded-sm transition-colors duration-150 ease-out-quart ${
        selected ? 'bg-surface-tint' : 'hover:bg-surface-tint/60'
      } ${attentionClassName}`}
    >
      <button
        type="button"
        onClick={() => onSelect(bead.id)}
        className="text-left w-full focus-mark rounded-sm"
        aria-pressed={selected}
        title={`Select ${bead.id}`}
      >
        <span className="flex items-baseline gap-2">
          <span className="text-fg-faint" aria-hidden="true">
            {selected ? '▸' : ' '}
          </span>
          <span
            className={`min-w-0 line-clamp-2 text-body ${
              selected ? 'text-fg font-medium' : 'text-fg'
            }`}
          >
            {bead.title}
          </span>
        </span>
        <span className="flex items-baseline gap-3 pl-4 mt-0.5 text-label uppercase tracking-wider text-fg-faint">
          <span className="tnum">{bead.id}</span>
          {bead.priority != null && <span className="tnum">P{bead.priority}</span>}
          {hasNeighbourhood && (
            <span className="tnum normal-case tracking-normal">
              {depCount > 0 && `needs ${depCount}`}
              {depCount > 0 && blockCount > 0 && ' · '}
              {blockCount > 0 && `blocks ${blockCount}`}
            </span>
          )}
          {hasUnresolvedDeps && (
            <span className="normal-case tracking-normal text-warn">unresolved</span>
          )}
        </span>
      </button>
    </li>
  );
}
