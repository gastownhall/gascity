package api

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/orders"
)

// errOrderNotFound / errOrderAmbiguous are sentinel errors so callers
// can dispatch with errors.Is instead of substring-matching error
// messages.
var (
	errOrderNotFound  = errors.New("order not found")
	errOrderAmbiguous = errors.New("ambiguous order name")
	errNoOrderStores  = errors.New("order bead stores unavailable")
)

type orderResponse struct {
	Name           string            `json:"name"`
	ScopedName     string            `json:"scoped_name"`
	Description    string            `json:"description,omitempty"`
	Type           string            `json:"type"`
	Trigger        string            `json:"trigger,omitempty"`
	Gate           string            `json:"gate,omitempty" deprecated:"true"`
	Interval       string            `json:"interval,omitempty"`
	Schedule       string            `json:"schedule,omitempty"`
	Check          string            `json:"check,omitempty"`
	On             string            `json:"on,omitempty"`
	Formula        string            `json:"formula,omitempty"`
	Exec           string            `json:"exec,omitempty"`
	Pool           string            `json:"pool,omitempty"`
	Timeout        string            `json:"timeout,omitempty"`
	TimeoutMs      int64             `json:"timeout_ms"`
	CheckTimeout   string            `json:"check_timeout,omitempty"`
	CheckTimeoutMs int64             `json:"check_timeout_ms,omitempty"`
	Enabled        bool              `json:"enabled"`
	Rig            string            `json:"rig,omitempty"`
	CaptureOutput  bool              `json:"capture_output"`
	Env            map[string]string `json:"env,omitempty"`
}

func resolveOrder(aa []orders.Order, name string) (*orders.Order, error) {
	// Scoped name is always unambiguous — try it first.
	for i, a := range aa {
		if a.ScopedName() == name {
			return &aa[i], nil
		}
	}
	// Bare name match — collect all matches to detect ambiguity.
	var matches []int
	for i, a := range aa {
		if a.Name == name {
			matches = append(matches, i)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("%w: %s", errOrderNotFound, name)
	case 1:
		return &aa[matches[0]], nil
	default:
		var scoped []string
		for _, idx := range matches {
			scoped = append(scoped, aa[idx].ScopedName())
		}
		return nil, fmt.Errorf("%w %q; use scoped name: %s", errOrderAmbiguous, name, strings.Join(scoped, ", "))
	}
}

func toOrderResponse(a orders.Order) orderResponse {
	typ := "formula"
	if a.IsExec() {
		typ = "exec"
	}
	resp := orderResponse{
		Name:          a.Name,
		ScopedName:    a.ScopedName(),
		Description:   a.Description,
		Type:          typ,
		Trigger:       a.Trigger,
		Gate:          a.Trigger, // Deprecated alias: mirror trigger during the migration window.
		Interval:      a.Interval,
		Schedule:      a.Schedule,
		Check:         a.Check,
		On:            a.On,
		Formula:       a.Formula,
		Exec:          a.Exec,
		Pool:          a.Pool,
		Timeout:       a.Timeout,
		TimeoutMs:     a.TimeoutOrDefault().Milliseconds(),
		CheckTimeout:  a.CheckTimeout,
		Enabled:       a.IsEnabled(),
		Rig:           a.Rig,
		CaptureOutput: a.IsExec(), // exec orders capture output
		Env:           a.Env,
	}
	// check_timeout bounds only a condition trigger's check command, so surface
	// its effective millisecond deadline only for condition orders. Other
	// triggers have no check and would otherwise report a phantom 10s default.
	if a.Trigger == "condition" {
		resp.CheckTimeoutMs = a.CheckTimeoutOrDefault().Milliseconds()
	}
	return resp
}
