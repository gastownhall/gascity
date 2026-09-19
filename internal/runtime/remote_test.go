package runtime

import (
	"errors"
	"testing"
	"time"
)

func TestRemoteSessionCapabilitiesSupportOnlyDeclaredOperations(t *testing.T) {
	caps := RemoteSessionCapabilities{Operations: []RemoteSessionOperation{
		RemoteSessionCreate,
		RemoteSessionStatus,
		RemoteSessionCreate,
	}}

	if !caps.Supports(RemoteSessionCreate) {
		t.Fatal("create capability was not reported")
	}
	if caps.Supports(RemoteSessionFollowUp) {
		t.Fatal("undeclared follow-up capability was reported")
	}
	if got := caps.CanonicalOperations(); len(got) != 2 || got[0] != RemoteSessionCreate || got[1] != RemoteSessionStatus {
		t.Fatalf("CanonicalOperations = %v, want sorted unique create/status", got)
	}
}

func TestRemoteSessionRequestsValidateOpaqueIdentityAndFence(t *testing.T) {
	validRef := RemoteSessionRef{SessionID: "opaque-conversation", RunID: "opaque-run"}
	validFence := RemoteOwnershipFence{Token: "opaque-owner-generation"}

	tests := []struct {
		name string
		err  error
	}{
		{name: "create", err: (RemoteCreateRequest{RequestID: "request-1", Fence: validFence, Prompt: TextContent("work")}).Validate()},
		{name: "adopt", err: (RemoteAdoptRequest{Ref: validRef, Fence: validFence}).Validate()},
		{name: "follow-up", err: (RemoteFollowUpRequest{RequestID: "request-2", Ref: validRef, Fence: validFence, Content: TextContent("continue")}).Validate()},
		{name: "cancel", err: (RemoteMutationRequest{RequestID: "request-3", Ref: validRef, Fence: validFence}).Validate()},
	}
	for _, tc := range tests {
		if tc.err != nil {
			t.Errorf("%s request rejected: %v", tc.name, tc.err)
		}
	}

	if err := (RemoteCreateRequest{Fence: validFence, Prompt: TextContent("work")}).Validate(); err == nil {
		t.Fatal("create without idempotency request ID succeeded")
	}
	if err := (RemoteFollowUpRequest{RequestID: "request-2", Ref: validRef, Content: TextContent("continue")}).Validate(); err == nil {
		t.Fatal("mutating follow-up without ownership fence succeeded")
	}
	if err := (RemoteTranscriptQuery{Ref: RemoteSessionRef{}, Limit: 10}).Validate(); err == nil {
		t.Fatal("transcript query without opaque session identity succeeded")
	}
	if err := (RemoteTranscriptQuery{Ref: validRef, Limit: MaxRemoteTranscriptEvents + 1}).Validate(); err == nil {
		t.Fatal("unbounded transcript query succeeded")
	}
	if err := (RemoteCreateRequest{
		RequestID: "request-4",
		Fence:     validFence,
		Prompt:    TextContent("work"),
		Source:    RemoteSource{Repository: "https://token@example.com/acme/repo"},
	}).Validate(); err == nil {
		t.Fatal("create accepted a repository URL with embedded credentials")
	}
}

func TestRemoteSessionSnapshotValidatesTerminalFailureShape(t *testing.T) {
	now := time.Now().UTC()
	valid := RemoteSessionSnapshot{
		Ref:       RemoteSessionRef{SessionID: "opaque-session", RunID: "opaque-run"},
		Phase:     RemoteSessionFailed,
		Failure:   RemoteFailureQuota,
		UpdatedAt: now,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid failed snapshot rejected: %v", err)
	}

	invalid := valid
	invalid.Phase = RemoteSessionRunning
	if err := invalid.Validate(); err == nil {
		t.Fatal("nonterminal snapshot with terminal failure classification succeeded")
	}
}

func TestRemoteSessionErrorSupportsStableClassification(t *testing.T) {
	err := &RemoteSessionError{Kind: RemoteFailureAuth, Message: "authentication required", Retryable: false}
	if !errors.Is(err, ErrRemoteSession) {
		t.Fatalf("RemoteSessionError does not wrap ErrRemoteSession: %v", err)
	}
	if got := err.Error(); got != "remote session auth: authentication required" {
		t.Fatalf("Error() = %q", got)
	}
}

func TestRemoteSessionErrorRejectsUnknownClassification(t *testing.T) {
	err := (&RemoteSessionError{Kind: "billing-limit"}).Validate()
	if err == nil {
		t.Fatal("unknown provider error classification succeeded")
	}
}

func TestRemoteSessionSnapshotRejectsCredentialBearingGitHandoff(t *testing.T) {
	snapshot := RemoteSessionSnapshot{
		Ref:       RemoteSessionRef{SessionID: "opaque-session"},
		Phase:     RemoteSessionSucceeded,
		Handoff:   []RemoteGitHandoff{{Repository: "https://user:pass@example.com/acme/repo", Branch: "feature"}},
		UpdatedAt: time.Now().UTC(),
	}
	if err := snapshot.Validate(); err == nil {
		t.Fatal("snapshot accepted a credential-bearing Git handoff URL")
	}
}
