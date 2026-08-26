package beads

import (
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
)

type bdUpdateFailureBody struct {
	Error         string               `json:"error"`
	Failed        []bdUpdateIDFailure  `json:"failed"`
	SchemaVersion int                  `json:"schema_version"`
	Data          *bdUpdateFailureBody `json:"data"`
}

type bdUpdateIDFailure struct {
	ID    string `json:"id"`
	Error string `json:"error"`
	// Present only for --if-* guard failures. Including it keeps strict JSON
	// decoding aligned with bd's exact updateIDFailure wire type.
	GuardMismatch bool `json:"guard_mismatch"`
}

// classifyBDUpdateCloseBlocked accepts only bd's exact structured, single-ID
// update refusal. Exit 1 or human text alone is shared by infrastructure and
// policy failures, so neither is enough authority for a caller to force-close.
func classifyBDUpdateClosePolicy(id string, err error) error {
	if err == nil || bdExitCode(err) != 1 {
		return nil
	}
	detail := strings.TrimSpace(err.Error())
	lastBreak := strings.LastIndexByte(detail, '\n')
	if lastBreak < 0 {
		return nil
	}
	report := strings.TrimSpace(detail[lastBreak+1:])
	if len(report) == 0 || len(report) > 64<<10 || report[0] != '{' {
		return nil
	}

	decoder := json.NewDecoder(strings.NewReader(report))
	decoder.DisallowUnknownFields()
	var envelope bdUpdateFailureBody
	if decoder.Decode(&envelope) != nil {
		return nil
	}
	if decodeErr := decoder.Decode(&struct{}{}); decodeErr != io.EOF {
		return nil
	}

	body := &envelope
	if envelope.Data != nil {
		if envelope.SchemaVersion != 1 || envelope.Error != "" || len(envelope.Failed) != 0 ||
			envelope.Data.SchemaVersion != 0 || envelope.Data.Data != nil {
			return nil
		}
		body = envelope.Data
	} else if envelope.SchemaVersion != 1 {
		return nil
	}
	if body.Error != "1 of 1 issues failed to update" || len(body.Failed) != 1 {
		return nil
	}
	failure := body.Failed[0]
	if failure.ID != id {
		return nil
	}
	if isBDCloseBlockedMessage(id, failure.Error) {
		return ErrCloseBlocked
	}
	if isBDCloseOpenChildrenMessage(id, failure.Error) {
		return ErrCloseOpenChildren
	}
	return nil
}

func classifyBDUpdateCloseBlocked(id string, err error) bool {
	return errors.Is(classifyBDUpdateClosePolicy(id, err), ErrCloseBlocked)
}

func isBDCloseBlockedMessage(id, message string) bool {
	const updateWrapper = "updating issue: "
	// Direct updates wrap storage failures once; proxied-server updates expose
	// the same typed failure without that wrapper. The exact policy grammar
	// below remains the authority in either route.
	message = strings.TrimPrefix(message, updateWrapper)
	prefix := "cannot close blocked issue: " + id + " is blocked by ["
	return strings.HasPrefix(message, prefix) && strings.HasSuffix(message, "] (use --force to override)")
}

func isBDCloseOpenChildrenMessage(id, message string) bool {
	const updateWrapper = "updating issue: "
	// See isBDCloseBlockedMessage: proxied-server deliberately omits the
	// direct route's single storage-error wrapper.
	message = strings.TrimPrefix(message, updateWrapper)
	prefix := "cannot close " + id + ": "
	const suffix = " open child issue(s); close children first or use --force to override"
	if !strings.HasPrefix(message, prefix) || !strings.HasSuffix(message, suffix) {
		return false
	}
	countText := strings.TrimSuffix(strings.TrimPrefix(message, prefix), suffix)
	count, err := strconv.Atoi(countText)
	return err == nil && count > 0 && strconv.Itoa(count) == countText
}
