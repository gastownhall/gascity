package doctor

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/pgauth"
)

// PostgresAuthCheck verifies that every PG-backed scope (city + rigs) has
// a resolvable Postgres password under the steady-state on-disk + process-
// env perspective. The check runs the same resolution chain the bd
// subprocess will see, so a green result means a non-interactive agent
// will authenticate successfully.
//
// The check NEVER prints the password value. Verbose details surface the
// per-scope source enum and path; the explain table (gated by
// CheckContext.ExplainPostgresAuth) shows the per-tier evaluation chain.
type PostgresAuthCheck struct {
	cityPath string
	scopes   []postgresAuthScope
	results  []postgresAuthScopeResult // populated by Run; consumed by RenderExtras
}

// postgresAuthScope is a single scope the check probes.
type postgresAuthScope struct {
	kind        string // "city" or "rig"
	display     string // e.g. "rigs/pwu (127.0.0.1:5433)" or "city (127.0.0.1:5433)"
	relPath     string // scope path relative to city, with no leading "./"; ".beads/.env" for city
	root        string // absolute scope root path
	endpoint    pgauth.Endpoint
	displayName string // bare name used by the explain table header
}

// postgresAuthScopeResult holds the per-scope resolver outcome the
// summary aggregator and the explain table both consume.
type postgresAuthScopeResult struct {
	scope     postgresAuthScope
	resolved  pgauth.Resolved
	resolveOK bool
	err       error
}

// NewPostgresAuthCheck constructs a postgres-auth check for cityPath.
// Pass the loaded city config so the check can enumerate rigs without
// re-parsing. Scopes whose metadata.json reports backend != "postgres"
// are skipped (the registration site already gates on PG presence).
func NewPostgresAuthCheck(cityPath string, cfg *config.City) *PostgresAuthCheck {
	return &PostgresAuthCheck{cityPath: cityPath, scopes: collectPostgresAuthScopes(cityPath, cfg)}
}

// Name returns the check identifier (kebab-case, matches existing checks).
func (c *PostgresAuthCheck) Name() string { return "postgres-auth" }

// CanFix returns false. Every fix needs a value the operator must supply
// (chmod, scope-file write, credentials-file edit). The hint already
// names the exact command.
func (c *PostgresAuthCheck) CanFix() bool { return false }

// Fix is a no-op; CanFix is false.
func (c *PostgresAuthCheck) Fix(_ *CheckContext) error { return nil }

// Run probes every PG-backed scope and aggregates the outcome.
func (c *PostgresAuthCheck) Run(_ *CheckContext) *CheckResult {
	r := &CheckResult{Name: c.Name()}
	if len(c.scopes) == 0 {
		r.Status = StatusOK
		r.Message = "no postgres-backed scopes"
		return r
	}

	c.results = make([]postgresAuthScopeResult, 0, len(c.scopes))
	for _, s := range c.scopes {
		// envMap=nil per design §9.6 — probe the steady-state perspective.
		resolved, err := pgauth.ResolveFromEnv(nil, s.root, s.endpoint)
		c.results = append(c.results, postgresAuthScopeResult{
			scope:     s,
			resolved:  resolved,
			resolveOK: err == nil,
			err:       err,
		})
	}

	per := make([]perScopeReport, 0, len(c.results))
	for _, res := range c.results {
		per = append(per, classifyPostgresAuthResult(res))
	}
	sortPerScopeReports(per)

	r.Status = aggregatePostgresAuthStatus(per)
	r.Message = aggregatePostgresAuthMessage(per)
	if hint := aggregatePostgresAuthFixHint(per); hint != "" {
		r.FixHint = hint
	}
	r.Details = aggregatePostgresAuthDetails(per)
	return r
}

// RenderExtras emits the per-scope --explain-postgres-auth table when
// CheckContext.ExplainPostgresAuth is true. No-op otherwise.
func (c *PostgresAuthCheck) RenderExtras(ctx *CheckContext, w io.Writer) {
	if ctx == nil || !ctx.ExplainPostgresAuth {
		return
	}
	if len(c.scopes) == 0 {
		fmt.Fprintln(w, "  no postgres-backed scopes (this flag has no effect)") //nolint:errcheck // best-effort output
		return
	}
	per := make([]perScopeReport, 0, len(c.results))
	for _, res := range c.results {
		per = append(per, classifyPostgresAuthResult(res))
	}
	sortPerScopeReports(per)
	for i, p := range per {
		if i > 0 {
			fmt.Fprintln(w) //nolint:errcheck // best-effort output
		}
		renderPostgresExplainTable(w, p)
	}
}

