//go:build windows

package sling

import "fmt"

// CheckSlingQueryBeadID reports whether beadID may be substituted for {} in a
// sling_query. On Windows it may only when it starts with an ASCII letter or
// digit and holds nothing but those and '.', '_' and '-'. PowerShell runs the
// command, and when the sling program is a batch file it hands the ID on to
// cmd.exe, which runs & | < > as commands and expands %NAME% whatever the
// quoting. A forced sling skips the store lookup, so the ID can be caller text.
func CheckSlingQueryBeadID(beadID string) error {
	for i, r := range beadID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case i > 0 && (r == '.' || r == '_' || r == '-'):
		default:
			return slingQueryBeadIDRefusal(beadID)
		}
	}
	if beadID == "" {
		return slingQueryBeadIDRefusal(beadID)
	}
	return nil
}

func slingQueryBeadIDRefusal(beadID string) error {
	return fmt.Errorf("sling_query: refusing bead ID %q: on Windows a bead ID passed to a sling_query must start with an ASCII letter or digit and hold only those and '.', '_' and '-', because a batch-file sling program hands it to cmd.exe, which runs & | < > as commands and expands %%NAME%% whatever the quoting", beadID)
}
