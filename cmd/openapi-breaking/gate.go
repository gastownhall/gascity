package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// levelErr is oasdiff's numeric level for ERR-level (breaking) changes.
const levelErr = 3

// checkProblemTypeRemoved is the gate-native check for a problem-type URN
// that disappears from the ErrorModel's x-gascity-problem-types extension.
// oasdiff ignores vendor extensions, so the gate diffs that set itself.
const checkProblemTypeRemoved = "problem-type-removed"

// componentsSection is the target for spec-wide (non-operation) findings.
const componentsSection = "components"

// finding is one change in oasdiff's JSON changelog shape.
type finding struct {
	ID          string `json:"id"`
	Text        string `json:"text"`
	Level       int    `json:"level"`
	Operation   string `json:"operation,omitempty"`
	Path        string `json:"path,omitempty"`
	Section     string `json:"section,omitempty"`
	Fingerprint string `json:"fingerprint"`
}

// target names where the change lands: "METHOD /path" for operation changes,
// otherwise the spec section.
func (f finding) target() string {
	if f.Operation != "" {
		return f.Operation + " " + f.Path
	}
	return f.Section
}

// fingerprintFor mirrors oasdiff's checker.ComputeFingerprint,
// SHA256("{id}:{operation}:{path}:{args joined by ;}")[:12], so gate-native
// findings are fingerprinted and waived exactly like oasdiff's.
func fingerprintFor(id, operation, path string, args ...string) string {
	sum := sha256.Sum256([]byte(id + ":" + operation + ":" + path + ":" + strings.Join(args, ";")))
	return hex.EncodeToString(sum[:])[:12]
}

// parseOasdiffChangelog decodes `oasdiff changelog --format json` output and
// keeps only ERR-level findings.
func parseOasdiffChangelog(data []byte) ([]finding, error) {
	var all []finding
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, fmt.Errorf("decoding oasdiff changelog: %w", err)
	}
	var breaking []finding
	for _, f := range all {
		if f.Level < levelErr {
			continue
		}
		if f.ID == "" || f.Fingerprint == "" {
			return nil, fmt.Errorf("oasdiff finding missing id or fingerprint: %+v", f)
		}
		breaking = append(breaking, f)
	}
	return breaking, nil
}

// problemTypeRemovals reports every problem-type URN present in the base
// spec's ErrorModel.type x-gascity-problem-types set but absent from the
// revision's. Clients branch on these URNs; removing one is a wire break.
func problemTypeRemovals(base, revision []byte) ([]finding, error) {
	baseURNs, err := problemTypes(base)
	if err != nil {
		return nil, fmt.Errorf("base spec: %w", err)
	}
	revURNs, err := problemTypes(revision)
	if err != nil {
		return nil, fmt.Errorf("revision spec: %w", err)
	}
	present := make(map[string]bool, len(revURNs))
	for _, u := range revURNs {
		present[u] = true
	}
	var out []finding
	for _, u := range baseURNs {
		if present[u] {
			continue
		}
		text := fmt.Sprintf("removed the problem type `%s` from ErrorModel.type x-gascity-problem-types", u)
		out = append(out, finding{
			ID:          checkProblemTypeRemoved,
			Text:        text,
			Level:       levelErr,
			Section:     componentsSection,
			Fingerprint: fingerprintFor(checkProblemTypeRemoved, "", componentsSection, u),
		})
	}
	return out, nil
}

