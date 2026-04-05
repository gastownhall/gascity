//go:build acceptance_c

package workerinference_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/gastownhall/gascity/internal/worker/workertest"
)

func TestValidateClaudeCredentialsExpired(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".credentials.json")
	writeClaudeCredentials(t, path, time.Now().Add(-time.Minute))

	err := validateClaudeCredentials(path, time.Now())
	require.Error(t, err)
	require.Contains(t, err.Error(), "expired")
}

func TestValidateClaudeCredentialsFresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".credentials.json")
	writeClaudeCredentials(t, path, time.Now().Add(10*time.Minute))

	err := validateClaudeCredentials(path, time.Now())
	require.NoError(t, err)
}

func TestLiveFailureResultClassifiesAuthErrors(t *testing.T) {
	result := liveFailureResult(
		workertest.ProfileID("claude/tmux-cli"),
		workertest.RequirementInferenceContinuation,
		"live worker did not complete within timeout",
		map[string]string{"transcript_tail": "Please run /login · API Error: 401 authentication_error: OAuth token has expired."},
	)

	require.Equal(t, workertest.ResultEnvironmentErr, result.Status)
}

func TestLiveFailureResultClassifiesProviderIncidents(t *testing.T) {
	result := liveFailureResult(
		workertest.ProfileID("codex/tmux-cli"),
		workertest.RequirementInferenceFreshTask,
		"live worker did not complete within timeout",
		map[string]string{"transcript_tail": "HTTP 429 rate_limit exceeded, try again later"},
	)

	require.Equal(t, workertest.ResultProviderIssue, result.Status)
}

func writeClaudeCredentials(t *testing.T, path string, expiry time.Time) {
	t.Helper()

	data, err := json.Marshal(map[string]any{
		"claudeAiOauth": map[string]any{
			"expiresAt": expiry.UnixMilli(),
		},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
}
