package opjournal

import (
	"strconv"
	"testing"
)

// Kills a formatter that drifts from the lane's inline form or from what
// the store validates, and a runtime key without its prefix (M18).
func TestFormatDeliveryKey(t *testing.T) {
	const head = "gc-op-abcdefghijklmnopqrstuvwxyz"
	for _, n := range []uint64{1, 7, 1 << 40} {
		k := FormatDeliveryKey(head, n)
		if lane := head + ":" + strconv.FormatUint(n, 10); k != lane {
			t.Fatalf("FormatDeliveryKey = %q, want the lane's %q", k, lane)
		}
		if err := validateDeliveryKey(k); err != nil {
			t.Fatalf("validateDeliveryKey(%q): %v", k, err)
		}
		gotHead, gotN, ok := splitDeliveryKey(k)
		if !ok || gotHead != head || uint64(gotN) != n {
			t.Fatalf("splitDeliveryKey(%q) = %q, %d, %v", k, gotHead, gotN, ok)
		}
		if rk := RuntimeDeliveryKey(k); rk != "gc:"+k || rk[:len(RuntimeDeliveryKeyPrefix)] != RuntimeDeliveryKeyPrefix {
			t.Fatalf("RuntimeDeliveryKey(%q) = %q", k, rk)
		}
	}
}
