package runtime

import (
	"os"
	"sort"
	"strings"
)

// RedactedValue replaces a credential in rendered text.
const RedactedValue = "<redacted>"

// substitutionFloor is the shortest value [RedactSecrets] will substitute.
// Substitution is blind to word boundaries, so hiding a short value mangles
// unrelated text: with "no" a secret, herdr's "agent not found" becomes
// "agent <redacted>t found", and both the herdr client's isAgentNotFound and
// [IsSessionGone] decide by matching that phrase — a redactor silently taking
// over branches that turn a tolerated missing session into a hard failure.
//
// The floor buys that back because the two populations barely overlap: the
// credentials that reach a session are long (the instance token is 32 hex
// characters, an Anthropic key and a GitHub token both run past 30), while the
// values that shred text are short function words. 8 is the low end of that gap
// — long enough that no English word or shell token in an error message is
// swallowed whole, short enough to still cover anything a minting routine
// produces. It is not a proof: a user-configured secret below the floor stays
// legible in text this process did not write, and an inert value above it can
// still collide with a matched phrase. Both are recorded in the callers.
const substitutionFloor = 8

// envRedactionInert names variables whose values must stay legible in rendered
// text even though they are not argv-safe.
//
// The two questions are not the same one. [envArgvSafe] answers "may this value
// travel in a world-readable argv", where the conservative answer for an
// unknown name is no and the cost of a false positive is a temp file. Redaction
// asks "would hiding this value destroy the message", where the cost of a false
// positive is the diagnostic itself. These names are the standard POSIX process
// environment: every one is set on the controller, every one holds a substring
// of almost any path a failing command prints, and none of them authenticates
// anything. Scrubbing against them turns a pre_start failure into a wall of
// <redacted> — a leak fix becoming a diagnostics outage.
//
// They are deliberately NOT in [envArgvSafe]. Listing them there would also
// widen argv routing, so a pack that set PATH or HOME as session env would
// start passing it as a process argument — a silent behavior change on a
// question this file exists to answer conservatively.
var envRedactionInert = map[string]bool{
	"HOME":    true,
	"LOGNAME": true,
	"OLDPWD":  true,
	"PATH":    true,
	"PWD":     true,
	"SHELL":   true,
	"SHLVL":   true,
	"TMPDIR":  true,
	"USER":    true,
}

// redactableEnvValue reports whether this key/value pair should be scrubbed out
// of rendered text. Argv-safe values stay legible because they are already
// readable in /proc/<pid>/cmdline, so hiding them costs diagnostics and buys
// nothing; [envRedactionInert] names stay legible because hiding them costs the
// whole message. Everything else is assumed to carry credential material — the
// underlying predicate is an allow list, so an unrecognized name is a secret.
func redactableEnvValue(key, value string) bool {
	return ArgvSecretEnvValue(key, value) && !envRedactionInert[key]
}

// SecretEnvValues returns the values of env that must not appear in rendered
// text. See [redactableEnvValue] for which values those are.
func SecretEnvValues(env map[string]string) []string {
	secrets := make([]string, 0, len(env))
	for k, v := range env {
		if redactableEnvValue(k, v) {
			secrets = append(secrets, v)
		}
	}
	return secrets
}

// processSecretEnvValues returns the same for this process's own environment.
//
// A child that inherits os.Environ() can echo any of it back: a `set -x` trace
// prints every expansion, and a failing curl prints the header it sent. The
// controller's environment is where the fleet's credentials live, so a command
// spawned with it needs its output scrubbed against that set too, not only
// against the session env the caller assembled.
func processSecretEnvValues() []string {
	environ := os.Environ()
	secrets := make([]string, 0, len(environ))
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if ok && redactableEnvValue(k, v) {
			secrets = append(secrets, v)
		}
	}
	return secrets
}

