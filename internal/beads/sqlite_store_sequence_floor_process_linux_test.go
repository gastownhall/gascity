//go:build linux

package beads

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/testutil"
)

const (
	sqliteSequenceFloorChildDirEnv   = "GC_SQLITE_SEQUENCE_FLOOR_CHILD_DIR"
	sqliteSequenceFloorChildValueEnv = "GC_SQLITE_SEQUENCE_FLOOR_CHILD_VALUE"
	sqliteSequenceFloorBoundaryEnv   = "GC_SQLITE_SEQUENCE_FLOOR_BOUNDARY"
)

func TestSQLiteSequenceFloorHelperProcess(t *testing.T) {
	dir := os.Getenv(sqliteSequenceFloorChildDirEnv)
	if dir == "" {
		return
	}
	value, err := strconv.ParseInt(os.Getenv(sqliteSequenceFloorChildValueEnv), 10, 64)
	if err != nil {
		t.Fatalf("parsing sequence-floor child value: %v", err)
	}
	opened, err := OpenSQLiteStore(dir, WithSQLiteStoreIDPrefix(sqliteGraphPrefix))
	if err != nil {
		t.Fatalf("opening sequence-floor child store: %v", err)
	}
	store := opened.(*SQLiteStore)
	defer store.CloseStore() //nolint:errcheck
	boundary := os.Getenv(sqliteSequenceFloorBoundaryEnv)
	if boundary == "" {
		store.sequenceFloorBeforePersist = func() {
			_, _ = fmt.Fprintln(os.Stdout, "ready")
			scanner := bufio.NewScanner(os.Stdin)
			if !scanner.Scan() || scanner.Text() != "persist" {
				t.Fatal("sequence-floor child did not receive persist barrier")
			}
		}
	} else {
		hit := false
		observeSQLiteSequenceFloorBoundary = func(reached string) {
			if hit || reached != boundary {
				return
			}
			hit = true
			_, _ = fmt.Fprintln(os.Stdout, "ready")
			bufio.NewScanner(os.Stdin).Scan()
		}
	}
	if err := store.SetSequenceFloor(value); err != nil {
		t.Fatalf("setting sequence-floor child value %d: %v", value, err)
	}
	_, _ = fmt.Fprintln(os.Stdout, "persisted")
}

func TestSQLiteStoreSequenceFloorSIGKILLAtBoundaries(t *testing.T) {
	for _, boundary := range []string{
		"sequence-floor-lock-open",
		"sequence-floor-lock-held",
		"sequence-floor-temp-open",
		"sequence-floor-temp-close-before",
		"sequence-floor-temp-close-after",
		"sequence-floor-renamed",
		"sequence-floor-directory-open",
		"sequence-floor-directory-close-before",
		"sequence-floor-directory-close-after",
		"sequence-floor-lock-release-before",
		"sequence-floor-lock-release-after",
		"sequence-floor-lock-close-before",
		"sequence-floor-lock-close-after",
	} {
		t.Run(boundary, func(t *testing.T) {
			dir := t.TempDir()
			opened, err := OpenSQLiteStore(dir, WithSQLiteStoreIDPrefix(sqliteGraphPrefix))
			if err != nil {
				t.Fatalf("creating SQLite store: %v", err)
			}
			store := opened.(*SQLiteStore)
			if err := store.SetSequenceFloor(10); err != nil {
				t.Fatalf("setting initial sequence floor: %v", err)
			}
			if err := store.CloseStore(); err != nil {
				t.Fatalf("closing initial SQLite store: %v", err)
			}

			command := exec.Command(os.Args[0], "-test.run=^TestSQLiteSequenceFloorHelperProcess$")
			command.Env = append(os.Environ(), sqliteSequenceFloorBoundaryEnv+"="+boundary)
			child := startSQLiteSequenceFloorChild(t, command, dir, 50)
			child.kill()

			opened, err = OpenSQLiteStore(dir, WithSQLiteStoreIDPrefix(sqliteGraphPrefix))
			if err != nil {
				t.Fatalf("reopening SQLite store after SIGKILL: %v", err)
			}
			store = opened.(*SQLiteStore)
			if err := store.SetSequenceFloor(100); err != nil {
				_ = store.CloseStore()
				t.Fatalf("setting post-SIGKILL sequence floor: %v", err)
			}
			if err := store.CloseStore(); err != nil {
				t.Fatalf("closing post-SIGKILL SQLite store: %v", err)
			}
			contents, err := os.ReadFile(filepath.Join(dir, sqliteGraphSequenceFloorFilename))
			if err != nil {
				t.Fatalf("reading post-SIGKILL sequence floor: %v", err)
			}
			if string(contents) != "100\n" {
				t.Fatalf("post-SIGKILL sequence floor = %q, want canonical 100\\n", contents)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatalf("reading SQLite store directory: %v", err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), "."+sqliteGraphSequenceFloorFilename+"-") {
					t.Fatalf("SIGKILL at %q left sequence-floor temp residue %q", boundary, entry.Name())
				}
			}
		})
	}
}

