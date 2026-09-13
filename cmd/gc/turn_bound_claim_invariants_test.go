package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// claimCASAllowedFiles is the closed set of non-test cmd/gc files that may
// perform a claim compare-and-swap.
//
// The point of the set is that it is CLOSED, not that these four are special.
// Pull semantics are absolute — the controller never assigns, workers discover
// and claim — and the way that invariant dies is a well-meaning controller-side
// "pre-stamp the assignee so the worker finds it faster" landing in the
// reconciler or the order dispatcher. Every entry below is a worker-side pull:
//
//   - cmd_hook_claim.go / claim_class_route.go: the fenced hook claim path
//     (F-A/F-B/F-C run here, so a claim added here inherits the turn binding).
//   - cmd_bd_by_id.go: the `gc bd update <id> --claim` verb an agent runs for
//     itself, in its own turn.
//   - cmd_agent_script.go: the deterministic non-LLM executor, which claims and
//     then immediately executes the work in the same process — it has no turn to
//     outlive, which is why the hook fences do not apply to it.
//
// One entry is not a pull path and is here for a different reason:
//
//   - class_store_emit.go FORWARDS a claim; it cannot originate one. It is the
//     relocated class store's emission wrapper, and it holds no id, no assignee
//     and no policy — its Claim exists only so the capability survives wrapping,
//     because claim_class_route.go's binding probe is a type assertion and a
//     wrapper without the method degrades `gc hook --claim` to "this binding
//     cannot claim" on every split city. The invariant this guard protects —
//     the controller never assigns — is untouched: nothing here decides to
//     claim, and the callers that do are still exactly the four above.
//
// Adding a file here is a design decision about pull semantics; make it
// deliberately.
var claimCASAllowedFiles = map[string]bool{
	"cmd_hook_claim.go":    true,
	"claim_class_route.go": true,
	"cmd_bd_by_id.go":      true,
	"cmd_agent_script.go":  true,
	"class_store_emit.go":  true,
}

// claimCASMarkers are the two shapes a claim compare-and-swap takes in this
// package: the store contract's Claim, and the raw bd argv.
var claimCASMarkers = []string{".Claim(", `"--claim"`}

// TestClaimCASStaysOnTheWorkerPullPath pins invariant 4: no non-test code path
// outside the sanctioned worker-side pull files performs a claim CAS.
//
// It is a source-level guard for the same reason
// TestGCNonTestFilesStayOnWorkerBoundary is: the property is about which files
// may contain a call at all, and no runtime assertion can observe a call that a
// future commit has not written yet.
func TestClaimCASStaysOnTheWorkerPullPath(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading cmd/gc: %v", err)
	}
	var violations []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if claimCASAllowedFiles[name] {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		for _, marker := range claimCASMarkers {
			if strings.Contains(string(raw), marker) {
				violations = append(violations, name+" contains "+marker)
			}
		}
	}
	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf("claim CAS outside the worker pull path (pull semantics are absolute: the controller never assigns):\n  %s",
			strings.Join(violations, "\n  "))
	}
}

// renderedHookConfigRoots are the trees holding every hook command gc renders
// into a managed session's provider configuration.
var renderedHookConfigRoots = []string{
	"../../internal/hooks/config",
	"../../internal/bootstrap/packs/core/overlay/per-provider",
}

// TestRenderedHookCommandsReachGcHookOnlyThroughHookRun pins invariant 5: every
// managed rendered hook command that can reach the `gc hook` data plane goes
// through `gc hook run`.
//
// `gc hook run` is what exports GC_HOOK_CALLBACK_LANE, so it is the seam that
// makes F-A cover a provider callback at all. A new provider overlay that wires
// `gc hook ...` directly would silently re-open the hole this round closes —
// and it would look completely reasonable in review, because the rendered
// command is claim-free today. The guard is on the SHAPE, not on the argv.
func TestRenderedHookCommandsReachGcHookOnlyThroughHookRun(t *testing.T) {
	commands := renderedHookCommands(t)
	if len(commands) == 0 {
		t.Fatal("found no rendered hook commands; this gate has lost its subject")
	}
	for _, cmd := range commands {
		// `gc prime --hook` and `gc handoff --hook-format` are not the hook data
		// plane; only the `gc hook` verb itself is.
		for _, occurrence := range gcHookOccurrences(cmd.command) {
			if !strings.HasPrefix(occurrence, "gc hook run") {
				t.Errorf("%s renders %q, which reaches gc hook WITHOUT the managed `gc hook run` wrapper, so its child carries no callback-lane marker:\n  %s",
					cmd.file, occurrence, cmd.command)
			}
		}
		if strings.Contains(cmd.command, "--claim") {
			t.Errorf("%s renders a hook command that claims: %s", cmd.file, cmd.command)
		}
	}
}

// shippedPromptRoot holds the worker prompts the core pack renders into a
// session's startup context.
const shippedPromptRoot = "../../internal/bootstrap/packs/core/assets/prompts"

