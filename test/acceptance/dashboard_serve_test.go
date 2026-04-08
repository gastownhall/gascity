//go:build acceptance_a

// Dashboard serve acceptance tests.
//
// Regression test for Issue #432: when a city runs under the default
// supervisor-managed flow, the per-city [api] port is ignored. The dashboard
// currently accepts that dead URL, starts anyway, and serves an empty page.
// The product requirement is looser than the implementation choice:
// the dashboard must either work end-to-end or fail fast with a clear API
// error, but it must not silently serve a degraded dashboard.
package acceptance_test

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

func TestDashboardServe_SupervisorManagedCity_DoesNotServeEmptyDashboard(t *testing.T) {
	shortRoot, err := os.MkdirTemp("", "gca-dashboard-*")
	if err != nil {
		t.Fatalf("creating short city root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(shortRoot) })

	c := helpers.NewCityInRoot(t, testEnv, shortRoot)
	c.Init("claude")

	cityAPIPort := reserveLoopbackPort(t)
	dashboardPort := reserveLoopbackPort(t)
	c.AppendToConfig(fmt.Sprintf("\n[api]\nport = %d\n", cityAPIPort))

	stopOut, stopErr := c.GC("stop", c.Dir)
	if stopErr != nil {
		t.Fatalf("gc stop before supervisor handoff failed: %v\n%s", stopErr, stopOut)
	}

	if !c.WaitForCondition(func() bool {
		out, err := c.GC("status", c.Dir)
		if err != nil {
			return false
		}
		return !strings.Contains(out, "Controller: standalone")
	}, 20*time.Second) {
		out, err := c.GC("status", c.Dir)
		t.Fatalf("standalone controller did not stop before supervisor handoff: %v\n%s", err, out)
	}

	startOut, startErr := c.GC("start", c.Dir)
	if startErr != nil {
		t.Fatalf("gc start under supervisor failed: %v\n%s", startErr, startOut)
	}

	apiURL := fmt.Sprintf("http://127.0.0.1:%d", cityAPIPort)
	dashboard := startDashboardServe(t, c, dashboardPort, apiURL)

	dashboardURL := fmt.Sprintf("http://127.0.0.1:%d", dashboardPort)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if exited, err := dashboard.exited(); exited {
			logs := dashboard.logs(t)
			if err == nil {
				t.Fatalf("dashboard exited successfully instead of connecting or failing with an API error:\n%s", logs)
			}
			lower := strings.ToLower(logs)
			if !strings.Contains(lower, "api") &&
				!strings.Contains(lower, "connection refused") &&
				!strings.Contains(lower, "unreachable") &&
				!strings.Contains(lower, "failed to reach") {
				t.Fatalf("dashboard exited without a clear API connectivity error:\n%s", logs)
			}
			return
		}

		page, err := httpGetText(dashboardURL + "/")
		if err == nil {
			options, _ := httpGetText(dashboardURL + "/api/options")
			if dashboardLooksHealthy(page, options) {
				return
			}
			if dashboardLooksEmpty(page, options) {
				t.Fatalf("dashboard served an empty/degraded page instead of working or failing fast\nstart output:\n%s\napi URL: %s\noptions: %s\nlogs:\n%s", startOut, apiURL, strings.TrimSpace(options), dashboard.logs(t))
			}
		}

		time.Sleep(200 * time.Millisecond)
	}

	t.Fatalf("dashboard never became healthy and never failed fast\nstart output:\n%s\nlogs:\n%s", startOut, dashboard.logs(t))
}

type backgroundCmd struct {
	cmd     *exec.Cmd
	logPath string
	done    chan struct{}
	waitErr error
}

func startDashboardServe(t *testing.T, c *helpers.City, port int, apiURL string) *backgroundCmd {
	t.Helper()

	gcPath, err := helpers.ResolveGCPath(c.Env)
	if err != nil {
		t.Fatal(err)
	}

	logFile, err := os.CreateTemp(c.Dir, "dashboard-serve-*.log")
	if err != nil {
		t.Fatalf("creating dashboard log file: %v", err)
	}

	cmd := exec.Command(gcPath, "dashboard", "serve", "--port", strconv.Itoa(port), "--api", apiURL)
	cmd.Dir = c.Dir
	cmd.Env = c.Env.List()
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		t.Fatalf("starting gc dashboard serve: %v", err)
	}

	bg := &backgroundCmd{
		cmd:     cmd,
		logPath: logFile.Name(),
		done:    make(chan struct{}),
	}
	go func() {
		bg.waitErr = cmd.Wait()
		_ = logFile.Close()
		close(bg.done)
	}()

	t.Cleanup(func() {
		if exited, _ := bg.exited(); exited {
			return
		}
		if bg.cmd.Process != nil {
			_ = bg.cmd.Process.Kill()
		}
		select {
		case <-bg.done:
		case <-time.After(5 * time.Second):
			t.Fatalf("dashboard process did not exit after kill: %s", bg.logPath)
		}
	})

	return bg
}

func (b *backgroundCmd) exited() (bool, error) {
	select {
	case <-b.done:
		return true, b.waitErr
	default:
		return false, nil
	}
}

func (b *backgroundCmd) logs(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(b.logPath)
	if err != nil {
		return fmt.Sprintf("reading %s: %v", b.logPath, err)
	}
	return string(data)
}

func dashboardLooksHealthy(page, options string) bool {
	return strings.Contains(page, "💓 active") && strings.TrimSpace(options) != "{}"
}

func dashboardLooksEmpty(page, options string) bool {
	trimmedOptions := strings.TrimSpace(options)
	return strings.Contains(page, "💓 no heartbeat") ||
		strings.Contains(page, "Workspace services fetch failed") ||
		trimmedOptions == "{}"
}

func httpGetText(rawURL string) (string, error) {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get(rawURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close() //nolint:errcheck

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: status %d: %s", rawURL, resp.StatusCode, string(body))
	}
	return string(body), nil
}

func reserveLoopbackPort(t *testing.T) int {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving port: %v", err)
	}
	port := lis.Addr().(*net.TCPAddr).Port
	if err := lis.Close(); err != nil {
		t.Fatalf("closing reserved port listener: %v", err)
	}
	return port
}
