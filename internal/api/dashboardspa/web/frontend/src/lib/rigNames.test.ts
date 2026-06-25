import { describe, expect, it } from 'vitest';
import { resolveRigName, rigNameOptions } from './rigNames';

const RIGS = [
  { name: 'gascity', path: '/home/ds/gascity' },
  { name: 'ds-research', path: '/home/ds/ds-research' },
  { name: 'gascity-packs', path: '/home/ds/gascity-packs' },
  { name: 'gascity-dashboard', path: '/home/ds/gascity-dashboard' },
] as const;

describe('resolveRigName', () => {
  it('returns the canonical name when the value already is a rig name', () => {
    expect(resolveRigName('ds-research', RIGS)).toBe('ds-research');
  });

  it('maps a filesystem path to its canonical rig name', () => {
    expect(resolveRigName('/home/ds/gascity', RIGS)).toBe('gascity');
  });

  it('drops a path that is not a registered rig (e.g. /home/ds/gascity-main)', () => {
    expect(resolveRigName('/home/ds/gascity-main', RIGS)).toBeUndefined();
  });

  it('drops an unknown bare name', () => {
    expect(resolveRigName('gascity-main', RIGS)).toBeUndefined();
  });

  it('treats empty / whitespace / nullish as unresolved', () => {
    expect(resolveRigName('', RIGS)).toBeUndefined();
    expect(resolveRigName('   ', RIGS)).toBeUndefined();
    expect(resolveRigName(undefined, RIGS)).toBeUndefined();
    expect(resolveRigName(null, RIGS)).toBeUndefined();
  });

  it('trims surrounding whitespace before matching', () => {
    expect(resolveRigName('  ds-research  ', RIGS)).toBe('ds-research');
  });

  it('prefers a name match over a path match when both could apply', () => {
    const rigs = [
      { name: 'overlap', path: '/home/ds/other' },
      { name: 'other', path: '/home/ds/overlap' },
    ];
    expect(resolveRigName('overlap', rigs)).toBe('overlap');
  });
});

describe('rigNameOptions', () => {
  it('lists real rig names only, sorted, with no filesystem paths', () => {
    expect(rigNameOptions(RIGS)).toEqual([
      'ds-research',
      'gascity',
      'gascity-dashboard',
      'gascity-packs',
    ]);
  });

  it('de-duplicates and ignores blank names', () => {
    const rigs = [
      { name: 'gascity', path: '/a' },
      { name: 'gascity', path: '/b' },
      { name: '   ', path: '/c' },
    ];
    expect(rigNameOptions(rigs)).toEqual(['gascity']);
  });

  it('returns an empty list when there are no rigs', () => {
    expect(rigNameOptions([])).toEqual([]);
  });
});
