package runtimecontract

import (
	"os"
	"regexp"
	"sort"
	"testing"
)

// classification ties one RunProviderTests subtest to the wire catalog.
// Exactly one field is set: Code names the catalog requirement that gates
// the behavior over the wire, or Deferred explains why it is not yet
// wire-covered (a group still being ported to the catalog, or a harness
// wrapper / concurrency case that is not a distinct wire behavior).
type classification struct {
	Code     Code
	Deferred string
}

// contractCoverage classifies every RunProviderTests subtest in
// internal/runtime/runtimetest/conformance.go. This is the lockstep
// binding: TestEveryContractCaseIsClassified parses that source and fails
// CI if any case is unlisted here — so a new provider-contract case cannot
// ship without either a wire probe (a catalog Code) or a deliberate,
// documented deferral. As groups are ported, Deferred entries become Code
// entries until the wire suite reaches full RunProviderTests parity.
var contractCoverage = map[string]classification{
	// Harness wrapper — not a distinct provider behavior.
	"SharedSession": {Deferred: "harness: shares one session across the session-op groups"},

	// --- Lifecycle (wire-covered) ---
	"Start_CreatesRunningSession":    {Code: ReqLifecycleStartRunning},
	"Start_DuplicateReturnsError":    {Code: ReqLifecycleDuplicateErr},
	"Stop_MakesSessionNotRunning":    {Code: ReqLifecycleStopNotRunning},
	"Stop_Idempotent_NotRunning":     {Code: ReqLifecycleStopIdempotent},
	"Stop_Idempotent_AlreadyStopped": {Code: ReqLifecycleStopIdempotent},
	"StopForCleanup_NeverStarted":    {Code: ReqLifecycleStopIdempotent},
	"IsRunning_UnknownSession":       {Code: ReqLifecycleUnknownNotRunning},

	// --- Optional error-bearing capabilities (in-process provider
	// interfaces the exec provider maps onto observe.v1 and is-attached) ---
	"ObserveLivenessWithError_StoppedIsAbsent": {Code: ReqObserveAbsentAfterStop},
	"Start_FreshOnlyRefusesAHeldName":          {Code: ReqObserveFreshOnlyRefuses},
	"ListRunning_AttestedAfterStartAndStop":    {Code: ReqObserveInventoryComplete},
	"IsAttachedWithError_UnattachedSession":    {Code: ReqObserveAttachedUnattachedFalse},
	"IsAttachedWithError_MissingSession":       {Code: ReqObserveAttachedMissingFalse},

	// --- Concurrency (wire-gated under observe.v1, whose rule 9 is the
	// promise) ---
	"Start_ConcurrentDistinctSessions":        {Code: ReqConcurrencyStart},
	"Stop_ConcurrentDistinctSessions":         {Code: ReqConcurrencyStop},
	"Interrupt_ConcurrentDistinctSessions":    {Deferred: "interrupt is a driving op: it ports with the signaling group, once the pack-selected carrier decides which wire op carries it"},
	"IsRunning_ConcurrentDistinctSessions":    {Code: ReqConcurrencyIsRunning},
	"ProcessAlive_ConcurrentDistinctSessions": {Code: ReqConcurrencyProcessAlive},
	"ListRunning_ConcurrentDistinctPrefixes":  {Code: ReqConcurrencyListRunning},

	// --- Discovery (wire-gated when list-running is implemented) ---
	"ListRunning_FindsSessions":   {Code: ReqDiscoveryFindsSessions},
	"ListRunning_PrefixFiltering": {Code: ReqDiscoveryPrefixFiltering},
	"ListRunning_ExcludesStopped": {Code: ReqDiscoveryExcludesStopped},
	"ListRunning_EmptyPrefix":     {Code: ReqDiscoveryEmptyPrefix},

	// --- ProcessAlive (wire-gated when process-alive is implemented) ---
	"ProcessAlive_EmptyNamesReturnsTrue": {Code: ReqProcessEmptyNamesAlive},
	"ProcessAlive_FalseAfterStop":        {Code: ReqProcessFalseAfterStop},

	// --- Metadata (wire-gated when implemented; mandatory under
	// observe.v1) ---
	"SetGetMeta_RoundTrip":           {Code: ReqMetaRoundTrip},
	"GetMeta_UnsetKey":               {Code: ReqMetaUnsetKeyEmpty},
	"RemoveMeta_ThenGetReturnsEmpty": {Code: ReqMetaRemoveThenEmpty},
	"SetMeta_EmptyValueOverwrites":   {Code: ReqMetaEmptyValueOverwrites},
	"SetMeta_OverwritesPrevious":     {Code: ReqMetaOverwritesPrevious},
	"Meta_MultipleKeys":              {Code: ReqMetaMultipleKeysIndependent},

	// --- Observation group (deferred) ---
	"Peek_NoError":            {Deferred: "observation group not yet ported"},
	"GetLastActivity_NoError": {Deferred: "observation group not yet ported"},
	"ClearScrollback_NoError": {Deferred: "observation group not yet ported"},
	"CopyTo_NoError":          {Deferred: "observation group not yet ported"},

	// --- Signaling group (deferred) ---
	"SendKeys_RunningSession":  {Deferred: "signaling group not yet ported"},
	"SendKeys_MissingSession":  {Deferred: "signaling group not yet ported"},
	"Interrupt_RunningSession": {Deferred: "signaling group not yet ported"},
	"Interrupt_MissingSession": {Deferred: "signaling group not yet ported"},
	"Nudge_RunningSession":     {Deferred: "signaling group not yet ported"},
	"Nudge_MissingSession":     {Deferred: "signaling group not yet ported"},

	// --- Fenced input (deferred: an optional in-process capability) ---
	"FencedInput_MatchingFenceDelivers":            {Deferred: "optional in-process capability; RPP has no fenced-input op"},
	"FencedInput_MismatchedFenceRefusesEveryInput": {Deferred: "optional in-process capability; RPP has no fenced-input op"},
	"FencedInput_AbsentSessionRefuses":             {Deferred: "optional in-process capability; RPP has no fenced-input op"},
}

