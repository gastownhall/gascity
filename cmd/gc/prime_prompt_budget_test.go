package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/shellquote"
)

// This file covers ga-q8wgom.1.2: gc prime --strict reports a prompt-delivery
// budget decision by reusing (not duplicating) ga-q8wgom.1.1's promptDelivery
// / promptDeliverySupportFor logic. The ten scenarios below are the bead's
// named test list verbatim. All of them reference the not-yet-existing
// promptBudgetJSON type and primeJSONResult.PromptBudget field, so the whole
// package fails to compile until GREEN adds them -- that package-wide
// compile failure is the RED signal for every scenario here, not just
// TestPrimePromptBudgetTypedJSONFields.

// writePromptBudgetCity writes a minimal city.toml plus a prompt template
// file, chdirs into it, and points GC_CITY_PATH at it -- mirroring
// TestDoPrimeStrictKnownAgent's proven fixture shape (main_test.go).
func writePromptBudgetCity(t *testing.T, toml, promptContent string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	fullPromptPath := filepath.Join(dir, "prompts/worker.md")
	if err := os.MkdirAll(filepath.Dir(fullPromptPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fullPromptPath, []byte(promptContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_CITY_PATH", dir)
}

const promptBudgetBareTOML = `[workspace]
name = "test-city"

[[agent]]
name = "worker"
session = "tmux"
prompt_template = "prompts/worker.md"
`

// 1. below-threshold argv delivery.
func TestPrimePromptBudgetBelowThresholdArgvDelivery(t *testing.T) {
	const prompt = "small prompt content for scenario one"
	writePromptBudgetCity(t, promptBudgetBareTOML, prompt)

	var stdout, stderr bytes.Buffer
	code := doPrimeWithMode([]string{"worker"}, &stdout, &stderr, false, true)
	if code != 0 {
		t.Fatalf("doPrimeWithMode(strict) = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), prompt) {
		t.Errorf("stdout = %q, want to contain rendered prompt %q", stdout.String(), prompt)
	}
	quoted := shellquote.Quote(prompt)
	wantSubstrings := []string{
		"raw_bytes=" + strconv.Itoa(len(prompt)),
		"raw_limit=100000",
		"argv_bytes=" + strconv.Itoa(len(quoted)),
		"argv_limit=128000",
		"configured_mode=arg",
		"effective_mode=argv",
		"runtime=tmux",
		"oversized_fallback=false",
		"hard_fail=false",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr = %q, want substring %q", stderr.String(), want)
		}
	}
}

// 2. exact-threshold fallback: raw bytes AT maxPromptSuffixRawBytes trips the
// guard even though this is well below the byte count anyone would call
// "huge" — the guard is a hard threshold, not a heuristic.
func TestPrimePromptBudgetExactThresholdFallback(t *testing.T) {
	prompt := repeatToBytes("a", maxPromptSuffixRawBytes)
	writePromptBudgetCity(t, promptBudgetBareTOML, prompt)

	var stdout, stderr bytes.Buffer
	code := doPrimeWithMode([]string{"worker"}, &stdout, &stderr, false, true)
	if code != 0 {
		t.Fatalf("doPrimeWithMode(strict) = %d, want 0; stderr=%s", code, stderr.String())
	}
	if stdout.Len() != len(prompt) {
		t.Errorf("stdout len = %d, want %d (rendered prompt must still reach stdout on a nudge-fallback)", stdout.Len(), len(prompt))
	}
	wantSubstrings := []string{
		"raw_bytes=" + strconv.Itoa(maxPromptSuffixRawBytes),
		"raw_limit=100000",
		"argv_limit=128000",
		"configured_mode=arg",
		"effective_mode=nudge-fallback",
		"runtime=tmux",
		"oversized_fallback=true",
		"hard_fail=false",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr, want substring %q (stderr len=%d)", want, stderr.Len())
		}
	}
}

// 3. ACP oversized success: ACP is never argv-bound, so an oversized prompt
// still delivers cleanly with no fallback and no hard-fail.
func TestPrimePromptBudgetACPOversizedSuccess(t *testing.T) {
	const toml = `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
provider = "stub"
session = "acp"
prompt_template = "prompts/worker.md"

[providers.stub]
command = "/bin/echo"
supports_acp = true
acp_command = "/bin/echo"
acp_args = ["acp"]
`
	prompt := repeatToBytes("a", maxPromptSuffixRawBytes*2)
	writePromptBudgetCity(t, toml, prompt)

	var stdout, stderr bytes.Buffer
	code := doPrimeWithMode([]string{"worker"}, &stdout, &stderr, false, true)
	if code != 0 {
		t.Fatalf("doPrimeWithMode(strict) = %d, want 0; stderr=%s", code, stderr.String())
	}
	wantSubstrings := []string{
		"raw_bytes=" + strconv.Itoa(maxPromptSuffixRawBytes*2),
		"configured_mode=acp",
		"effective_mode=nudge",
		"runtime=acp",
		"oversized_fallback=false",
		"hard_fail=false",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr, want substring %q (stderr len=%d)", want, stderr.Len())
		}
	}
}

// 4. subprocess oversized failure: subprocess has no confirmed post-start
// delivery path, so an oversized prompt must hard-fail before any prompt
// content reaches stdout.
func TestPrimePromptBudgetSubprocessOversizedFailure(t *testing.T) {
	const marker = "HARDFAILMARKERMUSTNOTLEAK"
	prompt := marker + repeatToBytes("a", maxPromptSuffixRawBytes)
	const toml = `[workspace]
name = "test-city"

[[agent]]
name = "worker"
session = "subprocess"
prompt_template = "prompts/worker.md"
`
	writePromptBudgetCity(t, toml, prompt)

	var stdout, stderr bytes.Buffer
	code := doPrimeWithMode([]string{"worker"}, &stdout, &stderr, false, true)
	if code == 0 {
		t.Fatalf("doPrimeWithMode(strict) = 0, want non-zero for an oversized prompt on an unsupported runtime; stderr=%s", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout must stay empty on a strict hard-fail (no launch is safe), got len=%d", stdout.Len())
	}
	if strings.Contains(stderr.String(), marker) {
		t.Errorf("stderr leaks prompt content: %q", stderr.String())
	}
	for _, want := range []string{"subprocess", "100000", "128000", "hard_fail=true"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr = %q, want actionable substring %q", stderr.String(), want)
		}
	}
}

// 5. configured-none success: prompt_mode="none" routes to the nudge
// unconditionally, just like ACP, regardless of prompt size.
func TestPrimePromptBudgetConfiguredNoneSuccess(t *testing.T) {
	const prompt = "small prompt content for scenario five"
	const toml = `[workspace]
name = "test-city"

[[agent]]
name = "worker"
provider = "stub"
session = "tmux"
prompt_template = "prompts/worker.md"

[providers.stub]
command = "/bin/echo"
prompt_mode = "none"
`
	writePromptBudgetCity(t, toml, prompt)

	var stdout, stderr bytes.Buffer
	code := doPrimeWithMode([]string{"worker"}, &stdout, &stderr, false, true)
	if code != 0 {
		t.Fatalf("doPrimeWithMode(strict) = %d, want 0; stderr=%s", code, stderr.String())
	}
	wantSubstrings := []string{
		"raw_bytes=" + strconv.Itoa(len(prompt)),
		"configured_mode=none",
		"effective_mode=nudge",
		"runtime=tmux",
		"oversized_fallback=false",
		"hard_fail=false",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr = %q, want substring %q", stderr.String(), want)
		}
	}
}

