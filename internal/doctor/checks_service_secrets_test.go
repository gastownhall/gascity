package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func writeSecretFile(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte("secret\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	// WriteFile mode is umask-filtered; chmod to the exact fixture mode.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
}

func serviceSecretsTestConfig() *config.City {
	return &config.City{
		Services: []config.Service{
			{Name: "bridge", Kind: "proxy_process", StateRoot: ".gc/services/bridge"},
			// Same state root as bridge: the shared dir must be checked once.
			{Name: "bridge-admin", Kind: "proxy_process", StateRoot: ".gc/services/bridge"},
			{Name: "intake", Kind: "proxy_process", StateRoot: ".gc/services/intake"},
		},
	}
}

func TestServiceSecretsPermsCheckOKWhenTight(t *testing.T) {
	cityPath := t.TempDir()
	writeSecretFile(t, filepath.Join(cityPath, ".gc", "services", "bridge", "secrets", "bot-token.txt"), 0o600)

	check := NewServiceSecretsPermsCheck(serviceSecretsTestConfig(), cityPath)
	r := check.Run(&CheckContext{CityPath: cityPath})
	if r.Status != StatusOK {
		t.Fatalf("Status = %v, want OK (message %q)", r.Status, r.Message)
	}
}

func TestServiceSecretsPermsCheckOKWhenNoSecretsDir(t *testing.T) {
	cityPath := t.TempDir()
	check := NewServiceSecretsPermsCheck(serviceSecretsTestConfig(), cityPath)
	r := check.Run(&CheckContext{CityPath: cityPath})
	if r.Status != StatusOK {
		t.Fatalf("Status = %v, want OK (message %q)", r.Status, r.Message)
	}
}

func TestServiceSecretsPermsCheckFlagsLooseFilesAndDirs(t *testing.T) {
	cityPath := t.TempDir()
	loose := filepath.Join(cityPath, ".gc", "services", "bridge", "secrets", "bot-token.txt")
	writeSecretFile(t, loose, 0o644)
	nested := filepath.Join(cityPath, ".gc", "services", "intake", "secrets", "nested")
	writeSecretFile(t, filepath.Join(nested, "key.pem"), 0o600)
	if err := os.Chmod(nested, 0o750); err != nil {
		t.Fatalf("chmod nested: %v", err)
	}

	check := NewServiceSecretsPermsCheck(serviceSecretsTestConfig(), cityPath)
	r := check.Run(&CheckContext{CityPath: cityPath})
	if r.Status != StatusWarning {
		t.Fatalf("Status = %v, want Warning (message %q)", r.Status, r.Message)
	}
	if r.Severity != SeverityAdvisory {
		t.Fatalf("Severity = %v, want advisory", r.Severity)
	}
	joined := strings.Join(r.Details, "\n")
	if !strings.Contains(joined, loose) {
		t.Fatalf("details missing loose file %s:\n%s", loose, joined)
	}
	if !strings.Contains(joined, nested) {
		t.Fatalf("details missing loose dir %s:\n%s", nested, joined)
	}
	// bridge and bridge-admin share a state root: the shared secrets dir
	// must be walked once, so exactly the two distinct loose entries (the
	// bridge token and the intake nested dir) are reported, not three.
	if len(r.Details) != 2 {
		t.Fatalf("Details count = %d, want 2 (shared bridge/bridge-admin root must be audited once):\n%s", len(r.Details), joined)
	}
	if !strings.Contains(r.Message, "2 group/other-accessible entries") {
		t.Fatalf("Message = %q, want it to report 2 entries", r.Message)
	}
}

func TestServiceSecretsPermsCheckFixTightensPerms(t *testing.T) {
	cityPath := t.TempDir()
	loose := filepath.Join(cityPath, ".gc", "services", "bridge", "secrets", "bot-token.txt")
	writeSecretFile(t, loose, 0o644)
	nested := filepath.Join(cityPath, ".gc", "services", "bridge", "secrets", "nested")
	writeSecretFile(t, filepath.Join(nested, "key.pem"), 0o640)
	if err := os.Chmod(nested, 0o755); err != nil {
		t.Fatalf("chmod nested: %v", err)
	}

	check := NewServiceSecretsPermsCheck(serviceSecretsTestConfig(), cityPath)
	if !check.CanFix() {
		t.Fatal("CanFix = false, want true")
	}
	if r := check.Run(&CheckContext{CityPath: cityPath}); r.Status != StatusWarning {
		t.Fatalf("pre-fix Status = %v, want Warning", r.Status)
	}
	if err := check.Fix(&CheckContext{CityPath: cityPath}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	for path, want := range map[string]os.FileMode{
		loose:                            0o600,
		filepath.Join(nested, "key.pem"): 0o600,
		nested:                           0o700,
	} {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if st.Mode().Perm() != want {
			t.Fatalf("%s mode = %o, want %o", path, st.Mode().Perm(), want)
		}
	}
	if r := check.Run(&CheckContext{CityPath: cityPath}); r.Status != StatusOK {
		t.Fatalf("post-fix Status = %v, want OK (message %q)", r.Status, r.Message)
	}
}

func TestServiceSecretsPermsCheckNilConfig(t *testing.T) {
	check := NewServiceSecretsPermsCheck(nil, t.TempDir())
	if r := check.Run(&CheckContext{}); r.Status != StatusOK {
		t.Fatalf("Status = %v, want OK", r.Status)
	}
}

func TestServiceSecretsPermsCheckWalkErrorStaysAdvisory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: chmod 0000 does not block reads")
	}
	cityPath := t.TempDir()
	// An unreadable subdir surfaces a permission error (not fs.ErrNotExist)
	// during the walk. The audit must report StatusError but keep advisory
	// severity so an infrastructure error in a hygiene check never flips
	// `gc doctor` to a blocking, non-zero exit.
	locked := filepath.Join(cityPath, ".gc", "services", "bridge", "secrets", "locked")
	if err := os.MkdirAll(locked, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", locked, err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatalf("chmod %s: %v", locked, err)
	}
	// Restore perms before t.TempDir cleanup so removal can recurse in.
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })

	check := NewServiceSecretsPermsCheck(serviceSecretsTestConfig(), cityPath)
	r := check.Run(&CheckContext{CityPath: cityPath})
	if r.Status != StatusError {
		t.Fatalf("Status = %v, want Error (message %q)", r.Status, r.Message)
	}
	if r.Severity != SeverityAdvisory {
		t.Fatalf("Severity = %v, want advisory — an audit error must not block doctor", r.Severity)
	}
}

