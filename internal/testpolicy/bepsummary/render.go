package bepsummary

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// maxValidationLabels caps the validation-failure labels listed per phase.
const maxValidationLabels = 20

// WriteMarkdown renders the report as GitHub-flavored markdown suitable for
// $GITHUB_STEP_SUMMARY.
func WriteMarkdown(w io.Writer, rep Report) error {
	var b strings.Builder
	title := "Bazel test cache report"
	if rep.Context != "" {
		title += " (" + rep.Context + ")"
	}
	fmt.Fprintf(&b, "## %s\n\n", title)
	b.WriteString("Test targets per invocation. **Cached** results were reused (local action cache / remote cache / disk cache); **executed** ones ran (remote executor / local strategy). Hit rate = cached / (cached + executed).\n\n")
	b.WriteString("| Phase | Targets | Cached (local / remote / disk) | Executed (remote / local) | Not run | Passed | Flaky | Failed | Hit rate | Test time run / skipped | Exit |\n")
	b.WriteString("| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | --- |\n")
	for _, p := range rep.Phases {
		if p.Missing {
			fmt.Fprintf(&b, "| %s | _no BEP file (`%s`)_ | | | | | | | | | |\n", p.Label, p.Source)
			continue
		}
		exit := p.ExitCode
		if p.Truncated {
			exit += " (BEP truncated)"
		}
		writeCountsRow(&b, p.Label, p.Targets, exit)
	}
	if len(rep.Phases) > 1 {
		writeCountsRow(&b, "**total**", rep.Totals, "")
	}

	for _, p := range rep.Phases {
		if n := len(p.ValidationFailed); n > 0 {
			// The aspect fails every dependent too, so the list can be long.
			shown, more := p.ValidationFailed, ""
			if n > maxValidationLabels {
				shown, more = shown[:maxValidationLabels], fmt.Sprintf(" and %d more", n-maxValidationLabels)
			}
			fmt.Fprintf(&b, "\n**%s: validation (nogo) failed for %d target(s)** (the target or a dependency): `%s`%s. A test target among them counts as %s whatever its test run reported.\n",
				p.Label, n, strings.Join(shown, "`, `"), more, StatusFailedValidation)
		}
	}

	writeActions(&b, rep.Phases)
	writeSlowest(&b, rep.Slowest)
	_, err := io.WriteString(w, b.String())
	return err
}

func writeCountsRow(b *strings.Builder, label string, c TargetCounts, exit string) {
	fmt.Fprintf(b, "| %s | %d | %d (%d / %d / %d) | %d (%d / %d) | %d | %d | %d | %d | %s | %s / %s | %s |\n",
		label, c.Total,
		c.Cached, c.CachedLocal, c.CachedRemote, c.CachedDisk,
		c.Executed, c.ExecutedRemote, c.ExecutedLocal,
		c.NotRun, c.Passed, c.Flaky, c.Failed,
		percent(c.CacheHitRate()),
		formatMillis(c.ExecutedMillis), formatMillis(c.CachedMillis),
		exit)
}

func writeActions(b *strings.Builder, phases []Phase) {
	found := false
	for _, p := range phases {
		if p.Actions != nil {
			found = true
		}
	}
	if !found {
		return
	}
	b.WriteString("\n### Actions\n\n")
	b.WriteString("| Phase | Created | Executed | Remote cache hit | Disk cache hit | Remote | Local | Internal | Action cache hits / misses | Runners |\n")
	b.WriteString("| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | --- |\n")
	for _, p := range phases {
		a := p.Actions
		if a == nil {
			continue
		}
		ac := "n/a"
		if a.HasActionCacheStats {
			ac = fmt.Sprintf("%d / %d", a.ActionCacheHits, a.ActionCacheMisses)
		}
		runners := make([]string, 0, len(a.Runners))
		for _, r := range a.Runners {
			if r.Name == runnerTotal {
				continue
			}
			runners = append(runners, fmt.Sprintf("%s: %d", r.Name, r.Count))
		}
		fmt.Fprintf(b, "| %s | %d | %d | %d | %d | %d | %d | %d | %s | %s |\n",
			p.Label, a.Created, a.Executed, a.RemoteCacheHits, a.DiskCacheHits,
			a.RemoteExecuted, a.LocalExecuted, a.Internal, ac, strings.Join(runners, ", "))
	}
}

func writeSlowest(b *strings.Builder, slowest []SlowTest) {
	if len(slowest) == 0 {
		b.WriteString("\n_No test executed: every result came from a cache._\n")
		return
	}
	fmt.Fprintf(b, "\n### Slowest executed tests (top %d)\n\n", len(slowest))
	b.WriteString("| Phase | Target | Ran | Status | Attempts | Time |\n")
	b.WriteString("| --- | --- | --- | --- | ---: | ---: |\n")
	for _, s := range slowest {
		fmt.Fprintf(b, "| %s | `%s` | %s | %s | %d | %s |\n",
			s.Phase, s.Label, outcomeWhere(s.Outcome), s.Status, s.Attempts, formatMillis(s.ExecutedMillis))
	}
}

func outcomeWhere(o Outcome) string {
	switch o {
	case OutcomeExecutedRemote:
		return "remote"
	case OutcomeExecutedLocal:
		return "local"
	default:
		return string(o)
	}
}

func percent(f float64) string {
	return fmt.Sprintf("%.1f%%", f*100)
}

func formatMillis(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d >= time.Minute:
		return d.Round(time.Second).String()
	default:
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
}
