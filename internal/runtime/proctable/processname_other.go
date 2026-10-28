//go:build !windows

package proctable

// processNameKey is the form process names are compared in: the name itself,
// case-sensitively, exactly as the platform reports it.
func processNameKey(name string) string {
	return name
}
