package sqlite

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSQLiteWriterReservationPlanUsesJournalModeAndSidecarState(t *testing.T) {
	for _, scenario := range []struct {
		name        string
		writeFormat byte
		readFormat  byte
		wal         bool
		shm         bool
		journal     []byte
		want        sqliteWriterReservationPlan
		wantErr     bool
	}{
		{
			name:        "rollback journal allows readers",
			writeFormat: 1,
			readFormat:  1,
			want:        sqliteWriterReservationPlan{},
		},
		{
			name:        "live WAL locks index without pending byte",
			writeFormat: 2,
			readFormat:  2,
			wal:         true,
			shm:         true,
			want:        sqliteWriterReservationPlan{lockWALIndex: true},
		},
		{
			name:        "persistent WAL without sidecars blocks bootstrap",
			writeFormat: 2,
			readFormat:  2,
			want:        sqliteWriterReservationPlan{lockPending: true},
		},
		{
			name:        "recovery WAL without shared-memory index blocks bootstrap",
			writeFormat: 2,
			readFormat:  2,
			wal:         true,
			want:        sqliteWriterReservationPlan{lockPending: true},
		},
		{
			name:        "shared-memory index without WAL fails closed",
			writeFormat: 2,
			readFormat:  2,
			shm:         true,
			wantErr:     true,
		},
		{
			name:        "hot rollback journal is admitted for private recovery",
			writeFormat: 1,
			readFormat:  1,
			journal:     sqliteRollbackJournalForTest(),
			want:        sqliteWriterReservationPlan{},
		},
		{
			name:        "truncated rollback journal fails closed",
			writeFormat: 1,
			readFormat:  1,
			journal:     sqliteTruncatedRollbackJournalForTest(),
			wantErr:     true,
		},
		{
			name:        "rollback journal without a record is not hot",
			writeFormat: 1,
			readFormat:  1,
			journal:     sqliteRollbackJournalHeaderForTest(),
			wantErr:     true,
		},
		{
			name:        "rollback journal with nonzero header padding fails closed",
			writeFormat: 1,
			readFormat:  1,
			journal:     sqliteRollbackJournalWithMalformedPaddingForTest(),
			wantErr:     true,
		},
		{
			name:        "rollback journal with a corrupt page record fails closed",
			writeFormat: 1,
			readFormat:  1,
			journal:     sqliteRollbackJournalWithCorruptChecksumForTest(),
			wantErr:     true,
		},
		{
			name:        "valid rollback journal permits zeroed incomplete next segment",
			writeFormat: 1,
			readFormat:  1,
			journal:     sqliteRollbackJournalWithZeroedNextSegmentForTest(),
			want:        sqliteWriterReservationPlan{},
		},
		{
			name:        "valid rollback journal permits an incomplete next record",
			writeFormat: 1,
			readFormat:  1,
			journal:     sqliteRollbackJournalWithIncompleteNextRecordForTest(),
			want:        sqliteWriterReservationPlan{},
		},
		{
			name:        "malformed rollback journal fails closed",
			writeFormat: 1,
			readFormat:  1,
			journal:     []byte("not a rollback journal"),
			wantErr:     true,
		},
		{
			name:        "WAL header with rollback journal fails closed",
			writeFormat: 2,
			readFormat:  2,
			journal:     sqliteRollbackJournalHeaderForTest(),
			wantErr:     true,
		},
		{
			name:        "WAL namespace with rollback journal fails closed",
			writeFormat: 2,
			readFormat:  2,
			wal:         true,
			shm:         true,
			journal:     sqliteRollbackJournalHeaderForTest(),
			wantErr:     true,
		},
		{
			name:        "mixed journal-mode header fails closed",
			writeFormat: 1,
			readFormat:  2,
			wantErr:     true,
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			directory := t.TempDir()
			databasePath := filepath.Join(directory, graphFilename)
			if err := os.WriteFile(databasePath, sqliteHeaderForTest(scenario.writeFormat, scenario.readFormat), 0o600); err != nil {
				t.Fatalf("writing SQLite header: %v", err)
			}
			if scenario.wal {
				if err := os.WriteFile(databasePath+"-wal", sqliteWALHeaderForTest(), 0o600); err != nil {
					t.Fatalf("writing WAL sidecar: %v", err)
				}
			}
			if scenario.shm {
				if err := os.WriteFile(databasePath+"-shm", sqliteSHMHeaderForTest(), 0o600); err != nil {
					t.Fatalf("writing SHM sidecar: %v", err)
				}
			}
			if scenario.journal != nil {
				if err := os.WriteFile(databasePath+"-journal", scenario.journal, 0o600); err != nil {
					t.Fatalf("writing rollback journal: %v", err)
				}
			}

			database, err := os.Open(databasePath)
			if err != nil {
				t.Fatalf("opening SQLite source: %v", err)
			}
			t.Cleanup(func() { _ = database.Close() })
			var wal *os.File
			if scenario.wal {
				wal, err = os.Open(databasePath + "-wal")
				if err != nil {
					t.Fatalf("opening WAL: %v", err)
				}
				t.Cleanup(func() { _ = wal.Close() })
			}
			var shm *os.File
			if scenario.shm {
				shm, err = os.Open(databasePath + "-shm")
				if err != nil {
					t.Fatalf("opening SHM: %v", err)
				}
				t.Cleanup(func() { _ = shm.Close() })
			}
			var journal *os.File
			if scenario.journal != nil {
				journal, err = os.Open(databasePath + "-journal")
				if err != nil {
					t.Fatalf("opening rollback journal: %v", err)
				}
				t.Cleanup(func() { _ = journal.Close() })
			}

			got, err := sqliteWriterReservationPlanFor(context.Background(), database, wal, shm, journal)
			if scenario.wantErr {
				if err == nil {
					t.Fatal("sqliteWriterReservationPlanFor() unexpectedly succeeded")
				}
				return
			}
			if err != nil {
				t.Fatalf("sqliteWriterReservationPlanFor(): %v", err)
			}
			if got != scenario.want {
				t.Fatalf("sqliteWriterReservationPlanFor() = %#v, want %#v", got, scenario.want)
			}
		})
	}
}