// 6. quote-inflated argv bytes: raw bytes stay under the raw threshold, but
// every embedded quote inflates the shellquote.Quote-encoded length past the
// argv threshold -- the guard must trip on the quoted count, not just raw.
func TestPrimePromptBudgetQuoteInflatedArgvBytes(t *testing.T) {
	prompt := repeatToBytes("'", maxPromptSuffixRawBytes/2)
	quoted := shellquote.Quote(prompt)
	if len(prompt) >= maxPromptSuffixRawBytes {
		t.Fatalf("fixture invalid: raw len %d already at/above raw threshold", len(prompt))
	}
	if len(quoted) < maxPromptSuffixQuotedBytes {
		t.Fatalf("fixture invalid: quoted len %d below quoted threshold", len(quoted))
	}
	writePromptBudgetCity(t, promptBudgetBareTOML, prompt)

	var stdout, stderr bytes.Buffer
	code := doPrimeWithMode([]string{"worker"}, &stdout, &stderr, false, true)
	if code != 0 {
		t.Fatalf("doPrimeWithMode(strict) = %d, want 0; stderr=%s", code, stderr.String())
	}
	wantSubstrings := []string{
		"raw_bytes=" + strconv.Itoa(len(prompt)),
		"argv_bytes=" + strconv.Itoa(len(quoted)),
		"configured_mode=arg",
		"effective_mode=nudge-fallback",
		"oversized_fallback=true",
		"hard_fail=false",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr, want substring %q (stderr len=%d)", want, stderr.Len())
		}
	}
}