func TestServiceSecretsPermsCheckFollowsSymlinkedSecretsDir(t *testing.T) {
	cityPath := t.TempDir()
	// A real directory holding a loose token, with the service's `secrets`
	// path pointing at it through a symlink. Core follows such a link when
	// it chmods the dir to 0700, so the audit must descend into the target
	// rather than skip the symlinked root.
	realDir := filepath.Join(cityPath, "real-bridge-secrets")
	loose := filepath.Join(realDir, "bot-token.txt")
	writeSecretFile(t, loose, 0o644)

	link := filepath.Join(cityPath, ".gc", "services", "bridge", "secrets")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(link), err)
	}
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatalf("symlink %s -> %s: %v", link, realDir, err)
	}

	check := NewServiceSecretsPermsCheck(serviceSecretsTestConfig(), cityPath)
	r := check.Run(&CheckContext{CityPath: cityPath})
	if r.Status != StatusWarning {
		t.Fatalf("Status = %v, want Warning — symlinked secrets dir must be followed (message %q)", r.Status, r.Message)
	}
	// Findings are reported against the configured `<state_root>/secrets`
	// path, not the resolved symlink target, so an operator can map them back
	// to service config; the symlink resolution stays an implementation detail.
	wantReport := filepath.Join(cityPath, ".gc", "services", "bridge", "secrets", "bot-token.txt")
	joined := strings.Join(r.Details, "\n")
	if !strings.Contains(joined, wantReport) {
		t.Fatalf("details should report the configured secrets path %s:\n%s", wantReport, joined)
	}
	if strings.Contains(joined, "real-bridge-secrets") {
		t.Fatalf("details should not leak the resolved symlink target path:\n%s", joined)
	}

	// --fix must also reach through the link and tighten the target file.
	if err := check.Fix(&CheckContext{CityPath: cityPath}); err != nil {
		t.Fatalf("Fix through symlinked secrets dir: %v", err)
	}
	st, err := os.Stat(loose)
	if err != nil {
		t.Fatalf("stat %s: %v", loose, err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("post-fix %s mode = %o, want 600", loose, st.Mode().Perm())
	}
}

