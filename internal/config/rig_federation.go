package config

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gastownhall/gascity/internal/pathutil"
)

// Spellings of the per-rig federation settings. Messages and docs build on
// these constants, so renaming a key or a value is a change here plus the
// struct tags on [Rig].
const (
	// RigFederationCity is the default federation: every agent of the city
	// whose identity matches a row in the rig's store may act on it.
	RigFederationCity = "city"
	// RigFederationRig restricts the city to identities inside the rig's
	// namespace when it acts on the rig's store.
	RigFederationRig = "rig"
	// RigNamespaceShared is the default namespace declaration: another city
	// attaching the same store may also serve this rig's namespace.
	RigNamespaceShared = "shared"
	// RigNamespaceExclusive declares that no other city serves this rig's
	// namespace on its store. It requires RigFederationRig.
	RigNamespaceExclusive = "exclusive"

	rigFederationKey = "federation"
	rigNamespaceKey  = "namespace"
)

// guardedRigName is the shape a rig name must have for federation = "rig": a
// single identity segment of letters, digits, '-' and '_' that starts and ends
// with a letter or digit. A trailing separator would let the rig's own
// session spellings ("beta-" + "--" + agent) fold outside its namespace. The
// fold check in validateRigFederationName additionally refuses an exact "--"
// run, which the bead store reads as "/".
var guardedRigName = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9_-]*[A-Za-z0-9])?$`)

// FoldIdentity is the one normalization every federation check applies to an
// identity or rig name: surrounding space trimmed, then [CanonicalActor].
func FoldIdentity(s string) string {
	return CanonicalActor(strings.TrimSpace(s))
}

// EffectiveFederation returns the rig's federation setting, trimmed, with
// blank meaning [RigFederationCity].
func (r *Rig) EffectiveFederation() string {
	v := strings.TrimSpace(r.Federation)
	if v == "" {
		return RigFederationCity
	}
	return v
}

// FederationGuarded reports whether the rig's store is restricted to the
// rig's own namespace. It fails closed: any value other than blank or
// "city" (compared trimmed and case-insensitively) counts as guarded, so a
// Rig built in code, or one that reached a path that skipped validation,
// never silently runs unguarded. Load-time validation refuses out-of-enum
// values; this accessor is the backstop.
func (r *Rig) FederationGuarded() bool {
	v := strings.ToLower(strings.TrimSpace(r.Federation))
	return v != "" && v != RigFederationCity
}

// EffectiveNamespace returns the rig's namespace declaration, trimmed, with
// blank meaning [RigNamespaceShared].
func (r *Rig) EffectiveNamespace() string {
	v := strings.TrimSpace(r.Namespace)
	if v == "" {
		return RigNamespaceShared
	}
	return v
}

// NamespaceExclusive reports whether the rig declares namespace =
// "exclusive".
func (r *Rig) NamespaceExclusive() bool {
	return strings.EqualFold(strings.TrimSpace(r.Namespace), RigNamespaceExclusive)
}

// IdentityNamespace returns the identity prefix that names this rig's own
// agents under the bead store's actor fold: CanonicalActor(name) + "/". It
// returns "" for a rig without a name.
func (r *Rig) IdentityNamespace() string {
	name := FoldIdentity(r.Name)
	if name == "" {
		return ""
	}
	return name + "/"
}

// AdmitsLexically reports whether identity is inside the rig's namespace:
// its fold, after trimming surrounding space, starts with
// [Rig.IdentityNamespace]. A rig that is not [Rig.FederationGuarded] admits
// every identity. A guarded rig admits neither an empty identity nor any
// identity when the rig has no name. The match is case-sensitive, like the
// store's own actor comparison.
//
// This is only the lexical arm of admission. Whether an identity is one of
// this city's own pool sessions of the rig is decided by the caller, which
// can see the city's session records.
func (r *Rig) AdmitsLexically(identity string) bool {
	if !r.FederationGuarded() {
		return true
	}
	ns := r.IdentityNamespace()
	if ns == "" {
		return false
	}
	// The bare namespace ("beta/", or its spelling "beta--") names no agent.
	folded := FoldIdentity(identity)
	return folded != ns && strings.HasPrefix(folded, ns)
}

// ValidateRigFederation checks the federation settings of rigs that need no
// filesystem knowledge: values in their enums, "exclusive" only together
// with federation = "rig", a one-segment name for every guarded rig, and no
// two rigs whose names are one namespace to the bead store. It runs on every
// config load (Parse and the composed root), so an invalid setting is a load
// error on every path, including reloads and one-shot commands.
func ValidateRigFederation(rigs []Rig) error {
	for i := range rigs {
		r := &rigs[i]
		if err := validateRigFederationValues(r); err != nil {
			return err
		}
		if r.FederationGuarded() {
			if err := validateRigFederationName(r); err != nil {
				return err
			}
		}
	}
	for i := range rigs {
		g := &rigs[i]
		if !g.FederationGuarded() {
			continue
		}
		for j := range rigs {
			if i == j {
				continue
			}
			if rigNamespacesCollide(g.Name, rigs[j].Name) {
				return fmt.Errorf("rig %q: %s = %q namespace %q collides with rig %q: names that differ only in '.', '-', '_' or letter case are one namespace to the bead store",
					g.Name, rigFederationKey, RigFederationRig, g.IdentityNamespace(), rigs[j].Name)
			}
		}
	}
	return nil
}

func validateRigFederationValues(r *Rig) error {
	switch r.Federation {
	case "", RigFederationCity, RigFederationRig:
	default:
		return fmt.Errorf("rig %q: %s %q is not one of %q, %q",
			r.Name, rigFederationKey, r.Federation, RigFederationCity, RigFederationRig)
	}
	switch r.Namespace {
	case "", RigNamespaceShared, RigNamespaceExclusive:
	default:
		return fmt.Errorf("rig %q: %s %q is not one of %q, %q",
			r.Name, rigNamespaceKey, r.Namespace, RigNamespaceShared, RigNamespaceExclusive)
	}
	if r.Namespace == RigNamespaceExclusive && r.Federation != RigFederationRig {
		return fmt.Errorf("rig %q: %s = %q requires %s = %q",
			r.Name, rigNamespaceKey, RigNamespaceExclusive, rigFederationKey, RigFederationRig)
	}
	return nil
}

// validateRigFederationName refuses a guarded rig name that is not one
// identity segment. The name is the namespace, so a name the fold turns into
// several segments ("a--b" reads as "a/b") would make another identity's
// prefix the rig's namespace.
func validateRigFederationName(r *Rig) error {
	if guardedRigName.MatchString(r.Name) && !strings.Contains(CanonicalActor(r.Name), "/") {
		return nil
	}
	return fmt.Errorf("rig %q: %s = %q needs a one-segment rig name (letters, digits, '-', '_', starting and ending with a letter or digit, no \"--\"): the name is the identity namespace %q",
		r.Name, rigFederationKey, RigFederationRig, r.IdentityNamespace())
}

// rigNamespaceCollisionKey is the form two rig names are compared in for the
// one-namespace check. It folds like the store and also lowercases. Admission
// itself is case-sensitive, so the lowercase only makes this check coarser:
// names that differ only in case are refused as a pair, which keeps a city
// from relying on letter case to separate two namespaces.
func rigNamespaceCollisionKey(name string) string {
	return strings.ToLower(FoldIdentity(name))
}

// rigNamespacesCollide reports whether the namespaces of two rig names are
// equal or one contains the other under rigNamespaceCollisionKey.
func rigNamespacesCollide(a, b string) bool {
	ka, kb := rigNamespaceCollisionKey(a), rigNamespaceCollisionKey(b)
	if ka == "" || kb == "" {
		return false
	}
	return ka == kb || strings.HasPrefix(ka, kb+"/") || strings.HasPrefix(kb, ka+"/")
}

// ValidateRigFederationLayout runs [ValidateRigFederation] and then the
// federation checks that need the rigs' resolved directories and the
// expanded agent list. cityRoot resolves relative rig paths and agent dirs;
// pass the city directory. For every guarded rig:
//
//   - the rig has its own directory: no other rig, and not the city root,
//     uses the same path;
//   - no agent is attached to it by path rather than by name, because such
//     an agent's identity is path-qualified and outside the namespace;
//   - no agent attached elsewhere has an identity that folds into the rig's
//     namespace;
//   - every qualified default sling target is inside the namespace.
//
// Callers run it beside [ValidateRigs].
func ValidateRigFederationLayout(cityRoot string, cfg *City) error {
	if cfg == nil {
		return nil
	}
	if err := ValidateRigFederation(cfg.Rigs); err != nil {
		return err
	}
	for i := range cfg.Rigs {
		g := &cfg.Rigs[i]
		if !g.FederationGuarded() {
			continue
		}
		if err := validateGuardedRigDirectory(cityRoot, cfg.Rigs, i); err != nil {
			return err
		}
		if err := validateGuardedRigAgents(cityRoot, cfg, g); err != nil {
			return err
		}
		if err := validateGuardedRigSlingTargets(g); err != nil {
			return err
		}
	}
	return nil
}

func rigFederationResolvePath(cityRoot, p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if !filepath.IsAbs(p) && cityRoot != "" {
		p = filepath.Join(cityRoot, p)
	}
	return filepath.Clean(p)
}

func validateGuardedRigDirectory(cityRoot string, rigs []Rig, gi int) error {
	g := &rigs[gi]
	gPath := rigFederationResolvePath(cityRoot, g.Path)
	if gPath == "" {
		return nil
	}
	if cityRoot != "" && pathutil.SamePath(gPath, cityRoot) {
		return fmt.Errorf("rig %q: %s = %q needs its own store directory; path %q is the city root",
			g.Name, rigFederationKey, RigFederationRig, g.Path)
	}
	for j := range rigs {
		if j == gi {
			continue
		}
		other := rigFederationResolvePath(cityRoot, rigs[j].Path)
		if other != "" && pathutil.SamePath(gPath, other) {
			return fmt.Errorf("rig %q: %s = %q needs its own store directory; path %q is also rig %q's",
				g.Name, rigFederationKey, RigFederationRig, g.Path, rigs[j].Name)
		}
	}
	return nil
}

func validateGuardedRigAgents(cityRoot string, cfg *City, g *Rig) error {
	rigNames := make(map[string]bool, len(cfg.Rigs))
	for i := range cfg.Rigs {
		rigNames[cfg.Rigs[i].Name] = true
	}
	gPath := rigFederationResolvePath(cityRoot, g.Path)
	ns := g.IdentityNamespace()
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		if a.Dir == g.Name {
			continue
		}
		// workdir.ConfiguredRigName attaches an agent whose dir is not a rig
		// name to the rig whose path it resolves to. That agent's identity is
		// path-qualified, so the guarded rig would refuse its own agent.
		if a.Dir != "" && !rigNames[a.Dir] && gPath != "" &&
			pathutil.SamePath(rigFederationResolvePath(cityRoot, a.Dir), gPath) {
			return fmt.Errorf("agent %q: attach it to rig %q by name (dir = %q): %s = %q admits only %q identities",
				a.QualifiedName(), g.Name, g.Name, rigFederationKey, RigFederationRig, ns)
		}
		if err := refuseIdentityInNamespace("agent", a.QualifiedName(), a.QualifiedName(), g); err != nil {
			return err
		}
		// Pool slots are "<name>-<n>": an agent named "beta-" gets the slot
		// "beta--1", which reads as "beta/1".
		if err := refuseIdentityInNamespace("agent", a.QualifiedName(), a.QualifiedName()+"-1", g); err != nil {
			return err
		}
		// Pool session names are "<base>-<session id>" built from the
		// agent's base name without its dir, so an agent "alpha/beta-" gets
		// sessions named "beta--<id>", which read as "beta/<id>".
		if err := refuseIdentityInNamespace("agent", a.QualifiedName(), a.BindingQualifiedName()+"-1", g); err != nil {
			return err
		}
		for _, n := range a.NamepoolNames {
			if err := refuseIdentityInNamespace("agent", a.QualifiedName(), a.QualifiedInstanceName(n), g); err != nil {
				return err
			}
		}
	}
	for i := range cfg.NamedSessions {
		s := &cfg.NamedSessions[i]
		if s.Dir == g.Name {
			continue
		}
		if err := refuseIdentityInNamespace("named session", s.QualifiedName(), s.QualifiedName(), g); err != nil {
			return err
		}
	}
	// Session names rendered at runtime from [workspace] session_template or
	// an agent's tmux_alias are not checked here; the store-side enforcement
	// judges those identities when they act.
	return nil
}

// refuseIdentityInNamespace refuses identity when it belongs to something
// outside rig g (owner names it in the message) but its fold lands inside
// g's namespace: a city agent named "beta--worker" reads as "beta/worker" and
// would be admitted as one of the rig's own agents.
func refuseIdentityInNamespace(kind, owner, identity string, g *Rig) error {
	ns := g.IdentityNamespace()
	if ns == "" {
		return nil
	}
	folded := FoldIdentity(identity)
	if !strings.HasPrefix(folded, ns) {
		return nil
	}
	subject := fmt.Sprintf("%s %q", kind, owner)
	if identity != owner {
		subject = fmt.Sprintf("%s %q: identity %q", kind, owner, identity)
	}
	return fmt.Errorf("%s reads as %q to the bead store, inside rig %q's namespace %q (%s = %q); rename it or attach it to the rig",
		subject, folded, g.Name, ns, rigFederationKey, RigFederationRig)
}

func validateGuardedRigSlingTargets(g *Rig) error {
	targets := make([]string, 0, 1+len(g.DefaultSlingTargets))
	if g.DefaultSlingTarget != "" {
		targets = append(targets, g.DefaultSlingTarget)
	}
	targets = append(targets, g.DefaultSlingTargets...)
	for _, t := range targets {
		// Only qualified targets are judged here. A bare target resolves to a
		// city-scoped identity; refusing it into a guarded rig is part of the
		// enforcement that this build does not have yet.
		if !strings.Contains(FoldIdentity(t), "/") {
			continue
		}
		if FoldIdentity(t) == g.IdentityNamespace() {
			return fmt.Errorf("rig %q: default_sling_target %q names no agent inside its namespace %q (%s = %q)",
				g.Name, t, g.IdentityNamespace(), rigFederationKey, RigFederationRig)
		}
		if !g.AdmitsLexically(t) {
			return fmt.Errorf("rig %q: default_sling_target %q is outside its namespace %q (%s = %q)",
				g.Name, t, g.IdentityNamespace(), rigFederationKey, RigFederationRig)
		}
	}
	return nil
}

// RigFederationWarnings returns one operator warning per rig that sets
// federation = "rig". This build validates the setting but does not yet
// enforce it on the rig's bead store, so a guarded rig is not protected;
// callers surface these as non-fatal warnings at start and reload.
func RigFederationWarnings(rigs []Rig) []string {
	var warnings []string
	for i := range rigs {
		if !rigs[i].FederationGuarded() {
			continue
		}
		warnings = append(warnings, fmt.Sprintf(
			"rig %q sets %s = %q, which this build does not enforce yet: the rig is NOT protected, and this city still acts on every row in its bead store; do not attach a bead store that another city also uses",
			rigs[i].Name, rigFederationKey, RigFederationRig))
	}
	return warnings
}