// collectPostgresAuthScopes walks the city + each rig, opens their
// metadata.json, and keeps only scopes whose backend is "postgres".
// Scopes that fail to parse or are not PG-backed are silently filtered
// here; the registration site is responsible for the "no PG scopes"
// case (the check is not registered when no scope has Backend==postgres).
func collectPostgresAuthScopes(cityPath string, cfg *config.City) []postgresAuthScope {
	var out []postgresAuthScope
	if scope, ok := loadPostgresAuthScope(cityPath, "city", "city", ".beads/.env"); ok {
		out = append(out, scope)
	}
	if cfg == nil {
		return out
	}
	for _, rig := range cfg.Rigs {
		if rig.Suspended {
			continue
		}
		rigPath := strings.TrimSpace(rig.Path)
		if rigPath == "" {
			continue
		}
		if !filepath.IsAbs(rigPath) {
			rigPath = filepath.Join(cityPath, rigPath)
		}
		rel := rig.Path
		if rel == "" {
			rel = filepath.Base(rigPath)
		}
		display := "rigs/" + rig.Name
		scope, ok := loadPostgresAuthScope(rigPath, "rig", display, filepath.Join(rel, ".beads", ".env"))
		if !ok {
			continue
		}
		scope.displayName = rig.Name
		out = append(out, scope)
	}
	return out
}

func loadPostgresAuthScope(scopeRoot, kind, displayBase, relEnv string) (postgresAuthScope, bool) {
	metaPath := filepath.Join(scopeRoot, ".beads", "metadata.json")
	meta, _, err := contract.LoadMetadataState(fsys.OSFS{}, metaPath)
	if err != nil || meta.Backend != "postgres" {
		return postgresAuthScope{}, false
	}
	endpoint := pgauth.Endpoint{
		Host: meta.PostgresHost,
		Port: meta.PostgresPort,
		User: meta.PostgresUser,
	}
	display := fmt.Sprintf("%s (%s:%s)", displayBase, endpoint.Host, endpoint.Port)
	scope := postgresAuthScope{
		kind:        kind,
		display:     display,
		relPath:     filepath.ToSlash(relEnv),
		root:        scopeRoot,
		endpoint:    endpoint,
		displayName: displayBase,
	}
	return scope, true
}

// perScopeReport is the per-scope shape consumed by aggregator and the
// explain table.
type perScopeReport struct {
	scope    postgresAuthScope
	resolved pgauth.Resolved
	status   CheckStatus
	message  string
	detail   string // verbose-mode line; empty when message already says everything
	fixHint  string
	// errParse / errPerm: when set, the failing tier is described by these
	// fields; the explain table renders [ERR] at the failing tier.
	errParse *pgauth.CredentialsParseError
	errPerm  *pgauth.PermissivePermissionError
	// rawErr is the unrecognized-error text for §3.3.6.
	rawErr string
}

