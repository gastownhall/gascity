import { describe, expect, it } from 'vitest';
import { resolveReadOnly } from './ReadOnlyContext';

// The read-only posture is security-relevant (gascity-dashboard-uzhr): it must
// fail CLOSED when the dashboard cannot confirm the backend's mode, so a click
// never 405s into an unhandled error — the regression the affordance removes.
describe('resolveReadOnly', () => {
  it('trusts the decoded flag when config is present', () => {
    expect(resolveReadOnly({ readOnly: true }, null)).toBe(true);
    expect(resolveReadOnly({ readOnly: false }, null)).toBe(false);
  });

  it('fails closed (read-only) when the config fetch errored', () => {
    // Network failure, or a backend too old to emit readOnly so the edge
    // decoder threw: config never lands, error is set -> disable mutations.
    expect(resolveReadOnly(undefined, 'config.readOnly must be a boolean')).toBe(true);
  });

  it('stays writable while the first config fetch is in flight', () => {
    // No data yet, no error yet: the sub-second pre-load window must not flash
    // every mutating control disabled on a normal page load.
    expect(resolveReadOnly(undefined, null)).toBe(false);
  });

  it('prefers a decoded writable flag over a stale error', () => {
    // A later successful refresh clears nothing about the prior error string in
    // some flows; a present config is authoritative over any lingering error.
    expect(resolveReadOnly({ readOnly: false }, 'earlier transient error')).toBe(false);
  });
});
