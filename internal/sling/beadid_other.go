//go:build !windows

package sling

// CheckSlingQueryBeadID reports whether beadID may be substituted for {} in a
// sling_query. Outside Windows every ID may: runtime.ShellQuote makes it one
// literal sh word.
func CheckSlingQueryBeadID(string) error {
	return nil
}
