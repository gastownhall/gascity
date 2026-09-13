import { describe, expect, it } from 'vitest';
import { stripTerminalControls } from './stripTerminalControls';

// gascity-dashboard-5e5v / xl07: raw terminal control bytes leaked into the
// rendered session-peek transcript. These cases are anchored on the real leak
// Stephanie reported (`... then proceed.^[`) plus the OSC / SGR / bare-C1
// classes that share the same path.

describe('stripTerminalControls', () => {
  it('strips the trailing lone ESC Stephanie saw (`... then proceed.^[`)', () => {
    const dirty = '... then proceed.\x1b';
    expect(stripTerminalControls(dirty)).toBe('... then proceed.');
  });

  it('cleans a turn carrying ESC, OSC, SGR colour, and a bare \\x9c at once', () => {
    const dirty = 'start\x1b]0;evil-title\x07 mid \x1b[31mred\x1b[0m tail.\x1b done\x9chere';
    // SGR sequences are preserved (ansi_up renders them); every other control
    // byte is removed. Printable text and spaces survive intact.
    expect(stripTerminalControls(dirty)).toBe('start mid \x1b[31mred\x1b[0m tail. donehere');
  });

  it('strips a bare \\x9c (8-bit C1 OSC string terminator)', () => {
    expect(stripTerminalControls('before\x9cafter')).toBe('beforeafter');
  });

  it('strips BEL-terminated OSC (ESC ] ... BEL)', () => {
    expect(stripTerminalControls('a\x1b]0;title\x07b')).toBe('ab');
  });

  it('strips ST-terminated OSC (ESC ] ... ESC \\)', () => {
    expect(stripTerminalControls('a\x1b]0;title\x1b\\b')).toBe('ab');
  });

  it('strips C1-ST-terminated OSC (ESC ] ... \\x9c)', () => {
    expect(stripTerminalControls('a\x1b]0;title\x9cb')).toBe('ab');
  });

  it('strips non-SGR CSI (cursor/erase) but keeps SGR colour', () => {
    const dirty = '\x1b[2J\x1b[1;1H\x1b[32mgreen\x1b[0m';
    expect(stripTerminalControls(dirty)).toBe('\x1b[32mgreen\x1b[0m');
  });

  it('preserves SGR sequences untouched', () => {
    const sgr = '\x1b[0m\x1b[1;31mbold-red\x1b[0m';
    expect(stripTerminalControls(sgr)).toBe(sgr);
  });

  it('preserves newlines and tabs', () => {
    const text = 'line one\nline two\tcol\r\nline three';
    expect(stripTerminalControls(text)).toBe(text);
  });

  it('does not over-strip ordinary printable text', () => {
    const text = 'plain text with [brackets] and 0;1;2 numbers — émojis 🚀';
    expect(stripTerminalControls(text)).toBe(text);
  });

  it('strips residual C0 controls but keeps whitespace', () => {
    expect(stripTerminalControls('a\x00b\x08c\x07d\te')).toBe('abcd\te');
  });

  it('an unterminated OSC does not swallow a following SGR', () => {
    // The OSC payload class excludes ESC, so an unterminated OSC fails to
    // match as a whole; the lone-ESC pass then strips its orphaned `ESC ]`
    // opener while the following SGR is preserved for ansi_up. The defense
    // property is that no ESC survives except the real SGR.
    const out = stripTerminalControls('\x1b]0;no-end\x1b[31mred\x1b[0m');
    expect(out).toBe('0;no-end\x1b[31mred\x1b[0m');
  });

  it('returns empty string unchanged', () => {
    expect(stripTerminalControls('')).toBe('');
  });
});
