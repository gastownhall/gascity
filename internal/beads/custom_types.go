package beads

// RequiredCustomTypes lists the bead types Gas City registers with every bd
// store it provisions (city and rigs). gc writes beads of each type, and a
// store that validates types refuses a write or a typed listing of one it does
// not know. doctor.CustomTypesCheck verifies and repairs the registration.
var RequiredCustomTypes = []string{
	"molecule", "convoy", "message", "event", "gate",
	"merge-request", "agent", "role", "rig", "session", "spec",
	"convergence", "step", "startup-health-episode",
}