func TestSQLiteWriterReservationPlanRejectsMalformedWALAndSHMHeaders(t *testing.T) {
	validDatabase := sqliteHeaderForTest(2, 2)
	validWAL := sqliteWALHeaderForTest()
	validSHM := sqliteSHMHeaderForTest()
	for _, scenario := range []struct {
		name        string
		database    []byte
		wal         []byte
		shm         []byte
		errorDetail string
	}{
		{name: "truncated database header", database: validDatabase[:99], errorDetail: "database header"},
		{name: "invalid database page size", database: sqliteHeaderWithPageSizeForTest(2, 2, 1000), errorDetail: "database page size"},
		{name: "truncated WAL header", database: validDatabase, wal: validWAL[:31], errorDetail: "truncated WAL header"},
		{name: "invalid WAL magic", database: validDatabase, wal: mutateSQLiteBytesForTest(validWAL, 0), errorDetail: "invalid WAL magic"},
		{name: "invalid WAL version", database: validDatabase, wal: sqliteWALHeaderForPageSizeAndVersionForTest(4096, 1), errorDetail: "WAL format version"},
		{name: "WAL page size mismatch", database: validDatabase, wal: sqliteWALHeaderForPageSizeAndVersionForTest(8192, sqliteWALFormatVersion), errorDetail: "does not match database page size"},
		{name: "invalid WAL checksum", database: validDatabase, wal: mutateSQLiteBytesForTest(validWAL, 24), errorDetail: "WAL header checksum"},
		{name: "truncated WAL frame", database: validDatabase, wal: append(append([]byte(nil), validWAL...), 1), errorDetail: "truncated WAL frame"},
		{name: "truncated SHM header", database: validDatabase, wal: validWAL, shm: validSHM[:135], errorDetail: "truncated WAL-index header"},
		{name: "contradictory SHM header copies", database: validDatabase, wal: validWAL, shm: mutateSQLiteBytesForTest(validSHM, sqliteWALIndexHeaderBytes), errorDetail: "header copies disagree"},
		{name: "invalid SHM version", database: validDatabase, wal: validWAL, shm: sqliteSHMHeaderMutationForTest(func(header []byte) {
			binary.NativeEndian.PutUint32(header[0:4], 1)
		}), errorDetail: "WAL-index format version"},
		{name: "uninitialized SHM", database: validDatabase, wal: validWAL, shm: sqliteSHMHeaderMutationForTest(func(header []byte) {
			header[12] = 0
		}), errorDetail: "not initialized"},
		{name: "SHM page size mismatch", database: validDatabase, wal: validWAL, shm: sqliteSHMHeaderMutationForTest(func(header []byte) {
			binary.NativeEndian.PutUint16(header[14:16], 8192)
		}), errorDetail: "does not match database page size"},
		// A WAL-index whose salts disagree with the WAL header describes a log
		// that does not exist yet, so it may not claim frames from this one.
		// The benign zero-claim form of this state has its own coverage in
		// TestSQLiteWriterReservationPlanAcceptsRestartHdrWindow.
		{name: "SHM salt mismatch", database: validDatabase, wal: validWAL, shm: sqliteSHMHeaderMutationForTest(func(header []byte) {
			binary.NativeEndian.PutUint32(header[16:20], 1)
			binary.NativeEndian.PutUint32(header[32:36], 0xaaaaaaaa)
		}), errorDetail: "salts do not match WAL"},
		{name: "invalid SHM checksum", database: validDatabase, wal: validWAL, shm: sqliteSHMChecksumCorruptionForTest(), errorDetail: "WAL-index header checksum"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			directory := t.TempDir()
			databasePath := filepath.Join(directory, graphFilename)
			if err := os.WriteFile(databasePath, scenario.database, 0o600); err != nil {
				t.Fatalf("writing database: %v", err)
			}
			database := openSQLitePlanComponentForTest(t, databasePath)
			var wal, shm *os.File
			if scenario.wal != nil {
				if err := os.WriteFile(databasePath+"-wal", scenario.wal, 0o600); err != nil {
					t.Fatalf("writing WAL: %v", err)
				}
				wal = openSQLitePlanComponentForTest(t, databasePath+"-wal")
			}
			if scenario.shm != nil {
				if err := os.WriteFile(databasePath+"-shm", scenario.shm, 0o600); err != nil {
					t.Fatalf("writing SHM: %v", err)
				}
				shm = openSQLitePlanComponentForTest(t, databasePath+"-shm")
			}
			_, err := sqliteWriterReservationPlanFor(context.Background(), database, wal, shm, nil)
			if err == nil || !strings.Contains(err.Error(), scenario.errorDetail) {
				t.Fatalf("sqliteWriterReservationPlanFor() error = %v, want detail %q", err, scenario.errorDetail)
			}
		})
	}
}

