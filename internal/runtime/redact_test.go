package runtime

import (
	"strings"
	"testing"
)

// sentinel stands in for a credential. It is synthetic; no real value belongs in
// a test, a log, or a bead.
const sentinel = "sk-test-NOT-A-REAL-CREDENTIAL-8f3a21"

// TestSecretEnvValuesFollowsTheAllowList pins which of a session's env values are
// withheld from error text. The predicate is an allow list, so an unrecognized
// name is assumed to carry credential material; argv-safe values stay legible,
// since they are already readable in /proc/<pid>/cmdline and hiding them costs
// diagnostics for nothing.
func TestSecretEnvValuesFollowsTheAllowList(t *testing.T) {
	got := SecretEnvValues(map[string]string{
		"ANTHROPIC_AUTH_TOKEN": sentinel,
		"SOME_NEW_TOKEN":       "unknown-name-so-assumed-secret",
		"GC_RIG":               "hauler",
		"ANTHROPIC_API_KEY":    "", // withheld variable: no value, nothing to hide
	})
	want := map[string]bool{sentinel: true, "unknown-name-so-assumed-secret": true}
	if len(got) != len(want) {
		t.Fatalf("SecretEnvValues = %q, want the two secret values", got)
	}
	for _, v := range got {
		if !want[v] {
			t.Errorf("SecretEnvValues included %q, which is not a credential", v)
		}
	}
}

// TestProcessSecretEnvValuesSkipsPOSIXNames is the reason [envRedactionInert]
// exists. This predicate runs over the controller's own environment, where every
// one of these is set, and every one of their values is a substring of almost
// any path a failing command prints. Treating them as credentials would rewrite
// an entire error message as <redacted> — turning a leak fix into a diagnostics
// outage.
//
// Every name in the set is pinned, not a sample. Dropping one is a silent
// diagnostics regression, not a build failure, so nothing else would catch it.
func TestProcessSecretEnvValuesSkipsPOSIXNames(t *testing.T) {
	inert := map[string]string{}
	for name := range envRedactionInert {
		// Distinct, above the substitution floor, and shaped like the paths
		// these actually hold.
		value := "/inert/" + strings.ToLower(name) + "/value"
		t.Setenv(name, value)
		inert[value] = name
	}
	if len(inert) != len(envRedactionInert) {
		t.Fatalf("built %d probe values for %d names", len(inert), len(envRedactionInert))
	}
	t.Setenv("ANTHROPIC_AUTH_TOKEN", sentinel)

	got := processSecretEnvValues()
	found := false
	for _, v := range got {
		if name, ok := inert[v]; ok {
			t.Errorf("%s was treated as a credential; its value %q would be cut out of every message", name, v)
		}
		if v == sentinel {
			found = true
		}
	}
	// The control: without this the negatives pass vacuously on an environment
	// where the predicate never fires at all.
	if !found {
		t.Fatal("the process credential was not found, so the negatives prove nothing")
	}
}

// TestRedactionInertNamesAreNotArgvSafe pins the two questions apart. Redaction
// asks "would hiding this destroy the message"; argv safety asks "may this be
// world-readable in /proc/<pid>/cmdline". Answering the first by adding a name
// to [envArgvSafe] silently answers the second too, so a pack that set PATH or
// HOME as session env would start passing it as a process argument.
func TestRedactionInertNamesAreNotArgvSafe(t *testing.T) {
	for name := range envRedactionInert {
		if ArgvSafeEnvKey(name) {
			t.Errorf("%s is redaction-inert AND argv-safe; that widens argv routing, not just redaction", name)
		}
		if !ArgvSecretEnvValue(name, "/some/path") {
			t.Errorf("%s stopped being routed through the private-file path", name)
		}
		if redactableEnvValue(name, "/some/path") {
			t.Errorf("%s is being scrubbed out of rendered text", name)
		}
	}
	// The control: an unrecognized name is secret on both questions, so the
	// assertions above are about this set and not about the predicates being
	// off.
	if !redactableEnvValue("SOME_NEW_TOKEN", "value-long-enough") {
		t.Fatal("an unrecognized name was not treated as a credential")
	}
}