// problemTypes extracts components.schemas.ErrorModel.properties.type
// ["x-gascity-problem-types"]. A spec without ErrorModel or the extension
// declares no problem types; a malformed extension is an error.
func problemTypes(spec []byte) ([]string, error) {
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]map[string]json.RawMessage `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(spec, &doc); err != nil {
		return nil, fmt.Errorf("decoding spec: %w", err)
	}
	raw, ok := doc.Components.Schemas["ErrorModel"].Properties["type"]["x-gascity-problem-types"]
	if !ok {
		return nil, nil
	}
	var urns []string
	if err := json.Unmarshal(raw, &urns); err != nil {
		return nil, fmt.Errorf("decoding ErrorModel.type x-gascity-problem-types: %w", err)
	}
	return urns, nil
}

// verdict is the gate outcome after waivers are applied.
type verdict struct {
	Unwaived []finding
	Waived   []finding
	Unused   []waiver
}

// evaluate applies waivers to findings. A waiver whose fingerprint matches a
// finding but whose check or target disagrees is an error: the stanza was
// copied from a different change.
func evaluate(findings []finding, waivers []waiver) (verdict, error) {
	byFingerprint := make(map[string]waiver, len(waivers))
	for _, w := range waivers {
		byFingerprint[w.Fingerprint] = w
	}
	used := map[string]bool{}
	var v verdict
	for _, f := range findings {
		w, ok := byFingerprint[f.Fingerprint]
		if !ok {
			v.Unwaived = append(v.Unwaived, f)
			continue
		}
		if w.Check != f.ID || w.Target != f.target() {
			return verdict{}, fmt.Errorf("waiver %s names check %q target %q, but the change with that fingerprint is check %q target %q",
				w.Fingerprint, w.Check, w.Target, f.ID, f.target())
		}
		used[w.Fingerprint] = true
		v.Waived = append(v.Waived, f)
	}
	for _, w := range waivers {
		if !used[w.Fingerprint] {
			v.Unused = append(v.Unused, w)
		}
	}
	sortFindings(v.Unwaived)
	sortFindings(v.Waived)
	return v, nil
}

func sortFindings(fs []finding) {
	sort.SliceStable(fs, func(i, j int) bool {
		if fs[i].target() != fs[j].target() {
			return fs[i].target() < fs[j].target()
		}
		if fs[i].ID != fs[j].ID {
			return fs[i].ID < fs[j].ID
		}
		return fs[i].Text < fs[j].Text
	})
}

// report writes the human-readable verdict. It returns true when the gate
// passes (no unwaived breaking changes).
func report(w io.Writer, v verdict, policyPath string) (bool, error) {
	var b strings.Builder
	for _, f := range v.Waived {
		fmt.Fprintf(&b, "waived  [%s] %s: %s (%s)\n", f.ID, f.target(), f.Text, f.Fingerprint)
	}
	for _, u := range v.Unused {
		fmt.Fprintf(&b, "note: waiver %s (%s %s) matches no change against this base; prune it once its PR has merged, or it will also waive a later identical change\n", u.Fingerprint, u.Check, u.Target)
	}
	pass := len(v.Unwaived) == 0
	if pass {
		b.WriteString("openapi-breaking: no unwaived breaking changes\n")
	} else {
		fmt.Fprintf(&b, "openapi-breaking: %d breaking change(s) to the OpenAPI contract:\n\n", len(v.Unwaived))
		for _, f := range v.Unwaived {
			fmt.Fprintf(&b, "  [%s] %s\n      %s\n", f.ID, f.target(), f.Text)
		}
		b.WriteString("\nExisting clients built against the base spec can break. Restore the contract,\n")
		fmt.Fprintf(&b, "or, if the break is intentional, add these waivers to %s:\n\n", policyPath)
		for _, f := range v.Unwaived {
			b.WriteString(waiverStanza(f))
			b.WriteString("\n")
		}
	}
	if _, err := io.WriteString(w, b.String()); err != nil {
		return false, fmt.Errorf("writing report: %w", err)
	}
	return pass, nil
}

func waiverStanza(f finding) string {
	return strings.Join([]string{
		"[[waiver]]",
		fmt.Sprintf("fingerprint = %q", f.Fingerprint),
		fmt.Sprintf("check = %q", f.ID),
		fmt.Sprintf("target = %q", f.target()),
		"# " + strings.ReplaceAll(f.Text, "\n", " "),
		`reason = "TODO: why breaking existing clients is acceptable"`,
		`pr = "#TODO"`,
		"",
	}, "\n")
}
