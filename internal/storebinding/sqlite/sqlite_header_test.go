package sqlite

// sqliteRollbackHeaderForTest builds a 100-byte SQLite database header whose
// read and write format version bytes both say 1, which is the rollback-journal
// mode rather than WAL. Nothing in it is platform-specific, so it lives here
// rather than beside any one platform's tests.
func sqliteRollbackHeaderForTest() []byte {
	header := make([]byte, 100)
	copy(header, "SQLite format 3\x00")
	header[18] = 1
	header[19] = 1
	return header
}
