//go:build linux

package beads

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

const (
	sqliteSequenceMintChildDirEnv   = "GC_SQLITE_SEQUENCE_MINT_CHILD_DIR"
	sqliteSequenceMintChildCountEnv = "GC_SQLITE_SEQUENCE_MINT_CHILD_COUNT"
)

// TestSQLiteSequenceMintHelperProcess is the child of
// TestSQLiteSequenceProcessesNeverMintSameID: it mints ids and prints them.
func TestSQLiteSequenceMintHelperProcess(t *testing.T) {
	dir := os.Getenv(sqliteSequenceMintChildDirEnv)
	if dir == "" {
		return
	}
	count, err := strconv.Atoi(os.Getenv(sqliteSequenceMintChildCountEnv))
	if err != nil {
		t.Fatalf("parsing mint count: %v", err)
	}
	opened, err := OpenSQLiteStore(dir, WithSQLiteStoreIDPrefix(sqliteGraphPrefix))
	if err != nil {
		t.Fatalf("opening mint child store: %v", err)
	}
	store := opened.(*SQLiteStore)
	defer store.CloseStore() //nolint:errcheck
	out := bufio.NewWriter(os.Stdout)
	for i := 0; i < count; i++ {
		// Mix committed creates with bare reservations so blocks turn over.
		id, err := store.nextID()
		if i%64 == 0 {
			var created Bead
			created, err = store.Create(Bead{Title: "child"})
			id = created.ID
		}
		if err != nil {
			t.Fatalf("minting in child: %v", err)
		}
		_, _ = out.WriteString("id " + id + "\n")
	}
	_ = out.Flush()
}

func TestSQLiteSequenceProcessesNeverMintSameID(t *testing.T) {
	dir := t.TempDir()
	seed := openSeqStore(t, dir)
	if err := seed.CloseStore(); err != nil {
		t.Fatal(err)
	}
	const children = 3
	count := int(2*sqliteSequenceBlockSize + 17)
	type child struct {
		cmd    *exec.Cmd
		stdout bytes.Buffer
		stderr bytes.Buffer
	}
	kids := make([]*child, children)
	for i := range kids {
		c := &child{cmd: exec.Command(os.Args[0], "-test.run=^TestSQLiteSequenceMintHelperProcess$")}
		c.cmd.Env = append(os.Environ(),
			sqliteSequenceMintChildDirEnv+"="+dir,
			sqliteSequenceMintChildCountEnv+"="+strconv.Itoa(count),
		)
		c.cmd.Stdout = &c.stdout
		c.cmd.Stderr = &c.stderr
		if err := c.cmd.Start(); err != nil {
			t.Fatalf("starting mint child: %v", err)
		}
		kids[i] = c
	}
	seen := map[string]int{}
	for i, c := range kids {
		if err := c.cmd.Wait(); err != nil {
			t.Fatalf("mint child %d: %v\nstdout:\n%s\nstderr:\n%s", i, err, c.stdout.String(), c.stderr.String())
		}
		got := 0
		for _, line := range strings.Split(c.stdout.String(), "\n") {
			id, ok := strings.CutPrefix(line, "id ")
			if !ok {
				continue
			}
			got++
			if prev, dup := seen[id]; dup {
				t.Fatalf("processes %d and %d both minted %q", prev, i, id)
			}
			seen[id] = i
		}
		if got != count {
			t.Fatalf("mint child %d printed %d ids, want %d\nstderr:\n%s", i, got, count, c.stderr.String())
		}
	}
}
