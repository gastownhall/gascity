import { describe, expect, it } from 'vitest';
import { beadStatusTone } from './StatusBadge';

describe('beadStatusTone', () => {
  it('uses one canonical tone map for bead detail and bead list badges', () => {
    expect(beadStatusTone('in_progress')).toBe('ok');
    expect(beadStatusTone('blocked')).toBe('stuck');
    expect(beadStatusTone('open')).toBe('warn');
    expect(beadStatusTone('closed')).toBe('neutral');
    expect(beadStatusTone('deferred')).toBe('warn');
  });
});
