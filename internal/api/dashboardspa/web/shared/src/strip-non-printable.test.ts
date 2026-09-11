import { test, describe } from 'node:test';
import assert from 'node:assert/strict';
import { stripNonPrintable } from './strip-non-printable.js';

// gascity-dashboard-vibg: stripNonPrintable must mirror
// exec.ts::sanitiseTerminalOutput's OSC handling. An OSC sequence can be
// terminated either by BEL (\x07, the xterm-legacy form) or by ST as
// ESC \\ (\x1b\\, the spec form most modern terminals emit). Only matching
// the BEL form left ST-terminated payloads as visible bracketed text once
// CTRL_RE stripped the bounding ESC bytes — a defense-in-depth gap if
// CTRL_RE were ever weakened.

describe('stripNonPrintable — OSC termination forms', () => {
  test('strips BEL-terminated OSC (ESC ] ... BEL)', () => {
    const dirty = 'before\x1b]0;evil-title\x07after';
    assert.equal(stripNonPrintable(dirty), 'beforeafter');
  });

  test('strips ST-terminated OSC (ESC ] ... ESC \\)', () => {
    const dirty = 'before\x1b]0;evil-title\x1b\\after';
    assert.equal(stripNonPrintable(dirty), 'beforeafter');
  });

  test('an unterminated OSC does not consume a following ANSI escape', () => {
    // The OSC char-class excludes \x1b, so an unterminated OSC payload does
    // not swallow the trailing CSI: OSC_RE fails to match (no terminator),
    // CTRL_RE then strips both ESC bytes, and CSI_RE removes the [31m. The
    // bracket text is left as inert, non-interpretable plain text — the
    // defense-in-depth property is that no ESC survives and the CSI is still
    // stripped on its own, not swallowed by a greedy OSC match.
    const out = stripNonPrintable('before\x1b]0;no-terminator\x1b[31mred');
    assert.ok(!out.includes('\x1b'), 'no ESC byte may survive');
    assert.ok(!out.includes('[31m'), 'the trailing CSI must still be stripped');
    assert.equal(out, 'before]0;no-terminatorred');
  });
});

describe('stripNonPrintable — CVE-2021-42574 bidi codepoints', () => {
  // All 12 Unicode bidi / RTL control codepoints from the trojan-source
  // CVE (Boucher/Anderson 2021): U+061C (ALM), U+200E (LRM), U+200F (RLM),
  // U+202A-202E (LRE/RLE/PDF/LRO/RLO), U+2066-2069 (LRI/RLI/FSI/PDI).
  const BIDI_CODEPOINTS = [
    0x061c, 0x200e, 0x200f, 0x202a, 0x202b, 0x202c, 0x202d, 0x202e, 0x2066, 0x2067, 0x2068, 0x2069,
  ];

  for (const cp of BIDI_CODEPOINTS) {
    const label = `U+${cp.toString(16).toUpperCase().padStart(4, '0')}`;
    test(`strips ${label}`, () => {
      const ch = String.fromCodePoint(cp);
      assert.equal(stripNonPrintable(`a${ch}b`), 'ab');
    });
  }
});
