import { describe, expect, it } from 'vitest';

import { createScopeBuffer } from './scopeBuffer';

describe('createScopeBuffer', () => {
  it('starts with every bin at zero', () => {
    const buf = createScopeBuffer(4);
    expect(buf.snapshot()).toEqual([0, 0, 0, 0]);
  });

  it('accumulates bumps in the newest bin', () => {
    const buf = createScopeBuffer(4);
    buf.bump();
    buf.bump();
    buf.bump();
    expect(buf.snapshot()).toEqual([0, 0, 0, 3]);
  });

  it('advances the window on shift and zeroes the new newest bin', () => {
    const buf = createScopeBuffer(4);
    buf.bump(); // newest bin holds 1
    buf.shift(); // that count ages by one; the fresh newest bin is 0
    expect(buf.snapshot()).toEqual([0, 0, 1, 0]);
    buf.bump(); // newest bin holds 1 again
    expect(buf.snapshot()).toEqual([0, 0, 1, 1]);
  });

  it('drops the oldest bin once a count ages past capacity', () => {
    const buf = createScopeBuffer(3);
    buf.bump(); // [0,0,1]
    buf.shift(); // [0,1,0]
    buf.shift(); // [1,0,0]
    expect(buf.snapshot()).toEqual([1, 0, 0]);
    buf.shift(); // the count falls off the oldest edge → [0,0,0]
    expect(buf.snapshot()).toEqual([0, 0, 0]);
  });

  it('wraps around the ring beyond capacity without corrupting order', () => {
    const buf = createScopeBuffer(3);
    // Drive more shifts than the ring holds, bumping each step, so the trace
    // scrolls cleanly through every slot and back to the start.
    buf.bump(); // [0,0,1]
    buf.shift();
    buf.bump(); // [0,1,1]
    buf.shift();
    buf.bump(); // [1,1,1]
    buf.shift();
    buf.bump(); // oldest fell off, newest re-filled → still [1,1,1]
    expect(buf.snapshot()).toEqual([1, 1, 1]);
  });

  it('returns snapshots in oldest→newest order', () => {
    const buf = createScopeBuffer(3);
    buf.bump(); // newest
    buf.shift();
    buf.bump();
    buf.bump(); // newest holds 2, the older bin holds 1
    expect(buf.snapshot()).toEqual([0, 1, 2]);
  });

  it('rejects a non-positive or non-integer bin count', () => {
    expect(() => createScopeBuffer(0)).toThrow();
    expect(() => createScopeBuffer(-3)).toThrow();
    expect(() => createScopeBuffer(2.5)).toThrow();
  });
});
