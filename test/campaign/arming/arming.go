// Package arming is the WD parity campaign's trace-arming harness.
//
// Unarmed detail records are not durable: the record path stashes them per
// template and returns before addRecord, and End() copies only promoted
// records, so an unarmed campaign window records nothing at all
// (DETECTOR.md section 3, "Campaign arming and durability"). That is the
// campaign's core failure mode, and it is silent — the readout would simply
// show an empty, all-matched table. This harness closes it: it arms every
// configured template for detail tracing, verifies the arms are live at every
// sample boundary, re-arms ahead of the 15m manual / 10m auto expiries, and
// reports any interval that ran unarmed so those cycles are discarded rather
// than trusted.
//
// It drives the shipped `gc trace` and `gc agent list` commands and changes no
// production behavior. It is deleted with the campaign at WE.
package arming

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// Arm mirrors one entry of `gc trace status --json` active_arms.
type Arm struct {
	ScopeType  string    `json:"scope_type"`
	ScopeValue string    `json:"scope_value"`
	Source     string    `json:"source"`
	Level      string    `json:"level"`
	ArmedAt    time.Time `json:"armed_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// Status is the subset of `gc trace status --json` the harness reads.
type Status struct {
	AsOf              time.Time `json:"as_of"`
	ControllerRunning bool      `json:"controller_running"`
	ActiveArms        []Arm     `json:"active_arms"`
}

type agentListItem struct {
	Name string `json:"name"`
}

type agentList struct {
	Agents []agentListItem `json:"agents"`
}

// Observation is one template's arm state at a sample boundary.
type Observation struct {
	Template  string    `json:"template"`
	Live      bool      `json:"live"`
	ArmedAt   time.Time `json:"armed_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Boundary is the post-arm state of every campaign template at one sample point.
type Boundary struct {
	At           time.Time              `json:"at"`
	Observations map[string]Observation `json:"observations"`
}

// Gap reasons. Any gap means the interval's cycles recorded nothing durable and
// must be excluded from the parity readout.
const (
	GapUnarmedAtOpen         = "unarmed_at_open"
	GapUnarmedAtClose        = "unarmed_at_close"
	GapExpiredInsideInterval = "expired_inside_interval"
	GapRearmedInsideInterval = "rearmed_inside_interval"
)

// Gap is one template-interval the campaign must discard.
type Gap struct {
	Template string    `json:"template"`
	From     time.Time `json:"from"`
	To       time.Time `json:"to"`
	Reason   string    `json:"reason"`
}

// Report is the harness verdict for a campaign window.
type Report struct {
	Templates      []string `json:"templates"`
	ArmWindow      string   `json:"arm_window"`
	SampleInterval string   `json:"sample_interval"`
	Boundaries     int      `json:"boundaries"`
	Rearms         int      `json:"rearms"`
	Gaps           []Gap    `json:"gaps"`
	Armed          bool     `json:"armed"`
}

// Config parameterizes one campaign window.
type Config struct {
	Binary   string        // gc binary; defaults to "gc"
	CityPath string        // passed as --city
	Window   time.Duration // gc trace start --for
	Interval time.Duration // spacing between sample boundaries
	Lead     time.Duration // re-arm when remaining life is under Interval+Lead
}

// CommandRunner runs one gc invocation and returns its stdout.
type CommandRunner func(ctx context.Context, args ...string) ([]byte, error)

// ExecRunner runs gc as a child process. It is the harness's default runner;
// tests substitute their own to drive a scripted arm store.
func ExecRunner(binary string) CommandRunner {
	return func(ctx context.Context, args ...string) ([]byte, error) {
		var stdout, stderr bytes.Buffer
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			if diagnostics := strings.TrimSpace(stderr.String()); diagnostics != "" {
				return nil, fmt.Errorf("%s %s: %w: %s", binary, strings.Join(args, " "), err, diagnostics)
			}
			return nil, fmt.Errorf("%s %s: %w", binary, strings.Join(args, " "), err)
		}
		return stdout.Bytes(), nil
	}
}

