import { CollapsibleHeader } from './CollapsibleHeader';

// ProjectGroupHeader: a typeset section divider, clickable to toggle
// collapse. The chevron is a rotated glyph, not a
// box. The count badge is plain tabular figures in fg-faint, not a
// pill. Whitespace + type carries the hierarchy (Flat Page Rule).
//
// Non-collapsible variant renders as a small-caps label (no chevron,
// no count). Used for the Orchestration pseudo-group that pins cross-
// rig sessions at the top — there's nothing to collapse, and it isn't
// a rig.

interface ProjectGroupHeaderProps {
  project: string;
  count: number;
  collapsed: boolean;
  onToggle: () => void;
  collapsible?: boolean;
}

export function ProjectGroupHeader({
  project,
  count,
  collapsed,
  onToggle,
  collapsible = true,
}: ProjectGroupHeaderProps) {
  if (!collapsible) {
    return (
      <div
        role="heading"
        aria-level={2}
        className="flex items-baseline gap-2 py-1 text-label uppercase tracking-wider text-fg-faint"
      >
        <span aria-hidden>·</span>
        <span>{project}</span>
        <span aria-hidden>·</span>
      </div>
    );
  }
  return (
    <CollapsibleHeader
      collapsed={collapsed}
      onToggle={onToggle}
      className="group flex items-baseline gap-2 w-full text-left focus-mark rounded-sm py-1"
      glyphClassName="group-hover:text-fg-muted tnum w-3"
    >
      {({ glyph }) => (
        <>
          {glyph}
          <span className="text-title font-medium text-fg group-hover:text-fg">{project}</span>
          <span className="text-label uppercase tracking-wider text-fg-faint tnum">{count}</span>
        </>
      )}
    </CollapsibleHeader>
  );
}
