//go:build !windows

package main

// cityDoltReadinessBelongsToBd is false outside Windows: the bd environment
// builder keeps its upstream recovery ladder.
func cityDoltReadinessBelongsToBd(string) bool { return false }