func TestServiceSecretsPermsCheckSkipsOutOfCitySymlinkRoot(t *testing.T) {
	cityPath := t.TempDir()
	// A real secrets tree OUTSIDE the city holding a loose token, reachable
	// only through a symlink at the service's in-city `secrets` path. Because
	// `gc doctor --fix` recursively chmods everything it walks, following such
	// a link would let a stray symlink turn --fix into a recursive chmod of an
	// arbitrary out-of-city tree. The check must instead skip the escaping
	// root: report it, but neither audit its entries nor repair them.
	outside := t.TempDir()
	loose := filepath.Join(outside, "bot-token.txt")
	writeSecretFile(t, loose, 0o644)

	link := filepath.Join(cityPath, ".gc", "services", "bridge", "secrets")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(link), err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("symlink %s -> %s: %v", link, outside, err)
	}

	check := NewServiceSecretsPermsCheck(serviceSecretsTestConfig(), cityPath)
	r := check.Run(&CheckContext{CityPath: cityPath})
	if r.Status != StatusWarning {
		t.Fatalf("Status = %v, want Warning — an escaping symlink root must be surfaced (message %q)", r.Status, r.Message)
	}
	if r.Severity != SeverityAdvisory {
		t.Fatalf("Severity = %v, want advisory", r.Severity)
	}
	joined := strings.Join(r.Details, "\n")
	if !strings.Contains(joined, link) {
		t.Fatalf("details should name the skipped symlinked secrets root %s:\n%s", link, joined)
	}
	// The out-of-city token must never be audited as an in-city loose entry.
	if strings.Contains(joined, "bot-token.txt") {
		t.Fatalf("out-of-city token must not be audited as a loose entry:\n%s", joined)
	}

	// The crux: --fix must leave the out-of-city target's mode untouched.
	if err := check.Fix(&CheckContext{CityPath: cityPath}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	st, err := os.Stat(loose)
	if err != nil {
		t.Fatalf("stat %s: %v", loose, err)
	}
	if st.Mode().Perm() != 0o644 {
		t.Fatalf("out-of-city %s mode = %o, want 644 unchanged — --fix must not chmod outside the city", loose, st.Mode().Perm())
	}
}

func TestServiceSecretsPermsCheckSurfacesUnresolvableSymlinkRoot(t *testing.T) {
	cityPath := t.TempDir()
	// A dangling secrets symlink: the link exists but its target does not, so
	// filepath.EvalSymlinks fails. The check must surface it rather than
	// silently treat the secrets dir as clean, and must not walk or chmod
	// through a link it cannot resolve.
	link := filepath.Join(cityPath, ".gc", "services", "bridge", "secrets")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(link), err)
	}
	missing := filepath.Join(cityPath, "does-not-exist")
	if err := os.Symlink(missing, link); err != nil {
		t.Fatalf("symlink %s -> %s: %v", link, missing, err)
	}

	check := NewServiceSecretsPermsCheck(serviceSecretsTestConfig(), cityPath)
	r := check.Run(&CheckContext{CityPath: cityPath})
	if r.Status != StatusWarning {
		t.Fatalf("Status = %v, want Warning — an unresolvable secrets symlink must be surfaced, not read clean (message %q)", r.Status, r.Message)
	}
	if r.Severity != SeverityAdvisory {
		t.Fatalf("Severity = %v, want advisory", r.Severity)
	}
	if joined := strings.Join(r.Details, "\n"); !strings.Contains(joined, link) {
		t.Fatalf("details should name the unresolved secrets symlink %s:\n%s", link, joined)
	}

	// Fix must be a no-op for an unresolvable root.
	if err := check.Fix(&CheckContext{CityPath: cityPath}); err != nil {
		t.Fatalf("Fix over unresolvable symlink root: %v", err)
	}
}

