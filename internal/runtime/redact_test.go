package runtime

import (
	"context"
	"strings"
	"testing"
	"time"
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

// TestCommandOutputTailRetainsEnoughToRedact is the same property through the
// bounded writer, where it is not free: the writer drops bytes as they stream,
// long before anyone knows what the secrets are. Retaining exactly the reported
// limit would put the head of a straddling credential beyond recovery.
func TestCommandOutputTailRetainsEnoughToRedact(t *testing.T) {
	const limit = 4096
	// Place the cut inside the sentinel: the last limit bytes begin partway
	// through it, so only retention beyond limit keeps it whole.
	const lead = 10
	filler := strings.Repeat("f", limit+20-len(sentinel))
	tail := newCommandOutputTail(limit)
	if _, err := tail.Write([]byte(strings.Repeat("s", lead) + sentinel + filler)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got := tail.Detail("stderr", []string{sentinel})
	if strings.Contains(got, "CREDENTIAL") {
		t.Errorf("a credential straddling the truncation boundary leaked: %q", got)
	}
	if !strings.Contains(got, RedactedValue) {
		t.Fatalf("the secret was not in the retained buffer at all: %q", got)
	}
	// The controls. The detail must still be bounded and still be marked as a
	// partial tail, or this passes on a writer that simply kept everything.
	if len(got) > limit+len("stderr: ... ") {
		t.Errorf("Detail returned %d bytes, so the limit is not being applied", len(got))
	}
	if !strings.HasPrefix(got, "stderr: ... ") {
		t.Errorf("Detail did not mark the output as truncated: %q", got[:min(40, len(got))])
	}
}

// TestCommandOutputTailRetainsAJWTLengthCredential pins the slack against the
// population it actually has to cover, not against the tokens that inspired it.
//
// The retained window is the whole guarantee: a value whose head falls outside
// it is gone before redaction runs, and its tail then survives into the reported
// text matching nothing. So the slack is an upper bound on credential length,
// and every unrecognized env value is a credential here by construction —
// service-account JWTs and PEM keys included, which run kilobytes, not the sub-
// 100 bytes an API key does. A fixture inside the old 1024 would have pinned
// only the comfortable half of that range.
func TestCommandOutputTailRetainsAJWTLengthCredential(t *testing.T) {
	const limit = 256
	// A service-account JWT's shape and order of magnitude (~1.5KB): the part
	// that leaks when the head is dropped is the signature at the end.
	const signature = ".SIGNATURE-NOT-A-REAL-CREDENTIAL"
	jwt := "eyJhbGciOiJSUzI1NiJ9." + strings.Repeat("QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVph", 40) + signature
	if len(jwt) <= 1024 {
		t.Fatalf("fixture is %d bytes, which the previous slack already covered", len(jwt))
	}
	// Lead the secret with enough filler that its head lands outside a
	// limit+1024 window but inside a limit+8192 one, and trail it with a marker
	// that must survive.
	tail := newCommandOutputTail(limit)
	if _, err := tail.Write([]byte(strings.Repeat("f", 4000) + jwt + "curl: (22) 401")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got := tail.Detail("stderr", []string{jwt})
	if strings.Contains(got, signature) {
		t.Errorf("the tail of a JWT-length credential survived the reported window: %q", got)
	}
	if !strings.Contains(got, RedactedValue) {
		t.Fatalf("the secret was not retained whole, so nothing was redacted: %q", got)
	}
	// The control: over-redaction that shredded the diagnostic would pass the
	// assertions above.
	if !strings.Contains(got, "curl: (22) 401") {
		t.Errorf("the surrounding diagnostic did not survive: %q", got)
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

// TestRunSetupCommandFailureOmitsCredentials is the end-to-end shape. This
// runner is the extracted core both host-side providers are slated to delegate
// to, so a leak here would reappear in both the moment they do — which is
// exactly the kind of regression a fix applied only to the live copies invites.
func TestRunSetupCommandFailureOmitsCredentials(t *testing.T) {
	const inherited = "inherited-NOT-A-REAL-CREDENTIAL"
	t.Setenv("ANTHROPIC_AUTH_TOKEN", inherited)

	err := RunSetupCommand(context.Background(),
		`echo "session=$GH_TOKEN" >&2; echo "inherited=$ANTHROPIC_AUTH_TOKEN"; exit 3`,
		map[string]string{"GH_TOKEN": sentinel}, 10*time.Second)
	if err == nil {
		t.Fatal("a setup command exiting 3 must fail")
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Errorf("the session env credential reached the failure: %v", err)
	}
	if strings.Contains(err.Error(), inherited) {
		t.Errorf("the inherited credential reached the failure: %v", err)
	}
	// The controls. Without these the assertions above pass on any error that
	// simply never captured the output — including one where the command
	// never ran.
	if !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("the command did not run as expected: %v", err)
	}
	for _, want := range []string{"stderr: session=" + RedactedValue, "stdout: inherited=" + RedactedValue} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("failure detail missing %q, so nothing here was scrubbed: %v", want, err)
		}
	}
}
