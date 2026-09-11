import type { ReactNode } from 'react';

interface CollapsibleHeaderRenderProps {
  glyph: ReactNode;
}

interface CollapsibleHeaderProps {
  collapsed: boolean;
  onToggle: () => void;
  children: (props: CollapsibleHeaderRenderProps) => ReactNode;
  className?: string;
  glyphClassName?: string;
}

export function CollapsibleHeader({
  collapsed,
  onToggle,
  children,
  className = 'w-full flex items-baseline justify-between gap-4 focus-mark',
  glyphClassName,
}: CollapsibleHeaderProps) {
  return (
    <button type="button" onClick={onToggle} className={className} aria-expanded={!collapsed}>
      {children({
        glyph: <CollapseGlyph collapsed={collapsed} className={glyphClassName ?? ''} />,
      })}
    </button>
  );
}

export function CollapseGlyph({
  collapsed,
  className = '',
}: {
  collapsed: boolean;
  className?: string;
}) {
  return (
    <span
      aria-hidden
      className={`inline-block text-fg-faint transition-transform duration-150 ease-out-quart ${className}`}
      style={{ transform: collapsed ? 'rotate(-90deg)' : 'rotate(0deg)' }}
    >
      ▾
    </span>
  );
}
