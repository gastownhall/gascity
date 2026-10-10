// Package waiverexpiry holds the one check that compares dated test-policy
// waivers with today's date.
//
// Three ledgers carry dated waivers: the runtime provider ledger
// (internal/testutil/providerledger), the resource census
// (internal/testpolicy/resourcecensus), and the beads conformance skips
// (internal/beads/beadstest). Their own tests check structure only and never
// read the clock, so a cached PASS for them stays true on every later day. Each
// ledger exports its dates as waiverclock.Expiry values, and this package's
// test judges all of them against time.Now under the mode
// internal/testpolicy/waiverclock reads from GC_WAIVER_CLOCK.
//
// Today is an input that no build system can key on, so the test must never be
// served from a cache. Its Bazel target is tagged external. scripts/waiver-clock-audit
// runs it with -count=1 in strict mode, and plain go test runs it in grace mode.
// It is small and needs no repository files, so re-running it every time is
// cheap.
package waiverexpiry
