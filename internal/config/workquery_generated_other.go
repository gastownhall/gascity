//go:build !windows

package config

// noteGeneratedQuery records nothing: here every generated query runs under
// `sh -c`, as generated.
func noteGeneratedQuery(queryKind, string) {}
