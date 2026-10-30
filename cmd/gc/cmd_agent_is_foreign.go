package main

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

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
//	            unresolvable city, bad usage); PROTECT the claim
//
// Exit 2 means "no answer", never "no match": a caller that treats it as
// foreign is still safe, and one that treats it as local is not.
//
// "local" does not mean alive. The verb answers whose identity this is; the
// caller still has to establish liveness itself.

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

Exit codes:
  0  local    this city can vouch for the identity
  1  foreign  this city cannot vouch for it; protect the claim
  2  unknown  this city cannot answer (no roster, a failed lookup, bad usage);
              protect the claim

"local" is not a liveness claim: a dead local claimant is still local, and the
caller has to establish liveness itself.

Callers that reap claims on this verdict must fail closed: treat a missing
verb, exit 2, or unparseable output as "protect", never as "local". Pass the
identity after "--" when it is untrusted text.`,
		Example: `  gc agent is-foreign repo/worker-3
  gc agent is-foreign --json -- "$assignee"`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			// exitForCode keeps 1 and 2 apart; a bare errExit would collapse
			// "foreign" and "cannot answer" into one code.
			return exitForCode(cmdAgentIsForeign(args, jsonOutput, stdout, stderr))
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output in JSON format")
	return cmd
}

func cmdAgentIsForeign(args []string, jsonOutput bool, stdout, stderr io.Writer) int {
	if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
		// Bad usage is a degraded answer, not a foreign one: exit 2 so a
		// caller that mis-invokes the verb protects the claim.
		fmt.Fprintln(stderr, "gc agent is-foreign: usage: gc agent is-foreign [--json] [--] <identity>") //nolint:errcheck // best-effort stderr
		identity := ""
		if len(args) > 0 {
			identity = strings.Join(args, " ")
		}
		return writeAgentIsForeignUnknown(identity, "", "", claimReasonNoConfig, "usage: exactly one identity is required", jsonOutput, stdout, stderr)
	}
	identity := strings.TrimSpace(args[0])
	cityPath, err := resolveCity()
	if err != nil {
		return writeAgentIsForeignUnknown(identity, "", "", claimReasonNoConfig, fmt.Sprintf("resolve city: %v", err), jsonOutput, stdout, stderr)
	}
	return doAgentIsForeign(fsys.OSFS{}, cityPath, identity, jsonOutput, stdout, stderr)
}

func doAgentIsForeign(fs fsys.FS, cityPath, identity string, jsonOutput bool, stdout, stderr io.Writer) int {
	tomlPath := filepath.Join(cityPath, "city.toml")
	// Config-load warnings go to stderr, never stdout: stdout carries the
	// JSON a caller parses, and a warning mixed into it would make a healthy
	// verdict unparseable, which a fail-closed caller turns into a protect.
	cfg, err := loadCityConfigFS(fs, tomlPath, stderr)
	if err != nil {
		return writeAgentIsForeignUnknown(identity, tomlPath, "", claimReasonNoConfig, fmt.Sprintf("load city config: %v", err), jsonOutput, stdout, stderr)
	}
	cityName := cfg.EffectiveCityName()
	openSessionStore := func() (beads.Store, error) {
		// One-shot open: reuse the config loaded above instead of reloading
		// city.toml and every pack include on a bd-backed city.
		store, err := openCityStoreAtWithConfig(cityPath, cfg)
		if err != nil {
			return nil, fmt.Errorf("opening city store: %w", err)
		}
		return cliSessionStore(store, cfg, cityPath), nil
	}
	verdict := claimIdentityProvenance(cfg, cityName, identity, openSessionStore)
	rosterSource := agentIsForeignRosterSource(tomlPath, cfg)
	if verdict.Verdict == claimVerdictUnknown {
		detail := verdict.Detail
		if verdict.Reason == claimReasonNoConfig {
			detail = "resolved city config carries no roster"
		}
		return writeAgentIsForeignUnknown(identity, rosterSource, cityName, verdict.Reason, detail, jsonOutput, stdout, stderr)
	}
	out := AgentIsForeignJSON{
		SchemaVersion: "1",
		Identity:      identity,
		Verdict:       string(verdict.Verdict),
		Reason:        string(verdict.Reason),
		Detail:        verdict.Detail,
		RosterSource:  rosterSource,
		CityName:      cityName,
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

// writeAgentIsForeignUnknown emits the degraded verdict. It still writes
// well-formed output so a caller can log why it protected a claim; a bare
// non-zero exit with nothing to read is a refusal nobody can diagnose.
func writeAgentIsForeignUnknown(identity, rosterSource, cityName string, reason claimProvenanceReason, detail string, jsonOutput bool, stdout, stderr io.Writer) int {
	out := AgentIsForeignJSON{
		SchemaVersion: "1",
		Identity:      identity,
		Verdict:       agentIsForeignVerdictUnknown,
		Reason:        string(reason),
		Detail:        detail,
		RosterSource:  rosterSource,
		CityName:      cityName,
	}
	writeAgentIsForeignResult(out, jsonOutput, stdout, stderr)
	return agentIsForeignExitUnknown
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
