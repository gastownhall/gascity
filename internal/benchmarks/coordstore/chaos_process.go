package coordstore

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"
)

// ChaosProcessConfig configures a re-exec chaos child process.
type ChaosProcessConfig struct {
	Backend         string
	SocketPath      string
	DataDir         string
	AckedWritesPath string
}

// ChaosProcess manages a re-exec child and forwards StoreAdapter calls through
// its ChaosClient.
type ChaosProcess struct {
	*ChaosClient

	cfg ChaosProcessConfig
	mu  sync.Mutex
	cmd *exec.Cmd
}

// NewChaosProcess returns an unstarted chaos child process manager.
func NewChaosProcess(cfg ChaosProcessConfig) *ChaosProcess {
	return &ChaosProcess{
		ChaosClient: NewChaosClient(ChaosClientConfig{
			SocketPath:      cfg.SocketPath,
			AckedWritesPath: cfg.AckedWritesPath,
		}),
		cfg: cfg,
	}
}

// Start launches the child process and waits for its READY line.
func (p *ChaosProcess) Start(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd != nil && p.cmd.Process != nil {
		return nil
	}
	cmd := exec.CommandContext(ctx, os.Args[0])
	cmd.Env = append(os.Environ(),
		"CHAOS_SERVER_BACKEND="+p.cfg.Backend,
		"CHAOS_SERVER_SOCKET="+p.cfg.SocketPath,
		"CHAOS_SERVER_DATA_DIR="+p.cfg.DataDir,
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("chaos stdout pipe: %w", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("chaos start: %w", err)
	}
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() {
		cmd.Process.Kill() //nolint:errcheck
		cmd.Wait()         //nolint:errcheck
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("chaos wait ready: %w", err)
		}
		return fmt.Errorf("chaos child exited before READY")
	}
	if line := scanner.Text(); line != "READY" {
		cmd.Process.Kill() //nolint:errcheck
		cmd.Wait()         //nolint:errcheck
		return fmt.Errorf("chaos child ready line %q, want READY", line)
	}
	p.cmd = cmd
	return nil
}

// Kill terminates the child process.
func (p *ChaosProcess) Kill(context.Context) error {
	p.mu.Lock()
	cmd := p.cmd
	p.cmd = nil
	p.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if err := cmd.Process.Kill(); err != nil {
		return fmt.Errorf("chaos kill: %w", err)
	}
	if err := cmd.Wait(); err != nil {
		return nil
	}
	return nil
}

// Restart starts a new child process and returns the restart duration.
func (p *ChaosProcess) Restart(ctx context.Context) (time.Duration, error) {
	start := time.Now()
	if err := p.Start(ctx); err != nil {
		return 0, err
	}
	return time.Since(start), nil
}

// Close kills the child process.
func (p *ChaosProcess) Close() error {
	return p.Kill(context.Background())
}
