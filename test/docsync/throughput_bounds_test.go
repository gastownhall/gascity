package docsync

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// throughputBounds mirrors specs/proposals/0001-bounds.toml, the checked
// ledger of registered bounds for the CI-throughput program. A bound the
// proposal calls "registered" counts only if it lives here; this test keeps
// the ledger and the proposal in sync (see ADR 0001).
type throughputBounds struct {
	Meta struct {
		Owner    string `toml:"owner"`
		Proposal string `toml:"proposal"`
		Rederive string `toml:"rederive"`
	} `toml:"meta"`
	Workload struct {
		ShapeMix   map[string]float64 `toml:"shape_mix"`
		BurstBasis string             `toml:"burst_basis"`
	} `toml:"workload"`
	Publication struct {
		CompressionMinChangesPerPR int     `toml:"compression_min_changes_per_pr"`
		ForgeHeadroom              float64 `toml:"forge_headroom"`
		Basis                      string  `toml:"basis"`
		BatchMergeStructure        string  `toml:"batch_merge_structure"`
		BatchOverlap               string  `toml:"batch_overlap"`
	} `toml:"publication"`
	Admission struct {
		CoverageMinAt500PerDay   float64 `toml:"coverage_min_at_500_per_day"`
		CoverageMinAt2000PerDay  float64 `toml:"coverage_min_at_2000_per_day"`
		CoverageMinAt10000PerDay float64 `toml:"coverage_min_at_10000_per_day"`
		ExceptionRateMax         float64 `toml:"exception_rate_max"`
		HumanTouchBudget         string  `toml:"human_touch_budget"`
	} `toml:"admission"`
	Compute struct {
		FSEPinSHA          string  `toml:"fse_pin_sha"`
		FloorFSEPerChange  float64 `toml:"floor_fse_per_change"`
		Ratchet            string  `toml:"ratchet"`
		EstimateBasis      string  `toml:"estimate_basis"`
		CurrentEstimateFSE float64 `toml:"current_estimate_fse"`
	} `toml:"compute"`
	Human struct {
		Unit               string `toml:"unit"`
		Bound              string `toml:"bound"`
		MinutesConversion  string `toml:"minutes_conversion"`
		CurrentObservation string `toml:"current_observation"`
	} `toml:"human"`
	Queue struct {
		AdmissionSLO           string  `toml:"admission_slo"`
		Bound                  string  `toml:"bound"`
		DrainMax               string  `toml:"drain_max"`
		CreationToReady        string  `toml:"creation_to_ready"`
		AmplificationMaxPhase3 float64 `toml:"amplification_max_phase3"`
		AmplificationMaxStress float64 `toml:"amplification_max_stress"`
	} `toml:"queue"`
	Safety struct {
		MissRateReviewedMax     float64 `toml:"miss_rate_reviewed_max"`
		MissRateAutoAdmittedMax float64 `toml:"miss_rate_auto_admitted_max"`
		AuditFractionMin        float64 `toml:"audit_fraction_min"`
		Confidence              float64 `toml:"confidence"`
		MinAuditedRuns          int     `toml:"min_audited_runs"`
		ObservationWindowDays   int     `toml:"observation_window_days"`
		ExposureRestatement     string  `toml:"exposure_restatement"`
	} `toml:"safety"`
	Flakes struct {
		PerJobFlakeAssumedMax float64 `toml:"per_job_flake_assumed_max"`
		Classification        string  `toml:"classification"`
		Quarantine            string  `toml:"quarantine"`
		ReplacedBy            string  `toml:"replaced_by"`
	} `toml:"flakes"`
	Model struct {
		ExtrapolationCap float64 `toml:"extrapolation_cap"`
		ForgeCanary      string  `toml:"forge_canary"`
	} `toml:"model"`
	ValueAnchor struct {
		Metrics []string `toml:"metrics"`
	} `toml:"value_anchor"`
}

var shaRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

