package main

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// "gc agent is-foreign" exposes the foreign-identity gate
// (claimIdentityProvenance) to callers outside the gc process: the core
// orphan-sweep order and any patrol formula that reaps claims. Those callers
// ask rather than re-derive identity rules in shell, because a second copy
// drifts, and hand-rolled copies have reproduced ga-8yi7ne, where stripping
// any binding prefix made a neighbor's "qcore/pool.omp-1" resolve against a
// local "qcore/omp" (gastownhall/gascity#5955).
//
// EXIT CODES ARE THE CONTRACT, because a shell caller reads them before it
// reads JSON:
//
//	0  local    this city can vouch for the identity: its roster resolves it,
//	            or one of its own session records, open or closed, answers to it
//	1  foreign  this city cannot vouch for it; PROTECT the claim
//	2  unknown  this city cannot answer (no roster, a failed lookup, an
//	            unresolvable city, an unknown rig, bad usage); PROTECT the claim
//
// Exit 2 means "no answer", never "no match": a caller that treats it as
// foreign is still safe, and one that treats it as local is not.
//
// "local" does not mean alive. The verb answers whose identity this is; the
// caller still has to establish liveness itself.
//
// --rig-namespace <rig> asks about a claim in that rig's store. On a rig with
// federation = "rig" an identity outside the rig's namespace that is none of
// this city's own pool sessions of the rig is foreign (reason "namespace").
// The flag is deliberately not --rig: --rig is a root flag that an older gc
// accepts and answers from the roster alone, while --rig-namespace is an
// unknown flag there, so the old binary exits non-zero and the claim is
// protected. A caller also requires the JSON to echo rig_namespace before it
// reads a local answer as one about the rig.

// AgentIsForeignJSON is the JSON output format for "gc agent is-foreign --json".
type AgentIsForeignJSON struct {
	SchemaVersion string `json:"schema_version"`
	Identity      string `json:"identity"`
	// Verdict is "local", "foreign" or "unknown", matching the exit code.
	Verdict string `json:"verdict"`
	// Reason names the test that decided the verdict (claimProvenanceReason).
	Reason string `json:"reason"`
	// Detail is the reason's subject: the matched session record ID, the
	// foreign binding, or why the verb could not answer.
	Detail string `json:"detail,omitempty"`
	// RosterSource names where the roster came from, so a verdict is
	// reproducible from the JSON alone.
	RosterSource string `json:"roster_source"`
	CityName     string `json:"city_name,omitempty"`
	// RigNamespace echoes --rig-namespace. Its presence is the
	// acknowledgement that this gc applied the rig's namespace: a caller
	// reads a local answer about a rig claim only when it is echoed.
	RigNamespace string `json:"rig_namespace,omitempty"`
	// NamespaceChecked reports that the rig is guarded (federation = "rig")
	// and its namespace decided the identity's admission.
	NamespaceChecked bool `json:"namespace_checked,omitempty"`
	// NamespaceVia names the namespace arm that admitted the identity:
	// "lexical" (inside "<rig>/") or "session_record" (one of this city's
	// own pool sessions of the rig).
	NamespaceVia string `json:"namespace_via,omitempty"`
}

// Verdict strings. Callers match on these alongside the exit codes.
const (
	agentIsForeignVerdictLocal   = string(claimVerdictLocal)
	agentIsForeignVerdictForeign = string(claimVerdictForeign)
	agentIsForeignVerdictUnknown = string(claimVerdictUnknown)
)

// Exit codes of "gc agent is-foreign", one per verdict.
const (
	agentIsForeignExitLocal   = 0
	agentIsForeignExitForeign = 1
	agentIsForeignExitUnknown = 2
)

func newAgentIsForeignCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	var rigNamespace string
	cmd := &cobra.Command{
		Use:   "is-foreign <identity>",
		Short: "Report whether this city can vouch for a claim's assignee",
		Long: `Report whether this city can vouch for a claim's assignee identity.

An identity is local when it resolves against this city's roster (configured
agents, named sessions, pool and namepool instances), or when one of this
city's own session records, open or closed, answers to it under its bead ID,
session name, alias, configured named identity or sole alias-history entry.
Anything else is foreign: on a shared rig store, another city's claims look
exactly like this. Every identity shape is checked, qualified or bare.

With --rig-namespace <rig>, the claim is one in that rig's bead store. The
rig must be one this city configures (exact name), or the verb cannot answer.
On a rig with federation = "rig", an identity outside the rig's namespace
("<rig>/...") that is none of this city's own pool sessions of the rig is
foreign, with reason "namespace"; on any other rig the flag changes nothing.
The JSON echoes rig_namespace: a caller must not read a local answer about a
rig claim unless it does, because an older gc that lacks the flag cannot have
applied the namespace.

Exit codes:
  0  local    this city can vouch for the identity
  1  foreign  this city cannot vouch for it; protect the claim
  2  unknown  this city cannot answer (no roster, a failed lookup, an unknown
              rig, bad usage); protect the claim

"local" is not a liveness claim: a dead local claimant is still local, and the
caller has to establish liveness itself.

Callers that reap claims on this verdict must fail closed: treat a missing
verb, exit 2, or unparseable output as "protect", never as "local". Pass the
identity after "--" when it is untrusted text.`,
		Example: `  gc agent is-foreign repo/worker-3
  gc agent is-foreign --json -- "$assignee"
  gc agent is-foreign --rig-namespace beta --json -- "$assignee"`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			req := agentIsForeignRequest{args: args, jsonOutput: jsonOutput}
			if cmd.Flags().Changed("rig-namespace") {
				req.rigNamespace = &rigNamespace
			}
			// exitForCode keeps 1 and 2 apart; a bare errExit would collapse
			// "foreign" and "cannot answer" into one code.
			return exitForCode(cmdAgentIsForeign(req, stdout, stderr))
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output in JSON format")
	cmd.Flags().StringVar(&rigNamespace, "rig-namespace", "", "Ask about a claim in this rig's bead store, applying its namespace when federation = \"rig\"")
	return cmd
}

// agentIsForeignRequest is one "gc agent is-foreign" invocation.
type agentIsForeignRequest struct {
	args       []string
	jsonOutput bool
	// rigNamespace is --rig-namespace's value; nil when the flag was not
	// given. An empty value names no rig, and the verb cannot answer.
	rigNamespace *string
}

func cmdAgentIsForeign(req agentIsForeignRequest, stdout, stderr io.Writer) int {
	args := req.args
	rigNamespace := ""
	if req.rigNamespace != nil {
		rigNamespace = *req.rigNamespace
	}
	if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
		// Bad usage is a degraded answer, not a foreign one: exit 2 so a
		// caller that mis-invokes the verb protects the claim.
		fmt.Fprintln(stderr, "gc agent is-foreign: usage: gc agent is-foreign [--rig-namespace <rig>] [--json] [--] <identity>") //nolint:errcheck // best-effort stderr
		identity := ""
		if len(args) > 0 {
			identity = strings.Join(args, " ")
		}
		out := agentIsForeignUnknownJSON(identity, "", "", claimReasonNoConfig, "usage: exactly one identity is required")
		out.RigNamespace = rigNamespace
		writeAgentIsForeignResult(out, req.jsonOutput, stdout, stderr)
		return agentIsForeignExitUnknown
	}
	identity := strings.TrimSpace(args[0])
	cityPath, err := resolveCity()
	if err != nil {
		out := agentIsForeignUnknownJSON(identity, "", "", claimReasonNoConfig, fmt.Sprintf("resolve city: %v", err))
		out.RigNamespace = rigNamespace
		writeAgentIsForeignResult(out, req.jsonOutput, stdout, stderr)
		return agentIsForeignExitUnknown
	}
	if req.rigNamespace != nil {
		return doAgentIsForeignInRig(fsys.OSFS{}, cityPath, rigNamespace, identity, req.jsonOutput, stdout, stderr)
	}
	return doAgentIsForeign(fsys.OSFS{}, cityPath, nil, identity, req.jsonOutput, stdout, stderr)
}

// doAgentIsForeignInRig answers about a claim in rig's store
// (--rig-namespace).
func doAgentIsForeignInRig(fs fsys.FS, cityPath, rig, identity string, jsonOutput bool, stdout, stderr io.Writer) int {
	return doAgentIsForeign(fs, cityPath, &rig, identity, jsonOutput, stdout, stderr)
}

