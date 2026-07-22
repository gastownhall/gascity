package beads

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
)

func TestBdStoreLifecycleMutationEnvReachesOnlyMutatingChild(t *testing.T) {
	scope := newLifecycleMutationLeaseScope(t)

	processScope, processScopePresent := os.LookupEnv(lifecycleMutationScopeEnv)
	processToken, processTokenPresent := os.LookupEnv(lifecycleMutationTokenEnv)
	assertProcessEnvUnchanged := func(where string) {
		t.Helper()
		gotScope, gotScopePresent := os.LookupEnv(lifecycleMutationScopeEnv)
		if gotScope != processScope || gotScopePresent != processScopePresent {
			t.Fatalf(
				"%s process %s = (%q, %t), want unchanged (%q, %t)",
				where,
				lifecycleMutationScopeEnv,
				gotScope,
				gotScopePresent,
				processScope,
				processScopePresent,
			)
		}
		gotToken, gotTokenPresent := os.LookupEnv(lifecycleMutationTokenEnv)
		if gotToken != processToken || gotTokenPresent != processTokenPresent {
			t.Fatalf(
				"%s process %s = (%q, %t), want unchanged (%q, %t)",
				where,
				lifecycleMutationTokenEnv,
				gotToken,
				gotTokenPresent,
				processToken,
				processTokenPresent,
			)
		}
	}

	type runnerCall struct {
		command string
		env     map[string]string
	}
	var calls []runnerCall

	const targetRow = `[{"id":"bd-42","title":"lifecycle target","status":"open","issue_type":"task","created_at":"2026-07-16T00:00:00Z"}]`
	legacyRunner := func(dir, name string, args ...string) ([]byte, error) {
		assertProcessEnvUnchanged("legacy runner")
		command := name + " " + strings.Join(args, " ")
		calls = append(calls, runnerCall{command: command})
		if dir != scope {
			return nil, fmt.Errorf("legacy runner dir = %q, want %q", dir, scope)
		}
		if name != "bd" || len(args) == 0 {
			return nil, fmt.Errorf("unexpected legacy command %q", command)
		}
		switch args[0] {
		case "query":
			// Canonical cross-tier read used by the lifecycle transaction's Get.
			return []byte(targetRow), nil
		case "dep":
			// Dependency hydration paired with the canonical read.
			return []byte(`[]`), nil
		case "show":
			// Plain store Get after the transaction still reads issues-first.
			return []byte(targetRow), nil
		default:
			return nil, fmt.Errorf("unexpected legacy command %q", command)
		}
	}
	commandEnvRunner := func(dir, name string, env map[string]string, args ...string) ([]byte, error) {
		assertProcessEnvUnchanged("command-env runner")
		command := name + " " + strings.Join(args, " ")
		calls = append(calls, runnerCall{
			command: command,
			env:     copyLifecycleMutationCommandEnv(env),
		})
		if dir != scope {
			return nil, fmt.Errorf("command-env runner dir = %q, want %q", dir, scope)
		}
		if command != "bd update --json bd-42 --set-metadata gc.test=value" {
			return nil, fmt.Errorf("unexpected command-env command %q", command)
		}
		return []byte(`[{"id":"bd-42"}]`), nil
	}

	store := NewBdStore(
		scope,
		legacyRunner,
		WithBdStoreCommandEnvRunner(commandEnvRunner),
	)
	err := WithLifecycleMetadataTransaction(store, "bd-42", func(tx LifecycleMetadataTransaction) error {
		if _, err := tx.Get(); err != nil {
			return err
		}
		return tx.SetMetadata("gc.test", "value")
	})
	if err != nil {
		t.Fatalf("WithLifecycleMetadataTransaction: %v", err)
	}
	if _, err := store.Get("bd-42"); err != nil {
		t.Fatalf("Get after lifecycle transaction: %v", err)
	}
	assertProcessEnvUnchanged("after lifecycle transaction")

	// The canonical lifecycle read (bd query + dep hydration) and the final plain
	// store Get must reach bd with no lifecycle command-env; only the mutating
	// update child carries the delegated lease.
	mutations := 0
	reads := 0
	for _, call := range calls {
		if strings.HasPrefix(call.command, "bd update ") {
			mutations++
			if call.command != "bd update --json bd-42 --set-metadata gc.test=value" {
				t.Fatalf("mutation command = %q, want lifecycle metadata update", call.command)
			}
			if len(call.env) != 2 {
				t.Fatalf("mutation command env = %#v, want only lifecycle scope and token", call.env)
			}
			if got, want := call.env[lifecycleMutationScopeEnv], closeTransitionScopeKey(scope); got != want {
				t.Fatalf("mutation command scope = %q, want %q", got, want)
			}
			if got := call.env[lifecycleMutationTokenEnv]; got == "" {
				t.Fatal("mutation command token is empty")
			}
			continue
		}
		reads++
		if call.env != nil {
			t.Fatalf("read-only call %q received lifecycle command env %#v", call.command, call.env)
		}
	}
	if mutations != 1 {
		t.Fatalf("mutation calls = %d, want exactly 1; calls=%#v", mutations, calls)
	}
	if reads == 0 {
		t.Fatalf("expected lifecycle/plain reads, got none; calls=%#v", calls)
	}
}