// classifyPostgresAuthResult maps a resolver outcome onto one of the
// five branches in design §3.3.
func classifyPostgresAuthResult(res postgresAuthScopeResult) perScopeReport {
	out := perScopeReport{
		scope:    res.scope,
		resolved: res.resolved,
	}
	scopeRel := strings.TrimPrefix(res.scope.relPath, "./")
	if res.resolveOK {
		switch res.resolved.Source {
		case pgauth.SourceProcessEnvBeads, pgauth.SourceProcessEnvGC:
			out.status = StatusWarning
			out.message = fmt.Sprintf("%s: password from parent shell env", res.scope.display)
			out.detail = fmt.Sprintf("scope=%s  source=%s  user=%s", res.scope.root, res.resolved.Source.String(), res.resolved.User)
			out.fixHint = fmt.Sprintf("parent-shell env works for the current shell only. Persist via %s (chmod 600) for non-interactive use.", scopeRel)
		default:
			out.status = StatusOK
			label := humanSourceLabel(res.resolved.Source)
			out.message = fmt.Sprintf("%s: password from %s", res.scope.display, label)
			out.detail = fmt.Sprintf("scope=%s  source=%s  user=%s", res.scope.root, res.resolved.Source.String(), res.resolved.User)
		}
		return out
	}

	// Error path.
	var permErr *pgauth.PermissivePermissionError
	if errors.As(res.err, &permErr) {
		out.status = StatusError
		out.message = fmt.Sprintf("%s: credentials file mode %#o (group/other readable)", res.scope.display, permErr.Mode.Perm())
		tier := classifyPermissionErrorTier(res.scope, permErr.Path)
		out.detail = fmt.Sprintf("tier=%s  path=%s", tier, permErr.Path)
		out.fixHint = fmt.Sprintf("chmod 600 %s", permErr.Path)
		out.errPerm = permErr
		return out
	}
	var parseErr *pgauth.CredentialsParseError
	if errors.As(res.err, &parseErr) {
		out.status = StatusError
		out.message = fmt.Sprintf("%s: parse %s at line %d: %s", res.scope.display, parseErr.Path, parseErr.Line, parseErr.Reason)
		out.fixHint = fmt.Sprintf("edit %s line %d — see slice-2 reason vocabulary", parseErr.Path, parseErr.Line)
		out.errParse = parseErr
		return out
	}
	if errors.Is(res.err, pgauth.ErrNoPasswordResolvable) {
		out.status = StatusError
		out.message = fmt.Sprintf("%s: no password resolvable", res.scope.display)
		out.fixHint = fmt.Sprintf("set BEADS_POSTGRES_PASSWORD in %s (chmod 600)\nor add a [%s:%s] section to ~/.config/beads/credentials.",
			scopeRel, res.scope.endpoint.Host, res.scope.endpoint.Port)
		return out
	}
	// Defensive fallthrough — design §3.3.6.
	out.status = StatusError
	out.message = fmt.Sprintf("%s: pgauth returned unrecognized error: %s", res.scope.display, res.err.Error())
	out.fixHint = "please file a bug — postgres-auth check did not recognize this error shape"
	out.rawErr = res.err.Error()
	return out
}

// classifyPermissionErrorTier reconstructs which tier the resolver was
// reading when it hit the chmod wall. Design §3.3.4 — the resolver does
// not currently carry a Source on this error, so the doctor compares the
// path against known tier identifiers.
func classifyPermissionErrorTier(scope postgresAuthScope, path string) string {
	scopeEnv := filepath.Join(scope.root, ".beads", ".env")
	if filepath.Clean(path) == filepath.Clean(scopeEnv) {
		return pgauth.SourceScopeFile.String()
	}
	if def := pgauth.DefaultCredentialsPath(); def != "" && filepath.Clean(path) == filepath.Clean(def) {
		return pgauth.SourceCredentialsFileHome.String()
	}
	// Either tier 6 ($BEADS_CREDENTIALS_FILE) or an unknown path; pick
	// the less-specific label per design §3.3.4 implementation note.
	return "credentials_file"
}

// humanSourceLabel maps a pgauth.Source value to its operator-facing
// label (design §3.4 table). For SourceNone (used only on error path)
// this returns an empty string; callers must handle that case.
func humanSourceLabel(s pgauth.Source) string {
	switch s {
	case pgauth.SourceProjectedGC:
		return "projected env (GC_POSTGRES_PASSWORD)"
	case pgauth.SourceProjectedBeads:
		return "projected env (BEADS_POSTGRES_PASSWORD)"
	case pgauth.SourceProcessEnvGC:
		return "parent shell env (GC_POSTGRES_PASSWORD)"
	case pgauth.SourceScopeFile:
		return "scope file"
	case pgauth.SourceProcessEnvBeads:
		return "parent shell env (BEADS_POSTGRES_PASSWORD)"
	case pgauth.SourceCredentialsFileEnv:
		return "$BEADS_CREDENTIALS_FILE"
	case pgauth.SourceCredentialsFileHome:
		return "~/.config/beads/credentials"
	}
	return ""
}

// sortPerScopeReports orders the per-scope reports by severity desc,
// then by scope path asc. Stable order means deterministic output.
func sortPerScopeReports(reports []perScopeReport) {
	sort.SliceStable(reports, func(i, j int) bool {
		if reports[i].status != reports[j].status {
			return reports[i].status > reports[j].status
		}
		return reports[i].scope.root < reports[j].scope.root
	})
}

// aggregatePostgresAuthStatus returns the maximum severity across reports.
func aggregatePostgresAuthStatus(reports []perScopeReport) CheckStatus {
	out := StatusOK
	for _, p := range reports {
		if p.status > out {
			out = p.status
		}
	}
	return out
}

// aggregatePostgresAuthMessage builds the summary line per design §3.5.
func aggregatePostgresAuthMessage(reports []perScopeReport) string {
	if len(reports) == 0 {
		return "no postgres-backed scopes"
	}
	if len(reports) == 1 {
		return reports[0].message
	}
	// Multiple scopes: prepend count + first-issue prefix.
	first := reports[0].message
	return fmt.Sprintf("%d postgres-backed scope(s); first issue: %s", len(reports), first)
}

