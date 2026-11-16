package api

import (
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/beads"
)

// The status wire carries the controller's proxied-native account
// (BeadsDiagnostic.proxied); the CLI view must not drop it, or
// `gc status --json` prints beads.proxied on the local route only.
func TestStatusViewFromGen_CarriesProxiedDiagnostic(t *testing.T) {
	port, pid := int64(3307), int64(4242)
	generation, evidence, idle, verdict, detail, demoted := "4242:1", "argv", "never", "draining", "detail text", true
	body := &genclient.StatusBody{
		Name: "city",
		Path: "/tmp/city",
		Beads: &genclient.BeadsDiagnostic{
			BeadsStore:          "NativeDoltStore",
			NativeStoreEligible: true,
			Proxied: &genclient.ProxiedDiagnostic{
				Endpoint:   genclient.ProxiedEndpointStamp{Port: &port, Pid: &pid, Generation: &generation},
				Cursors:    genclient.Cursors{Main: 7, Ignored: 3},
				Evidence:   &evidence,
				IdlePolicy: &idle,
				Verdict:    &verdict,
				Detail:     &detail,
				Demoted:    &demoted,
			},
		},
	}

	got := statusViewFromGen(body)

	want := &beads.ProxiedDiagnostic{
		Endpoint:   beads.ProxiedEndpointStamp{Port: 3307, PID: 4242, Generation: "4242:1"},
		Evidence:   "argv",
		IdlePolicy: "never",
		Verdict:    beads.ProxiedVerdictDraining,
		Detail:     "detail text",
		Demoted:    true,
	}
	want.Cursors.Main, want.Cursors.Ignored = 7, 3
	if got.Beads == nil {
		t.Fatal("Beads = nil, want the diagnostic")
	}
	if !reflect.DeepEqual(got.Beads.Proxied, want) {
		t.Fatalf("Beads.Proxied = %+v, want %+v", got.Beads.Proxied, want)
	}
}

func TestStatusViewFromGen_NoProxiedDiagnosticStaysNil(t *testing.T) {
	got := statusViewFromGen(&genclient.StatusBody{
		Name:  "city",
		Path:  "/tmp/city",
		Beads: &genclient.BeadsDiagnostic{BeadsStore: "BdStore"},
	})
	if got.Beads == nil || got.Beads.Proxied != nil {
		t.Fatalf("Beads = %+v, want a diagnostic without proxied", got.Beads)
	}
}
