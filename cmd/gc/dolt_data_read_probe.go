package main

// managedDoltTableRef names one table in one database.
type managedDoltTableRef struct {
	DB    string
	Table string
}

// managedDoltDataReadOps is the I/O the data-read probe needs: table discovery
// and a per-database batch of reads.
type managedDoltDataReadOps struct {
	listTables func() ([]managedDoltTableRef, error)
	readBatch  func(db string, stmts []string) error
}

// managedDoltDataReadResult is the probe outcome. Err is a chunk-level read
// failure; Warning is an inconclusive one.
type managedDoltDataReadResult struct {
	Err     string
	Warning string
}

var managedDoltDataReadProbeFn = func(_, _, _ string) managedDoltDataReadResult {
	return managedDoltDataReadResult{}
}

func managedDoltIgnoredTableDiscoverySQL() string { return "" }

func managedDoltLeafReadStatementsFor(_ string, _ []string) []string { return nil }

func managedDoltDataReadProbeWith(_ managedDoltDataReadOps) managedDoltDataReadResult {
	return managedDoltDataReadResult{}
}
