package beadmeta

import (
	"errors"
	"fmt"
)

// ErrInvalidInputLifecycle identifies an unknown formula or persisted root policy.
var ErrInvalidInputLifecycle = errors.New("invalid input lifecycle")

const (
	// InputLifecycleGraph leaves completion to the graph after source
	// settlement; input proven purged from every store still retires it.
	InputLifecycleGraph = "graph"
	// InputLifecycleExternal retains terminal-input retirement.
	InputLifecycleExternal = "external"
)

// InputLifecycle validates a policy. Missing retains external retirement.
func InputLifecycle(value string) (string, error) {
	switch value {
	case "", InputLifecycleExternal:
		return InputLifecycleExternal, nil
	case InputLifecycleGraph:
		return value, nil
	default:
		return "", fmt.Errorf("%w: input_lifecycle value %q, want graph or external", ErrInvalidInputLifecycle, value)
	}
}
