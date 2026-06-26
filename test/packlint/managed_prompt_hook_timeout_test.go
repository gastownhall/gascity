// TestManagedPromptHookTimeoutExceedsWrapper guards gastownhall/gascity#3457:
// managed prompt hooks are wrapped in `gc hook run --timeout <d> --timeout-exit-code 0`
// so a wedged data-plane command fails open before it can block prompt
// submission. That only works if the provider's own per-hook timeout is long
// enough for the wrapper to reach its fail-open path. Copilot shipped a 10s
// provider timeout under a 15s wrapper, so the provider killed the hook 5s
// before the bound ever fired — the exact behavior the wrapper exists to
// prevent. This test fails if any shipped pack pairs a wrapper command with a
// sibling provider timeout (`timeoutSec` or Antigravity's `timeout`) that does
// not clear the wrapper timeout plus cleanup headroom.

package packlint

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// managedHookWrapperWaitDelay mirrors cmd.WaitDelay in cmd/gc/cmd_hook.go: after
// the wrapper's deadline fires it can take up to this long to SIGKILL the child
// process group and drain its pipes before returning the fail-open exit code.
// A provider timeout must clear the wrapper timeout by at least this margin or
// the provider can abandon the hook before the fail-open code is returned.
const managedHookWrapperWaitDelay = 2 * time.Second

// managedWrapperMarker identifies a `gc hook run` wrapper command body within a
// shipped hook config string.
const managedWrapperMarker = "gc hook run --timeout"

func TestManagedPromptHookTimeoutExceedsWrapper(t *testing.T) {
	root := repoRoot()
	packsDir := filepath.Join(root, "internal/bootstrap/packs")

	var violations []string
	checked := 0
	checkedFiles := map[string]int{}
	err := filepath.WalkDir(packsDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || filepath.Ext(path) != ".json" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}
		if !strings.Contains(string(data), managedWrapperMarker) {
			return nil
		}
		var doc any
		if err := json.Unmarshal(data, &doc); err != nil {
			// A pack JSON we cannot parse is a separate problem; skip it here so
			// this invariant stays focused on the timeout relationship.
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		walkHookTimeoutNodes(doc, func(wrapperCmd string, providerTimeoutSec float64) {
			checked++
			checkedFiles[filepath.ToSlash(rel)]++
			wrapperTimeout, ok := wrapperHookTimeout(wrapperCmd)
			if !ok {
				violations = append(violations, fmt.Sprintf(
					"%s: could not parse wrapper --timeout from %q", filepath.ToSlash(rel), wrapperCmd))
				return
			}
			providerTimeout := time.Duration(providerTimeoutSec * float64(time.Second))
			minProvider := wrapperTimeout + managedHookWrapperWaitDelay
			if providerTimeout < minProvider {
				violations = append(violations, fmt.Sprintf(
					"%s: provider hook timeout %v is below wrapper timeout %s + %s cleanup headroom (need >= %s)",
					filepath.ToSlash(rel), providerTimeout, wrapperTimeout, managedHookWrapperWaitDelay, minProvider))
			}
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", packsDir, err)
	}
	if checked == 0 {
		t.Fatalf("no wrapper command with a sibling provider timeout found under %s; "+
			"the invariant is no longer exercised — update the walker", packsDir)
	}
	// Antigravity ships the same wrapped prompt hooks as Copilot but pins the
	// provider timeout under the key "timeout" rather than "timeoutSec". Guard
	// against a regression where the walker stops matching that key and silently
	// drops Antigravity from the invariant (gastownhall/gascity#3457).
	const antigravityHooks = "internal/bootstrap/packs/core/overlay/per-provider/antigravity/.agents/hooks.json"
	if checkedFiles[antigravityHooks] == 0 {
		t.Errorf("timeout-headroom invariant did not check any wrapped prompt hook in %s; "+
			"walkHookTimeoutNodes likely stopped matching the \"timeout\" sibling key", antigravityHooks)
	}
	if len(violations) > 0 {
		t.Errorf("managed prompt hook provider timeout(s) below the wrapper fail-open budget"+
			" (gastownhall/gascity#3457).\nRaise the provider timeoutSec above the `gc hook run"+
			" --timeout` value plus cleanup headroom, or lower the wrapper timeout below the"+
			" smallest provider timeout.\n\n%s", strings.Join(violations, "\n"))
	}
}

// walkHookTimeoutNodes invokes fn for every JSON object that contains both a
// string field holding a `gc hook run` wrapper command and a sibling numeric
// provider timeout. Providers spell that sibling differently: most use
// `timeoutSec` (Copilot), while Antigravity uses `timeout`, so the walker
// checks `timeoutSec` first and falls back to `timeout`. Known limit: a
// provider that declares the timeout on a parent object rather than as a
// sibling of the command is not matched; the shipped configs that pin a hook
// timeout place it as a sibling.
func walkHookTimeoutNodes(node any, fn func(wrapperCmd string, providerTimeoutSec float64)) {
	switch v := node.(type) {
	case map[string]any:
		wrapperCmd := ""
		for _, val := range v {
			if s, ok := val.(string); ok && strings.Contains(s, managedWrapperMarker) {
				wrapperCmd = s
				break
			}
		}
		if wrapperCmd != "" {
			if sec, ok := numericField(v["timeoutSec"]); ok {
				fn(wrapperCmd, sec)
			} else if sec, ok := numericField(v["timeout"]); ok {
				fn(wrapperCmd, sec)
			}
		}
		for _, val := range v {
			walkHookTimeoutNodes(val, fn)
		}
	case []any:
		for _, item := range v {
			walkHookTimeoutNodes(item, fn)
		}
	}
}

// numericField coerces a JSON-decoded value to a float64 second count.
func numericField(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

// wrapperHookTimeout extracts the duration that follows the wrapper's
// `--timeout` flag (not `--timeout-exit-code`) from a hook command string.
func wrapperHookTimeout(command string) (time.Duration, bool) {
	fields := strings.Fields(command)
	for i, f := range fields {
		if f == "--timeout" && i+1 < len(fields) {
			d, err := time.ParseDuration(fields[i+1])
			if err != nil {
				return 0, false
			}
			return d, true
		}
	}
	return 0, false
}
