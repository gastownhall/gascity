package main

// contractWaivers lists operations the in-process contract suite does not
// exercise, each with the reason and the owner that covers it instead. The
// coverage guard in TestAPIContractSuite fails when a spec operation is
// neither exercised nor listed here, when a listed operation leaves the spec,
// and when the suite starts exercising a listed operation (delete the entry).
var contractWaivers = map[string]string{
	"add-pack": "resolves and fetches a remote git/registry pack source (network); owned by internal/api handler_packs_write_test.go and pack_source_policy_test.go",
}

// contractKnownSpecViolations lists operations whose live responses are
// known not to match the OpenAPI document, with the bug they track. A
// violation on any other operation fails the suite. Delete an entry when the
// handler (or the spec) is fixed.
var contractKnownSpecViolations = map[string]string{}
