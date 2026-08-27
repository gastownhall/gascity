package main

import (
	"reflect"
	"strings"
	"testing"
)

// TestSessionStartAdmissionLeaseFamilyNamesEveryCertifiedFamily pins the
// `start_lease` vocabulary the start-commit trace publishes.
//
// The v59 journey and the WD.15 parity join both filter start-commit records on
// this string because the admission SOURCE cannot answer "which family
// authorized this start": it is sticky to whichever entry admitted the key first
// (sessionStartController.admit), so a certified wake that lost the race to a
// bead.updated admission still traces as `in_process`. ga-f7v2ft.142 ruled that
// asserting on the source proves nothing, and ga-ij8mh ruling 3 re-pointed the
// journey onto the lease; ga-f7v2ft.157 found the two configured-named pin legs
// had been missed by that sweep.
//
// The guard that matters here is TOTALITY. A new certified family that reaches
// the start-commit boundary without an arm in sessionStartAdmissionLeaseFamily
// would trace as an ordinary keyed start, and every lease-filtered assertion
// would silently stop seeing it — the exact failure mode (zero matching records,
// not wrong-shaped ones) that produced ga-f7v2ft.157.
func TestSessionStartAdmissionLeaseFamilyNamesEveryCertifiedFamily(t *testing.T) {
	tests := []struct {
		name      string
		admission sessionStartAdmission
		want      string
	}{
		{
			name:      "configured dependency",
			admission: sessionStartAdmission{ConfiguredDependency: &configuredDependencyStartLease{}},
			want:      configuredDependencyLeaseFamily,
		},
		{
			name:      "configured named wake",
			admission: sessionStartAdmission{ConfiguredNamedWake: &configuredNamedWakeStartLease{}},
			want:      configuredNamedWakeLeaseFamily,
		},
		{
			name:      "strict default pool wake",
			admission: sessionStartAdmission{StrictDefaultPoolWake: &strictDefaultPoolWakeStartLease{}},
			want:      strictDefaultPoolWakeLeaseFamily,
		},
		{
			name:      "wait dependency",
			admission: sessionStartAdmission{WaitDependency: &sessionWaitDependencyStartLease{}},
			want:      waitDependencyLeaseFamily,
		},
		{
			name:      "pool allocation",
			admission: sessionStartAdmission{PoolAllocation: &routedWorkPoolStartLease{}},
			want:      poolAllocationLeaseFamily,
		},
		{
			name:      "ordinary keyed start carries no lease",
			admission: sessionStartAdmission{},
			want:      "",
		},
	}
	seen := map[string]string{}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := sessionStartAdmissionLeaseFamily(test.admission); got != test.want {
				t.Fatalf("sessionStartAdmissionLeaseFamily(%s) = %q, want %q", test.name, got, test.want)
			}
		})
		if test.want == "" {
			continue
		}
		if prior, clash := seen[test.want]; clash {
			t.Fatalf("lease family %q names both %q and %q; the journey filters cannot tell them apart",
				test.want, prior, test.name)
		}
		seen[test.want] = test.name
	}
}

// TestEveryAdmissionLeaseIsNamedOrExplicitlyExcluded is the totality guard with
// teeth: it walks sessionStartAdmission by reflection instead of trusting a
// hand-maintained table, so a lease field added tomorrow fails the build unless
// it is either given a family name or listed as a deliberate exclusion.
//
// Without this, a new certified family reaching the start-commit boundary would
// simply trace as an ordinary keyed start and every lease-filtered assertion
// would stop seeing it — silently, with zero matching records rather than
// wrong-shaped ones. That is precisely how ga-f7v2ft.157 presented.
func TestEveryAdmissionLeaseIsNamedOrExplicitlyExcluded(t *testing.T) {
	// PoolDrainAck fences a STOP, not a start. It can ride the same admission
	// key, but the start-commit trace is a start boundary, so naming it there
	// would attribute a start to a drain acknowledgement.
	excluded := map[string]string{
		"PoolDrainAck": "drain acknowledgement is a stop lease; it authorizes no start",
	}
	admissionType := reflect.TypeOf(sessionStartAdmission{})
	for i := range admissionType.NumField() {
		field := admissionType.Field(i)
		if field.Type.Kind() != reflect.Pointer || field.Type.Elem().Kind() != reflect.Struct ||
			!strings.HasSuffix(field.Type.Elem().Name(), "Lease") {
			continue
		}
		if _, ok := excluded[field.Name]; ok {
			continue
		}
		admission := reflect.New(admissionType).Elem()
		admission.Field(i).Set(reflect.New(field.Type.Elem()))
		family := sessionStartAdmissionLeaseFamily(admission.Interface().(sessionStartAdmission))
		if family == "" {
			t.Fatalf("admission lease field %s (%s) has no start_lease family; add an arm to "+
				"sessionStartAdmissionLeaseFamily or an entry to this test's exclusion set with a reason",
				field.Name, field.Type.Elem().Name())
		}
	}
}

// TestSessionStartAdmissionLeaseFamilyPrefersTheWakeFamiliesOverPoolAllocation
// pins the arm ORDER, which is load-bearing for the pin legs: an admission can
// carry a wake lease and a pool-allocation lease at once (the controller
// coalesces them onto one key), and the journey asks "did the wake family
// authorize this start". Reporting `pool_allocation` there would read as a
// different owner and drop the record from the filter.
func TestSessionStartAdmissionLeaseFamilyPrefersTheWakeFamiliesOverPoolAllocation(t *testing.T) {
	admission := sessionStartAdmission{
		ConfiguredNamedWake: &configuredNamedWakeStartLease{},
		PoolAllocation:      &routedWorkPoolStartLease{},
	}
	if got := sessionStartAdmissionLeaseFamily(admission); got != configuredNamedWakeLeaseFamily {
		t.Fatalf("coalesced wake + pool admission lease family = %q, want %q", got, configuredNamedWakeLeaseFamily)
	}
}