// wireOnlyCodes are the observe group's requirements that no
// runtime.Provider method backs: the `observe`, `inventory` and
// `kill-object` wire shapes (an exec pack's own ops, which the exec
// provider maps), the attach answer's exact form, the fault and city-scope
// tokens and the probe latency. The rest of the group (absent after stop,
// FreshOnly, the attested listing, the error-bearing attachment answer)
// back RunProviderTests cases. RPP-DISCOVERY-005 is wire-only too:
// RunProviderTests cannot induce an observation failure generically (it is
// provider-specific: kill the tmux server, break the ssh transport), so the
// in-tree providers pin ListRunning's error-vs-absence rule in their own
// tests, and the wire catalog gates it through the conformance fault.
var wireOnlyCodes = map[Code]bool{
	ReqDiscoveryFailureIsNotAbsence: true,

	ReqObserveDeclaredOpsRespond:   true,
	ReqObserveAbsentNeverStarted:   true,
	ReqObserveRunningWithIdentity:  true,
	ReqObserveObjectStable:         true,
	ReqObserveAgentLiveness:        true,
	ReqObserveMetaIsIdentity:       true,
	ReqObserveKillObjectExact:      true,
	ReqObserveAttachedIsStrict:     true,
	ReqObserveFaultsReadUnknown:    true,
	ReqObserveWithinBound:          true,
	ReqObserveCityScoped:           true,
	ReqObserveInventoryHasActivity: true,
	ReqObservePeekNoScreen:         true,
}

// conformanceSourcePath is the in-tree provider contract whose cases this
// catalog mirrors. Relative to this package dir (go test's working dir).
const conformanceSourcePath = "../runtimetest/conformance.go"

var contractCaseRE = regexp.MustCompile(`t\.Run\("([^"]+)"`)

// extractContractCases parses every t.Run subtest name from the in-tree
// provider conformance suite.
func extractContractCases(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile(conformanceSourcePath)
	if err != nil {
		t.Fatalf("reading provider contract source %s: %v", conformanceSourcePath, err)
	}
	matches := contractCaseRE.FindAllSubmatch(src, -1)
	if len(matches) == 0 {
		t.Fatalf("no t.Run cases found in %s — did the contract suite move?", conformanceSourcePath)
	}
	var names []string
	for _, m := range matches {
		names = append(names, string(m[1]))
	}
	return names
}

