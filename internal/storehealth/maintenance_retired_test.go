package storehealth

import (
	"reflect"
	"testing"
)

// TestHealthCarriesNoMaintenanceRunFields pins that Health no longer reports
// the retired maintenance loop's last-run fields, so the status contract
// cannot show data nothing produces any more.
func TestHealthCarriesNoMaintenanceRunFields(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeOf(Health{})
	for _, name := range []string{"LastGCAt", "LastGCStatus"} {
		if _, ok := typ.FieldByName(name); ok {
			t.Errorf("storehealth.Health still has retired field %s", name)
		}
	}
}
