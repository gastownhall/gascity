package main

import (
	"bytes"
	"errors"
	"io"
	"log"
	"os"
	"strings"
	"testing"
)

// Before reusing a pooled connection the driver checks it, and a connection
// the server already closed produces "closing bad idle connection: EOF". The
// driver returns ErrBadConn before sending anything and database/sql retries
// on a fresh connection, so the line carries no signal and is dropped. Every
// other driver message, including a read that fails mid-query, must pass.
func TestMySQLDriverLoggerDropsOnlyTheIdleCheckLine(t *testing.T) {
	t.Setenv("GC_DEBUG", "")
	var buf bytes.Buffer
	logger := filteredMySQLLogger{out: log.New(&buf, "[mysql] ", 0)}

	// The driver's call shape: mc.log("closing bad idle connection: ", err)
	// with a "file:line " prefix prepended.
	logger.Print("connection.go:801 ", "closing bad idle connection: ", io.EOF)
	if buf.Len() != 0 {
		t.Fatalf("idle-check line passed the filter:\n%s", buf.String())
	}

	for _, msg := range [][]any{
		{"packets.go:58 ", io.ErrUnexpectedEOF},
		{"packets.go:155 ", errors.New("write unix @->/tmp/dolt.sock: write: broken pipe")},
		{"auth.go:341 ", "unknown auth plugin:", "caching_sha3"},
	} {
		logger.Print(msg...)
	}
	for _, want := range []string{"unexpected EOF", "broken pipe", "unknown auth plugin"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("driver message %q was dropped:\n%s", want, buf.String())
		}
	}
}

func TestMySQLDriverLoggerKeepsEverythingUnderGCDebug(t *testing.T) {
	t.Setenv("GC_DEBUG", "1")
	var buf bytes.Buffer
	logger := filteredMySQLLogger{out: log.New(&buf, "[mysql] ", 0)}
	logger.Print("connection.go:801 ", "closing bad idle connection: ", io.EOF)
	if !strings.Contains(buf.String(), "closing bad idle connection: EOF") {
		t.Fatalf("GC_DEBUG dropped the idle-check line:\n%s", buf.String())
	}
}

// The driver copies its default logger into each connection config when the
// config is built, so the filtered logger only takes effect if it is installed
// before dispatch opens any store.
func TestMainExitCodeInstallsMySQLDriverLogger(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("reading main.go: %v", err)
	}
	body := string(source)
	start := strings.Index(body, "\nfunc mainExitCode(")
	if start < 0 {
		t.Fatal("could not find func mainExitCode in main.go")
	}
	fn := body[start+1:]
	if end := strings.Index(fn, "\nfunc "); end >= 0 {
		fn = fn[:end]
	}
	install := strings.Index(fn, "installMySQLDriverLogger()")
	dispatch := strings.Index(fn, "run(args, stdout, stderr)")
	if install < 0 || dispatch < 0 || install > dispatch {
		t.Fatal("mainExitCode must call installMySQLDriverLogger() before dispatching the command")
	}
}
