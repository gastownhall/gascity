package sqlite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
)

const graphSequenceFloorFilename = "graph.seqfloor"

const sqliteSequenceFloorMaxBytes = 128

// sqliteSequenceFloorState records the authoritative allocator floor carried
// beside a Graph-compatible SQLite database. An absent sidecar is the valid
// genesis floor zero; a present sidecar must use the exact writer format.
type sqliteSequenceFloorState struct {
	present bool
	value   int64
}

func parseSQLiteSequenceFloor(contents []byte) (int64, error) {
	text := string(contents)
	if text == "" {
		return 0, fmt.Errorf("empty sequence floor")
	}
	if text[len(text)-1] != '\n' {
		return 0, fmt.Errorf("sequence floor lacks trailing newline")
	}
	value, err := strconv.ParseInt(text[:len(text)-1], 10, 64)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("invalid nonnegative sequence floor %q", text)
	}
	if text != strconv.FormatInt(value, 10)+"\n" {
		return 0, fmt.Errorf("non-canonical sequence floor %q", text)
	}
	return value, nil
}

func captureSQLiteSequenceFloorContext(ctx context.Context, path string, present bool, mode os.FileMode) (state sqliteSequenceFloorState, returnErr error) {
	if err := ctx.Err(); err != nil {
		return sqliteSequenceFloorState{}, err
	}
	if !present {
		return sqliteSequenceFloorState{}, nil
	}
	if !mode.IsRegular() {
		return sqliteSequenceFloorState{}, fmt.Errorf("inspecting SQLite sequence floor: not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return sqliteSequenceFloorState{}, fmt.Errorf("reading SQLite sequence floor: %w", err)
	}
	defer func() {
		if err := closeSQLiteCensusFile(file); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("closing SQLite sequence floor census descriptor: %w", err))
		}
	}()
	return captureSQLiteSequenceFloorFromFile(ctx, file)
}

func captureSQLiteSequenceFloorFromFile(ctx context.Context, file *os.File) (sqliteSequenceFloorState, error) {
	state, _, err := captureSQLiteSequenceFloorCensusFromFile(ctx, file)
	return state, err
}

// captureSQLiteSequenceFloorCensusFromFile reads the bounded sequence-floor
// representation once from an already-pinned descriptor. Its parsed value and
// hash therefore describe the same bytes, rather than two independently
// opened path lookups.
func captureSQLiteSequenceFloorCensusFromFile(ctx context.Context, file *os.File) (sqliteSequenceFloorState, string, error) {
	if file == nil {
		return sqliteSequenceFloorState{}, "", errors.New("reading SQLite sequence floor: missing census descriptor")
	}
	if err := ctx.Err(); err != nil {
		return sqliteSequenceFloorState{}, "", err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return sqliteSequenceFloorState{}, "", fmt.Errorf("seeking SQLite sequence floor: %w", err)
	}
	contents, err := readSQLiteSequenceFloorContext(ctx, file)
	if err != nil {
		return sqliteSequenceFloorState{}, "", fmt.Errorf("reading SQLite sequence floor: %w", err)
	}
	value, err := parseSQLiteSequenceFloor(contents)
	if err != nil {
		return sqliteSequenceFloorState{}, "", fmt.Errorf("reading SQLite sequence floor: %w", err)
	}
	sum := sha256.Sum256(contents)
	return sqliteSequenceFloorState{present: true, value: value}, hex.EncodeToString(sum[:]), nil
}

func readSQLiteSequenceFloorContext(ctx context.Context, source io.Reader) ([]byte, error) {
	contents := make([]byte, 0, sqliteSequenceFloorMaxBytes)
	buffer := make([]byte, 32)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		read, err := source.Read(buffer)
		if read > 0 {
			if len(contents)+read > sqliteSequenceFloorMaxBytes {
				return nil, errors.New("sequence floor exceeds bounded size")
			}
			contents = append(contents, buffer[:read]...)
		}
		if err == io.EOF {
			return contents, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

func (s sqliteSequenceFloorState) identity() string {
	if !s.present {
		return "absent"
	}
	return "present:" + strconv.FormatInt(s.value, 10)
}
