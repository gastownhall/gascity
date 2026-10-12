package beads

import (
	"errors"
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
