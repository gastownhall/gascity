package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/sling"
)

// remoteSlingPreflightTimeout bounds the read-only round-trip of a remote
// --dry-run. NewRemoteEventsClient hands back the SSE transport shape
// (Timeout: 0, no deadline) because its purpose is a long-lived stream, so a
// caller that borrows it for one REST read supplies its own deadline — as
// cmd_events.go does for its own probe, which threads a context through that
// client instead of relying on a transport timeout it does not have.
const remoteSlingPreflightTimeout = 20 * time.Second

// remoteCityConfig is the projection of a hosted city's configuration that a
// sling pre-flight reads. It carries exactly the three inputs the domain's own
// store-reachability predicate takes — the city's rigs, the agent rows, and the
// city root — so the pre-flight calls THAT predicate (agentutil) rather than
// restating its rules, and cannot drift from the local guard.
type remoteCityConfig struct {
	cityName string
	cityPath string
	cfg      *config.City
	// agents indexes every identity an operator may name a target with
	// (binding name, qualified name, dir/name) onto its configured agent.
	agents map[string]config.Agent
}

// fetchRemoteCityConfig reads GET /v0/city/<name>/config — the endpoint that
// already serves the hosted city's rigs (name/path/prefix) and agents
// (name/dir/scope) — plus the city root from the status snapshot the projected
// dir paths resolve against. It is READ-ONLY by construction: no mutating
// endpoint is contacted.
//
// Why not a dry_run field on the sling request: the hosted city decodes that
// body with encoding/json and no DisallowUnknownFields (internal/api
// decodeBody), so a city running a gc that predates the field would silently
// ignore dry_run and PERFORM the route. A pre-flight that can write on the
// version it has not talked to is worse than the refusal it replaces, so the
// pre-flight is computed here from reads every served version already answers.
func fetchRemoteCityConfig(c *api.Client, target *remoteTarget) (remoteCityConfig, error) {
	out := remoteCityConfig{cityName: strings.TrimSpace(target.CityName), agents: map[string]config.Agent{}}
	opts, err := remoteClientOptions(target)
	if err != nil {
		return remoteCityConfig{}, err
	}
	gen, err := api.NewRemoteEventsClient(target.BaseURL, opts)
	if err != nil {
		return remoteCityConfig{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), remoteSlingPreflightTimeout)
	defer cancel()
	resp, err := gen.GetV0CityByCityNameConfigWithResponse(ctx, out.cityName)
	if err != nil {
		return remoteCityConfig{}, fmt.Errorf("read hosted city config: %w", err)
	}
	if resp == nil {
		return remoteCityConfig{}, fmt.Errorf("read hosted city config: nil response")
	}
	if resp.StatusCode() == http.StatusNotFound || resp.JSON200 == nil {
		return remoteCityConfig{}, fmt.Errorf(
			"hosted city %q does not serve GET /v0/config (HTTP %d); cannot pre-flight a route against it",
			out.cityName, resp.StatusCode())
	}
	body := resp.JSON200
	if body.Workspace.Name != "" {
		out.cityName = body.Workspace.Name
	}
	out.cfg = &config.City{}
	// The served workspace prefix is the city's EFFECTIVE HQ prefix, so feeding
	// it back as the override makes config.EffectiveHQPrefix return it verbatim
	// and the HQ/rig store split below matches the hosted city's own.
	out.cfg.Workspace.Prefix = cfgStr(body.Workspace.Prefix)
	if body.Rigs != nil {
		for _, r := range *body.Rigs {
			out.cfg.Rigs = append(out.cfg.Rigs, config.Rig{
				Name:   r.Name,
				Path:   r.Path,
				Prefix: cfgStr(r.Prefix),
			})
		}
	}
	if body.Agents != nil {
		for _, a := range *body.Agents {
			agent := config.Agent{
				Name:  a.Name,
				Dir:   cfgStr(a.Dir),
				Scope: cfgStr(a.Scope),
			}
			out.indexAgent(agent)
		}
	}
	// Projected agent dirs may be city-relative, and workdir.ConfiguredRigName
	// resolves those against the city root. The status snapshot carries that root;
	// without it a relative dir would resolve to nothing and the predicate would
	// answer for the wrong store.
	if st, err := c.GetStatus(); err == nil {
		out.cityPath = strings.TrimSpace(st.Body.CityPath)
	}
	return out, nil
}

func (rc *remoteCityConfig) indexAgent(a config.Agent) {
	add := func(k string) {
		if k = strings.TrimSpace(k); k != "" {
			rc.agents[strings.ToLower(k)] = a
		}
	}
	add(a.Name)
	add(a.QualifiedName())
}

