//go:build !linux && !darwin

package proctable

// OpenFiles returns nothing on platforms without open-file scanning support.
func OpenFiles(int) ([]string, error) {
	return nil, nil
}