// TestRedactSecretsReplacesLongestFirst pins the ordering. Replacement is
// destructive: hiding a secret that occurs inside a longer one rewrites the
// longer one's rendering, and the longer one then no longer matches itself, so
// it survives in the message minus the overlap — a leak sitting right next to a
// <redacted> marker claiming otherwise. Derived per-worker tokens make the
// overlap ordinary rather than exotic.
//
// The control is the same input in the opposite order: both must produce the
// same output, because the real order comes off a map range and is not a choice
// anyone makes.
func TestRedactSecretsReplacesLongestFirst(t *testing.T) {
	const base = "inst-8f3a21c0"
	const derived = base + "-worker-3"
	text := "herdr rejected GC_INSTANCE_TOKEN=" + derived

	for _, order := range [][]string{{base, derived}, {derived, base}} {
		got := RedactSecrets(text, order)
		if strings.Contains(got, "-worker-3") {
			t.Errorf("RedactSecrets(%q order) leaked the tail of the longer secret: %s", order, got)
		}
		if want := "herdr rejected GC_INSTANCE_TOKEN=" + RedactedValue; got != want {
			t.Errorf("RedactSecrets(%q order) = %q, want %q", order, got, want)
		}
	}
}

// TestRedactSecretsHonorsTheSubstitutionFloor pins the floor as a control-flow
// guard, not a cosmetic one. Substitution is blind to word boundaries, so a
// two-byte value would rewrite the phrase [IsSessionGone] decides by matching,
// silently converting a tolerated missing session into a hard failure.
func TestRedactSecretsHonorsTheSubstitutionFloor(t *testing.T) {
	const text = "agent not found: %12"
	if got := RedactSecrets(text, []string{"no", "1"}); got != text {
		t.Errorf("a sub-floor value rewrote the text control flow reads: %s", got)
	}
	// Just under the floor. Without this the floor is only pinned from one
	// side: any value of 7 or less passes the assertion above, so the constant
	// could drift down to 3 unnoticed by this package.
	if got := RedactSecrets("id=abcdefg", []string{"abcdefg"}); got != "id=abcdefg" {
		t.Errorf("a 7-byte value was substituted, so the floor is below 8: %s", got)
	}
	// The control: the same call with a value at the floor must still redact,
	// so the checks above are about length and not about substitution being off.
	if got := RedactSecrets("token=abcdefgh", []string{"abcdefgh"}); !strings.Contains(got, RedactedValue) {
		t.Errorf("a value at the floor was not redacted: %s", got)
	}
}

// TestRedactSecretsTailRedactsBeforeTruncating pins the order. Redacting a
// tail that has already been cut leaves a credential straddling the cut
// decapitated, and the surviving suffix no longer matches its own secret, so no
// later pass can catch it either — a near-complete token rendered verbatim by
// any command whose output merely runs long.
func TestRedactSecretsTailRedactsBeforeTruncating(t *testing.T) {
	const limit = 40
	const head, tail = "headhead--", "--tailtail"
	text := head + sentinel + tail
	if len(text) <= limit {
		t.Fatalf("the fixture must exceed the limit or nothing truncates (%d)", len(text))
	}

	// The counterfactual, asserted rather than described: cutting first puts
	// the secret's head beyond recovery, and what is left no longer matches the
	// secret, so redacting afterwards cannot catch it.
	cutFirst := RedactSecrets(text[len(text)-limit:], []string{sentinel})
	if !strings.Contains(cutFirst, "CREDENTIAL") {
		t.Fatal("the fixture does not straddle the cut, so the order is untested here")
	}

	got, _ := RedactSecretsTail(text, limit, []string{sentinel})
	if strings.Contains(got, "CREDENTIAL") {
		t.Errorf("the straddling secret survived: %q", got)
	}
	if !strings.Contains(got, RedactedValue) {
		t.Errorf("nothing was redacted at all: %q", got)
	}
	if len(got) > limit {
		t.Errorf("RedactSecretsTail returned %d bytes, want at most %d", len(got), limit)
	}
	// The control: redaction shortened the text below the limit, so no
	// truncation was needed and both ends survive intact. Redacting first does
	// not merely move the cut — it often removes the need for one.
	if !strings.HasPrefix(got, head) || !strings.HasSuffix(got, tail) {
		t.Errorf("RedactSecretsTail shredded text around the secret: %q", got)
	}
}