// findAgent resolves a sling target the way the domain does: a pool instance
// collapses to its template before the lookup, then the identities the city
// publishes are tried.
func (rc *remoteCityConfig) findAgent(target string) (config.Agent, bool) {
	t := strings.TrimSpace(agentutil.NormalizePoolRouteTarget(rc.cfg, strings.TrimSpace(target)))
	for _, key := range []string{t, strings.ToLower(t)} {
		if a, ok := rc.agents[strings.ToLower(key)]; ok {
			return a, true
		}
	}
	return config.Agent{}, false
}

// beadStoreRef resolves the workflow store a bead lives in — "city:<name>" for
// the HQ store, "rig:<name>" for a rig store — using the same prefix resolution
// the hosted city's own sling handler applies (BeadPrefixForCity, IsHQPrefix,
// FindRigByPrefix). ok=false means the projected config does not classify the
// id, which the preview reports as undetermined rather than guessing a store.
func (rc *remoteCityConfig) beadStoreRef(beadID string) (string, bool) {
	prefix := sling.BeadPrefixForCity(rc.cfg, beadID)
	if prefix == "" {
		return "", false
	}
	if sling.IsHQPrefix(rc.cfg, prefix) {
		return "city:" + rc.cityName, true
	}
	if rig, ok := sling.FindRigByPrefix(rc.cfg, prefix); ok {
		return "rig:" + rig.Name, true
	}
	return "", false
}

// remoteSlingPreflight is `gc sling --dry-run` against a remote city: it answers
// "would this route work, and what would it change" and writes nothing. It
// reuses the domain's own agreement predicate and the domain's own refusal type,
// so what it prints is the same verdict the real sling reaches — not a second,
// locally-restated rule that can drift from it.
func remoteSlingPreflight(c *api.Client, target *remoteTarget, args []string, isFormula, jsonOutput bool, stdout, stderr io.Writer) int {
	fail := func(code, message string) int {
		if jsonOutput {
			return writeJSONError(stdout, stderr, code, message, 1)
		}
		fmt.Fprintln(stderr, message) //nolint:errcheck // best-effort stderr
		return 1
	}
	targetArg, beadArg := strings.TrimSpace(args[0]), strings.TrimSpace(args[1])

	// Echo the resolved target before the first request, exactly as the real
	// remote sling does: a pre-flight is only worth what it says about the city
	// it actually interrogated, and a stale env or sticky-default context must
	// not let it read as a green light for somewhere else.
	if !jsonOutput {
		fmt.Fprintln(stderr, formatRemoteTarget(target)) //nolint:errcheck // best-effort stderr
	}

	rc, err := fetchRemoteCityConfig(c, target)
	if err != nil {
		return fail("preflight_failed", "gc sling: "+err.Error())
	}
	agent, ok := rc.findAgent(targetArg)
	if !ok {
		return fail("unknown_target", fmt.Sprintf(
			"gc sling: target %q is not configured in city %q; nothing would be routed", targetArg, rc.cityName))
	}

	preview := remoteSlingPreview{Target: targetArg}
	if isFormula {
		preview.Formula = beadArg
	} else {
		bead, err := c.GetBead(beadArg)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				return fail("missing_bead", fmt.Sprintf(
					"gc sling: bead %q not found in city %q; nothing would be routed", beadArg, rc.cityName))
			}
			return fail("preflight_failed", "gc sling: "+err.Error())
		}
		preview.Bead = bead.Body.ID
		preview.BeadType = bead.Body.Type
		preview.BeadStatus = bead.Body.Status
		preview.CurrentRoute = bead.Body.Metadata[beadmeta.RoutedToMetadataKey]

		storeRef, classified := rc.beadStoreRef(bead.Body.ID)
		reachRef := agentutil.AgentReachableStoreLabel(&agent, rc.cityPath, rc.cityName, rc.cfg)
		preview.StoreRef, preview.ReachableStoreRef = storeRef, reachRef
		switch {
		case !classified:
			preview.Agreement = agreementUndetermined
			preview.Note = "bead prefix is not classified by the hosted city's rigs"
		case sling.IsCustomSlingQuery(agent):
			// The local guard skips an agent with a custom work_query (it reads a
			// queue of its own, not the built-in reachability), so the pre-flight
			// must not claim more than the guard itself checks.
			preview.Agreement = agreementUndetermined
			preview.Note = "target declares a custom work_query; the built-in store guard does not apply to it"
		case agentutil.AgentIsCrossStoreEligible(&agent):
			preview.Agreement = agreementAgrees
			preview.Note = "city-scoped target reads any store (vp-kvp)"
		case agentutil.AgentReachesWorkflowStore(storeRef, &agent, rc.cityPath, rc.cfg):
			preview.Agreement = agreementAgrees
		default:
			// The domain's own typed refusal, verbatim — a pre-flight that invents a
			// second cross-store message teaches a second vocabulary about tr-6s7yx.
			domain := &sling.CrossStoreRouteError{
				BeadID:            bead.Body.ID,
				StoreRef:          storeRef,
				Target:            agent.QualifiedName(),
				ReachableStoreRef: reachRef,
			}
			return fail("cross_store", domain.Error())
		}
	}
	return preview.render(jsonOutput, stdout, stderr)
}

