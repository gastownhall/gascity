package opjournal

import (
	"testing"
)

// Kills a formatter that drifts from what the store validates, and a runtime
// key without its prefix (M18).
func TestFormatDeliveryKey(t *testing.T) {
	const head = "gc-op-abcdefghijklmnopqrstuvwxyz"
	for _, n := range []uint64{1, 7, 1 << 40} {
		k := FormatDeliveryKey(head, n)
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

// Kills a formatter that drifts from the key the lane's take assigns: a
// fresh take's delivery key is FormatDeliveryKey of its head op and 1.
func TestFormatDeliveryKeyIsTheLanesTakeKey(t *testing.T) {
	h := newLaneHarness(t)
	op := h.propose(t, laneMsg("p", RolePrompt, "do the thing"))
	exec := newFakeExec(h.clk)
	mustPass(t, h.lane(t, "ctl"), view(), exec)
	ds := exec.delivered()
	if len(ds) != 1 {
		t.Fatalf("%d deliveries, want 1", len(ds))
	}
	if want := FormatDeliveryKey(op.ID, 1); ds[0].Key != want {
		t.Fatalf("take key = %q, want FormatDeliveryKey's %q", ds[0].Key, want)
	}
}
