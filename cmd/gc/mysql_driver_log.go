package main

import (
	"fmt"
	"log"
	"os"
	"strings"

	mysql "github.com/go-sql-driver/mysql"
)

// The go-sql-driver logger writes connection events straight to stderr. One of
// them carries no signal: before reusing a pooled connection the driver checks
// it, and when the server has already closed it the driver logs "closing bad
// idle connection: EOF" and returns driver.ErrBadConn. Nothing has been sent
// on that connection yet, so database/sql discards it and retries on a fresh
// one; the caller never sees an error. After a Dolt restart, or whenever the
// server reaps idle connections first, that line repeats for every pooled
// connection. Drop exactly that line. Every other driver message still
// reaches stderr in the driver's own format, and GC_DEBUG restores the full
// stream.

const mysqlBenignIdleCheckMessage = "closing bad idle connection"

type filteredMySQLLogger struct{ out *log.Logger }

// Print implements mysql.Logger.
func (l filteredMySQLLogger) Print(v ...any) {
	if !gcDebugEnabled() && strings.Contains(fmt.Sprint(v...), mysqlBenignIdleCheckMessage) {
		return
	}
	l.out.Print(v...)
}

// installMySQLDriverLogger replaces the driver's default logger. The driver
// copies its default logger into each connection config when a DSN is parsed
// or a config is built, so this must run before anything opens a MySQL
// connection; mainExitCode calls it first.
func installMySQLDriverLogger() {
	_ = mysql.SetLogger(filteredMySQLLogger{out: log.New(os.Stderr, "[mysql] ", log.Ldate|log.Ltime)})
}
