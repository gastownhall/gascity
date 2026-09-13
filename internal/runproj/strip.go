package runproj

import "regexp"

// stripNonPrintable removes ANSI escapes (OSC + CSI), C0/DEL/C1 control bytes,
// and Unicode bidi/RTL controls from operator-influenced strings. Faithful port
// of TS stripNonPrintable (shared/src/strip-non-printable.ts). This is the
// single sanitisation choke point for scope refs that enter the DTO
// (gascity-dashboard-5e5v).
var (
	// OSC: ESC ] ... terminated by BEL or ESC \ ; the inner class excludes ESC.
	oscRe = regexp.MustCompile("\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)")
	// CSI: ESC [ params final-letter.
	csiRe = regexp.MustCompile("\x1b\\[[?0-9;]*[a-zA-Z]")
	// All control chars: C0 (<0x20, incl. tab/newline/CR), DEL, C1 (0x80-0x9f).
	ctrlRe = regexp.MustCompile(`[\x{00}-\x{1f}\x{7f}-\x{9f}]`)
	// The 12 Unicode bidi/RTL control codepoints (CVE-2021-42574):
	// U+061C, U+200E, U+200F, U+202A-202E, U+2066-2069.
	bidiRe = regexp.MustCompile(`[\x{061c}\x{200e}\x{200f}\x{202a}-\x{202e}\x{2066}-\x{2069}]`)
)

func stripNonPrintable(value string) string {
	value = oscRe.ReplaceAllString(value, "")
	value = csiRe.ReplaceAllString(value, "")
	value = ctrlRe.ReplaceAllString(value, "")
	value = bidiRe.ReplaceAllString(value, "")
	return value
}