// TestThroughputBoundsLedger verifies the bounds ledger exists, parses, holds
// every section the proposal's "registered" language depends on, and that the
// proposal and ledger reference each other.
func TestThroughputBoundsLedger(t *testing.T) {
	root := repoRoot()
	ledgerPath := filepath.Join(root, "specs", "proposals", "0001-bounds.toml")

	var b throughputBounds
	if _, err := toml.DecodeFile(ledgerPath, &b); err != nil {
		t.Fatalf("decoding bounds ledger %s: %v", ledgerPath, err)
	}

	if b.Meta.Owner == "" {
		t.Error("meta.owner is empty")
	}
	if b.Meta.Rederive == "" {
		t.Error("meta.rederive is empty: ratchets must name their re-derivation rule")
	}
	proposalPath := filepath.Join(root, "specs", "proposals", b.Meta.Proposal)
	proposal, err := os.ReadFile(proposalPath)
	if err != nil {
		t.Fatalf("meta.proposal %q does not resolve: %v", b.Meta.Proposal, err)
	}
	if !strings.Contains(string(proposal), "0001-bounds.toml") {
		t.Error("proposal never references the bounds ledger; \"registered\" has no registry")
	}

	var mixSum float64
	for _, v := range b.Workload.ShapeMix {
		mixSum += v
	}
	if len(b.Workload.ShapeMix) < 4 || mixSum < 0.999 || mixSum > 1.001 {
		t.Errorf("workload.shape_mix must cover the four shapes and sum to 1.0, got %v (sum %.3f)", b.Workload.ShapeMix, mixSum)
	}
	if b.Workload.BurstBasis == "" {
		t.Error("workload.burst_basis is empty: the burst profile must name its measurement basis")
	}

	if b.Publication.CompressionMinChangesPerPR < 7 {
		t.Errorf("publication.compression_min_changes_per_pr = %d: below the 6.94 zero-headroom floor", b.Publication.CompressionMinChangesPerPR)
	}
	if b.Publication.ForgeHeadroom <= 0 || b.Publication.ForgeHeadroom >= 1 {
		t.Errorf("publication.forge_headroom = %v: must be a fraction in (0,1)", b.Publication.ForgeHeadroom)
	}
	if b.Publication.BatchMergeStructure == "" || b.Publication.BatchOverlap == "" {
		t.Error("publication.batch_merge_structure and batch_overlap are required: the identity model depends on the publication commit structure")
	}

	milestones := []float64{
		b.Admission.CoverageMinAt500PerDay,
		b.Admission.CoverageMinAt2000PerDay,
		b.Admission.CoverageMinAt10000PerDay,
	}
	for i, m := range milestones {
		if m <= 0 || m > 1 {
			t.Errorf("admission coverage milestone %d = %v: must be in (0,1]", i, m)
		}
		if i > 0 && m < milestones[i-1] {
			t.Errorf("admission coverage milestones must be non-decreasing, got %v", milestones)
		}
	}
	if b.Admission.ExceptionRateMax <= 0 || b.Admission.ExceptionRateMax >= 1 {
		t.Errorf("admission.exception_rate_max = %v: must be a rate in (0,1)", b.Admission.ExceptionRateMax)
	}
	if b.Admission.HumanTouchBudget == "" {
		t.Error("admission.human_touch_budget is empty: the coverage milestones must name the budget that forces them")
	}

	if !shaRE.MatchString(b.Compute.FSEPinSHA) {
		t.Errorf("compute.fse_pin_sha %q is not a full commit SHA: the FSE yardstick must be pinned", b.Compute.FSEPinSHA)
	}
	if b.Compute.FloorFSEPerChange <= 0 || b.Compute.FloorFSEPerChange >= 1 {
		t.Errorf("compute.floor_fse_per_change = %v: must be in (0,1)", b.Compute.FloorFSEPerChange)
	}
	if b.Compute.Ratchet == "" || b.Human.Bound == "" || b.Queue.Bound == "" {
		t.Error("compute.ratchet, human.bound, and queue.bound must each state their ratchet rule")
	}

	if b.Queue.AmplificationMaxPhase3 < b.Queue.AmplificationMaxStress {
		t.Errorf("queue amplification bounds inverted: phase3 %v < stress %v", b.Queue.AmplificationMaxPhase3, b.Queue.AmplificationMaxStress)
	}
	if b.Queue.AmplificationMaxStress < 1 {
		t.Errorf("queue.amplification_max_stress = %v: below 1 is unsatisfiable (every change needs one state)", b.Queue.AmplificationMaxStress)
	}

	if b.Safety.MissRateAutoAdmittedMax >= b.Safety.MissRateReviewedMax {
		t.Errorf("safety: auto-admitted bound %v must be stricter than reviewed bound %v", b.Safety.MissRateAutoAdmittedMax, b.Safety.MissRateReviewedMax)
	}
	for name, v := range map[string]float64{
		"miss_rate_reviewed_max":      b.Safety.MissRateReviewedMax,
		"miss_rate_auto_admitted_max": b.Safety.MissRateAutoAdmittedMax,
		"audit_fraction_min":          b.Safety.AuditFractionMin,
	} {
		if v <= 0 || v >= 1 {
			t.Errorf("safety.%s = %v: must be a rate in (0,1)", name, v)
		}
	}
	if b.Safety.MinAuditedRuns <= 0 || b.Safety.ObservationWindowDays <= 0 {
		t.Error("safety sample size and observation window must be positive")
	}
	if b.Safety.ExposureRestatement == "" {
		t.Error("safety.exposure_restatement is empty: bounds must be restated at exposure")
	}

	if b.Flakes.PerJobFlakeAssumedMax <= 0 || b.Flakes.Classification == "" || b.Flakes.Quarantine == "" {
		t.Error("flakes: assumption, mechanical classification rule, and quarantine rule are all required")
	}

	if b.Model.ExtrapolationCap < 1 {
		t.Errorf("model.extrapolation_cap = %v: must be >= 1", b.Model.ExtrapolationCap)
	}
	if b.Model.ForgeCanary == "" {
		t.Error("model.forge_canary is empty: modeled resources need a named validating canary")
	}

	if len(b.ValueAnchor.Metrics) == 0 {
		t.Error("value_anchor.metrics is empty: the metric suite needs at least one externally-anchored signal")
	}
}