func TestBdStoreLifecycleMutationCloseEnvReachesOnlyCloseChild(t *testing.T) {
	scope := newLifecycleMutationLeaseScope(t)
	processScope, processScopePresent := os.LookupEnv(lifecycleMutationScopeEnv)
	processToken, processTokenPresent := os.LookupEnv(lifecycleMutationTokenEnv)
	assertProcessEnvUnchanged := func(where string) {
		t.Helper()
		gotScope, gotScopePresent := os.LookupEnv(lifecycleMutationScopeEnv)
		gotToken, gotTokenPresent := os.LookupEnv(lifecycleMutationTokenEnv)
		if gotScope != processScope || gotScopePresent != processScopePresent ||
			gotToken != processToken || gotTokenPresent != processTokenPresent {
			t.Fatalf(
				"%s process lifecycle env = scope(%q, %t) token(%q, %t), want unchanged scope(%q, %t) token(%q, %t)",
				where,
				gotScope,
				gotScopePresent,
				gotToken,
				gotTokenPresent,
				processScope,
				processScopePresent,
				processToken,
				processTokenPresent,
			)
		}
	}

	type runnerCall struct {
		verb string
		env  map[string]string
	}
	var calls []runnerCall
	status := "open"
	reason := ""
	session := ""

	legacyRunner := func(dir, name string, args ...string) ([]byte, error) {
		assertProcessEnvUnchanged("legacy close-transition runner")
		if dir != scope || name != "bd" || len(args) == 0 {
			return nil, fmt.Errorf("unexpected legacy command dir=%q name=%q args=%q", dir, name, args)
		}
		calls = append(calls, runnerCall{verb: args[0]})
		switch args[0] {
		case "version":
			return []byte("bd version 1.1.0\n"), nil
		case "query":
			return []byte(fmt.Sprintf(
				`[{"id":"bd-42","title":"lifecycle target","status":%q,"issue_type":"task","close_reason":%q}]`,
				status,
				reason,
			)), nil
		case "dep":
			return []byte(`[]`), nil
		case "sql":
			return []byte(fmt.Sprintf(
				`[{"id":"bd-42","status":%q,"close_reason":%q,"closed_by_session":%q}]`,
				status,
				reason,
				session,
			)), nil
		default:
			return nil, fmt.Errorf("unexpected legacy bd verb %q", args[0])
		}
	}
	commandEnvRunner := func(dir, name string, env map[string]string, args ...string) ([]byte, error) {
		assertProcessEnvUnchanged("command-env close-transition runner")
		if dir != scope || name != "bd" || len(args) == 0 || args[0] != "close" {
			return nil, fmt.Errorf("unexpected command-env command dir=%q name=%q args=%q", dir, name, args)
		}
		calls = append(calls, runnerCall{
			verb: args[0],
			env:  copyLifecycleMutationCommandEnv(env),
		})
		status = "closed"
		reason = argValue(args, "--reason")
		session = "close-child"
		return []byte(fmt.Sprintf(
			`[{"id":"bd-42","title":"lifecycle target","status":"closed","issue_type":"task","close_reason":%q}]`,
			reason,
		)), nil
	}

	store := NewBdStore(
		scope,
		legacyRunner,
		WithBdStoreCommandEnvRunner(commandEnvRunner),
	)
	// This test isolates command-local lifecycle environment propagation; the
	// conditional-write capability probe has dedicated coverage elsewhere.
	store.condWriteProbed = true
	store.condWriteCapable = true
	transition, err := store.CloseWithReasonIfOpen("bd-42", "lifecycle close")
	if err != nil {
		t.Fatalf("CloseWithReasonIfOpen: %v", err)
	}
	if !transition.Transitioned {
		t.Fatal("CloseWithReasonIfOpen Transitioned = false, want true")
	}
	assertProcessEnvUnchanged("after close transition")

	wantVerbs := []string{"version", "query", "dep", "sql", "close", "sql", "query", "dep", "sql"}
	if len(calls) != len(wantVerbs) {
		t.Fatalf("runner calls = %#v, want verbs %v", calls, wantVerbs)
	}
	for i, wantVerb := range wantVerbs {
		if calls[i].verb != wantVerb {
			t.Fatalf("runner call %d verb = %q, want %q; calls=%#v", i, calls[i].verb, wantVerb, calls)
		}
		if wantVerb != "close" && calls[i].env != nil {
			t.Fatalf("read-only %s call received lifecycle command env %#v", wantVerb, calls[i].env)
		}
	}
	closeEnv := calls[4].env
	if len(closeEnv) != 2 {
		t.Fatalf("close command env = %#v, want only lifecycle scope and token", closeEnv)
	}
	if got, want := closeEnv[lifecycleMutationScopeEnv], closeTransitionScopeKey(scope); got != want {
		t.Fatalf("close command scope = %q, want %q", got, want)
	}
	if got := closeEnv[lifecycleMutationTokenEnv]; got == "" {
		t.Fatal("close command token is empty")
	}
}

