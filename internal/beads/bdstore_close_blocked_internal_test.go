package beads

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

type closeBlockedExitError struct{ code int }

func (e *closeBlockedExitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }
func (e *closeBlockedExitError) ExitCode() int { return e.code }

func closeBlockedCommandError(code int, detail string) error {
	// classifyBDExecResult preserves bd's two-line refusal: the human error
	// first and the machine report as the final stderr line.
	return fmt.Errorf("%w: Error updating gc-1: command failed\n%s", &closeBlockedExitError{code: code}, detail)
}

func TestClassifyBDUpdateCloseBlocked(t *testing.T) {
	t.Parallel()

	// Exact abc4 contracts: cmd/bd/update.go wraps the per-ID storage error
	// once with "updating issue: "; update_proxied_server.go returns the same
	// typed policy error unwrapped.
	const blocked = `{"error":"1 of 1 issues failed to update","failed":[{"id":"gc-1","error":"updating issue: cannot close blocked issue: gc-1 is blocked by [gc-2] (use --force to override)"}],"schema_version":1}`
	const proxiedBlocked = `{"error":"1 of 1 issues failed to update","failed":[{"id":"gc-1","error":"cannot close blocked issue: gc-1 is blocked by [gc-2] (use --force to override)"}],"schema_version":1}`
	const wrapped = `{"schema_version":1,"data":{"error":"1 of 1 issues failed to update","failed":[{"id":"gc-1","error":"updating issue: cannot close blocked issue: gc-1 is blocked by [gc-2] (use --force to override)"}]}}`
	const missingSchema = `{"error":"1 of 1 issues failed to update","failed":[{"id":"gc-1","error":"updating issue: cannot close blocked issue: gc-1 is blocked by [gc-2] (use --force to override)"}]}`
	const wrongSchema = `{"error":"1 of 1 issues failed to update","failed":[{"id":"gc-1","error":"updating issue: cannot close blocked issue: gc-1 is blocked by [gc-2] (use --force to override)"}],"schema_version":2}`

	for _, tc := range []struct {
		name string
		id   string
		err  error
		want bool
	}{
		{name: "flat_exact_single_id", id: "gc-1", err: closeBlockedCommandError(1, blocked), want: true},
		{name: "proxied_flat_exact_single_id", id: "gc-1", err: closeBlockedCommandError(1, proxiedBlocked), want: true},
		{name: "wrapped_exact_single_id", id: "gc-1", err: closeBlockedCommandError(1, wrapped), want: true},
		{name: "wrong_exit_code", id: "gc-1", err: closeBlockedCommandError(13, blocked)},
		{name: "unstructured_text_is_not_machine_proof", id: "gc-1", err: closeBlockedCommandError(1, "cannot close blocked issue: gc-1 is blocked by [gc-2] (use --force to override)")},
		{name: "json_embedded_before_trailing_output_is_not_authoritative", id: "gc-1", err: closeBlockedCommandError(1, blocked+"\npost-command hook failed")},
		{name: "json_on_prefixed_same_line_is_not_exact_report", id: "gc-1", err: fmt.Errorf("%w: %s", &closeBlockedExitError{code: 1}, blocked)},
		{name: "missing_schema_version", id: "gc-1", err: closeBlockedCommandError(1, missingSchema)},
		{name: "wrong_schema_version", id: "gc-1", err: closeBlockedCommandError(1, wrongSchema)},
		{name: "unknown_top_level_field", id: "gc-1", err: closeBlockedCommandError(1, strings.TrimSuffix(blocked, "}")+`,"trusted":true}`)},
		{name: "unexpected_wrapper", id: "gc-1", err: closeBlockedCommandError(1, `{"error":"1 of 1 issues failed to update","failed":[{"id":"gc-1","error":"updating: cannot close blocked issue: gc-1 is blocked by [gc-2] (use --force to override)"}],"schema_version":1}`)},
		{name: "double_direct_wrapper", id: "gc-1", err: closeBlockedCommandError(1, `{"error":"1 of 1 issues failed to update","failed":[{"id":"gc-1","error":"updating issue: updating issue: cannot close blocked issue: gc-1 is blocked by [gc-2] (use --force to override)"}],"schema_version":1}`)},
		{name: "different_id", id: "gc-other", err: closeBlockedCommandError(1, blocked)},
		{name: "different_failure_class", id: "gc-1", err: closeBlockedCommandError(1, `{"error":"1 of 1 issues failed to update","failed":[{"id":"gc-1","error":"driver: bad connection"}],"schema_version":1}`)},
		{name: "ambiguous_multi_failure", id: "gc-1", err: closeBlockedCommandError(1, `{"error":"2 of 2 issues failed to update","failed":[{"id":"gc-1","error":"cannot close blocked issue: gc-1 is blocked by [gc-2] (use --force to override)"},{"id":"gc-3","error":"driver: bad connection"}],"schema_version":1}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyBDUpdateCloseBlocked(tc.id, tc.err)
			if got != tc.want {
				t.Fatalf("classifyBDUpdateCloseBlocked(%q, %v) = %v, want %v", tc.id, tc.err, got, tc.want)
			}
		})
	}
}

func TestBdStoreUpdateMapsOnlyTypedBlockedClose(t *testing.T) {
	t.Parallel()
	status := "closed"
	const blocked = `{"error":"1 of 1 issues failed to update","failed":[{"id":"gc-1","error":"updating issue: cannot close blocked issue: gc-1 is blocked by [gc-2] (use --force to override)"}],"schema_version":1}`

	calls := 0
	store := NewBdStore(t.TempDir(), func(string, string, ...string) ([]byte, error) {
		calls++
		if calls == 1 {
			return nil, closeBlockedCommandError(1, blocked)
		}
		return nil, nil
	})
	err := store.Update("gc-1", UpdateOpts{Status: &status})
	if err != nil {
		t.Fatalf("Update blocked close: %v", err)
	}
	if calls != 2 {
		t.Fatalf("Update calls = %d, want no-write refusal plus forced retry", calls)
	}

	store = NewBdStore(t.TempDir(), func(string, string, ...string) ([]byte, error) {
		return nil, closeBlockedCommandError(1, `{"error":"1 of 1 issues failed to update","failed":[{"id":"gc-1","error":"driver: bad connection"}],"schema_version":1}`)
	})
	err = store.Update("gc-1", UpdateOpts{Status: &status})
	if errors.Is(err, ErrCloseBlocked) {
		t.Fatalf("generic Update error was mislabeled ErrCloseBlocked: %v", err)
	}
}

func TestBdStoreUpdateRetriesClosePolicyOnlyWithoutAssigneeAuthority(t *testing.T) {
	t.Parallel()
	const blocked = `{"error":"1 of 1 issues failed to update","failed":[{"id":"gc-1","error":"updating issue: cannot close blocked issue: gc-1 is blocked by [gc-2] (use --force to override)"}],"schema_version":1}`
	const openChildren = `{"error":"1 of 1 issues failed to update","failed":[{"id":"gc-1","error":"updating issue: cannot close gc-1: 2 open child issue(s); close children first or use --force to override"}],"schema_version":1}`
	const proxiedBlocked = `{"error":"1 of 1 issues failed to update","failed":[{"id":"gc-1","error":"cannot close blocked issue: gc-1 is blocked by [gc-2] (use --force to override)"}],"schema_version":1}`
	const proxiedOpenChildren = `{"error":"1 of 1 issues failed to update","failed":[{"id":"gc-1","error":"cannot close gc-1: 2 open child issue(s); close children first or use --force to override"}],"schema_version":1}`

	for _, tc := range []struct {
		name   string
		detail string
	}{
		{name: "direct live blocker", detail: blocked},
		{name: "direct open children", detail: openChildren},
		{name: "proxied live blocker", detail: proxiedBlocked},
		{name: "proxied open children", detail: proxiedOpenChildren},
	} {
		for _, shape := range []struct {
			name  string
			mixed bool
		}{
			{name: "status only"},
			{name: "mixed fields", mixed: true},
		} {
			t.Run(tc.name+"/"+shape.name, func(t *testing.T) {
				var calls [][]string
				store := NewBdStore(t.TempDir(), func(_ string, _ string, args ...string) ([]byte, error) {
					calls = append(calls, append([]string(nil), args...))
					if len(calls) == 1 {
						return nil, closeBlockedCommandError(1, tc.detail)
					}
					return nil, nil
				})
				closed := "closed"
				opts := UpdateOpts{Status: &closed}
				if shape.mixed {
					title := "terminal title"
					opts.Title = &title
					opts.Metadata = map[string]string{"outcome": "pass"}
				}
				if err := store.Update("gc-1", opts); err != nil {
					t.Fatalf("Update: %v", err)
				}
				if len(calls) != 2 {
					t.Fatalf("calls = %d, want refusal plus one forced retry", len(calls))
				}
				first := strings.Join(calls[0], " ")
				second := strings.Join(calls[1], " ")
				if strings.Contains(first, "--force") {
					t.Fatalf("initial update = %q, must preserve assignee fence", first)
				}
				if second != first+" --force" {
					t.Fatalf("forced retry = %q, want exact same-field command plus --force after proven no-write refusal %q", second, first)
				}
				if shape.mixed && (!strings.Contains(second, "--title terminal title") || !strings.Contains(second, "--set-metadata outcome=pass")) {
					t.Fatalf("forced retry dropped sibling fields: %q", second)
				}
			})
		}
	}
}

func TestBdStoreUpdateReturnsForcedRetryFailureWithoutAThirdWrite(t *testing.T) {
	t.Parallel()
	const blocked = `{"error":"1 of 1 issues failed to update","failed":[{"id":"gc-1","error":"updating issue: cannot close blocked issue: gc-1 is blocked by [gc-2] (use --force to override)"}],"schema_version":1}`
	wantErr := errors.New("forced transaction rejected")
	var calls [][]string
	store := NewBdStore(t.TempDir(), func(_ string, _ string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		if len(calls) == 1 {
			return nil, closeBlockedCommandError(1, blocked)
		}
		return nil, wantErr
	})
	closed := "closed"
	title := "must remain part of the atomic command"
	err := store.Update("gc-1", UpdateOpts{Status: &closed, Title: &title})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Update error = %v, want forced retry error %v", err, wantErr)
	}
	if len(calls) != 2 {
		t.Fatalf("commands = %d, want exactly refusal plus forced atomic retry", len(calls))
	}
	first := strings.Join(calls[0], " ")
	second := strings.Join(calls[1], " ")
	if second != first+" --force" || !strings.Contains(second, "--title "+title) {
		t.Fatalf("forced failure command = %q, want exact all-field retry of %q", second, first)
	}
}

func TestBdStoreUpdateNeverCombinesCloseForceWithAssigneeEdit(t *testing.T) {
	t.Parallel()
	const openChildren = `{"error":"1 of 1 issues failed to update","failed":[{"id":"gc-1","error":"updating issue: cannot close gc-1: 1 open child issue(s); close children first or use --force to override"}],"schema_version":1}`
	var calls [][]string
	store := NewBdStore(t.TempDir(), func(_ string, _ string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		return nil, closeBlockedCommandError(1, openChildren)
	})
	closed := "closed"
	assignee := "new-owner"
	err := store.Update("gc-1", UpdateOpts{Status: &closed, Assignee: &assignee})
	if !errors.Is(err, ErrCloseOpenChildren) {
		t.Fatalf("Update error = %v, want typed open-children refusal", err)
	}
	if len(calls) != 1 || strings.Contains(strings.Join(calls[0], " "), "--force") {
		t.Fatalf("commands = %v, must fail closed without dual-purpose --force", calls)
	}
}