// TestShippedPromptsDoNotLoopTheClaimInsideOneToolCall pins the prompt-side half
// of turn-bound claims.
//
// A shipped prompt that tells a worker to run `gc hook --claim` inside a
// `sleep`/retry loop stretches ONE tool call across the whole loop, which is
// precisely the shape that outlives a provider's tool budget: the call is killed
// or abandoned, the claim command survives, and it can win a claim no turn is
// left to execute. F-B now refuses such a claim, so the loop cannot even do what
// it was written to do — but a prompt that still instructs it wastes the
// worker's budget and teaches the wrong protocol. Re-checking belongs BETWEEN
// tool calls.
func TestShippedPromptsDoNotLoopTheClaimInsideOneToolCall(t *testing.T) {
	entries, err := os.ReadDir(shippedPromptRoot)
	if err != nil {
		t.Fatalf("reading %s: %v", shippedPromptRoot, err)
	}
	checked := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		path := filepath.Join(shippedPromptRoot, entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		checked++
		for _, block := range shellBlocksMentioningClaim(string(raw)) {
			if strings.Contains(block, "sleep") {
				t.Errorf("%s ships a claim inside a sleep loop; a claim must not outlive its tool call:\n%s", path, block)
			}
		}
	}
	if checked == 0 {
		t.Fatal("found no shipped prompts; this gate has lost its subject")
	}
}

// shellBlocksMentioningClaim returns each fenced shell block in a prompt that
// invokes the claim.
func shellBlocksMentioningClaim(prompt string) []string {
	var out []string
	parts := strings.Split(prompt, "```")
	// Odd indices are the fenced bodies (with their info string on the first line).
	for i := 1; i < len(parts); i += 2 {
		if strings.Contains(parts[i], "hook --claim") {
			out = append(out, parts[i])
		}
	}
	return out
}

// TestNoShippedPromptAcquiresWorkThroughTheByIDClaimDoor keeps the `gc bd
// update --claim` door from silently becoming a second, UNFENCED way to acquire
// routed work.
//
// The door is genuinely worker-pull, so it is allowlisted for the claim CAS —
// but it carries the identical orphaned-tool-call exposure as `gc hook --claim`
// and has none of F-A/F-B/F-C. It is not fenced today because nothing needs it
// to be: no shipped prompt or template tells a worker to acquire work with it
// (the `gc agent script` deterministic executor claims through the raw `bd`
// binary, not this door, and executes in-process immediately afterwards).
//
// That "nothing needs it" is the entire justification, and it is exactly the
// kind of fact that quietly stops being true. If a prompt ever routes
// acquisition through this door, this test fails and the choice becomes
// explicit: fence the door, or don't ship the prompt. Releasing or updating a
// bead the worker already holds is unaffected — only acquisition is pinned.
func TestNoShippedPromptAcquiresWorkThroughTheByIDClaimDoor(t *testing.T) {
	entries, err := os.ReadDir(shippedPromptRoot)
	if err != nil {
		t.Fatalf("reading %s: %v", shippedPromptRoot, err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		path := filepath.Join(shippedPromptRoot, entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if !strings.Contains(line, "--claim") {
				continue
			}
			if strings.Contains(line, "bd update") || strings.Contains(line, "bd  update") {
				t.Errorf("%s tells a worker to acquire work through the unfenced by-id claim door, which has the same orphaned-tool-call exposure `gc hook --claim` was just fenced against:\n  %s",
					path, strings.TrimSpace(line))
			}
		}
	}
}

type renderedHookCommand struct {
	file    string
	command string
}

// renderedHookCommands collects every "command" string in the rendered hook
// configuration trees. It reads the raw JSON text rather than decoding each
// provider's distinct schema: the providers disagree about structure and agree
// only that a hook is a shell string under a "command" key, and a decoder per
// provider would be one more thing to forget to extend.
func renderedHookCommands(t *testing.T) []renderedHookCommand {
	t.Helper()
	var out []renderedHookCommand
	for _, root := range renderedHookConfigRoots {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".json") {
				return err
			}
			raw, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			for _, command := range jsonCommandStrings(string(raw)) {
				out = append(out, renderedHookCommand{file: path, command: command})
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", root, err)
		}
	}
	return out
}

// jsonCommandStrings pulls the value of every `"command": "..."` pair out of raw
// JSON text, keeping the JSON escaping intact (the commands contain escaped
// quotes, and unescaping them is unnecessary for a substring shape check).
func jsonCommandStrings(raw string) []string {
	const key = `"command"`
	var out []string
	for idx := 0; ; {
		at := strings.Index(raw[idx:], key)
		if at < 0 {
			return out
		}
		rest := raw[idx+at+len(key):]
		colon := strings.Index(rest, ":")
		if colon < 0 {
			return out
		}
		rest = rest[colon+1:]
		open := strings.Index(rest, `"`)
		if open < 0 {
			return out
		}
		value := rest[open+1:]
		end := 0
		for end < len(value) {
			if value[end] == '\\' {
				end += 2
				continue
			}
			if value[end] == '"' {
				break
			}
			end++
		}
		if end >= len(value) {
			return out
		}
		out = append(out, value[:end])
		idx = len(raw) - len(value) + end
	}
}

// gcHookOccurrences returns each `gc hook ...` invocation in a rendered command,
// normalized past an optional `--city <path>` global flag so the shape check
// sees the subcommand either way.
func gcHookOccurrences(command string) []string {
	var out []string
	for idx := 0; ; {
		at := strings.Index(command[idx:], "gc ")
		if at < 0 {
			return out
		}
		start := idx + at
		rest := command[start:]
		normalized := strings.TrimPrefix(rest, "gc ")
		if strings.HasPrefix(normalized, "--city ") {
			normalized = strings.TrimPrefix(normalized, "--city ")
			if space := strings.Index(normalized, " "); space >= 0 {
				normalized = normalized[space+1:]
			}
		}
		if strings.HasPrefix(normalized, "hook") {
			end := len(normalized)
			if limit := strings.Index(normalized, " -- "); limit >= 0 {
				end = limit
			}
			out = append(out, "gc "+normalized[:end])
		}
		idx = start + 3
	}
}