func TestCommandChildEnvStripsAmbientLifecycleInheritanceUnlessExplicit(t *testing.T) {
	base := map[string]string{
		"BEADS_DIR":               "/scope/.beads",
		lifecycleMutationScopeEnv: "/ambient-scope",
		lifecycleMutationTokenEnv: "ambient-token",
	}

	ordinary := commandChildEnv(base, nil)
	if _, ok := ordinary[lifecycleMutationScopeEnv]; ok {
		t.Fatalf("ordinary child inherited %s: %#v", lifecycleMutationScopeEnv, ordinary)
	}
	if _, ok := ordinary[lifecycleMutationTokenEnv]; ok {
		t.Fatalf("ordinary child inherited %s: %#v", lifecycleMutationTokenEnv, ordinary)
	}
	if got := ordinary["BEADS_DIR"]; got != "/scope/.beads" {
		t.Fatalf("ordinary child BEADS_DIR = %q, want base environment retained", got)
	}

	explicit := map[string]string{
		lifecycleMutationScopeEnv: "/owner-scope",
		lifecycleMutationTokenEnv: "owner-token",
	}
	mutating := commandChildEnv(base, explicit)
	if got := mutating[lifecycleMutationScopeEnv]; got != "/owner-scope" {
		t.Fatalf("mutating child scope = %q, want explicit owner scope", got)
	}
	if got := mutating[lifecycleMutationTokenEnv]; got != "owner-token" {
		t.Fatalf("mutating child token = %q, want explicit owner token", got)
	}
	if base[lifecycleMutationTokenEnv] != "ambient-token" {
		t.Fatalf("commandChildEnv mutated caller base map: %#v", base)
	}
}

func TestBdStoreStatusMutationsShareLifecycleLeaseAndPreserveRetryEnv(t *testing.T) {
	closed := "closed"
	tests := []struct {
		name        string
		conditional bool
		operation   func(*BdStore) error
	}{
		{
			name: "Claim",
			operation: func(store *BdStore) error {
				_, claimed, err := store.Claim("bd-42")
				if err == nil && !claimed {
					return errors.New("Claim claimed = false, want true")
				}
				return err
			},
		},
		{
			name: "Update",
			operation: func(store *BdStore) error {
				return store.Update("bd-42", UpdateOpts{Status: &closed})
			},
		},
		{
			name: "UpdateAll",
			operation: func(store *BdStore) error {
				updated, err := store.UpdateAll([]string{"bd-42", "bd-43"}, UpdateOpts{Status: &closed})
				if err == nil && updated != 2 {
					return fmt.Errorf("UpdateAll updated = %d, want 2", updated)
				}
				return err
			},
		},
		{
			name: "Reopen",
			operation: func(store *BdStore) error {
				return store.Reopen("bd-42")
			},
		},
		{
			name:        "UpdateIfMatch",
			conditional: true,
			operation: func(store *BdStore) error {
				return store.UpdateIfMatch("bd-42", 7, UpdateOpts{Status: &closed})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scope := newLifecycleMutationLeaseScope(t)
			legacyRunner := func(string, string, ...string) ([]byte, error) {
				return nil, errors.New("status mutation unexpectedly used legacy runner")
			}
			var environments []map[string]string
			commandEnvRunner := func(_ string, name string, env map[string]string, args ...string) ([]byte, error) {
				if name != "bd" || len(args) == 0 {
					return nil, fmt.Errorf("unexpected command %s %q", name, args)
				}
				environments = append(environments, copyLifecycleMutationCommandEnv(env))
				inherited, err := lifecycleMutationInheritanceFromCommandEnv(env)
				if err != nil {
					return nil, err
				}
				nested, err := acquireLifecycleMutationLease(scope, inherited)
				if err != nil {
					return nil, fmt.Errorf("synchronous status hook reentry: %w", err)
				}
				nested.Unlock()
				if !tt.conditional && len(environments) == 1 {
					return nil, errors.New("Error 1213 (40001): serialization failure")
				}
				return []byte(`[{"id":"bd-42"}]`), nil
			}

			store := NewBdStore(scope, legacyRunner, WithBdStoreCommandEnvRunner(commandEnvRunner))
			if tt.conditional {
				store.condWriteProbed = true
				store.condWriteCapable = true
			}
			done := make(chan error, 1)
			go func() { done <- tt.operation(store) }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("status mutation: %v", err)
				}
			case <-time.After(testutil.GoroutineRaceTimeout):
				t.Fatal("status mutation deadlocked during synchronous lifecycle hook reentry")
			}

			wantCalls := 2
			if tt.conditional {
				wantCalls = 1
			}
			if len(environments) != wantCalls {
				t.Fatalf("command-env calls = %d, want %d", len(environments), wantCalls)
			}
			for i, env := range environments {
				if len(env) != 2 || env[lifecycleMutationScopeEnv] != closeTransitionScopeKey(scope) || env[lifecycleMutationTokenEnv] == "" {
					t.Fatalf("command-env call %d = %#v, want exact scope/token inheritance", i, env)
				}
				if !reflect.DeepEqual(env, environments[0]) {
					t.Fatalf("retry env %d = %#v, want identical to first attempt %#v", i, env, environments[0])
				}
			}
		})
	}
}

