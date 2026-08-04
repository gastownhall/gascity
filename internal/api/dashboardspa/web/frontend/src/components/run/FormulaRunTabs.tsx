import type { RunDisplayNode } from 'gas-city-dashboard-shared';
import { RunNodeSessionPanel } from './RunNodeSessionPanel';

interface FormulaRunTabsProps {
  selectedNode: RunDisplayNode | null;
}

/**
 * The run-evidence panel hosts the selected node's session transcript. It keeps
 * the labelled tab/tabpanel structure (a single Session tab) so the transcript
 * reads as one view of the run, matching the surrounding run-detail chrome.
 */
export function FormulaRunTabs({ selectedNode }: FormulaRunTabsProps) {
  return (
    <section aria-label="Run evidence">
      <div
        className="flex items-baseline gap-2 text-label"
        role="tablist"
        aria-label="Run evidence views"
      >
        <button
          id="run-evidence-tab-session"
          type="button"
          role="tab"
          aria-selected
          aria-controls="run-evidence-panel"
          className="focus-mark rounded-sm px-0.5 uppercase tracking-wider text-fg font-semibold underline decoration-fg underline-offset-4"
        >
          Session
        </button>
      </div>
      <div
        id="run-evidence-panel"
        role="tabpanel"
        aria-labelledby="run-evidence-tab-session"
        className="pt-5"
      >
        <RunNodeSessionPanel node={selectedNode} visible />
      </div>
    </section>
  );
}
