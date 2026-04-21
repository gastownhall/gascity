package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

type blockingStatusProvider struct {
	*runtime.Fake
	delay time.Duration
}

func newBlockingStatusProvider(delay time.Duration) *blockingStatusProvider {
	return &blockingStatusProvider{
		Fake:  runtime.NewFake(),
		delay: delay,
	}
}

func (p *blockingStatusProvider) IsRunning(name string) bool {
	time.Sleep(p.delay)
	return p.Fake.IsRunning(name)
}

func (p *blockingStatusProvider) IsAttached(name string) bool {
	time.Sleep(p.delay)
	return p.Fake.IsAttached(name)
}

func (p *blockingStatusProvider) ProcessAlive(name string, processNames []string) bool {
	time.Sleep(p.delay)
	return p.Fake.ProcessAlive(name, processNames)
}

func (p *blockingStatusProvider) ListRunning(prefix string) ([]string, error) {
	time.Sleep(p.delay)
	return p.Fake.ListRunning(prefix)
}

func (p *blockingStatusProvider) GetLastActivity(name string) (time.Time, error) {
	time.Sleep(p.delay)
	return p.Fake.GetLastActivity(name)
}

func TestDoCityStatusTimesOutSlowProvider(t *testing.T) {
	oldTimeout := statusProviderCallTimeout
	statusProviderCallTimeout = 20 * time.Millisecond
	t.Cleanup(func() { statusProviderCallTimeout = oldTimeout })

	base := newBlockingStatusProvider(200 * time.Millisecond)
	if err := base.Start(context.Background(), "mayor", runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}
	sp := &statusProvider{base: base}
	dops := newFakeDrainOps()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "city"},
		Agents: []config.Agent{
			{Name: "mayor", MaxActiveSessions: intPtr(1)},
		},
	}

	start := time.Now()
	var stdout, stderr bytes.Buffer
	code := doCityStatus(sp, dops, cfg, "/tmp/city", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		t.Fatalf("doCityStatus took %v, want <150ms", elapsed)
	}
	if !strings.Contains(stdout.String(), "0/1 agents running") {
		t.Fatalf("stdout missing degraded status summary, got:\n%s", stdout.String())
	}
}

func TestDoRigStatusTimesOutSlowProvider(t *testing.T) {
	oldTimeout := statusProviderCallTimeout
	statusProviderCallTimeout = 20 * time.Millisecond
	t.Cleanup(func() { statusProviderCallTimeout = oldTimeout })

	base := newBlockingStatusProvider(200 * time.Millisecond)
	if err := base.Start(context.Background(), "frontend--polecat", runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}
	sp := &statusProvider{base: base}
	dops := newFakeDrainOps()
	rig := config.Rig{Name: "frontend", Path: "/tmp/frontend"}
	agents := []config.Agent{
		{Name: "polecat", Dir: "frontend", MaxActiveSessions: intPtr(1)},
	}

	start := time.Now()
	var stdout, stderr bytes.Buffer
	code := doRigStatus(sp, dops, rig, agents, "", "city", "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		t.Fatalf("doRigStatus took %v, want <150ms", elapsed)
	}
	if !strings.Contains(stdout.String(), "stopped") {
		t.Fatalf("stdout missing degraded stopped status, got:\n%s", stdout.String())
	}
}
