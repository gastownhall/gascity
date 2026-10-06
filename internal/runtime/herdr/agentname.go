package herdr

import (
	"fmt"
	"hash/fnv"
	"strings"
)

// herdrAgentName maps a gc session name to a valid herdr agent name. herdr
// ≥0.7.5 enforces ^[a-z][a-z0-9_-]{0,31}$ on agent names, while gc session
// names carry rig names verbatim ("Indigo--anthony") and can exceed 32
// characters — every such `agent start` was rejected with
// invalid_agent_name on every reconcile tick. The mapping is deterministic:
// lowercase, map any other rune to '-', prefix names that don't start with a
// letter, and compress over-long names to a 24-char head plus an fnv32 hash
// of the full original so distinct sessions stay distinct. The exact gc name
// is persisted at metaBoundName, which is the reverse map ListRunning uses.
func herdrAgentName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	s := b.String()
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		s = "a" + s
	}
	if len(s) > 32 {
		h := fnv.New32a()
		_, _ = h.Write([]byte(name))
		s = fmt.Sprintf("%s-%08x", s[:23], h.Sum32())
	}
	return s
}
