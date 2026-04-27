// Package progname provides the runtime binary name for use in error messages
// and user-facing output. The name is set once at startup by cmd/gc via Set()
// and defaults to "gc".
package progname

var name = "gc"

// Set sets the binary name. Call once at startup from main().
func Set(n string) { name = n }

// Get returns the binary name.
func Get() string { return name }
