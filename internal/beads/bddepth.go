package beads

import (
	"fmt"
	"os"
	"strconv"
)

const (
	// BdDepthEnvKey names the environment variable that counts how many bd
	// launches deep this process is. Every launch of a bd binary stamps it.
	BdDepthEnvKey = "GC_BD_DEPTH"

	// MaxBdDepth is the deepest bd launch chain gc allows. A bd on PATH that
	// calls `gc bd` makes gc launch bd again without end; the fuse turns that
	// into a refusal after MaxBdDepth launches.
	MaxBdDepth = 16
)

// BdDepthError is returned by GuardBdLaunch when a bd launch is refused: the
// process is already MaxBdDepth launches deep, or its GC_BD_DEPTH is not a
// non-negative decimal integer. It is final, so the bd retry loops must not
// retry it.
type BdDepthError struct {
	// BdPath is the resolved bd binary whose launch was refused.
	BdPath string
	// Value is the raw GC_BD_DEPTH of the refusing process.
	Value string
	// Depth is the parsed depth; meaningful only when Malformed is false.
	Depth uint64
	// Malformed reports that Value did not parse.
	Malformed bool
}

// Error implements error.
func (e *BdDepthError) Error() string {
	if e.Malformed {
		return fmt.Sprintf("refusing to launch bd %s: %s=%q is not a non-negative decimal integer", e.BdPath, BdDepthEnvKey, e.Value)
	}
	return fmt.Sprintf("refusing to launch bd %s: %s=%d reached the bound %d; a bd that calls gc bd recurses (see ga-2t6u9dy)", e.BdPath, BdDepthEnvKey, e.Depth, MaxBdDepth)
}

// GuardBdLaunch is the recursion fuse every launch of a bd binary goes through,
// called immediately before the exec.Command* line with the resolved bd path
// and the child's environment. It reads GC_BD_DEPTH from this process's
// environment (unset or empty is 0), never from env, and returns a *BdDepthError
// when depth+1 would exceed MaxBdDepth. Otherwise it returns a copy of env with
// GC_BD_DEPTH=<depth+1> as the last entry, so the stamp wins over any earlier
// one. A nil env means the child inherits everything, so it is materialized
// from os.Environ before the stamp is appended. The process environment is
// never written.
func GuardBdLaunch(bdPath string, env []string) ([]string, error) {
	var depth uint64
	if raw := os.Getenv(BdDepthEnvKey); raw != "" {
		n, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return nil, &BdDepthError{BdPath: bdPath, Value: raw, Malformed: true}
		}
		if n >= MaxBdDepth {
			return nil, &BdDepthError{BdPath: bdPath, Value: raw, Depth: n}
		}
		depth = n
	}
	if env == nil {
		env = os.Environ()
	}
	out := make([]string, 0, len(env)+1)
	out = append(out, env...)
	return append(out, BdDepthEnvKey+"="+strconv.FormatUint(depth+1, 10)), nil
}