func TestBdStoreCloseReadsReasonInsideLifecycleLease(t *testing.T) {
	scope := newLifecycleMutationLeaseScope(t)
	const (
		id            = "bd-42"
		updatedReason = "committed lifecycle close reason"
	)

	var mu sync.Mutex
	status := "open"
	reason := "old close reason"
	closedWithReason := ""
	legacyRunner := func(_ string, name string, args ...string) ([]byte, error) {
		if name != "bd" || len(args) == 0 || args[0] != "show" {
			return nil, fmt.Errorf("unexpected legacy command %s %q", name, args)
		}
		mu.Lock()
		defer mu.Unlock()
		return []byte(fmt.Sprintf(
			`[{"id":%q,"title":"reason target","status":%q,"issue_type":"task","metadata":{"close_reason":%q}}]`,
			id,
			status,
			reason,
		)), nil
	}
	commandEnvRunner := func(_ string, name string, env map[string]string, args ...string) ([]byte, error) {
		if name != "bd" || len(args) == 0 {
			return nil, fmt.Errorf("unexpected command-env command %s %q", name, args)
		}
		if _, err := lifecycleMutationInheritanceFromCommandEnv(env); err != nil {
			return nil, err
		}
		mu.Lock()
		defer mu.Unlock()
		switch args[0] {
		case "update":
			reason = strings.TrimPrefix(argValue(args, "--set-metadata"), "close_reason=")
			return []byte(`[{"id":"bd-42"}]`), nil
		case "close":
			closedWithReason = argValue(args, "--reason")
			status = "closed"
			return []byte(`[{"id":"bd-42"}]`), nil
		default:
			return nil, fmt.Errorf("unexpected command-env bd verb %q", args[0])
		}
	}

	first := NewBdStore(scope, legacyRunner, WithBdStoreCommandEnvRunner(commandEnvRunner))
	second := NewBdStore(scope, legacyRunner, WithBdStoreCommandEnvRunner(commandEnvRunner))
	transactionEntered := make(chan struct{})
	allowMetadataWrite := make(chan struct{})
	transactionDone := make(chan error, 1)
	go func() {
		transactionDone <- WithLifecycleMetadataTransaction(first, id, func(tx LifecycleMetadataTransaction) error {
			close(transactionEntered)
			<-allowMetadataWrite
			return tx.SetMetadata("close_reason", updatedReason)
		})
	}()
	select {
	case <-transactionEntered:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("lifecycle metadata transaction did not enter")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- second.Close(id) }()
	waitForCloseTransitionScopeRefs(t, scope)
	close(allowMetadataWrite)

	for operation, done := range map[string]<-chan error{
		"lifecycle metadata transaction": transactionDone,
		"close":                          closeDone,
	} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s: %v", operation, err)
			}
		case <-time.After(testutil.GoroutineRaceTimeout):
			t.Fatalf("%s did not finish", operation)
		}
	}
	mu.Lock()
	gotReason := closedWithReason
	mu.Unlock()
	if gotReason != updatedReason {
		t.Fatalf("bd close reason = %q, want metadata committed before the serialized read %q", gotReason, updatedReason)
	}
}
