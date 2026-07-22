package beads

import (
	"fmt"
	"slices"
	"testing"
)

type bdTransitionDependenciesFixture struct {
	status       string
	reason       string
	revision     int64
	depListCalls int
}

func newBDTransitionDependenciesFixture() *bdTransitionDependenciesFixture {
	return &bdTransitionDependenciesFixture{
		status:   "open",
		revision: 11,
	}
}

func (f *bdTransitionDependenciesFixture) runner(_ string, name string, args ...string) ([]byte, error) {
	if name != "bd" || len(args) == 0 {
		return nil, fmt.Errorf("unexpected command %s %q", name, args)
	}
	if len(args) == 2 && args[1] == "--help" && slices.Contains(conditionalWriteProbeVerbs, args[0]) {
		return []byte("Flags:\n      --if-revision int\n"), nil
	}

	switch args[0] {
	case "version":
		return []byte("bd version 1.1.0\n"), nil
	case "query":
		// Deliberately omit dependencies: bd query does not request dependency
		// hydration, so transition code must pair this canonical row with dep-list.
		return []byte(fmt.Sprintf(
			`[{"id":"bd-root","title":"root","status":%q,"issue_type":"molecule","close_reason":%q,"revision":%d}]`,
			f.status,
			f.reason,
			f.revision,
		)), nil
	case "dep":
		if len(args) < 4 || args[1] != "list" || args[2] != "bd-root" || args[3] != "--json" {
			return nil, fmt.Errorf("unexpected bd dep args %q", args)
		}
		f.depListCalls++
		return []byte(`[{"id":"bd-blocker","title":"blocker","status":"open","issue_type":"task","dependency_type":"blocks"}]`), nil
	case "sql":
		return []byte(fmt.Sprintf(
			`[{"id":"bd-root","status":%q,"close_reason":%q,"closed_by_session":""}]`,
			f.status,
			f.reason,
		)), nil
	case "close":
		expected, ok := revisionArg(args)
		if !ok || expected != f.revision {
			return nil, fmt.Errorf("unexpected close revision in %q", args)
		}
		f.status = "closed"
		f.revision++
		f.reason = argValue(args, "--reason")
		if f.reason == "" {
			f.reason = "Closed"
		}
		return []byte(fmt.Sprintf(`[{"id":"bd-root","status":"closed","revision":%d}]`, f.revision)), nil
	default:
		return nil, fmt.Errorf("unexpected bd args %q", args)
	}
}

func TestBdStoreAtomicCloseTransitionsHydrateDependencies(t *testing.T) {
	tests := []struct {
		name string
		act  func(*BdStore) (CloseTransition, error)
	}{
		{
			name: "CloseWithReasonIfOpen",
			act: func(store *BdStore) (CloseTransition, error) {
				return store.CloseWithReasonIfOpen("bd-root", "all children closed")
			},
		},
		{
			name: "CloseAllWithTransitions",
			act: func(store *BdStore) (CloseTransition, error) {
				result, err := store.CloseAllWithTransitions([]string{"bd-root"}, nil)
				if err != nil {
					return CloseTransition{}, err
				}
				transition, ok := result.TransitionFor("bd-root")
				if !ok {
					return CloseTransition{}, fmt.Errorf("missing complete transition for bd-root")
				}
				return transition, nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newBDTransitionDependenciesFixture()
			transition, err := tt.act(NewBdStore("/city", fixture.runner))
			if err != nil {
				t.Fatalf("atomic close: %v", err)
			}
			if !transition.Transitioned {
				t.Fatalf("transition = %#v, want owned open-to-closed transition", transition)
			}

			want := Dep{IssueID: "bd-root", DependsOnID: "bd-blocker", Type: "blocks"}
			if len(transition.Before.Dependencies) != 1 || transition.Before.Dependencies[0] != want {
				t.Errorf("Before.Dependencies = %#v, want %#v", transition.Before.Dependencies, []Dep{want})
			}
			if len(transition.After.Dependencies) != 1 || transition.After.Dependencies[0] != want {
				t.Errorf("After.Dependencies = %#v, want %#v", transition.After.Dependencies, []Dep{want})
			}
			if fixture.depListCalls == 0 {
				t.Error("bd dep list was not called; bd query intentionally omitted dependencies")
			}
		})
	}
}
