package doctor

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/gitcred"
	"github.com/gastownhall/gascity/internal/remotesource"
)

// PackCredentialsCheck validates the city's pack-source credential rules: it
// loads the layered credentials.toml (surfacing insecure permissions and parse
// errors as blocking), and reports which configured rules match a remote import
// in the city's pack.toml and which are unused. It does NOT perform a live
// network probe — that is a deferred follow-up (see the credentialed-pack-imports
// plan) so doctor gains no new network surface here.
type PackCredentialsCheck struct {
	imports map[string]config.Import
}

// NewPackCredentialsCheck builds the pack-credentials check for the city's
// direct imports.
func NewPackCredentialsCheck(imports map[string]config.Import) *PackCredentialsCheck {
	return &PackCredentialsCheck{imports: imports}
}

// Name returns the check identifier.
func (c *PackCredentialsCheck) Name() string { return "pack-credentials" }

// Run loads the credential rules and reports coverage against the remote
// imports.
func (c *PackCredentialsCheck) Run(ctx *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}

	rules, err := gitcred.Load(ctx.CityPath)
	if err != nil {
		r.Status = StatusError
		r.Message = fmt.Sprintf("pack credentials could not be loaded: %v", err)
		r.FixHint = "fix the credentials.toml permissions (must be 0600) and pointer cardinality, then re-run gc doctor"
		return r
	}

	loaded := rules.All()
	if len(loaded) == 0 && !rules.HasCommandLayer() {
		r.Status = StatusOK
		r.Message = "no pack credentials configured"
		return r
	}

	// Determine which remote imports would match a rule and which rules are
	// unused, purely from the resolver — no network.
	unmatchedRemotes := c.remoteImportsWithoutRule(rules)
	usedOrigins := c.matchedRuleOrigins(rules)

	var details []string
	for _, lr := range loaded {
		state := "unused"
		if usedOrigins[originMatchKey(lr)] {
			state = "matches an import"
		}
		details = append(details, fmt.Sprintf("%s (%s) — %s", lr.Match, lr.Origin, state))
	}
	sort.Strings(details)
	r.Details = details

	if len(unmatchedRemotes) > 0 {
		sort.Strings(unmatchedRemotes)
		r.Status = StatusWarning
		r.Message = fmt.Sprintf("%d remote import(s) have no matching credential rule", len(unmatchedRemotes))
		r.FixHint = "register a credential for each host: gc import credential add <host> --helper 'gh auth token'"
		r.Details = append(r.Details, unmatchedRemotes...)
		return r
	}

	r.Status = StatusOK
	r.Message = fmt.Sprintf("%d pack credential rule(s) configured", len(loaded))
	return r
}

// remoteImportsWithoutRule returns the redacted clone URLs of networked remote
// imports that no credential rule matches. A public import legitimately has no
// rule, so this is advisory only (StatusWarning), not an error.
func (c *PackCredentialsCheck) remoteImportsWithoutRule(rules *gitcred.Rules) []string {
	var missing []string
	for _, imp := range c.imports {
		if !credentialRelevantRemote(imp.Source) {
			continue
		}
		if _, ok := rules.MatchSource(imp.Source); ok {
			continue
		}
		missing = append(missing, gitcred.RedactUserinfo(remotesource.Parse(imp.Source).CloneURL))
	}
	return missing
}

// matchedRuleOrigins reports which rules match at least one networked remote
// import.
func (c *PackCredentialsCheck) matchedRuleOrigins(rules *gitcred.Rules) map[string]bool {
	used := make(map[string]bool)
	for _, imp := range c.imports {
		if !credentialRelevantRemote(imp.Source) {
			continue
		}
		if lr, ok := rules.MatchSource(imp.Source); ok {
			used[originMatchKey(lr)] = true
		}
	}
	return used
}

// credentialRelevantRemote reports whether a source is a networked remote that
// could require a credential. file:// and local-path sources authenticate no
// network fetch (they clone from disk), so they are excluded — otherwise a
// local file:// import would be wrongly flagged as a remote with no rule.
func credentialRelevantRemote(source string) bool {
	return remotesource.IsRemote(source) && !strings.HasPrefix(source, "file://")
}

func originMatchKey(lr gitcred.LoadedRule) string {
	return lr.Origin + "\x00" + lr.Match
}

// CanFix returns false — credential registration is an explicit user action.
func (c *PackCredentialsCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *PackCredentialsCheck) Fix(_ *CheckContext) error { return nil }

// WarmupEligible returns false — credential checks run on demand only.
func (c *PackCredentialsCheck) WarmupEligible() bool { return false }
