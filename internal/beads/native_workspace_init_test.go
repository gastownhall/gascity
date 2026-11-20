package beads

import (
	"context"
	"errors"
	"testing"

	"github.com/steveyegge/beads/issueops"
)

type workspaceInitIdentityFixture struct {
	found              issueops.VerifyIdentityResult
	readErr            error
	writeErr           error
	readCalls          int
	writes             int
	omitWrite          bool
	failReadAfterWrite bool
}

func (f *workspaceInitIdentityFixture) InitVerifier() (issueops.InitVerifier, error) { return f, nil }

func (f *workspaceInitIdentityFixture) Bootstrapper() (issueops.Bootstrapper, error) { return f, nil }

func (f *workspaceInitIdentityFixture) VerifyIdentity(context.Context, issueops.VerifyIdentityRequest) (issueops.VerifyIdentityResult, error) {
	f.readCalls++
	if f.failReadAfterWrite && f.writes != 0 {
		return issueops.VerifyIdentityResult{}, errors.New("confirmation unavailable")
	}
	return f.found, f.readErr
}

func (f *workspaceInitIdentityFixture) Bootstrap(_ context.Context, req issueops.BootstrapRequest) (issueops.BootstrapResult, error) {
	f.writes++
	if !f.omitWrite {
		f.found = issueops.VerifyIdentityResult(req)
	}
	return issueops.BootstrapResult{Prefix: f.found.Prefix, ProjectID: f.found.ProjectID}, f.writeErr
}

func TestOwnedWorkspaceIdentityCompletesInterruptedBdInit(t *testing.T) {
	f := &workspaceInitIdentityFixture{}
	if err := ensureNativeWorkspaceIdentity(context.Background(), f, "gcg", "recorded-project"); err != nil {
		t.Fatal(err)
	}
	if f.writes != 1 || f.readCalls != 2 || f.found.Prefix != "gcg" || f.found.ProjectID != "recorded-project" {
		t.Fatalf("interrupted identity was not completed and freshly confirmed: %+v", f)
	}
	if err := ensureNativeWorkspaceIdentity(context.Background(), f, "gcg", "recorded-project"); err != nil {
		t.Fatal(err)
	}
	if f.writes != 1 {
		t.Fatalf("completed identity was rewritten %d times", f.writes)
	}
}

func TestOwnedWorkspaceIdentityRefusesPartialForeignAndUnreadable(t *testing.T) {
	for _, tc := range []struct {
		name    string
		found   issueops.VerifyIdentityResult
		readErr error
	}{
		{"prefix-only", issueops.VerifyIdentityResult{Prefix: "gcg"}, nil},
		{"project-only", issueops.VerifyIdentityResult{ProjectID: "recorded-project"}, nil},
		{"foreign-prefix", issueops.VerifyIdentityResult{Prefix: "foreign", ProjectID: "recorded-project"}, nil},
		{"foreign-project", issueops.VerifyIdentityResult{Prefix: "gcg", ProjectID: "foreign"}, nil},
		{"unreadable", issueops.VerifyIdentityResult{}, errors.New("read unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &workspaceInitIdentityFixture{found: tc.found, readErr: tc.readErr}
			if err := ensureNativeWorkspaceIdentity(context.Background(), f, "gcg", "recorded-project"); err == nil {
				t.Fatal("identity refusal reported success")
			}
			if f.writes != 0 || f.found != tc.found {
				t.Fatalf("refusal wrote identity: %+v", f)
			}
		})
	}
}

func TestOwnedWorkspaceIdentityLostAckRequiresFreshExactRead(t *testing.T) {
	for _, tc := range []struct {
		name                string
		omitWrite, failRead bool
		wantOK              bool
	}{
		{"committed", false, false, true},
		{"not-committed", true, false, false},
		{"unreadable-outcome", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &workspaceInitIdentityFixture{writeErr: errors.New("lost acknowledgement"), omitWrite: tc.omitWrite, failReadAfterWrite: tc.failRead}
			err := ensureNativeWorkspaceIdentity(context.Background(), f, "gcg", "recorded-project")
			if (err == nil) != tc.wantOK || f.writes != 1 || f.readCalls != 2 {
				t.Fatalf("err=%v fixture=%+v", err, f)
			}
		})
	}
}

func TestOwnedWorkspaceIdentityCanceledDoesNotReadOrWrite(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &workspaceInitIdentityFixture{}
	if err := ensureNativeWorkspaceIdentity(ctx, f, "gcg", "recorded-project"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if f.readCalls != 0 || f.writes != 0 {
		t.Fatalf("canceled initializer reached storage: %+v", f)
	}
}
