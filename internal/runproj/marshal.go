package runproj

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// The union DTO types below are TypeScript discriminated unions. Go has no
// native tagged unions, so each carries every arm's fields and a custom
// MarshalJSON that emits exactly the active arm's keys, in the same order the
// TS object literals use. Key order is load-bearing for byte-for-byte golden
// parity (the generator used JSON.stringify, which preserves insertion order).
//
// We build each object with an ordered list of (key, value) pairs rather than a
// map, because encoding/json sorts map keys and would break parity.

type kv struct {
	key   string
	value any
}

// marshalObject renders an ordered set of key/value pairs as a JSON object,
// preserving the given key order (unlike a Go map, which json sorts).
func marshalObject(pairs []kv) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, p := range pairs {
		if i > 0 {
			buf.WriteByte(',')
		}
		k, err := json.Marshal(p.key)
		if err != nil {
			return nil, err
		}
		buf.Write(k)
		buf.WriteByte(':')
		v, err := json.Marshal(p.value)
		if err != nil {
			return nil, err
		}
		buf.Write(v)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// MarshalJSON renders the active formula arm. TS: {status:'known', name} |
// {status:'unavailable', error}.
func (f RunLaneFormula) MarshalJSON() ([]byte, error) {
	switch f.Status {
	case "known":
		return marshalObject([]kv{{"status", "known"}, {"name", f.Name}})
	case "unavailable":
		return marshalObject([]kv{{"status", "unavailable"}, {"error", f.Error}})
	default:
		return nil, fmt.Errorf("runproj: invalid RunLaneFormula status %q", f.Status)
	}
}

// MarshalJSON renders the active scope arm. TS: {status:'available', kind, ref,
// rootStoreRef} | {status:'unavailable', error}.
func (s RunLaneScope) MarshalJSON() ([]byte, error) {
	switch s.Status {
	case "available":
		return marshalObject([]kv{
			{"status", "available"},
			{"kind", s.Kind},
			{"ref", s.Ref},
			{"rootStoreRef", s.RootStoreRef},
		})
	case "unavailable":
		return marshalObject([]kv{{"status", "unavailable"}, {"error", s.Error}})
	default:
		return nil, fmt.Errorf("runproj: invalid RunLaneScope status %q", s.Status)
	}
}

// MarshalJSON renders the active external-reference arm. TS:
// {status:'available', label, url} | {status:'label_only', label} |
// {status:'unavailable', error}.
func (e RunLaneExternalReference) MarshalJSON() ([]byte, error) {
	switch e.Status {
	case "available":
		return marshalObject([]kv{
			{"status", "available"},
			{"label", e.Label},
			{"url", e.URL},
		})
	case "label_only":
		return marshalObject([]kv{{"status", "label_only"}, {"label", e.Label}})
	case "unavailable":
		return marshalObject([]kv{{"status", "unavailable"}, {"error", e.Error}})
	default:
		return nil, fmt.Errorf("runproj: invalid RunLaneExternalReference status %q", e.Status)
	}
}

// MarshalJSON renders the active updated-at arm. TS: {status:'available', at} |
// {status:'unavailable', error}.
func (u RunLaneUpdatedAt) MarshalJSON() ([]byte, error) {
	switch u.Status {
	case "available":
		return marshalObject([]kv{{"status", "available"}, {"at", u.At}})
	case "unavailable":
		return marshalObject([]kv{{"status", "unavailable"}, {"error", u.Error}})
	default:
		return nil, fmt.Errorf("runproj: invalid RunLaneUpdatedAt status %q", u.Status)
	}
}

// MarshalJSON renders the active progress arm. TS: {status:'active_step',
// stepId, stage, attempt} | {status:'stage_only', stage, error} |
// {status:'unavailable', error}.
func (p RunLaneProgress) MarshalJSON() ([]byte, error) {
	switch p.Status {
	case "active_step":
		return marshalObject([]kv{
			{"status", "active_step"},
			{"stepId", p.StepID},
			{"stage", p.Stage},
			{"attempt", p.Attempt},
		})
	case "stage_only":
		return marshalObject([]kv{
			{"status", "stage_only"},
			{"stage", p.Stage},
			{"error", p.Error},
		})
	case "unavailable":
		return marshalObject([]kv{{"status", "unavailable"}, {"error", p.Error}})
	default:
		return nil, fmt.Errorf("runproj: invalid RunLaneProgress status %q", p.Status)
	}
}

// MarshalJSON renders the active stage-position arm. TS: {status:'available',
// index, key, label} | {status:'unavailable', error}.
func (s RunLaneStagePosition) MarshalJSON() ([]byte, error) {
	switch s.Status {
	case "available":
		return marshalObject([]kv{
			{"status", "available"},
			{"index", s.Index},
			{"key", s.Key},
			{"label", s.Label},
		})
	case "unavailable":
		return marshalObject([]kv{{"status", "unavailable"}, {"error", s.Error}})
	default:
		return nil, fmt.Errorf("runproj: invalid RunLaneStagePosition status %q", s.Status)
	}
}

// MarshalJSON renders the active step-attempt arm. TS: {status:'available',
// value} | {status:'unavailable', error}.
func (a RunLaneStepAttempt) MarshalJSON() ([]byte, error) {
	switch a.Status {
	case "available":
		return marshalObject([]kv{{"status", "available"}, {"value", a.Value}})
	case "unavailable":
		return marshalObject([]kv{{"status", "unavailable"}, {"error", a.Error}})
	default:
		return nil, fmt.Errorf("runproj: invalid RunLaneStepAttempt status %q", a.Status)
	}
}

// MarshalJSON renders the lane-health arm. The builder only ever emits the
// unavailable arm. TS: {status:'unavailable', error}.
func (h RunLaneHealthState) MarshalJSON() ([]byte, error) {
	switch h.Status {
	case "unavailable":
		return marshalObject([]kv{{"status", "unavailable"}, {"error", h.Error}})
	default:
		return nil, fmt.Errorf("runproj: invalid RunLaneHealthState status %q", h.Status)
	}
}

// MarshalJSON renders the census arm. The builder only ever emits the
// unavailable arm. TS: {status:'unavailable', error}.
func (c RunCensusState) MarshalJSON() ([]byte, error) {
	switch c.Status {
	case "unavailable":
		return marshalObject([]kv{{"status", "unavailable"}, {"error", c.Error}})
	default:
		return nil, fmt.Errorf("runproj: invalid RunCensusState status %q", c.Status)
	}
}
