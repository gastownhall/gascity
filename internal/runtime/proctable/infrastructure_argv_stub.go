//go:build !linux && !darwin

package proctable

import "errors"

// rootArgv is unavailable on platforms without process table access; the
// scanner reports no roots there either.
func rootArgv(int) ([]string, error) {
	return nil, errors.New("proctable: argv unavailable on this platform")
}