// assertSkippedOutOfCityRoot runs the check against cfg and asserts the given
// out-of-city secrets tree is surfaced as a skipped root, never audited, and —
// the crux — never chmodded by Fix. configured is the operator-facing secrets
// path the skip note must name; loose is the out-of-city token that must keep
// its 0644 mode.
func assertSkippedOutOfCityRoot(t *testing.T, cfg *config.City, cityPath, configured, loose string) {
	t.Helper()
	check := NewServiceSecretsPermsCheck(cfg, cityPath)
	r := check.Run(&CheckContext{CityPath: cityPath})
	if r.Status != StatusWarning {
		t.Fatalf("Status = %v, want Warning — an out-of-city secrets root must be surfaced (message %q)", r.Status, r.Message)
	}
	if r.Severity != SeverityAdvisory {
		t.Fatalf("Severity = %v, want advisory", r.Severity)
	}
	joined := strings.Join(r.Details, "\n")
	if !strings.Contains(joined, configured) {
		t.Fatalf("details should name the skipped configured secrets root %s:\n%s", configured, joined)
	}
	// The out-of-city token must never be audited as an in-city loose entry.
	if strings.Contains(joined, filepath.Base(loose)) {
		t.Fatalf("out-of-city token %s must not be audited as a loose entry:\n%s", filepath.Base(loose), joined)
	}
	// The crux: --fix must leave the out-of-city target's mode untouched.
	if err := check.Fix(&CheckContext{CityPath: cityPath}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	st, err := os.Stat(loose)
	if err != nil {
		t.Fatalf("stat %s: %v", loose, err)
	}
	if st.Mode().Perm() != 0o644 {
		t.Fatalf("out-of-city %s mode = %o, want 644 unchanged — --fix must not chmod outside the city", loose, st.Mode().Perm())
	}
}

func TestServiceSecretsPermsCheckSkipsAbsoluteOutOfCityStateRoot(t *testing.T) {
	cityPath := t.TempDir()
	// An absolute state_root pointing entirely outside the city, with a real
	// secrets dir and a loose token out there. state_root is not validated at
	// config load (only by the sibling config-valid check), so a city.toml with
	// an absolute out-of-city state_root parses cleanly and reaches this check.
	// The final `secrets` component is a plain directory, not a symlink, so a
	// guard that only resolved symlinks would walk and chmod the out-of-city
	// tree. The walk root itself must be city-confined.
	outside := t.TempDir()
	loose := filepath.Join(outside, "secrets", "bot-token.txt")
	writeSecretFile(t, loose, 0o644)

	cfg := &config.City{Services: []config.Service{
		{Name: "bridge", Kind: "proxy_process", StateRoot: outside},
	}}
	assertSkippedOutOfCityRoot(t, cfg, cityPath, filepath.Join(outside, "secrets"), loose)
}

func TestServiceSecretsPermsCheckSkipsDotDotStateRootEscape(t *testing.T) {
	parent := t.TempDir()
	cityPath := filepath.Join(parent, "city")
	if err := os.MkdirAll(cityPath, 0o700); err != nil {
		t.Fatalf("mkdir city: %v", err)
	}
	// A relative state_root that climbs out of the city with `..`. Joined to the
	// city path it lands in a sibling dir; like the absolute case it is a plain
	// directory, not a symlink, so the walk root must be confined after `..`
	// segments are collapsed against the real filesystem.
	escape := filepath.Join(parent, "escape")
	loose := filepath.Join(escape, "secrets", "bot-token.txt")
	writeSecretFile(t, loose, 0o644)

	cfg := &config.City{Services: []config.Service{
		{Name: "bridge", Kind: "proxy_process", StateRoot: "../escape"},
	}}
	assertSkippedOutOfCityRoot(t, cfg, cityPath, filepath.Join(escape, "secrets"), loose)
}

func TestServiceSecretsPermsCheckSkipsAncestorSymlinkEscape(t *testing.T) {
	cityPath := t.TempDir()
	// The configured secrets path is lexically inside the city, but an ancestor
	// directory (`.gc/services`) is a symlink pointing outside the city. The
	// final `secrets` component is a real directory, not itself a symlink, so a
	// guard that only resolves the final component (os.Lstat on `secrets`) sees
	// a plain dir and misses the escape — letting --fix chmod the out-of-city
	// tree through the ancestor link. Resolving the full walk root catches it.
	outside := t.TempDir()
	loose := filepath.Join(outside, "bridge", "secrets", "bot-token.txt")
	writeSecretFile(t, loose, 0o644)

	servicesLink := filepath.Join(cityPath, ".gc", "services")
	if err := os.MkdirAll(filepath.Dir(servicesLink), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(servicesLink), err)
	}
	if err := os.Symlink(outside, servicesLink); err != nil {
		t.Fatalf("symlink %s -> %s: %v", servicesLink, outside, err)
	}

	// .gc/services -> outside, so .gc/services/bridge/secrets resolves to
	// outside/bridge/secrets even though the configured path looks in-city.
	cfg := &config.City{Services: []config.Service{
		{Name: "bridge", Kind: "proxy_process", StateRoot: ".gc/services/bridge"},
	}}
	configured := filepath.Join(cityPath, ".gc", "services", "bridge", "secrets")
	assertSkippedOutOfCityRoot(t, cfg, cityPath, configured, loose)
}