// TestEveryContractCaseIsClassified is the lockstep guarantee: every
// RunProviderTests case must be either wire-gated (a catalog Code) or
// explicitly deferred. Adding a provider-contract case without classifying
// it here fails CI — the wire suite can never silently fall behind the
// contract it claims to mirror.
func TestEveryContractCaseIsClassified(t *testing.T) {
	cases := extractContractCases(t)
	known := map[Code]bool{}
	for _, req := range catalog {
		known[req.Code] = true
	}

	seen := map[string]bool{}
	for _, name := range cases {
		seen[name] = true
		cls, ok := contractCoverage[name]
		if !ok {
			t.Errorf("RunProviderTests case %q is not classified for RPP wire conformance: add a catalog Code or a Deferred reason in contractCoverage", name)
			continue
		}
		switch {
		case cls.Code != "" && cls.Deferred != "":
			t.Errorf("%q is both coded (%s) and deferred — pick one", name, cls.Code)
		case cls.Code != "" && !known[cls.Code]:
			t.Errorf("%q maps to %s, which is not in the catalog", name, cls.Code)
		case cls.Code == "" && cls.Deferred == "":
			t.Errorf("%q has an empty classification", name)
		}
	}

	// Flag stale coverage entries so the map cannot rot in the other
	// direction (a renamed/removed contract case).
	for name := range contractCoverage {
		if !seen[name] {
			t.Errorf("contractCoverage names %q, which no longer exists in %s", name, conformanceSourcePath)
		}
	}
}

// TestEveryCatalogCodeBacksAContractCase asserts the wire suite never
// claims more than the in-tree contract: every catalog Code must be the
// classification of at least one RunProviderTests case — except the
// protocol group, which is wire-only. The handshake is not a
// runtime.Provider method, so it has no RunProviderTests counterpart; it is
// validated by internal/runtime/exec/handshake_test.go and rppcheck.
func TestEveryCatalogCodeBacksAContractCase(t *testing.T) {
	backed := map[Code]bool{}
	for _, cls := range contractCoverage {
		if cls.Code != "" {
			backed[cls.Code] = true
		}
	}
	for _, req := range catalog {
		if wireOnlyCodes[req.Code] {
			// The observe group's wire shapes, tokens and latency, which no
			// runtime.Provider method backs; see wireOnlyCodes.
			continue
		}
		if req.Group == GroupServer {
			// server-stop backs TeardownServer, which is not a
			// RunProviderTests case: it is contract-tested by
			// runtimetest.RunServerTeardownContract (RUNTIME-CONTRACT-023)
			// over the same golden reference.
			continue
		}
		if req.Group == GroupFSHost {
			// fs.host is a handshake token whose promise (a shared filesystem
			// view and user) no runtime.Provider method states; gc's side of
			// it, host staging, is the RUNTIME-CONTRACT-022 suite
			// (runtimetest.RunHostStagingContract), not a RunProviderTests case.
			continue
		}
		if req.Group == GroupProtocol || req.Group == GroupConnection || req.Group == GroupProvision {
			// Wire-only groups: no runtime.Provider method to contract-test.
			// protocol is the handshake; connection (exec) is the slim wire
			// primitive, validated by the runtimecontract probe and the
			// runtimecapability env runner. The Go-side connection method
			// (Place.Exec) and its RunProviderTests case land with the carrier
			// rewrite that moves the legacy driving ops over exec. provision is
			// the un-weld's box-without-agent op: in-repo Provision is still the
			// welded Start, so there is no distinct RunProviderTests case to
			// mirror — it is validated by the runtimecontract probe.
			// TODO(connection-rewrite): drop the GroupConnection exemption once
			// Place.Exec has a RunProviderTests case, so the lockstep guarantee
			// re-binds the connection group too.
			// TODO(provision-mirror): drop the GroupProvision exemption if/when
			// Provision becomes a distinct, separately contract-tested method.
			continue
		}
		if !backed[req.Code] {
			t.Errorf("catalog code %s does not back any RunProviderTests case — the wire suite must not exceed the contract", req.Code)
		}
	}
}

// TestParityProgressIsVisible reports remaining parity gaps so the path to
// full coverage stays explicit (and so this file is the single place that
// shrinks as groups land). It never fails; it logs.
func TestParityProgressIsVisible(t *testing.T) {
	var deferred []string
	for name, cls := range contractCoverage {
		if cls.Deferred != "" {
			deferred = append(deferred, name)
		}
	}
	sort.Strings(deferred)
	t.Logf("wire-gated catalog requirements: %d; RunProviderTests cases deferred to future groups: %d",
		len(catalog), len(deferred))
	for _, name := range deferred {
		t.Logf("  deferred: %s (%s)", name, contractCoverage[name].Deferred)
	}
}
