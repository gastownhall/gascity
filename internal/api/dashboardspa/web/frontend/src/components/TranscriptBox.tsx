import { useCallback, useEffect, useId, useRef, useState, type ReactNode } from 'react';

interface TranscriptBoxProps {
  children: ReactNode;
  /** Optional caption rendered on the same row as the expand button. */
  caption?: string;
}

/**
 * Scrollable, expandable container for transcript content.
 *
 * Collapsed: fixed-height scrolling pane with an expand button, auto-scrolling
 * to the bottom when content grows (only if the user is already pinned there,
 * i.e. `tail -f` style).
 *
 * Expanded: fixed full-screen-ish overlay (90% of the viewport) that fades
 * and scales in. Escape collapses it in capture phase so it wins over any
 * parent modal's Escape handler.
 */
export function TranscriptBox({ children, caption }: TranscriptBoxProps) {
  const [expanded, setExpanded] = useState(false);
  // Separate refs for each scroll container so the correct DOM node is always
  // targeted regardless of which branch is mounted.
  const collapsedRef = useRef<HTMLDivElement>(null);
  const expandedRef = useRef<HTMLDivElement>(null);
  // Whether the user is scrolled to the bottom of the active container.
  const pinnedRef = useRef(true);
  // Drives the enter animation for the expanded overlay.
  const [visible, setVisible] = useState(false);
  // Stable id so the dialog's accessible name comes from the visible heading
  // (aria-labelledby) instead of a diverging hard-coded aria-label.
  const headingId = useId();

  const activeRef = expanded ? expandedRef : collapsedRef;

  // Sticky-bottom: scroll to bottom when content changes, but only if pinned.
  // Runs after every render so new SSE turns are caught immediately.
  useEffect(() => {
    const el = activeRef.current;
    if (!el || !pinnedRef.current) return;
    el.scrollTop = el.scrollHeight;
  });

  // Update pinned flag as the user scrolls.
  const handleScroll = useCallback((e: React.UIEvent<HTMLDivElement>) => {
    const el = e.currentTarget;
    pinnedRef.current = el.scrollHeight - el.scrollTop - el.clientHeight < 8;
  }, []);

  // Trigger the enter animation on the frame after the overlay mounts.
  useEffect(() => {
    if (!expanded) {
      setVisible(false);
      return;
    }
    // DESIGN.md §6: respect prefers-reduced-motion — skip the enter
    // animation and show the overlay synchronously (matches the
    // motion-reduce:transition-none class below, which removes the CSS tween).
    if (window.matchMedia('(prefers-reduced-motion: reduce)').matches) {
      setVisible(true);
      return;
    }
    // rAF so the initial opacity-0/scale-95 classes are painted before we add
    // the transition targets — without this the browser skips the animation.
    const id = requestAnimationFrame(() => setVisible(true));
    return () => cancelAnimationFrame(id);
  }, [expanded]);

  // Collapse on Escape in capture phase so this handler wins over any parent
  // modal's Escape listener, which runs on the bubble phase.
  useEffect(() => {
    if (!expanded) return;
    const handleKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        e.stopPropagation();
        setExpanded(false);
        pinnedRef.current = true;
      }
    };
    document.addEventListener('keydown', handleKey, /* capture */ true);
    return () => document.removeEventListener('keydown', handleKey, true);
  }, [expanded]);

  const handleExpand = () => {
    pinnedRef.current = true;
    setExpanded(true);
  };

  const handleCollapse = () => {
    pinnedRef.current = true;
    setExpanded(false);
  };

  if (expanded) {
    return (
      <>
        {/* Backdrop — sits above parent-modal z-50 (z-[60]) */}
        <div
          className="fixed inset-0 z-[60] bg-fg/30"
          aria-hidden="true"
          onClick={handleCollapse}
        />
        {/* Expanded panel — positioned above the backdrop */}
        <div
          role="dialog"
          aria-modal="true"
          aria-labelledby={headingId}
          className={[
            'fixed inset-[5%] z-[61] flex flex-col',
            'bg-surface border border-rule rounded-md',
            'transition-[opacity,transform] duration-150 ease-out motion-reduce:transition-none',
            visible ? 'opacity-100 scale-100' : 'opacity-0 scale-95',
          ].join(' ')}
          onClick={(e) => e.stopPropagation()}
        >
          <div className="px-5 pt-4 pb-3 border-b border-rule shrink-0 space-y-1">
            <h2 id={headingId} className="text-label uppercase tracking-wider text-fg-faint">
              Chat Transcript
            </h2>
            <div className="flex items-baseline gap-3">
              {caption && (
                <span className="text-label uppercase tracking-wider text-fg-faint tnum">
                  {caption}
                </span>
              )}
              <button
                type="button"
                onClick={handleCollapse}
                aria-label="Collapse transcript"
                className="text-label uppercase tracking-wider text-fg-muted hover:text-fg transition-colors duration-150 ease-out focus-mark rounded-sm px-1 ml-auto"
              >
                collapse ×
              </button>
            </div>
          </div>
          <div ref={expandedRef} onScroll={handleScroll} className="flex-1 overflow-y-auto p-5">
            {children}
          </div>
        </div>
      </>
    );
  }

  return (
    <div>
      {/* Collapsed pane carries no section heading: each caller already labels
          the surface (AgentLivePeek "Live peek", run-node title h3), so a
          second "Chat Transcript" h2 was a duplicate label and a heading
          inversion. The label lives only in the expanded dialog, which needs
          it for its accessible name. */}
      <header className="flex items-baseline justify-end mb-4 gap-3">
        <div className="flex items-baseline gap-3 min-w-0">
          {caption && (
            <span className="text-label uppercase tracking-wider text-fg-faint tnum shrink-0">
              {caption}
            </span>
          )}
          <button
            type="button"
            onClick={handleExpand}
            aria-label="Expand transcript"
            className="text-label uppercase tracking-wider text-fg-muted hover:text-fg transition-colors duration-150 ease-out focus-mark rounded-sm shrink-0"
          >
            expand ⤢
          </button>
        </div>
      </header>
      <div ref={collapsedRef} onScroll={handleScroll} className="h-96 overflow-y-auto p-4">
        {children}
      </div>
    </div>
  );
}