// Harness arms, verifies, and re-arms detail tracing across a campaign window.
type Harness struct {
	cfg        Config
	exec       CommandRunner
	now        func() time.Time
	sleep      func(ctx context.Context, d time.Duration) error
	templates  []string
	boundaries []Boundary
	gaps       []Gap
	rearms     int
}

// New validates the window and builds a harness. The arm window must outlast a
// sample interval plus its safety lead, or the harness could never prove a
// sample ran armed end to end.
func New(cfg Config, runner CommandRunner, now func() time.Time) (*Harness, error) {
	if strings.TrimSpace(cfg.CityPath) == "" {
		return nil, fmt.Errorf("arming: city path is empty")
	}
	if cfg.Interval <= 0 {
		return nil, fmt.Errorf("arming: sample interval must be positive")
	}
	if cfg.Lead < 0 {
		return nil, fmt.Errorf("arming: re-arm lead must be non-negative")
	}
	if cfg.Window <= cfg.Interval+cfg.Lead {
		return nil, fmt.Errorf("arming: arm window %s cannot cover a %s sample interval plus a %s lead", cfg.Window, cfg.Interval, cfg.Lead)
	}
	if cfg.Binary == "" {
		cfg.Binary = "gc"
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	if runner == nil {
		runner = ExecRunner(cfg.Binary)
	}
	return &Harness{cfg: cfg, exec: runner, now: now, sleep: sleepContext}, nil
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// DiscoverTemplates reads the configured trace templates from `gc agent list`.
// A trace record's template is the agent's TemplateParams.TemplateName, which is
// the configured agent name.
func (h *Harness) DiscoverTemplates(ctx context.Context) ([]string, error) {
	out, err := h.run(ctx, "agent", "list", "--json")
	if err != nil {
		return nil, fmt.Errorf("arming: listing configured agents: %w", err)
	}
	var list agentList
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("arming: decoding agent list: %w", err)
	}
	templates := make([]string, 0, len(list.Agents))
	for _, agent := range list.Agents {
		name := strings.TrimSpace(agent.Name)
		if name != "" && !slices.Contains(templates, name) {
			templates = append(templates, name)
		}
	}
	if len(templates) == 0 {
		return nil, fmt.Errorf("arming: city %s configures no agents to arm", h.cfg.CityPath)
	}
	h.templates = templates
	return templates, nil
}

// Sample closes the previous interval and opens the next one: it reads the arm
// store, re-arms anything that cannot cover the coming interval, re-reads to
// prove the write took, and evaluates the interval that just closed.
func (h *Harness) Sample(ctx context.Context) (Boundary, []Gap, error) {
	now := h.now()
	coverUntil := now.Add(h.cfg.Interval + h.cfg.Lead)

	status, err := h.status(ctx)
	if err != nil {
		return Boundary{}, nil, err
	}
	for _, template := range TemplatesNeedingArm(h.templates, status, now, coverUntil) {
		if _, err := h.run(ctx, "trace", "start", "--template", template, "--for", h.cfg.Window.String(), "--level", "detail"); err != nil {
			return Boundary{}, nil, fmt.Errorf("arming: arming template %q: %w", template, err)
		}
		h.rearms++
	}

	status, err = h.status(ctx)
	if err != nil {
		return Boundary{}, nil, err
	}
	boundary := Boundary{At: now, Observations: observeArms(h.templates, status)}
	if unarmed := TemplatesNeedingArm(h.templates, status, now, coverUntil); len(unarmed) > 0 {
		return Boundary{}, nil, fmt.Errorf("arming: templates %v are still unarmed after arming them; the campaign window would record nothing", unarmed)
	}

	var gaps []Gap
	if len(h.boundaries) > 0 {
		gaps = CoverageGaps(h.templates, h.boundaries[len(h.boundaries)-1], boundary)
		h.gaps = append(h.gaps, gaps...)
	}
	h.boundaries = append(h.boundaries, boundary)
	return boundary, gaps, nil
}

// Run arms the fleet and samples every interval until the window closes.
func (h *Harness) Run(ctx context.Context, window time.Duration) (Report, error) {
	if len(h.templates) == 0 {
		if _, err := h.DiscoverTemplates(ctx); err != nil {
			return Report{}, err
		}
	}
	deadline := h.now().Add(window)
	for {
		if _, _, err := h.Sample(ctx); err != nil {
			return h.Report(), err
		}
		// The window is not proven armed until a boundary closes its last
		// interval, so the loop always samples once at or past the deadline.
		if !h.now().Before(deadline) {
			return h.Report(), nil
		}
		if err := h.sleep(ctx, h.cfg.Interval); err != nil {
			return h.Report(), err
		}
	}
}

// Report summarizes the window. Armed is false whenever any interval ran
// unarmed, which is the loud failure the campaign needs.
func (h *Harness) Report() Report {
	return Report{
		Templates:      slices.Clone(h.templates),
		ArmWindow:      h.cfg.Window.String(),
		SampleInterval: h.cfg.Interval.String(),
		Boundaries:     len(h.boundaries),
		Rearms:         h.rearms,
		Gaps:           slices.Clone(h.gaps),
		Armed:          len(h.boundaries) > 0 && len(h.gaps) == 0,
	}
}

func (h *Harness) status(ctx context.Context) (Status, error) {
	out, err := h.run(ctx, "trace", "status", "--json")
	if err != nil {
		return Status{}, fmt.Errorf("arming: reading trace status: %w", err)
	}
	var status Status
	if err := json.Unmarshal(out, &status); err != nil {
		return Status{}, fmt.Errorf("arming: decoding trace status: %w", err)
	}
	return status, nil
}

func (h *Harness) run(ctx context.Context, args ...string) ([]byte, error) {
	return h.exec(ctx, append([]string{args[0], args[1], "--city", h.cfg.CityPath}, args[2:]...)...)
}

// TemplatesNeedingArm returns the templates whose detail arm is missing,
// expired, or too short-lived to cover the interval ending at coverUntil.
func TemplatesNeedingArm(templates []string, status Status, now, coverUntil time.Time) []string {
	live := detailArms(status)
	need := make([]string, 0, len(templates))
	for _, template := range templates {
		arm, ok := live[template]
		if !ok || !arm.ExpiresAt.After(now) || arm.ExpiresAt.Before(coverUntil) {
			need = append(need, template)
		}
	}
	return need
}

func detailArms(status Status) map[string]Arm {
	live := make(map[string]Arm, len(status.ActiveArms))
	for _, arm := range status.ActiveArms {
		if arm.Level != "detail" {
			continue
		}
		if existing, ok := live[arm.ScopeValue]; ok && existing.ExpiresAt.After(arm.ExpiresAt) {
			continue
		}
		live[arm.ScopeValue] = arm
	}
	return live
}

func observeArms(templates []string, status Status) map[string]Observation {
	live := detailArms(status)
	observations := make(map[string]Observation, len(templates))
	for _, template := range templates {
		arm, ok := live[template]
		if !ok {
			continue
		}
		observations[template] = Observation{
			Template:  template,
			Live:      arm.ExpiresAt.After(status.AsOf),
			ArmedAt:   arm.ArmedAt,
			ExpiresAt: arm.ExpiresAt,
		}
	}
	return observations
}

// CoverageGaps proves the interval (prev.At, cur.At] ran armed for every
// template. Continuity needs both ends: the arm observed when the interval
// opened had to outlast it, and the arm observed when it closed has to be the
// same arm — a fresh armed_at means the store lost the arm in between.
func CoverageGaps(templates []string, prev, cur Boundary) []Gap {
	gaps := make([]Gap, 0)
	for _, template := range templates {
		gap := Gap{Template: template, From: prev.At, To: cur.At}
		opened, hadOpen := prev.Observations[template]
		closed, hadClose := cur.Observations[template]
		switch {
		case !hadOpen || !opened.Live:
			gap.Reason = GapUnarmedAtOpen
		case !hadClose || !closed.Live:
			gap.Reason = GapUnarmedAtClose
		case opened.ExpiresAt.Before(cur.At):
			gap.Reason = GapExpiredInsideInterval
		case closed.ArmedAt.After(prev.At):
			gap.Reason = GapRearmedInsideInterval
		default:
			continue
		}
		gaps = append(gaps, gap)
	}
	return gaps
}