// SetupCommandSecrets is the value set to scrub from a session lifecycle
// command's captured output when the command runs HOST-SIDE. Those runners
// build the child environment the same way — os.Environ() overlaid with the
// session env the caller assembled — so both halves are in scope for anything
// the command echoes, and both belong here. Shared so the three host-side
// runners (this package's [RunSetupCommand], the tmux adapter's and herdr's)
// cannot drift apart on which half they cover.
//
// Not for a runner that executes the command somewhere else. The ssh provider
// runs pre_start on the far box, which inherits that box's environment and not
// this process's, so only [SecretEnvValues] of the session env applies there;
// adding our own would scrub the controller's credentials out of text that
// never had a way to contain them.
func SetupCommandSecrets(env map[string]string) []string {
	return append(SecretEnvValues(env), processSecretEnvValues()...)
}

// RedactSecrets replaces every one of secrets with [RedactedValue], so one pass
// covers an argv we render, the stderr we append and a message a subprocess
// wrote.
//
// Longest first, because replacement is destructive: a secret that occurs
// inside a longer one rewrites the longer one's rendering, and the longer one
// then no longer matches itself, so it survives in the text minus the overlap —
// a partial credential printed beside a <redacted> marker claiming otherwise.
// Callers assemble secrets by ranging a map, so left in arrival order that leak
// would be intermittent. (This orders containment, which is the reachable case;
// two secrets that overlap without either containing the other would still shed
// the tail of whichever loses.)
//
// Values below [substitutionFloor] are skipped.
func RedactSecrets(text string, secrets []string) string {
	ordered := make([]string, 0, len(secrets))
	for _, secret := range secrets {
		if len(secret) >= substitutionFloor {
			ordered = append(ordered, secret)
		}
	}
	sort.SliceStable(ordered, func(i, j int) bool { return len(ordered[i]) > len(ordered[j]) })

	for _, secret := range ordered {
		text = strings.ReplaceAll(text, secret, RedactedValue)
	}
	return text
}

// tailRedactionSlack is how much beyond its reported limit a bounded output
// tail must retain, and therefore the longest credential this file can promise
// to scrub out of one: a value whose head falls outside the retained window is
// gone before [RedactSecretsTail] ever sees it, and its tail then survives into
// the reported text unmatched.
//
// So the number is a claim about the population, not a comfort margin. API keys
// and bearer tokens run well under 100 bytes, but this package's whole posture
// is that an unrecognized variable holds credential material, and that
// population is much larger: Kubernetes service-account and OIDC JWTs reach
// 850-1200 bytes routinely, a base64 service-account blob more, an RSA-4096 PEM
// about 3.2KB. 1024 covered the tokens and quietly failed the rest. 8192 clears
// every shape actually used to authenticate; the buffer is transient and one
// per setup command, so the memory is not worth trading against a partial
// credential in a durable error.
//
// Anything longer still straddles. [OutputTailRetention]'s callers hold whole
// values, not lengths, so bounding this exactly would mean plumbing a
// max-secret-length hint down from each caller — tracked as ga-y2m41 rather
// than guessed at here.
const tailRedactionSlack = 8192

// OutputTailRetention returns how many bytes a bounded output writer must keep
// in order to report limit bytes safely. Retaining exactly limit is not enough:
// see [RedactSecretsTail] for why the extra is load-bearing.
func OutputTailRetention(limit int) int {
	if limit <= 0 {
		return limit
	}
	return limit + tailRedactionSlack
}

// RedactSecretsTail scrubs secrets from text and then bounds it to the last
// limit bytes, reporting whether that trim dropped anything.
//
// The order is the point. Substitution matches whole values, so redacting after
// a cut leaves any credential straddling it decapitated — and the surviving
// suffix no longer matches its own secret, so nothing downstream can catch it.
// That renders a near-complete token verbatim, reachable from any command whose
// output merely runs past the limit. Redacting the whole buffer first costs
// nothing, because the buffer is already in hand.
//
// It is why a caller retaining a tail must keep [OutputTailRetention] bytes
// rather than limit: a value can only be redacted here if the buffer still
// holds all of it, and the slack is what guarantees that for every value whose
// end lands inside the window actually reported.
func RedactSecretsTail(text string, limit int, secrets []string) (string, bool) {
	text = RedactSecrets(text, secrets)
	if limit > 0 && len(text) > limit {
		return text[len(text)-limit:], true
	}
	return text, false
}