// TestTrimDecapitatedSecretCutsTheSurvivingTail pins what a bounded writer owes
// once it has discarded output: what it kept can begin with the tail of a
// credential whose head is gone, that tail matches nothing, and redaction alone
// prints it as soon as shrinking the rest pulls the reported window back to it.
func TestTrimDecapitatedSecretCutsTheSurvivingTail(t *testing.T) {
	const after = "\ncurl: (22) 401"
	text := sentinel[10:] + after

	// The counterfactual, asserted rather than described.
	if got := RedactSecrets(text, []string{sentinel}); !strings.Contains(got, "CREDENTIAL") {
		t.Fatal("redaction already removes the fixture's fragment, so the trim is untested here")
	}

	if got := TrimDecapitatedSecret(text, []string{sentinel}); got != after {
		t.Errorf("TrimDecapitatedSecret = %q, want %q", got, after)
	}
	// The controls: the cut is only ever a secret's suffix. Text that begins
	// with none is left whole, and so is the suffix of a value below the
	// substitution floor, which redaction leaves legible anyway; at the floor
	// the same shape is cut, so the second case is about length.
	for _, tc := range []struct {
		text, secret, want string
	}{
		{text: "curl: (22) 401", secret: sentinel, want: "curl: (22) 401"},
		{text: "efg: id", secret: "abcdefg", want: "efg: id"},
		{text: "fgh: id", secret: "abcdefgh", want: ": id"},
	} {
		if got := TrimDecapitatedSecret(tc.text, []string{tc.secret}); got != tc.want {
			t.Errorf("TrimDecapitatedSecret(%q, %q) = %q, want %q", tc.text, tc.secret, got, tc.want)
		}
	}
}

// TestTrimDecapitatedSecretCutsTheLongestTail pins the choice between secrets:
// which value lost its head is unknowable, so the longest suffix any of them
// offers goes, whichever order the secrets arrive in (they come off a map
// range).
func TestTrimDecapitatedSecretCutsTheLongestTail(t *testing.T) {
	const base = "inst-8f3a21c0"
	const derived = base + "-worker-3"
	// Both a two-byte suffix of base and an eleven-byte suffix of derived
	// begin the text.
	const text = "c0-worker-3 rejected"

	for _, order := range [][]string{{base, derived}, {derived, base}} {
		if got, want := TrimDecapitatedSecret(text, order), " rejected"; got != want {
			t.Errorf("TrimDecapitatedSecret(%q order) = %q, want %q", order, got, want)
		}
	}
}

// TestTrimDecapitatedSecretKeepsWholeValuesWhole pins the cut's second half. A
// value whose last bytes repeat its first offers a suffix that each of its
// whole occurrences begins with, so kept bytes that happen to begin on a whole
// occurrence look decapitated too. Cutting that suffix alone would take the
// value's head itself, and redaction would then miss the rest. PEM blocks open
// and close with dashes.
func TestTrimDecapitatedSecretKeepsWholeValuesWhole(t *testing.T) {
	const body = "Tk9ULUEtUkVBTC1LRVktTUFURVJJQUw="
	const pem = "-----BEGIN TEST KEY-----\n" + body + "\n-----END TEST KEY-----"
	const after = "\ncurl: (22) 401"
	text := pem + after

	// The counterfactual: the dashes alone are a matching suffix, and cutting
	// just them leaves the body for redaction to miss.
	if got := RedactSecrets(text[len("-----"):], []string{pem}); !strings.Contains(got, body) {
		t.Fatal("the fixture does not repeat its first bytes at its end, so the extension is untested here")
	}

	got := RedactSecrets(TrimDecapitatedSecret(text, []string{pem}), []string{pem})
	if strings.Contains(got, body) || strings.Contains(got, "BEGIN") {
		t.Errorf("the trim decapitated a whole value, which then survived redaction: %q", got)
	}
	// The control: the text after the value survives, so the assertion above
	// is not passing on a trim that swallowed everything.
	if got != after {
		t.Errorf("TrimDecapitatedSecret then RedactSecrets = %q, want %q", got, after)
	}
}

// TestSetupCommandSecretsCoversBothEnvironments pins that the set spans the
// session env the caller assembled AND this process's own. Every setup-command
// runner builds its child env as os.Environ() plus the overlay, so a command
// echoing either half — `set -x` traces every expansion — must be scrubbed
// against both.
func TestSetupCommandSecretsCoversBothEnvironments(t *testing.T) {
	const inherited = "inherited-NOT-A-REAL-CREDENTIAL"
	t.Setenv("ANTHROPIC_AUTH_TOKEN", inherited)

	got := SetupCommandSecrets(map[string]string{"GH_TOKEN": sentinel})
	var sawSession, sawInherited bool
	for _, v := range got {
		switch v {
		case sentinel:
			sawSession = true
		case inherited:
			sawInherited = true
		}
	}
	if !sawSession {
		t.Error("the session env credential is missing from the secret set")
	}
	if !sawInherited {
		t.Error("the inherited process credential is missing from the secret set")
	}
}
