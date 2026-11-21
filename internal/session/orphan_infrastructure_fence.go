package session

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

// cityInfrastructureArgvReason is CityInfrastructureRoot's reason for a root
// proctable.IsCityInfrastructureArgv recognizes.
const cityInfrastructureArgvReason = "city infrastructure (managed Dolt watchdog, bd proxy or nudge poller)"

// CityInfrastructureRoot is the kill-path fence both session orphan kills —
// the reconciler's process-table sweep and a session's pre-start orphan kill
// — consult before terminating a scanned root. It returns why the root must
// be left running, or "" when nothing fences it.
//
// A root is fenced when it is city infrastructure by argv
// (proctable.RootIsCityInfrastructure), or when it is a Dolt SQL server whose
// config a live bd proxy owns (proxyendpoint.ServerOwner): bd's proxy spawns
// that backend, which can inherit an agent session's environment and reparent
// to init, and terminating it takes the city's store down while the proxy
// still fronts it. The ownership proof reads the process table the scanner
// enumerated (proctable.RootArgv, proctable.RootAlive), so under test it reads
// only the injected procfs. It proves root-level ownership, not the backend's
// exact generation, and it is re-evaluated on every call: a backend whose
// proxy has gone is no longer fenced.
//
// An argv-fence classification error is returned and the caller leaves the
// root running. A backend whose argv or proxy cannot be read adds no fence:
// that is the ordinary orphan it looks like.
func CityInfrastructureRoot(live runtime.LiveRuntime) (string, error) {
	infrastructure, err := rootIsCityInfrastructure(live)
	if err != nil {
		return "", err
	}
	if infrastructure {
		return cityInfrastructureArgvReason, nil
	}
	if argv, err := proctable.RootArgv(live.PID); err == nil {
		proxyPID, owned := proxyendpoint.ServerOwner(argv, proxyendpoint.ProcessTable{
			Alive: proctable.RootAlive,
			Argv:  proctable.RootArgv,
		})
		if owned {
			return fmt.Sprintf("bd-owned Dolt server (live bd proxy pid=%d)", proxyPID), nil
		}
	}
	return "", nil
}
