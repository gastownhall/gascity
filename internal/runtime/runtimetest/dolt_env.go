package runtimetest

// WithheldManagedDoltEnv returns the Dolt keys of a session env whose
// gc-managed local port the controller withheld, so the session's host-local
// bd follows the scope's .beads/dolt-server.port mirror across a managed Dolt
// restart onto a new port (gastownhall/gascity#1656).
//
// It is the contract between the two halves of that withhold: cmd/gc asserts
// that a managed-direct session env carries exactly these values, and every
// runtime adapter that rebuilds a session's Dolt target for a process off the
// controller host (the k8s pod projection, the t3bridge thread env) is tested
// against them. Host, port and socket are empty, GC_DOLT_MANAGED_LOCAL marks
// the target as the gc-managed local server (an empty port alone also means a
// scope without Dolt or a failed resolution), and the shared-server pin keeps
// bd off ~/.beads/shared-server once no env port outranks it.
func WithheldManagedDoltEnv() map[string]string {
	return map[string]string{
		"GC_DOLT_HOST":             "",
		"GC_DOLT_PORT":             "",
		"GC_DOLT_MANAGED_LOCAL":    "1",
		"BEADS_DOLT_SERVER_HOST":   "",
		"BEADS_DOLT_SERVER_PORT":   "",
		"BEADS_DOLT_SERVER_SOCKET": "",
		"BEADS_DOLT_PORT":          "",
		"BEADS_DOLT_SHARED_SERVER": "0",
		"BD_DOLT_SHARED_SERVER":    "false",
	}
}
