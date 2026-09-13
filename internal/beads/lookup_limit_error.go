package beads

import (
	"errors"
	"fmt"
	"strings"
)

// LookupLimitError reports that a bounded bead lookup exceeded its cap.
type LookupLimitError struct {
	Kind  string
	Label string
	Limit int
}

func (e LookupLimitError) Error() string {
	kind := strings.TrimSpace(e.Kind)
	if kind == "" {
		kind = "bead"
	}
	if e.Label == "" {
		return fmt.Sprintf("%s lookup hit limit %d", kind, e.Limit)
	}
	return fmt.Sprintf("%s lookup hit limit %d for label %q", kind, e.Limit, e.Label)
}

// IsLookupLimitError reports whether err is a bounded lookup cap error.
func IsLookupLimitError(err error) bool {
	var value LookupLimitError
	if errors.As(err, &value) {
		return true
	}
	var pointer *LookupLimitError
	return errors.As(err, &pointer)
}