// 7. Unicode byte counts: 25001 repeats of a 4-byte rune is 100004 bytes but
// only 25001 runes. A rune-counting implementation would wrongly treat this
// as small; the reported raw_bytes must reflect the byte length.
func TestPrimePromptBudgetUnicodeByteCounts(t *testing.T) {
	prompt := strings.Repeat("\U0001F389", 25001)
	if len(prompt) < maxPromptSuffixRawBytes {
		t.Fatalf("fixture invalid: expected >= %d raw bytes, got %d", maxPromptSuffixRawBytes, len(prompt))
	}
	writePromptBudgetCity(t, promptBudgetBareTOML, prompt)

	var stdout, stderr bytes.Buffer
	code := doPrimeWithMode([]string{"worker"}, &stdout, &stderr, false, true)
	if code != 0 {
		t.Fatalf("doPrimeWithMode(strict) = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "raw_bytes="+strconv.Itoa(len(prompt))) {
		t.Errorf("stderr must report raw_bytes=%d (byte length)", len(prompt))
	}
	if strings.Contains(stderr.String(), "raw_bytes=25001") {
		t.Errorf("stderr reports raw_bytes=25001 (rune count), want byte count %d", len(prompt))
	}
	if !strings.Contains(stderr.String(), "effective_mode=nudge-fallback") {
		t.Errorf("stderr, want effective_mode=nudge-fallback for a byte-oversized UTF-8 prompt")
	}
}

// 8. stderr-vs-stdout separation: the budget diagnostic must land only on
// stderr; stdout stays exactly the rendered prompt with no diagnostic text
// mixed in.
func TestPrimePromptBudgetStderrVsStdoutSeparation(t *testing.T) {
	const prompt = "small prompt content for scenario eight"
	writePromptBudgetCity(t, promptBudgetBareTOML, prompt)

	var stdout, stderr bytes.Buffer
	code := doPrimeWithMode([]string{"worker"}, &stdout, &stderr, false, true)
	if code != 0 {
		t.Fatalf("doPrimeWithMode(strict) = %d, want 0; stderr=%s", code, stderr.String())
	}
	if stdout.String() != prompt {
		t.Errorf("stdout = %q, want exactly the rendered prompt %q (no diagnostic text mixed in)", stdout.String(), prompt)
	}
	for _, leaked := range []string{"raw_bytes=", "argv_bytes=", "configured_mode=", "effective_mode="} {
		if strings.Contains(stdout.String(), leaked) {
			t.Errorf("stdout leaks strict diagnostic field %q, want it only on stderr", leaked)
		}
	}
	if !strings.Contains(stderr.String(), "raw_bytes=") {
		t.Errorf("stderr must carry the budget diagnostic, got %q", stderr.String())
	}
}

// 9. typed JSON fields: gc prime --json --strict must expose the budget as
// typed fields on primeJSONResult, not just formatted stderr text.
func TestPrimePromptBudgetTypedJSONFields(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	const prompt = "small prompt content for scenario nine"
	writePromptBudgetCity(t, promptBudgetBareTOML, prompt)

	var stdout, stderr bytes.Buffer
	cmd := newPrimeCmd(&stdout, &stderr)
	cmd.SetOut(&stderr)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--json", "--strict", "worker"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("gc prime --json --strict = %v; stderr=%q", err, stderr.String())
	}

	var got primeJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("--json output is not JSON: %v; stdout=%q", err, stdout.String())
	}
	if got.PromptBudget == nil {
		t.Fatalf("primeJSONResult.PromptBudget = nil, want a populated budget")
	}
	quoted := shellquote.Quote(prompt)
	want := promptBudgetJSON{
		RawBytes:          len(prompt),
		RawLimit:          maxPromptSuffixRawBytes,
		ArgvBytes:         len(quoted),
		ArgvLimit:         maxPromptSuffixQuotedBytes,
		ConfiguredMode:    "arg",
		EffectiveMode:     "argv",
		Runtime:           "tmux",
		OversizedFallback: false,
		HardFail:          false,
	}
	if *got.PromptBudget != want {
		t.Errorf("PromptBudget = %+v, want %+v", *got.PromptBudget, want)
	}
}

// 10. unchanged non-strict output: turning strict mode off must leave the
// existing prime contract untouched -- no budget diagnostic anywhere.
func TestPrimePromptBudgetUnchangedNonStrictOutput(t *testing.T) {
	const prompt = "small prompt content for scenario ten"
	writePromptBudgetCity(t, promptBudgetBareTOML, prompt)

	var stdout, stderr bytes.Buffer
	code := doPrimeWithMode([]string{"worker"}, &stdout, &stderr, false, false)
	if code != 0 {
		t.Fatalf("doPrimeWithMode(non-strict) = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), prompt) {
		t.Errorf("stdout = %q, want to contain rendered prompt %q", stdout.String(), prompt)
	}
	for _, absent := range []string{"raw_bytes=", "argv_bytes=", "configured_mode=", "effective_mode=", "hard_fail="} {
		if strings.Contains(stderr.String(), absent) {
			t.Errorf("stderr = %q, non-strict mode must not emit budget field %q", stderr.String(), absent)
		}
	}
}