const (
	agreementAgrees       = "agrees"
	agreementUndetermined = "not determined"
)

// remoteSlingPreview is the outcome of the read-only pre-flight.
type remoteSlingPreview struct {
	Target            string `json:"target"`
	Bead              string `json:"bead_id,omitempty"`
	BeadType          string `json:"bead_type,omitempty"`
	BeadStatus        string `json:"bead_status,omitempty"`
	Formula           string `json:"formula,omitempty"`
	CurrentRoute      string `json:"current_routed_to,omitempty"`
	StoreRef          string `json:"store_ref,omitempty"`
	ReachableStoreRef string `json:"reachable_store_ref,omitempty"`
	Agreement         string `json:"store_agreement,omitempty"`
	Note              string `json:"note,omitempty"`
}

// render prints the preview. stdout carries the answer (JSON object with
// --json); stderr keeps the target echo the real remote sling prints, so a
// pre-flight names the control plane it just interrogated.
func (p remoteSlingPreview) render(jsonOutput bool, stdout, stderr io.Writer) int {
	if jsonOutput {
		payload := map[string]any{
			"schema_version": "1",
			// dry_run is the local `sling --json` key; an operator repointing a
			// script at a remote city keeps seeing it.
			"dry_run":     true,
			"would_write": false,
			"target":      p.Target,
		}
		putIfSet(payload, "bead_id", p.Bead)
		putIfSet(payload, "bead_type", p.BeadType)
		putIfSet(payload, "bead_status", p.BeadStatus)
		putIfSet(payload, "formula", p.Formula)
		putIfSet(payload, "current_routed_to", p.CurrentRoute)
		putIfSet(payload, "store_ref", p.StoreRef)
		putIfSet(payload, "reachable_store_ref", p.ReachableStoreRef)
		putIfSet(payload, "store_agreement", p.Agreement)
		putIfSet(payload, "note", p.Note)
		enc, err := json.Marshal(payload)
		if err != nil {
			fmt.Fprintln(stderr, "gc sling: encoding result:", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		fmt.Fprintln(stdout, string(enc)) //nolint:errcheck // best-effort stdout
		return 0
	}
	var lines []string
	if p.Formula != "" {
		lines = append(lines, fmt.Sprintf("would launch: %s \u2192 %s", p.Formula, p.Target))
		// A formula launch has no source bead, so there is no bead store to
		// agree with. Say so rather than print an agreement nothing checked.
		lines = append(lines, fmt.Sprintf("store agreement: %s (a formula launch has no source bead)", agreementUndetermined))
	} else {
		lines = append(lines, fmt.Sprintf("would route: %s (%s, %s) \u2192 %s",
			p.Bead, orUnknown(p.BeadType), orUnknown(p.BeadStatus), p.Target))
		if p.CurrentRoute != "" {
			lines = append(lines, fmt.Sprintf("current %s: %s", beadmeta.RoutedToMetadataKey, p.CurrentRoute))
		}
		lines = append(lines, fmt.Sprintf("store agreement: %s (bead %s, target reads %s)",
			orUnknown(p.Agreement), orUnknown(p.StoreRef), orUnknown(p.ReachableStoreRef)))
	}
	if p.Note != "" {
		lines = append(lines, "note: "+p.Note)
	}
	// The last line is the promise: this verb wrote nothing. It is asserted, not
	// implied — the tests fail if any mutating request leaves this path.
	lines = append(lines, "dry-run: nothing written")
	fmt.Fprintln(stdout, strings.Join(lines, "\n")) //nolint:errcheck // best-effort stdout
	return 0
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	return s
}

func cfgStr(s *string) string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(*s)
}
