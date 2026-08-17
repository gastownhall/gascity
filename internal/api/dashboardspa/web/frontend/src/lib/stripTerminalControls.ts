// Strip terminal control bytes from a session transcript turn before it is
// rendered. This is the DISPLAY-ONLY sibling of the backend's
// exec.ts::sanitiseTerminalOutput / shared stripNonPrintable — it does not
// alter what the supervisor stores, it only cleans operator-facing text in
// the peek/transcript path (gascity-dashboard-5e5v / xl07).
//
// The operator saw raw bytes leak into peeks — e.g. `... then proceed.^[`
// (a trailing lone ESC), OSC title sequences, and a bare `\x9c` (the 8-bit
// C1 OSC string terminator). `ansi_up.ansi_to_html` colorizes SGR (`\x1b[…m`)
// but passes every other control sequence through as visible text, so those
// bytes reach the `<pre>` verbatim.
//
// Contract: SGR colour sequences are PRESERVED so ansi_up can still render
// colour; `\t`, `\n`, `\r` are PRESERVED so layout survives. Everything else
// in the control range is removed. This mirrors the backend OSC/CSI/C1
// handling but keeps SGR and whitespace, because this output is multi-line
// rendered terminal text, not a single-line audit/argv record.

// OSC: ESC ] … terminated by BEL (\x07, xterm-legacy), ST as ESC \\ (the
// spec form modern terminals emit), or the single-byte C1 ST \x9c. The
// payload class excludes \x1b so an unterminated OSC cannot swallow a
// following ANSI escape. Mirrors strip-non-printable.ts::OSC_RE, extended
// with the \x9c terminator so a complete OSC is consumed in one pass rather
// than leaving the \x9c for the C1 strip.
const OSC_RE = /\x1b\][^\x07\x1b\x9c]*(?:\x07|\x1b\\|\x9c)/g;
// CSI that is NOT an SGR (colour) sequence: ESC [ params <final> where the
// final byte is anything except `m`. SGR (`…m`) is left for ansi_up to turn
// into coloured spans. Covers cursor moves, erase, scroll-region, etc.
const CSI_NON_SGR_RE = /\x1b\[[?0-9;]*[a-ln-zA-Z]/g;
// Any ESC that does NOT introduce a surviving SGR sequence. Runs after the
// OSC and non-SGR-CSI passes have consumed those forms, so the only ESC worth
// keeping is a valid SGR (`\x1b[ params m`); the negative lookahead preserves
// exactly that and removes everything else — stray ST (ESC \\), charset
// selects (ESC ( ), ESC = / ESC >, an unterminated OSC's leading ESC, and the
// bare trailing ESC (`^[`) the operator saw. The optional final byte consumes a
// single ESC-Fe/Fp byte (e.g. the `\\` of ESC \\) when present.
const LONE_ESC_RE = /\x1b(?!\[[?0-9;]*m)[@-Z\\-_]?/g;
// Residual control bytes after the ESC-based passes: C0 below 0x20 except \t
// (\x09) \n (\x0a) \r (\x0d) and ESC (\x1b, already handled above and possibly
// part of a kept SGR), DEL (\x7f), and the C1 range (\x80-\x9f) — including
// the bare \x9c that leaked into the operator's peek. SGR digits/semicolons and
// ordinary printable text are all >= 0x20, so this never touches kept content.
const RESIDUAL_CTRL_RE = /[\x00-\x08\x0b\x0c\x0e-\x1a\x1c-\x1f\x7f-\x9f]/g;

/**
 * Remove terminal control sequences from transcript text while preserving
 * SGR colour sequences, printable content, and `\t` / `\n` / `\r`.
 *
 * Order matters: OSC first (so a complete title sequence is consumed before
 * its bytes are seen as a stray ESC / C1), then non-SGR CSI, then any lone ESC
 * that is not a surviving SGR, then residual control bytes. Exported for tests.
 */
export function stripTerminalControls(text: string): string {
  return text
    .replace(OSC_RE, '')
    .replace(CSI_NON_SGR_RE, '')
    .replace(LONE_ESC_RE, '')
    .replace(RESIDUAL_CTRL_RE, '');
}
