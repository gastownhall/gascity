package git

import (
	"errors"
	"os"
	"strings"
	"syscall"
)

// SameCommit reports whether actual matches expected, allowing the same
// case-insensitive and abbreviated commit forms accepted by git cache checks.
func SameCommit(actual, expected string) bool {
	actual = strings.TrimSpace(actual)
	expected = strings.TrimSpace(expected)
	if actual == "" || expected == "" {
		return false
	}
	if strings.EqualFold(actual, expected) {
		return true
	}
	return len(expected) >= 7 && len(expected) < len(actual) && strings.HasPrefix(strings.ToLower(actual), strings.ToLower(expected))
}

// MissingCheckoutMarker reports whether statting a repo cache's .git entry
// proved that the cache path is not a usable git checkout.
func MissingCheckoutMarker(info os.FileInfo, err error) bool {
	if err != nil {
		return os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR)
	}
	return info == nil || !info.IsDir()
}
