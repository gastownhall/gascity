//go:build acceptance_a

// Dashboard acceptance tests.
//
// The dashboard is now a TypeScript SPA served as a static bundle by
// `gc dashboard`. The Go layer has no data proxy; the SPA calls the
// supervisor's typed OpenAPI endpoints directly from the browser.
// These tests assert the minimum the static server promises:
//
//   - The SPA index loads and carries a <meta name="supervisor-url">
//     tag so the SPA can reach the supervisor.
//   - The compiled bundle assets (dashboard.js, dashboard.css) are
//     served.
//   - The legacy /api/* proxy is gone — hitting any of those paths
//     should 404.
//
// The behavioral tests that previously asserted on rendered HTML
// (selected-city meta, 💓 heartbeat banner, /api/options payload)
// belong in the browser-level test suite now — they depend on the
// live supervisor + SPA rendering, not on the Go static server's
// contract.
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

func TestDashboard_ServesSPABundle(t *testing.T) {
	c := newShortDashboardCity(t)
	startOut := startCityUnderSupervisor(t, c)
	dashboardPort := reserveLoopbackPort(t)

	dashboard := startDashboardCommand(t, c, "dashboard", "--port", strconv.Itoa(dashboardPort))
	waitForDashboardReady(t, dashboard, dashboardPort, startOut)

	base := fmt.Sprintf("http://127.0.0.1:%d", dashboardPort)

	// The SPA index must embed a supervisor-url meta tag that points
	// at a non-empty URL — that's how the SPA discovers the
	// supervisor at page-load time.
	page, err := httpGetText(base + "/")
	if err != nil {
		t.Fatalf("GET /: %v\nlogs:\n%s", err, dashboard.logs(t))
	}
	if !strings.Contains(page, `<meta name="supervisor-url"`) {
		t.Fatalf("index missing supervisor-url meta tag; body=\n%s", page)
	}
	if strings.Contains(page, `<meta name="supervisor-url" content="">`) {
		t.Fatalf("supervisor-url meta tag was not injected; body=\n%s", page)
	}

	// The compiled JS bundle and CSS must be served; without them
	// the SPA cannot render anything.
	if _, err := httpGetText(base + "/dashboard.js"); err != nil {
		t.Errorf("GET /dashboard.js: %v", err)
	}
	if _, err := httpGetText(base + "/dashboard.css"); err != nil {
		t.Errorf("GET /dashboard.css: %v", err)
	}

	// The old Go proxy surface is gone. /api/* is a reserved non-SPA
	// prefix: stale callers get an explicit 404 rather than silently
	// receiving the SPA index.html, which would mask migration breakage.
	for _, path := range []string{"/api/run", "/api/commands", "/api/options", "/api/mail/inbox"} {
		resp, err := http.Get(base + path) //nolint:gosec // acceptance test against localhost
		if err != nil {
			t.Errorf("GET %s: %v", path, err)
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: expected 404, got %d", path, resp.StatusCode)
		}
	}
}

type backgroundCmd struct {
	cmd     *exec.Cmd
	logPath string
	done    chan struct{}
	waitErr error
}

func newShortDashboardCity(t *testing.T) *helpers.City {
	t.Helper()

	shortRoot, err := os.MkdirTemp("", "gca-dashboard-*")
	if err != nil {
		t.Fatalf("creating short city root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(shortRoot) })

	c := helpers.NewCityInRoot(t, testEnv, shortRoot)
	c.Init("claude")
	return c
}

func startCityUnderSupervisor(t *testing.T, c *helpers.City) string {
	t.Helper()

	stopOut, stopErr := c.GC("stop", c.Dir)
	if stopErr != nil {
		t.Fatalf("gc stop before supervisor handoff failed: %v\n%s", stopErr, stopOut)
	}

	if !c.WaitForCondition(func() bool {
		out, err := c.GC("status", c.Dir)
		if err != nil {
			return false
		}
		return !strings.Contains(out, "Controller: standalone-managed")
	}, 20*time.Second) {
		out, err := c.GC("status", c.Dir)
		t.Fatalf("standalone controller did not stop before supervisor handoff: %v\n%s", err, out)
	}

	startOut, startErr := c.GC("start", c.Dir)
	if startErr != nil {
		t.Fatalf("gc start under supervisor failed: %v\n%s", startErr, startOut)
	}
	return startOut
}

// waitForDashboardReady polls the dashboard root until it returns a
// document containing the SPA shell. We key on the presence of the
// <meta name="supervisor-url"> tag since that's the server's only
// dynamic responsibility.
func waitForDashboardReady(t *testing.T, dashboard *backgroundCmd, port int, startOut string) {
	t.Helper()

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if exited, err := dashboard.exited(); exited {
			t.Fatalf("dashboard exited before serving index: %v\nstart output:\n%s\nlogs:\n%s", err, startOut, dashboard.logs(t))
		}
		page, err := httpGetText(base + "/")
		if err == nil && strings.Contains(page, `<meta name="supervisor-url"`) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("dashboard did not serve SPA index in time\nstart output:\n%s\nlogs:\n%s", startOut, dashboard.logs(t))
}

func startDashboardCommand(t *testing.T, c *helpers.City, args ...string) *backgroundCmd {
	t.Helper()

	gcPath, err := helpers.ResolveGCPath(c.Env)
	if err != nil {
		t.Fatal(err)
	}

	logFile, err := os.CreateTemp(c.Dir, "dashboard-*.log")
	if err != nil {
		t.Fatalf("creating dashboard log file: %v", err)
	}

	cmd := exec.Command(gcPath, args...)
	cmd.Dir = c.Dir
	cmd.Env = c.Env.List()
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		t.Fatalf("starting %q: %v", strings.Join(args, " "), err)
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

func httpGetText(rawURL string) (string, error) {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get(rawURL) //nolint:gosec // acceptance test against localhost
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
