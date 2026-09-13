import { useState } from 'react';
import { readBrowserStorage, writeBrowserStorage } from '../../lib/browserStorage';

const STORAGE_KEY = 'gascity:home-intro-dismissed';
const COMPONENT = 'FirstRunNote';

// First-visit orientation for the ambient home (gascity-dashboard-q89b).
// The home deliberately withholds healthy in-flight work (PRD R10), which
// reads as an empty page to a zero-context newcomer. This note explains the
// register once, then gets out of the way: Dismiss persists per browser.
// If storage is unavailable the note simply shows again next visit; the
// helper already reports the storage error.
//
// Editorial register per DESIGN.md: prose, not a card; no maroon (the One
// Mark budget stays with the status sentence); textual dismiss affordance.
export function FirstRunNote() {
  const [dismissed, setDismissed] = useState(
    () => readBrowserStorage('localStorage', STORAGE_KEY, COMPONENT).status === 'found',
  );
  if (dismissed) return null;

  const onDismiss = (): void => {
    setDismissed(true);
    writeBrowserStorage('localStorage', STORAGE_KEY, '1', COMPONENT);
  };

  return (
    <aside className="mt-6 max-w-[70ch]" data-testid="first-run-note">
      <p className="text-body text-fg-muted">
        New here? This page is the ambient home for a Gas City workspace: a calm census of the
        formula runs in flight. Healthy work stays quiet by design; the page speaks up only when a
        run needs an operator decision. The full record lives in Agents, Beads, Runs, and Mail
        above.
      </p>
      <button
        type="button"
        onClick={onDismiss}
        className="mt-2 text-label uppercase tracking-wider text-fg-muted hover:text-fg transition-colors duration-150 ease-out-quart focus-mark"
      >
        Dismiss
      </button>
    </aside>
  );
}
