package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// retiredMaintenanceKeys are the [maintenance.dolt] keys the retired Dolt
// maintenance loop used to decode. A city.toml that still carries them must
// keep loading, with a warning rather than a fatal unknown-field error.
var retiredMaintenanceKeys = []string{
	"maintenance",
	"maintenance.dolt",
	"maintenance.dolt.enabled",
	"maintenance.dolt.interval",
	"maintenance.dolt.alert_to",
	"maintenance.dolt.gc_timeout",
}

// TestMaintenanceDoltKeysAreRetired pins that the legacy maintenance config is
// no longer a decoded struct and is registered in retiredKeys instead.
func TestMaintenanceDoltKeysAreRetired(t *testing.T) {
	t.Parallel()
	if _, ok := reflect.TypeOf(City{}).FieldByName("Maintenance"); ok {
		t.Error("City still has a Maintenance field; [maintenance.dolt] must be retired, not decoded")
	}
	for _, key := range retiredMaintenanceKeys {
		rk, ok := retiredKeys[key]
		if !ok {
			t.Errorf("retiredKeys has no entry for %q", key)
			continue
		}
		if rk.RemovedIn == "" {
			t.Errorf("retiredKeys[%q] has no RemovedIn anchor", key)
		}
	}
}

// TestStaleMaintenanceDoltStanzaStaysHarmless proves a city.toml that still has
// [maintenance.dolt] loads without error and reports only retired-key
// warnings, never an unknown-field failure.
func TestStaleMaintenanceDoltStanzaStaysHarmless(t *testing.T) {
	t.Parallel()
	data := `
[workspace]
name = "test"

[maintenance.dolt]
enabled = true
interval = "168h"
alert_to = "gascity/mayor"
gc_timeout = "10m"
`
	if _, err := Parse([]byte(data)); err != nil {
		t.Fatalf("a stale [maintenance.dolt] stanza must stay harmless; Parse: %v", err)
	}
	var cfg City
	md, err := toml.Decode(data, &cfg)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if fatal := fatalUndecodedWarnings(md, "city.toml"); len(fatal) != 0 {
		t.Errorf("stale stanza produced fatal warnings: %v", fatal)
	}
	joined := strings.Join(CheckUndecodedKeys(md, "city.toml"), "; ")
	if !strings.Contains(joined, "maintenance.dolt") || !strings.Contains(joined, "was retired in") {
		t.Errorf("stale stanza should surface a retired-key warning, got %q", joined)
	}
}
