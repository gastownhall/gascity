//go:build windows

package runtime

// GCCommandName is how an agent's shell invokes gc. In PowerShell a bare gc is
// the Get-Content alias, so text that tells an agent which command to run
// names gc.exe.
const GCCommandName = "gc.exe"