// aggregatePostgresAuthFixHint surfaces the highest-severity scope's
// fix hint per §3.5.
func aggregatePostgresAuthFixHint(reports []perScopeReport) string {
	for _, p := range reports {
		if p.fixHint != "" {
			return p.fixHint
		}
	}
	return ""
}

// aggregatePostgresAuthDetails returns the per-scope detail lines for
// verbose mode. Each line gets a status glyph + scope display + per-
// scope message; verbose-only detail rows fold below their parent.
func aggregatePostgresAuthDetails(reports []perScopeReport) []string {
	if len(reports) <= 1 {
		// printResult already shows the message + hint; the detail row
		// from §3.3 (scope=... source=...) is the only verbose addition.
		if len(reports) == 1 && reports[0].detail != "" {
			return []string{reports[0].detail}
		}
		return nil
	}
	var details []string
	for _, p := range reports {
		details = append(details, fmt.Sprintf("%s %s — %s", statusGlyph(p.status), p.scope.display, p.message))
		if p.detail != "" {
			details = append(details, "  "+p.detail)
		}
	}
	return details
}

// statusGlyph returns the same icon vocabulary printResult uses.
func statusGlyph(s CheckStatus) string {
	switch s {
	case StatusError:
		return "✗"
	case StatusWarning:
		return "⚠"
	}
	return "✓"
}

// explainTier holds per-tier rendering state for the explain table.
type explainTier struct {
	number int
	label  string // column-B label (e.g. "projected env", "os.Getenv", "scope file")
	ident  string // column-C identifier (env-var name, scope-relative path, [host:port])
	status string // [YES] | [no] | [skip] | [ERR]
	note   string // when status is [ERR], the inline reason rendered below the row
}

// renderPostgresExplainTable writes a single scope's explain table to w.
// Layout per design §4.2: 2-space indent, three columns, status token
// right-aligned at column 70, footer with Source identifier + position.
func renderPostgresExplainTable(w io.Writer, p perScopeReport) {
	header := fmt.Sprintf("PG-backed scope: %s  (host=%s:%s user=%s)",
		p.scope.display, p.scope.endpoint.Host, p.scope.endpoint.Port, p.scope.endpoint.User)
	fmt.Fprintln(w, "  "+header) //nolint:errcheck // best-effort output

	winnerTier := sourceTier(p.resolved.Source)
	errTier := errorTierFor(p)
	tiers := buildExplainTiers(p, winnerTier, errTier)
	for _, t := range tiers {
		row := formatExplainRow(t.number, t.label, t.ident)
		line := padForStatus(row, t.status)
		// Append " ← winner" only on the winning row (when there is one).
		if t.status == "[YES]" {
			line += "  ← winner"
		}
		fmt.Fprintln(w, line) //nolint:errcheck // best-effort output
		if t.note != "" {
			fmt.Fprintln(w, "          "+t.note) //nolint:errcheck // best-effort output
		}
	}

	// Footer.
	fmt.Fprintln(w) //nolint:errcheck // best-effort output
	footer := explainFooter(p, winnerTier, errTier)
	fmt.Fprintln(w, "  "+footer) //nolint:errcheck // best-effort output
}

// formatExplainRow returns the leading row text up to (but not
// including) the status token. Indent + tier number + label + identifier.
func formatExplainRow(tier int, label, ident string) string {
	tierToken := fmt.Sprintf("Tier %d ", tier) // 7 chars when tier is single-digit
	if len(label) >= 14 {
		// Long-label tiers (6, 7) per design §4.2 — the label IS the
		// identifier; collapse column B and let the identifier carry.
		return fmt.Sprintf("  %s%s", tierToken, label)
	}
	if label == "" {
		return fmt.Sprintf("  %s%s", tierToken, ident)
	}
	labelCol := label
	for len(labelCol) < 14 {
		labelCol += " "
	}
	return fmt.Sprintf("  %s%s%s", tierToken, labelCol, ident)
}

// padForStatus right-aligns the status token at column 70 (1-indexed).
// If the prefix is already at or past column 70-len(status), a single
// space separates them (graceful narrow-terminal degradation per §8.4).
func padForStatus(prefix, status string) string {
	const targetEnd = 70
	want := targetEnd - len(status)
	if len(prefix) >= want {
		return prefix + " " + status
	}
	pad := strings.Repeat(" ", want-len(prefix))
	return prefix + pad + status
}