// doAgentIsForeign answers about identity: a claim in rig's store when rig
// is set (--rig-namespace), else one judged by the roster alone.
func doAgentIsForeign(fs fsys.FS, cityPath string, rig *string, identity string, jsonOutput bool, stdout, stderr io.Writer) int {
	tomlPath := filepath.Join(cityPath, "city.toml")
	rigEcho := ""
	if rig != nil {
		rigEcho = *rig
	}
	unknown := func(rosterSource, cityName string, reason claimProvenanceReason, detail string, checked bool) int {
		out := agentIsForeignUnknownJSON(identity, rosterSource, cityName, reason, detail)
		out.RigNamespace = rigEcho
		out.NamespaceChecked = checked
		writeAgentIsForeignResult(out, jsonOutput, stdout, stderr)
		return agentIsForeignExitUnknown
	}
	// Config-load warnings go to stderr, never stdout: stdout carries the
	// JSON a caller parses, and a warning mixed into it would make a healthy
	// verdict unparseable, which a fail-closed caller turns into a protect.
	cfg, err := loadCityConfigFS(fs, tomlPath, stderr)
	if err != nil {
		return unknown(tomlPath, "", claimReasonNoConfig, fmt.Sprintf("load city config: %v", err), false)
	}
	cityName := cfg.EffectiveCityName()
	// The city's bead store is opened at most once, whether the roster test
	// or the rig's namespace asks for it first. One-shot open: reuse the
	// config loaded above instead of reloading city.toml and every pack
	// include on a bd-backed city.
	var (
		cityOnce  sync.Once
		cityStore beads.Store
		cityErr   error
	)
	openCity := func() error {
		cityOnce.Do(func() {
			if cityStore, cityErr = openCityStoreAtWithConfig(cityPath, cfg); cityErr != nil {
				cityErr = fmt.Errorf("opening city store: %w", cityErr)
			}
		})
		return cityErr
	}
	// openSessionStore is the roster test's opener: claimIdentityProvenance
	// takes a raw store.
	openSessionStore := func() (beads.Store, error) {
		if err := openCity(); err != nil {
			return nil, err
		}
		return cliSessionStore(cityStore, cfg, cityPath), nil
	}
	rosterSource := agentIsForeignRosterSource(tomlPath, cfg)
	provenance := func() claimProvenance {
		return claimIdentityProvenance(cfg, cityName, identity, openSessionStore)
	}
	var verdict rigNamespaceProvenance
	if rig == nil {
		verdict = rigNamespaceProvenance{claimProvenance: provenance()}
	} else {
		sessions := newCityCLISessionResolver(cityPath, cfg, func() (beads.SessionStore, error) {
			if err := openCity(); err != nil {
				return beads.SessionStore{}, err
			}
			return cliStores(cityPath, cityStore).Sessions(), nil
		})
		verdict = rigNamespaceClaimProvenance(cfg, cityPath, *rig, identity, sessions, provenance)
	}
	if verdict.Verdict == claimVerdictUnknown {
		detail := verdict.Detail
		if verdict.Reason == claimReasonNoConfig && detail == "" {
			detail = "resolved city config carries no roster"
		}
		return unknown(rosterSource, cityName, verdict.Reason, detail, verdict.Checked)
	}
	out := AgentIsForeignJSON{
		SchemaVersion:    "1",
		Identity:         identity,
		Verdict:          string(verdict.Verdict),
		Reason:           string(verdict.Reason),
		Detail:           verdict.Detail,
		RosterSource:     rosterSource,
		CityName:         cityName,
		RigNamespace:     rigEcho,
		NamespaceChecked: verdict.Checked,
	}
	if verdict.Via != admitNone {
		out.NamespaceVia = verdict.Via.String()
	}
	code := agentIsForeignExitForeign
	if verdict.Verdict == claimVerdictLocal {
		code = agentIsForeignExitLocal
	}
	if !writeAgentIsForeignResult(out, jsonOutput, stdout, stderr) {
		return agentIsForeignExitUnknown
	}
	return code
}

// agentIsForeignUnknownJSON is the degraded verdict. It is still written as
// well-formed output so a caller can log why it protected a claim; a bare
// non-zero exit with nothing to read is a refusal nobody can diagnose.
func agentIsForeignUnknownJSON(identity, rosterSource, cityName string, reason claimProvenanceReason, detail string) AgentIsForeignJSON {
	return AgentIsForeignJSON{
		SchemaVersion: "1",
		Identity:      identity,
		Verdict:       agentIsForeignVerdictUnknown,
		Reason:        string(reason),
		Detail:        detail,
		RosterSource:  rosterSource,
		CityName:      cityName,
	}
}

func writeAgentIsForeignResult(out AgentIsForeignJSON, jsonOutput bool, stdout, stderr io.Writer) bool {
	if jsonOutput {
		if err := writeCLIJSONLine(stdout, out); err != nil {
			fmt.Fprintf(stderr, "gc agent is-foreign: %v\n", err) //nolint:errcheck // best-effort stderr
			return false
		}
		return true
	}
	detail := ""
	if strings.TrimSpace(out.Detail) != "" {
		detail = fmt.Sprintf(" (%s)", out.Detail)
	}
	fmt.Fprintf(stdout, "%s %s: %s%s\n", out.Identity, out.Verdict, out.Reason, detail) //nolint:errcheck // best-effort stdout
	return true
}

// agentIsForeignRosterSource describes the roster a verdict was read from. The
// counts let a surprising verdict be triaged from the JSON alone: a roster
// reporting 0 agents is a config-resolution failure wearing a successful load.
func agentIsForeignRosterSource(tomlPath string, cfg *config.City) string {
	agents, rigs := 0, 0
	if cfg != nil {
		agents = len(cfg.Agents)
		rigs = len(cfg.Rigs)
	}
	return fmt.Sprintf("%s (resolved: %d agents, %d rigs)", tomlPath, agents, rigs)
}
