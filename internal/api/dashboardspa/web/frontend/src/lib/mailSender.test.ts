import { describe, expect, it } from 'vitest';
import { formatMailSender } from './mailSender';

describe('formatMailSender', () => {
  it('passes clean aliases through', () => {
    expect(formatMailSender('mayor')).toBe('mayor');
    expect(formatMailSender('gascity-packs.project-lead')).toBe('gascity-packs.project-lead');
  });
  it('formats a worktree path as rig · agent, stripping the redundant rig prefix', () => {
    expect(formatMailSender('/home/ds/gascity-packs/gascity-packs-polecat-1')).toBe(
      'gascity-packs · polecat-1',
    );
  });
  it('canonicalizes a -main worktree rig to its base rig', () => {
    expect(formatMailSender('/home/ds/gascity-main/gascity-maintenance-pl')).toBe(
      'gascity · gascity-maintenance-pl',
    );
    expect(formatMailSender('/home/ds/gascity-packs-main/gascity-packs-pl')).toBe(
      'gascity-packs · gascity-packs-pl',
    );
  });
  it('handles a plain rig/agent path', () => {
    expect(formatMailSender('gascity/polecat-2')).toBe('gascity · polecat-2');
  });
  it('returns the basename when there is no parent segment', () => {
    expect(formatMailSender('/polecat-9')).toBe('polecat-9');
  });
});