// buildExplainTiers returns the seven tier rows in resolution order.
func buildExplainTiers(p perScopeReport, winnerTier, errTier int) []explainTier {
	scopeRel := strings.TrimSuffix(p.scope.relPath, "")
	tiers := []explainTier{
		{number: 1, label: "projected env", ident: "GC_POSTGRES_PASSWORD"},
		{number: 2, label: "projected env", ident: "BEADS_POSTGRES_PASSWORD"},
		{number: 3, label: "os.Getenv", ident: "GC_POSTGRES_PASSWORD"},
		{number: 4, label: "scope file", ident: scopeRel + " BEADS_POSTGRES_PASSWORD"},
		{number: 5, label: "os.Getenv", ident: "BEADS_POSTGRES_PASSWORD"},
		{number: 6, label: "$BEADS_CREDENTIALS_FILE", ident: fmt.Sprintf("[%s:%s]", p.scope.endpoint.Host, p.scope.endpoint.Port)},
		{number: 7, label: "~/.config/beads/credentials", ident: fmt.Sprintf("[%s:%s]", p.scope.endpoint.Host, p.scope.endpoint.Port)},
	}
	stop := winnerTier
	if errTier > 0 {
		stop = errTier
	}
	for i := range tiers {
		switch {
		case errTier > 0 && tiers[i].number == errTier:
			tiers[i].status = "[ERR]"
			tiers[i].note = explainErrorNote(p)
		case winnerTier > 0 && tiers[i].number == winnerTier:
			tiers[i].status = "[YES]"
		case stop > 0 && tiers[i].number > stop:
			tiers[i].status = "[skip]"
		default:
			tiers[i].status = "[no]"
		}
	}
	return tiers
}

// explainErrorNote returns the inline reason for an [ERR] row.
func explainErrorNote(p perScopeReport) string {
	if p.errPerm != nil {
		return fmt.Sprintf("mode %#o (group/other readable) — chmod 600 to enable", p.errPerm.Mode.Perm())
	}
	if p.errParse != nil {
		return fmt.Sprintf("line %d: %s", p.errParse.Line, p.errParse.Reason)
	}
	if p.rawErr != "" {
		return p.rawErr
	}
	return ""
}

// explainFooter returns the per-scope footer line per design §4.2.
func explainFooter(p perScopeReport, winnerTier, errTier int) string {
	if errTier > 0 {
		return fmt.Sprintf("Source identifier: %s   Resolution stopped at tier %d (see error above).", pgauth.SourceNone.String(), errTier)
	}
	if winnerTier == 0 {
		return fmt.Sprintf("Source identifier: %s   No password resolvable. See: gc doctor (errors).", pgauth.SourceNone.String())
	}
	return fmt.Sprintf("Source identifier: %s   Source position: tier %d of 7", p.resolved.Source.String(), winnerTier)
}

// sourceTier maps a pgauth.Source value to its resolution-chain tier
// number (1..7). Returns 0 for SourceNone or unknown values.
func sourceTier(s pgauth.Source) int {
	switch s {
	case pgauth.SourceProjectedGC:
		return 1
	case pgauth.SourceProjectedBeads:
		return 2
	case pgauth.SourceProcessEnvGC:
		return 3
	case pgauth.SourceScopeFile:
		return 4
	case pgauth.SourceProcessEnvBeads:
		return 5
	case pgauth.SourceCredentialsFileEnv:
		return 6
	case pgauth.SourceCredentialsFileHome:
		return 7
	}
	return 0
}

// errorTierFor returns the tier number an [ERR] row applies to, or 0
// when there is no error at a specific tier.
func errorTierFor(p perScopeReport) int {
	switch {
	case p.errPerm != nil:
		// Permission error: classify by path.
		scopeEnv := filepath.Join(p.scope.root, ".beads", ".env")
		if filepath.Clean(p.errPerm.Path) == filepath.Clean(scopeEnv) {
			return 4
		}
		if def := pgauth.DefaultCredentialsPath(); def != "" && filepath.Clean(p.errPerm.Path) == filepath.Clean(def) {
			return 7
		}
		return 6
	case p.errParse != nil:
		// Parse error: tier 6 if from $BEADS_CREDENTIALS_FILE, else 7.
		if def := pgauth.DefaultCredentialsPath(); def != "" && filepath.Clean(p.errParse.Path) == filepath.Clean(def) {
			return 7
		}
		return 6
	}
	return 0
}
