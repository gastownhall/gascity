//go:build !windows

package config

// openOptionValueFits reports whether value may be substituted into an open
// option's FlagTemplate. Every value fits: the rendered args are joined into
// the launch command with POSIX quoting, which sh reads back as the same
// literal words.
func openOptionValueFits(string) bool {
	return true
}

// openOptionValueRule states the rule openOptionValueFits enforces, for the
// message that refuses a value under it. Nothing is refused here.
const openOptionValueRule = ""
