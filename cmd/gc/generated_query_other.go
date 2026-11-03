//go:build !windows

package main

// generatedProgram returns nil: here every command, generated or not, runs
// under `sh -c`, the platform's interpreter.
func generatedProgram(string) generatedProgramRunner { return nil }