func TestSQLiteWriterReservationPlanValidatesWALIndexCommittedPrefix(t *testing.T) {
	validWAL, frameChecksums := sqliteWALWithFramesForTest(7, 0)
	for _, scenario := range []struct {
		name     string
		wal      []byte
		maxFrame uint32
		database uint32
		checksum [2]uint32
		wantErr  string
	}{
		{
			name:     "committed prefix with trailing uncommitted frame",
			wal:      validWAL,
			maxFrame: 1,
			database: 7,
			checksum: frameChecksums[0],
		},
		{
			name:     "prefix checksum from physical tail",
			wal:      validWAL,
			maxFrame: 1,
			database: 7,
			checksum: frameChecksums[1],
			wantErr:  "frame checksum does not match WAL",
		},
		{
			name:     "prefix database size mismatch",
			wal:      validWAL,
			maxFrame: 1,
			database: 8,
			checksum: frameChecksums[0],
			wantErr:  "database page count does not match WAL",
		},
		{
			name:     "advertised max frame is not committed",
			wal:      validWAL,
			maxFrame: 2,
			database: 7,
			checksum: frameChecksums[1],
			wantErr:  "max frame is not a commit frame",
		},
		{
			name: "committed frame follows advertised prefix",
			wal: func() []byte {
				wal, _ := sqliteWALWithFramesForTest(7, 8)
				return wal
			}(),
			maxFrame: 1,
			database: 7,
			checksum: frameChecksums[0],
			wantErr:  "committed frame follows WAL-index max frame",
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			directory := t.TempDir()
			databasePath := filepath.Join(directory, graphFilename)
			if err := os.WriteFile(databasePath, sqliteHeaderForTest(2, 2), 0o600); err != nil {
				t.Fatalf("writing database: %v", err)
			}
			if err := os.WriteFile(databasePath+"-wal", scenario.wal, 0o600); err != nil {
				t.Fatalf("writing WAL: %v", err)
			}
			shm := sqliteSHMHeaderMutationForTest(func(header []byte) {
				binary.NativeEndian.PutUint32(header[16:20], scenario.maxFrame)
				binary.NativeEndian.PutUint32(header[20:24], scenario.database)
				binary.NativeEndian.PutUint32(header[24:28], scenario.checksum[0])
				binary.NativeEndian.PutUint32(header[28:32], scenario.checksum[1])
			})
			if err := os.WriteFile(databasePath+"-shm", shm, 0o600); err != nil {
				t.Fatalf("writing SHM: %v", err)
			}

			_, err := sqliteWriterReservationPlanFor(
				context.Background(),
				openSQLitePlanComponentForTest(t, databasePath),
				openSQLitePlanComponentForTest(t, databasePath+"-wal"),
				openSQLitePlanComponentForTest(t, databasePath+"-shm"),
				nil,
			)
			if scenario.wantErr == "" {
				if err != nil {
					t.Fatalf("sqliteWriterReservationPlanFor(): %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), scenario.wantErr) {
				t.Fatalf("sqliteWriterReservationPlanFor() error = %v, want detail %q", err, scenario.wantErr)
			}
		})
	}
}

func openSQLitePlanComponentForTest(t *testing.T, path string) *os.File {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening %s: %v", filepath.Base(path), err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

func sqliteHeaderForTest(writeFormat, readFormat byte) []byte {
	return sqliteHeaderWithPageSizeForTest(writeFormat, readFormat, 4096)
}

func sqliteHeaderWithPageSizeForTest(writeFormat, readFormat byte, pageSize uint16) []byte {
	header := make([]byte, 100)
	copy(header, "SQLite format 3\x00")
	binary.BigEndian.PutUint16(header[16:18], pageSize)
	header[18] = writeFormat
	header[19] = readFormat
	return header
}

func sqliteWALHeaderForTest() []byte {
	return sqliteWALHeaderForPageSizeAndVersionForTest(4096, sqliteWALFormatVersion)
}

func sqliteWALHeaderForPageSizeAndVersionForTest(pageSize, version uint32) []byte {
	header := make([]byte, sqliteWALHeaderBytes)
	binary.BigEndian.PutUint32(header[0:4], sqliteWALMagicLittle)
	binary.BigEndian.PutUint32(header[4:8], version)
	binary.BigEndian.PutUint32(header[8:12], pageSize)
	binary.BigEndian.PutUint32(header[16:20], 0x13579bdf)
	binary.BigEndian.PutUint32(header[20:24], 0x2468ace0)
	checksum := sqliteRollingChecksum(binary.LittleEndian, header[:24], [2]uint32{})
	binary.BigEndian.PutUint32(header[24:28], checksum[0])
	binary.BigEndian.PutUint32(header[28:32], checksum[1])
	return header
}

func sqliteWALWithFramesForTest(databasePages ...uint32) ([]byte, [][2]uint32) {
	wal := sqliteWALHeaderForTest()
	checksum := [2]uint32{
		binary.BigEndian.Uint32(wal[24:28]),
		binary.BigEndian.Uint32(wal[28:32]),
	}
	checksums := make([][2]uint32, 0, len(databasePages))
	for index, pageCount := range databasePages {
		frame := make([]byte, sqliteWALFrameHeaderBytes+4096)
		binary.BigEndian.PutUint32(frame[0:4], uint32(index+1))
		binary.BigEndian.PutUint32(frame[4:8], pageCount)
		copy(frame[8:16], wal[16:24])
		for offset := sqliteWALFrameHeaderBytes; offset < len(frame); offset++ {
			frame[offset] = byte((index + offset) % 251)
		}
		checksum = sqliteRollingChecksum(binary.LittleEndian, frame[:8], checksum)
		checksum = sqliteRollingChecksum(binary.LittleEndian, frame[sqliteWALFrameHeaderBytes:], checksum)
		binary.BigEndian.PutUint32(frame[16:20], checksum[0])
		binary.BigEndian.PutUint32(frame[20:24], checksum[1])
		checksums = append(checksums, checksum)
		wal = append(wal, frame...)
	}
	return wal, checksums
}

func sqliteSHMHeaderForTest() []byte {
	shm := make([]byte, 32*1024)
	header := shm[:sqliteWALIndexHeaderBytes]
	binary.NativeEndian.PutUint32(header[0:4], sqliteWALFormatVersion)
	header[12] = 1
	header[13] = byte(sqliteWALMagicLittle & 1)
	binary.NativeEndian.PutUint16(header[14:16], 4096)
	binary.BigEndian.PutUint32(header[32:36], 0x13579bdf)
	binary.BigEndian.PutUint32(header[36:40], 0x2468ace0)
	checksum := sqliteRollingChecksum(binary.NativeEndian, header[:40], [2]uint32{})
	binary.NativeEndian.PutUint32(header[40:44], checksum[0])
	binary.NativeEndian.PutUint32(header[44:48], checksum[1])
	copy(shm[sqliteWALIndexHeaderBytes:sqliteWALIndexHeaderBytes*2], header)
	return shm
}

func sqliteSHMHeaderMutationForTest(mutate func([]byte)) []byte {
	shm := sqliteSHMHeaderForTest()
	header := shm[:sqliteWALIndexHeaderBytes]
	mutate(header)
	checksum := sqliteRollingChecksum(binary.NativeEndian, header[:40], [2]uint32{})
	binary.NativeEndian.PutUint32(header[40:44], checksum[0])
	binary.NativeEndian.PutUint32(header[44:48], checksum[1])
	copy(shm[sqliteWALIndexHeaderBytes:sqliteWALIndexHeaderBytes*2], header)
	return shm
}

func mutateSQLiteBytesForTest(source []byte, offset int) []byte {
	result := append([]byte(nil), source...)
	result[offset] ^= 0xff
	return result
}

func sqliteSHMChecksumCorruptionForTest() []byte {
	shm := sqliteSHMHeaderForTest()
	shm[40] ^= 0xff
	shm[sqliteWALIndexHeaderBytes+40] ^= 0xff
	return shm
}

func sqliteRollbackJournalHeaderForTest() []byte {
	header := make([]byte, 512)
	copy(header, []byte{0xd9, 0xd5, 0x05, 0xf9, 0x20, 0xa1, 0x63, 0xd7})
	binary.BigEndian.PutUint32(header[12:16], 0x13579bdf)
	binary.BigEndian.PutUint32(header[16:20], 1)
	binary.BigEndian.PutUint32(header[20:24], 512)
	binary.BigEndian.PutUint32(header[24:28], 4096)
	return header
}

func sqliteTruncatedRollbackJournalForTest() []byte {
	return sqliteRollbackJournalHeaderForTest()[:28]
}

func sqliteRollbackJournalForTest() []byte {
	journal := sqliteRollbackJournalHeaderForTest()
	binary.BigEndian.PutUint32(journal[8:12], 1)

	const pageSize = 4096
	record := make([]byte, 4+pageSize+4)
	binary.BigEndian.PutUint32(record[:4], 1)
	for index := 0; index < pageSize; index++ {
		record[4+index] = byte(index % 251)
	}
	binary.BigEndian.PutUint32(record[4+pageSize:], sqliteRollbackJournalChecksum(0x13579bdf, record[4:4+pageSize]))
	return append(journal, record...)
}

func sqliteRollbackJournalWithMalformedPaddingForTest() []byte {
	journal := sqliteRollbackJournalForTest()
	journal[28] = 1
	return journal
}

func sqliteRollbackJournalWithCorruptChecksumForTest() []byte {
	journal := sqliteRollbackJournalForTest()
	journal[len(journal)-1] ^= 0xff
	return journal
}

func sqliteRollbackJournalWithZeroedNextSegmentForTest() []byte {
	return append(sqliteRollbackJournalForTest(), make([]byte, 512)...)
}

func sqliteRollbackJournalWithIncompleteNextRecordForTest() []byte {
	return append(sqliteRollbackJournalForTest(), []byte{0, 0, 0, 4, 1, 2, 3, 4}...)
}

func sqliteRollbackJournalChecksum(seed uint32, page []byte) uint32 {
	checksum := seed
	for index := len(page) - 200; index > 0; index -= 200 {
		checksum += uint32(page[index])
	}
	return checksum
}
