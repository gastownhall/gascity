package exec //nolint:revive // internal package, always imported with alias

import (
	"fmt"
	"net/http"
	"os"
	osexec "os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/mailtest"
)

// TestMCPMailConformanceLive runs the conformance suite against a real
// mcp_agent_mail server. If the server is already running, it uses it.
// Otherwise it starts one via python3 and tears it down after tests.
//
// Gated by GC_TEST_MCP_MAIL=1 to avoid running in normal go test ./...
//
// Run with:
//
//	make test-mcp-mail
//
// Or directly:
//
//	GC_TEST_MCP_MAIL=1 go test ./internal/mail/exec/ -run TestMCPMailConformanceLive -v
//
// Override the server URL (skips auto-start):
//
//	GC_TEST_MCP_MAIL=1 GC_MCP_MAIL_URL=http://host:port go test ...
func TestMCPMailConformanceLive(t *testing.T) {
	if os.Getenv("GC_TEST_MCP_MAIL") == "" {
		t.Skip("set GC_TEST_MCP_MAIL=1 to run (or use make test-mcp-mail)")
	}

	for _, tool := range []string{"jq", "curl"} {
		if _, err := osexec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH", tool)
		}
	}

	serverURL := os.Getenv("GC_MCP_MAIL_URL")
	if serverURL == "" {
		serverURL = "http://127.0.0.1:8765"
	}

	// Use existing server or start one.
	if !mcpServerReachable(serverURL) {
		startMCPServer(t, serverURL)
	}

	scriptPath, err := findMCPScript()
	if err != nil {
		t.Skipf("MCP mail script not found: %v", err)
	}

	mailtest.RunProviderTests(t, func(t *testing.T) mail.Provider {
		dir := t.TempDir()

		// Unique project per test for isolation.
		// human_key must be an absolute path for mcp_agent_mail v0.3.0.
		project := fmt.Sprintf("/tmp/gctest-%s-%d", t.Name(), time.Now().UnixNano())

		wrapperPath := filepath.Join(dir, "mail-provider")
		wrapper := fmt.Sprintf("#!/usr/bin/env bash\n"+
			"export GC_MCP_MAIL_URL=%q\n"+
			"export GC_MCP_MAIL_PROJECT=%q\n"+
			"exec %q \"$@\"\n",
			serverURL, project, scriptPath)
		if err := os.WriteFile(wrapperPath, []byte(wrapper), 0o755); err != nil {
			t.Fatal(err)
		}

		return NewProvider(wrapperPath)
	})
}

// mcpServerReachable checks if the mcp_agent_mail health endpoint responds.
func mcpServerReachable(serverURL string) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(serverURL + "/health/liveness")
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return true
}

// startMCPServer starts mcp_agent_mail via python3 and registers cleanup.
// Skips the test if python3 or the module is not available.
func startMCPServer(t *testing.T, serverURL string) {
	t.Helper()

	python, err := osexec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not on PATH; mcp_agent_mail server not running")
	}

	// Verify the module is installed before starting.
	check := osexec.Command(python, "-c", "import mcp_agent_mail")
	if err := check.Run(); err != nil {
		t.Skip("mcp_agent_mail not installed; server not running")
	}

	cmd := osexec.Command(python, "-m", "mcp_agent_mail.http")
	cmd.Dir = t.TempDir()
	cmd.Stdout = os.Stderr // visible with -v
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Skipf("failed to start mcp_agent_mail: %v", err)
	}

	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	// Poll until server is ready.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if mcpServerReachable(serverURL) {
			t.Logf("mcp_agent_mail started (pid %d)", cmd.Process.Pid)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("mcp_agent_mail did not become ready within 15s")
}
