package beads

import (
	"errors"
	"strings"
	"testing"
)

// TestBdStoreRefusesASplitMetadataKey (mc-ojhnu): every bd update entry
// refuses a metadata key containing "=", which bd's --set-metadata would
// split into another key's write, before running bd.
func TestBdStoreRefusesASplitMetadataKey(t *testing.T) {
	ran := false
	s := NewBdStore(t.TempDir(), func(string, string, ...string) ([]byte, error) {
		ran = true
		return []byte("{}"), nil
	})
	bad := map[string]string{"wait_hold=true": ""}
	for name, call := range map[string]func() error{
		"Update":           func() error { return s.Update("b-1", UpdateOpts{Metadata: bad}) },
		"SetMetadata":      func() error { return s.SetMetadata("b-1", "wait_hold=true", "") },
		"SetMetadataBatch": func() error { return s.SetMetadataBatch("b-1", bad) },
		"UpdateAll":        func() error { _, err := s.UpdateAll([]string{"b-1"}, UpdateOpts{Metadata: bad}); return err },
		"UpdateReporting":  func() error { _, err := s.UpdateReporting("b-1", UpdateOpts{Metadata: bad}); return err },
	} {
		ran = false
		if err := call(); !errors.Is(err, ErrBdSplitMetadataKey) || ran {
			t.Errorf("%s = %v (bd ran %v), want ErrBdSplitMetadataKey before bd", name, err, ran)
		}
	}
}

// TestBdStoreRefusesASplitKeyOrLabelPastItsProbes (round 2, B05-B08): the
// entry points that probe first (UpdateIfMatch's capability, UpdateIfAssignment's
// collision read) or go through CloseAll and CompareAndSetMetadataKey refuse
// a split metadata key, and every update and create entry refuses a CSV
// label, before any bd write.
func TestBdStoreRefusesASplitKeyOrLabelPastItsProbes(t *testing.T) {
	var writes []string
	s := NewBdStore(t.TempDir(), func(_, _ string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[len(args)-1] == "--help" {
			return []byte(conditionalWriteFlag), nil // capable
		}
		if len(args) > 0 && (args[0] == "update" || args[0] == "close" || args[0] == "create") {
			writes = append(writes, args[0])
		}
		return nil, errors.New("not found")
	})
	key := map[string]string{"wait_hold=true": ""}
	label := []string{"note,gc:session"}
	for name, c := range map[string]struct {
		call func() error
		want error
	}{
		"CloseAll":                 {func() error { _, err := s.CloseAll([]string{"b-1"}, key); return err }, ErrBdSplitMetadataKey},
		"UpdateIfMatch":            {func() error { return s.UpdateIfMatch("b-1", 1, UpdateOpts{Metadata: key}) }, ErrBdSplitMetadataKey},
		"CompareAndSetMetadataKey": {func() error { _, err := s.CompareAndSetMetadataKey("b-1", "wait_hold=true", "", "x"); return err }, ErrBdSplitMetadataKey},
		"UpdateIfAssignment": {func() error {
			_, err := s.UpdateIfAssignment("b-1", "open", "a", UpdateOpts{Metadata: key})
			return err
		}, ErrBdSplitMetadataKey},
		"Update with a label":     {func() error { return s.Update("b-1", UpdateOpts{Labels: label}) }, ErrBdSplitLabel},
		"Update removing a label": {func() error { return s.Update("b-1", UpdateOpts{RemoveLabels: []string{`"gc:session"`}}) }, ErrBdSplitLabel},
		"UpdateAll a label":       {func() error { _, err := s.UpdateAll([]string{"b-1"}, UpdateOpts{Labels: label}); return err }, ErrBdSplitLabel},
		"Create a label":          {func() error { _, err := s.Create(Bead{Title: "w", Labels: label}); return err }, ErrBdSplitLabel},
	} {
		writes = nil
		if err := c.call(); !errors.Is(err, c.want) || len(writes) != 0 {
			t.Errorf("%s = %v (bd writes %v), want %v before any write", name, err, writes, c.want)
		}
	}
}

// TestBdStoreRefusesALabelCSVReadsAsAnother (round 3, D05-D07): bd parses a
// label as one CSV record, so a newline or trailing CR would add or match
// gc:session. UpdateReporting and Create refuse one with ErrBdSplitLabel,
// adding or removing, before any bd write; UpdateIfMatch and
// UpdateIfAssignment refuse every label before they write.
func TestBdStoreRefusesALabelCSVReadsAsAnother(t *testing.T) {
	var writes []string
	s := NewBdStore(t.TempDir(), func(_, _ string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[len(args)-1] == "--help" {
			return []byte(conditionalWriteFlag), nil // capable
		}
		if len(args) > 0 && (args[0] == "update" || args[0] == "create") {
			writes = append(writes, args[0])
		}
		return nil, errors.New("not found")
	})
	for _, label := range []string{"gc:session\n", "\ngc:session", "gc:session\r"} {
		for _, opts := range []UpdateOpts{{Labels: []string{label}}, {RemoveLabels: []string{label}}} {
			var unsupported *ConditionalUpdateFieldUnsupportedError
			for name, call := range map[string]func() error{
				"UpdateIfMatch": func() error { return s.UpdateIfMatch("b-1", 1, opts) },
				"UpdateIfAssignment": func() error {
					_, err := s.UpdateIfAssignment("b-1", "open", "a", opts)
					return err
				},
				"UpdateReporting": func() error { _, err := s.UpdateReporting("b-1", opts); return err },
				"Create": func() error {
					_, err := s.Create(Bead{Title: "w", Labels: append(opts.Labels, opts.RemoveLabels...)})
					return err
				},
			} {
				writes = nil
				err := call()
				refused := errors.Is(err, ErrBdSplitLabel) || strings.HasPrefix(name, "UpdateIf") && errors.As(err, &unsupported)
				if !refused || len(writes) != 0 {
					t.Errorf("%s(%+v) = %v (bd writes %v), want ErrBdSplitLabel before any write", name, opts, err, writes)
				}
			}
		}
	}
}
