package beads

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/steveyegge/beads/issueops"
)

// EnsureNativeWorkspaceIdentityWithoutAmbientEnv finishes the identity write of
// an owned, pending initializer whose bd init may have stopped after writing
// metadata.json. The caller must first validate ownership and persist the exact
// project identity from that metadata. This is not an adoption or repair API:
// any existing partial or different identity is refused without writing.
//
// The workspace continues to use its configured bd-owned backend; no process
// endpoint, schema policy or identity is selected from inherited BEADS_ values.
func EnsureNativeWorkspaceIdentityWithoutAmbientEnv(ctx context.Context, scopeRoot, prefix, projectID string) (retErr error) {
	if prefix == "" || projectID == "" || strings.TrimSpace(prefix) != prefix || strings.TrimSpace(projectID) != projectID {
		return errors.New("owned workspace initialization requires an exact prefix and project identity")
	}
	storage, _, err := openNativeStorageWithoutAmbientEnvWithCredentialCommand(ctx, scopeRoot, "", false)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, storage.Close()) }()
	return ensureNativeWorkspaceIdentity(ctx, storage, prefix, projectID)
}

type nativeWorkspaceIdentityStore interface {
	InitVerifier() (issueops.InitVerifier, error)
	Bootstrapper() (issueops.Bootstrapper, error)
}

func ensureNativeWorkspaceIdentity(ctx context.Context, store nativeWorkspaceIdentityStore, prefix, projectID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	verifier, err := store.InitVerifier()
	if err != nil {
		return err
	}
	read := func() (issueops.VerifyIdentityResult, error) {
		return verifier.VerifyIdentity(ctx, issueops.VerifyIdentityRequest{})
	}
	matches := func(found issueops.VerifyIdentityResult) bool {
		return found.Prefix == prefix && found.ProjectID == projectID
	}
	found, err := read()
	if err != nil {
		return fmt.Errorf("reading owned workspace identity: %w", err)
	}
	if matches(found) {
		return nil
	}
	if found.Prefix != "" || found.ProjectID != "" {
		return errors.New("owned workspace identity differs from its initialization intent; refusing to replace it")
	}
	writer, err := store.Bootstrapper()
	if err != nil {
		return err
	}
	_, writeErr := writer.Bootstrap(ctx, issueops.BootstrapRequest{Prefix: prefix, ProjectID: projectID})
	// A lost acknowledgement or racing same-identity bootstrap may have committed.
	// Only a fresh exact identity proves recovery; unreadable remains an error.
	found, readErr := read()
	if readErr == nil && matches(found) {
		return nil
	}
	if readErr != nil {
		return errors.Join(writeErr, fmt.Errorf("confirming owned workspace identity: %w", readErr))
	}
	if writeErr != nil {
		return writeErr
	}
	return errors.New("owned workspace bootstrap did not persist the requested identity")
}
