//go:build acceptance_a

// Dashboard acceptance tests.
//
// Issue #431: `gc dashboard` should work out of the box in a running city.
// Issue #432: if the user explicitly points `--api` at a dead endpoint, the
// command should fail fast instead of serving an empty dashboard.
package acceptance_test

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

func TestDashboard_DefaultCommand_WorksOutOfBoxUnderSupervisor(t *testing.T) {
	c := newShortDashboardCity(t)
	startOut := startCityUnderSupervisor(t, c)
	dashboardPort := reserveLoopbackPort(t)

	dashboard := startDashboardCommand(t, c, "dashboard", "--port", strconv.Itoa(dashboardPort))
	page, options := waitForHealthyDashboard(t, dashboard, dashboardPort, startOut)

	cityName := filepath.Base(c.Dir)
	if !strings.Contains(page, fmt.Sprintf(`meta name="selected-city" content="%s"`, cityName)) {
		t.Fatalf("dashboard did not default to the current city %q\npage:\n%s", cityName, page)
	}
	if strings.TrimSpace(options) == "{}" {
		t.Fatalf("dashboard /api/options stayed empty under supervisor auto-discovery\nstart output:\n%s\nlogs:\n%s", startOut, dashboard.logs(t))
	}
}

func TestDashboardServe_ExplicitDeadAPI_FailsFast(t *testing.T) {
	c := newShortDashboardCity(t)
	cityAPIPort := reserveLoopbackPort(t)
	c.AppendToConfig(fmt.Sprintf("\n[api]\nport = %d\n", cityAPIPort))
	startOut := startCityUnderSupervisor(t, c)
	dashboardPort := reserveLoopbackPort(t)

	apiURL := fmt.Sprintf("http://127.0.0.1:%d", cityAPIPort)
	dashboard := startDashboardCommand(t, c,
		"dashboard", "serve",
		"--port", strconv.Itoa(dashboardPort),
		"--api", apiURL,
	)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if exited, err := dashboard.exited(); exited {
			if err == nil {
				t.Fatalf("dashboard exited successfully for dead API override\nstart output:\n%s\nlogs:\n%s", startOut, dashboard.logs(t))
			}
			logs := strings.ToLower(dashboard.logs(t))
			if !strings.Contains(logs, "not reachable") &&
				!strings.Contains(logs, "connection refused") &&
				!strings.Contains(logs, "unreachable") &&
				!strings.Contains(logs, "failed to reach") {
				t.Fatalf("dashboard exited without a clear API connectivity error\nstart output:\n%s\nlogs:\n%s", startOut, dashboard.logs(t))
			}
			if strings.Contains(logs, "listening on http://localhost") {
				t.Fatalf("dashboard started serving before rejecting the dead API override\nstart output:\n%s\nlogs:\n%s", startOut, dashboard.logs(t))
			}
			return
		}
		time.Sleep(200 * time.Millisecond)
	}

	t.Fatalf("dashboard did not fail fast for dead API override\nstart output:\n%s\nlogs:\n%s", startOut, dashboard.logs(t))
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
		return !strings.Contains(out, "Controller: standalone")
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

func waitForHealthyDashboard(t *testing.T, dashboard *backgroundCmd, port int, startOut string) (string, string) {
	t.Helper()

	dashboardURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if exited, err := dashboard.exited(); exited {
			t.Fatalf("dashboard exited before becoming healthy: %v\nstart output:\n%s\nlogs:\n%s", err, startOut, dashboard.logs(t))
		}

		page, err := httpGetText(dashboardURL + "/")
		if err == nil {
			options, optErr := httpGetText(dashboardURL + "/api/options")
			if optErr == nil && dashboardLooksHealthy(page, options) {
				return page, options
			}
		}

		time.Sleep(200 * time.Millisecond)
	}

	t.Fatalf("dashboard never became healthy\nstart output:\n%s\nlogs:\n%s", startOut, dashboard.logs(t))
	return "", ""
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

func dashboardLooksHealthy(page, options string) bool {
	return strings.Contains(page, "💓 active") && strings.TrimSpace(options) != "{}"
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
