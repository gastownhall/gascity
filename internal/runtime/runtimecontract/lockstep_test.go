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
	"IsRunning_UnknownSession":       {Code: ReqLifecycleUnknownNotRunning},

	// --- Concurrency (deferred: a cross-cutting property, ported once the
	// single-session behaviors of each group are gated) ---
	"Start_ConcurrentDistinctSessions":        {Deferred: "concurrency group not yet ported"},
	"Stop_ConcurrentDistinctSessions":         {Deferred: "concurrency group not yet ported"},
	"Interrupt_ConcurrentDistinctSessions":    {Deferred: "concurrency group not yet ported"},
	"IsRunning_ConcurrentDistinctSessions":    {Deferred: "concurrency group not yet ported"},
	"ProcessAlive_ConcurrentDistinctSessions": {Deferred: "concurrency group not yet ported"},
	"ListRunning_ConcurrentDistinctPrefixes":  {Deferred: "concurrency group not yet ported"},

	// --- Discovery group (deferred) ---
	"ListRunning_FindsSessions":   {Deferred: "discovery group not yet ported"},
	"ListRunning_PrefixFiltering": {Deferred: "discovery group not yet ported"},
	"ListRunning_ExcludesStopped": {Deferred: "discovery group not yet ported"},
	"ListRunning_EmptyPrefix":     {Deferred: "discovery group not yet ported"},

	// --- ProcessAlive group (deferred) ---
	"ProcessAlive_EmptyNamesReturnsTrue": {Deferred: "process group not yet ported"},
	"ProcessAlive_FalseAfterStop":        {Deferred: "process group not yet ported"},

	// --- Metadata group (deferred) ---
	"SetGetMeta_RoundTrip":           {Deferred: "metadata group not yet ported"},
	"GetMeta_UnsetKey":               {Deferred: "metadata group not yet ported"},
	"RemoveMeta_ThenGetReturnsEmpty": {Deferred: "metadata group not yet ported"},
	"SetMeta_OverwritesPrevious":     {Deferred: "metadata group not yet ported"},
	"Meta_MultipleKeys":              {Deferred: "metadata group not yet ported"},

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
