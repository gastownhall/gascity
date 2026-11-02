package orders

import (
	"strings"
	"testing"
)

func TestParseRequiresBackend(t *testing.T) {
	a, err := Parse([]byte(`
[order]
exec = "gc dolt health"
trigger = "cooldown"
interval = "30s"
requires_backend = ["dolt", "doltlite"]
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := strings.Join(a.RequiresBackend, ","); got != "dolt,doltlite" {
		t.Fatalf("RequiresBackend = %q, want dolt,doltlite", got)
	}
	if err := Validate(a); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestParseRequiresBackendAbsentIsUnrestricted(t *testing.T) {
	a, err := Parse([]byte(`
[order]
exec = "true"
trigger = "manual"
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.RequiresBackend != nil {
		t.Fatalf("RequiresBackend = %v, want nil", a.RequiresBackend)
	}
	for _, backend := range []string{"dolt", "doltlite", "postgres", "http", "something-else"} {
		if !a.AppliesToBackend(backend) {
			t.Errorf("an order with no requires_backend must apply to %q", backend)
		}
	}
}

func TestValidateRequiresBackendAcceptsKnownValues(t *testing.T) {
	for _, backend := range RequiresBackendValues() {
		a := Order{Name: "o", Exec: "true", Trigger: "manual", RequiresBackend: []string{backend}}
		if err := Validate(a); err != nil {
			t.Errorf("Validate(requires_backend=[%q]) = %v, want nil", backend, err)
		}
	}
	if got := strings.Join(RequiresBackendValues(), ","); got != "dolt,doltlite,postgres,http" {
		t.Fatalf("RequiresBackendValues() = %q, want dolt,doltlite,postgres,http", got)
	}
}

func TestValidateRequiresBackendRejectsUnknownValues(t *testing.T) {
	for _, tc := range []struct {
		name     string
		backends []string
	}{
		{"unknown", []string{"mysql"}},
		{"empty string", []string{""}},
		{"wrong case", []string{"Dolt"}},
		{"bd alias", []string{"bd"}},
		{"one bad among good", []string{"dolt", "sqlite"}},
		{"empty list", []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := Order{Name: "o", Exec: "true", Trigger: "manual", RequiresBackend: tc.backends}
			err := Validate(a)
			if err == nil {
				t.Fatalf("Validate(requires_backend=%q) = nil, want error", tc.backends)
			}
			if !strings.Contains(err.Error(), "requires_backend") {
				t.Fatalf("error %q does not name requires_backend", err)
			}
		})
	}
}

func TestOrderAppliesToBackend(t *testing.T) {
	a := Order{RequiresBackend: []string{"dolt"}}
	if !a.AppliesToBackend("dolt") {
		t.Fatal("a dolt order must apply to a dolt scope")
	}
	for _, backend := range []string{"doltlite", "postgres", "http", "", "unknown"} {
		if a.AppliesToBackend(backend) {
			t.Errorf("a dolt order must not apply to backend %q", backend)
		}
	}
}
