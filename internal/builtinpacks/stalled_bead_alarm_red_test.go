package builtinpacks

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/orders"
)

func TestMaterializedCorePackDiscoversStalledBeadAlarmOrder(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "bundled-cache")
	if err := MaterializeSyntheticRepo(cache, Repository, testCommit); err != nil {
		t.Fatalf("materialize bundled repository: %v", err)
	}
	path := filepath.Join(cache, "internal", "bootstrap", "packs", "core", "orders", "stalled-bead-alarm.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read installed core order %s: %v", path, err)
	}
	order, err := orders.Parse(data)
	if err != nil {
		t.Fatalf("parse installed core order: %v", err)
	}
	order.Name = "stalled-bead-alarm"
	if err := orders.Validate(order); err != nil {
		t.Fatalf("validate installed core order: %v", err)
	}
	if order.Trigger != "cooldown" || order.Exec != "gc beads stall-check" || !order.NoWorkGate || !order.Idempotent {
		t.Fatalf("installed order = %+v, want cooldown direct stall-check with no_work_gate and idempotent", order)
	}
}