func TestSQLiteStoreSetSequenceFloorNeverLowersAcrossProcesses(t *testing.T) {
	dir := t.TempDir()
	opened, err := OpenSQLiteStore(dir, WithSQLiteStoreIDPrefix(sqliteGraphPrefix))
	if err != nil {
		t.Fatalf("creating SQLite store: %v", err)
	}
	if err := opened.(*SQLiteStore).CloseStore(); err != nil {
		t.Fatalf("closing initial SQLite store: %v", err)
	}

	lower := startSQLiteSequenceFloorChild(
		t,
		exec.Command(os.Args[0], "-test.run=^TestSQLiteSequenceFloorHelperProcess$"),
		dir,
		50,
	)
	higher := startSQLiteSequenceFloorChild(
		t,
		exec.Command(os.Args[0], "-test.run=^TestSQLiteSequenceFloorHelperProcess$"),
		dir,
		100,
	)
	higher.persist(t)
	lower.persist(t)

	reopened, err := OpenSQLiteStore(dir, WithSQLiteStoreIDPrefix(sqliteGraphPrefix))
	if err != nil {
		t.Fatalf("reopening SQLite store: %v", err)
	}
	store := reopened.(*SQLiteStore)
	t.Cleanup(func() { _ = store.CloseStore() })
	floor, err := store.SequenceFloor()
	if err != nil {
		t.Fatalf("reading cross-process sequence floor: %v", err)
	}
	if floor != 100 {
		t.Fatalf("cross-process sequence floor = %d, want 100", floor)
	}
	contents, err := os.ReadFile(filepath.Join(dir, sqliteGraphSequenceFloorFilename))
	if err != nil {
		t.Fatalf("reading sequence-floor bytes: %v", err)
	}
	if string(contents) != "100\n" {
		t.Fatalf("sequence-floor bytes = %q, want canonical 100\\n", contents)
	}
	if _, err := os.Stat(filepath.Join(dir, sqliteGraphSequenceFloorFilename+".lock")); !os.IsNotExist(err) {
		t.Fatalf("sequence-floor persistence created a lock sidecar: %v", err)
	}
}

type sqliteSequenceFloorChild struct {
	command  *exec.Cmd
	stdin    io.WriteCloser
	stdout   *bufio.Scanner
	stderr   bytes.Buffer
	finished bool
}

func startSQLiteSequenceFloorChild(t *testing.T, command *exec.Cmd, dir string, value int64) *sqliteSequenceFloorChild {
	t.Helper()
	if command.Env == nil {
		command.Env = os.Environ()
	}
	command.Env = append(
		command.Env,
		sqliteSequenceFloorChildDirEnv+"="+dir,
		sqliteSequenceFloorChildValueEnv+"="+strconv.FormatInt(value, 10),
	)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatalf("creating sequence-floor child stdin: %v", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatalf("creating sequence-floor child stdout: %v", err)
	}
	child := &sqliteSequenceFloorChild{command: command, stdin: stdin, stdout: bufio.NewScanner(stdout)}
	command.Stderr = &child.stderr
	if err := command.Start(); err != nil {
		t.Fatalf("starting sequence-floor child: %v", err)
	}
	t.Cleanup(func() { child.kill() })
	if line := child.line(t); line != "ready" {
		t.Fatalf("sequence-floor child readiness = %q, want ready", line)
	}
	return child
}

func (c *sqliteSequenceFloorChild) persist(t *testing.T) {
	t.Helper()
	if _, err := io.WriteString(c.stdin, "persist\n"); err != nil {
		t.Fatalf("releasing sequence-floor child barrier: %v", err)
	}
	if line := c.line(t); line != "persisted" {
		t.Fatalf("sequence-floor child persistence = %q, want persisted", line)
	}
	if err := c.stdin.Close(); err != nil {
		t.Fatalf("closing sequence-floor child stdin: %v", err)
	}
	if err := c.command.Wait(); err != nil {
		t.Fatalf("waiting for sequence-floor child: %v\nstderr:\n%s", err, c.stderr.String())
	}
	c.finished = true
}

func (c *sqliteSequenceFloorChild) line(t *testing.T) string {
	t.Helper()
	lines := make(chan struct {
		line string
		ok   bool
	}, 1)
	go func() {
		ok := c.stdout.Scan()
		lines <- struct {
			line string
			ok   bool
		}{line: c.stdout.Text(), ok: ok}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), testutil.ExecRaceTimeout)
	defer cancel()
	select {
	case result := <-lines:
		if !result.ok {
			t.Fatalf("sequence-floor child exited without a protocol line: %v\nstderr:\n%s", c.stdout.Err(), c.stderr.String())
		}
		return result.line
	case <-ctx.Done():
		t.Fatalf("timed out waiting for sequence-floor child protocol after %s", testutil.ExecRaceTimeout)
		return ""
	}
}

func (c *sqliteSequenceFloorChild) kill() {
	if c.finished {
		return
	}
	_ = c.stdin.Close()
	if c.command.Process != nil {
		_ = c.command.Process.Kill()
	}
	_ = c.command.Wait()
	c.finished = true
}
