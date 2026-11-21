package main

import "testing"

// A bd-owned Dolt server can be reparented to init while still carrying the
// agent session's environment it inherited. Its proxy.pid names a live
// `bd db-proxy-child` for the same root, so it is bd's server, not a session
// orphan: the process-table sweep must leave it running even once the row is
// closed.
func TestProcessSweep_BDOwnedDoltServerFenced(t *testing.T) {
	s := newProcSweep(t)
	configPath := writeBdProxyRoot(t, s.cityPath, 3001)

	// The live proxy: no session env, so it is not itself a sweep candidate.
	writeFakeProcEntry(t, s.root, 3001, 1, "bd", []string{"PATH=/usr/bin"})
	writeFakeProcCmdline(t, s.root, 3001, bdProxyChildArgv(configPath)...)
	writeFakeProcStartTime(t, s.root, 3001, 1, "bd", 100)
	stubLiveBdProxy(t, 3001, configPath)

	// The proxy's Dolt child, reparented to init with the session's env.
	s.process(3002, 1, "dolt", "4", sweepLeakAt)
	writeFakeProcCmdline(t, s.root, 3002, "dolt", "sql-server", "--config", configPath)
	t.Cleanup(func() { fencedInfrastructureRoots.put(normalizePathForCompare(s.cityPath), nil) })

	pids, log := s.sweep(nil, sweepClosed, nil)
	if len(pids) != 0 {
		t.Logf("sweep log: %q", log)
	}
	wantPIDs(t, "row closed: bd-owned dolt sql-server", pids)
}
