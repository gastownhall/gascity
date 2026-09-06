package sqlite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
)

const (
	sqliteDatabaseHeaderBytes = 100
	sqliteWALHeaderBytes      = 32
	sqliteWALFrameHeaderBytes = 24
	sqliteWALIndexHeaderBytes = 48
	sqliteWALIndexHeaderCount = 2
	sqliteWALIndexMinimumSize = 136
	sqliteWALFormatVersion    = 3007000
	sqliteWALMagicLittle      = 0x377f0682
	sqliteWALMagicBig         = 0x377f0683
)

type sqliteDatabaseHeader struct {
	pageSize    uint32
	writeFormat byte
	readFormat  byte
}

type sqliteWALHeader struct {
	magic      uint32
	pageSize   uint32
	salt       [2]uint32
	checksum   [2]uint32
	frameCount uint32
}

type sqliteWALIndexHeader struct {
	version        uint32
	initialized    byte
	bigEndChecksum byte
	pageSize       uint32
	maxFrame       uint32
	databasePages  uint32
	frameChecksum  [2]uint32
	salt           [2]uint32
}

// sqliteWALFrameEvidence describes the valid frame chain — the frames SQLite's
// own recovery would replay. walIndexRecover stops at the first frame whose
// salts, page number, or checksum break the chain and treats that
// discontinuity as the end of the log rather than as corruption, so physical
// frames past the break are unreachable. A WAL that a checkpoint restarted
// keeps stale-salt trailing frames as its routine steady state.
type sqliteWALFrameEvidence struct {
	validFrames            uint32
	terminator             string
	prefixChecksum         [2]uint32
	prefixDatabasePages    uint32
	committedAfterMaxFrame bool
}

// chainDetail describes how the valid chain ended, for rejection diagnostics.
func (e sqliteWALFrameEvidence) chainDetail(header sqliteWALHeader) string {
	if e.terminator == "" {
		return fmt.Sprintf("%d of %d frames valid", e.validFrames, header.frameCount)
	}
	return fmt.Sprintf("%d of %d frames valid (%s)", e.validFrames, header.frameCount, e.terminator)
}

func readSQLiteDatabaseHeader(ctx context.Context, file *os.File) (sqliteDatabaseHeader, error) {
	if err := ctx.Err(); err != nil {
		return sqliteDatabaseHeader{}, err
	}
	if file == nil {
		return sqliteDatabaseHeader{}, errors.New("missing pinned database descriptor")
	}
	var raw [sqliteDatabaseHeaderBytes]byte
	if _, err := file.ReadAt(raw[:], 0); err != nil {
		return sqliteDatabaseHeader{}, fmt.Errorf("reading database header: %w", err)
	}
	if string(raw[:16]) != "SQLite format 3\x00" {
		return sqliteDatabaseHeader{}, errors.New("invalid database magic")
	}
	pageSize, err := decodeSQLitePageSize(binary.BigEndian.Uint16(raw[16:18]))
	if err != nil {
		return sqliteDatabaseHeader{}, fmt.Errorf("invalid database page size: %w", err)
	}
	return sqliteDatabaseHeader{
		pageSize:    pageSize,
		writeFormat: raw[18],
		readFormat:  raw[19],
	}, nil
}

func validateSQLiteWALState(ctx context.Context, database sqliteDatabaseHeader, wal, shm *os.File) error {
	if wal == nil {
		if shm != nil {
			return errors.New("WAL-index exists without WAL")
		}
		return nil
	}
	empty, err := sqliteWALIsTruncated(wal)
	if err != nil {
		return fmt.Errorf("reading SQLite WAL header: %w", err)
	}
	if empty {
		return validateTruncatedSQLiteWALState(ctx, database, shm)
	}
	walHeader, err := readSQLiteWALHeader(ctx, wal)
	if err != nil {
		return fmt.Errorf("reading SQLite WAL header: %w", err)
	}
	if walHeader.pageSize != database.pageSize {
		return fmt.Errorf("reading SQLite WAL header: page size %d does not match database page size %d", walHeader.pageSize, database.pageSize)
	}
	if shm == nil {
		if _, err := validateSQLiteWALFrames(ctx, wal, walHeader, 0); err != nil {
			return fmt.Errorf("reading SQLite WAL frames: %w", err)
		}
		return nil
	}
	indexHeader, err := readSQLiteWALIndexHeader(ctx, shm)
	if err != nil {
		return fmt.Errorf("reading SQLite WAL-index header: %w", err)
	}
	if indexHeader.pageSize != database.pageSize {
		return fmt.Errorf("reading SQLite WAL-index header: page size %d does not match database page size %d", indexHeader.pageSize, database.pageSize)
	}
	if indexHeader.salt != walHeader.salt {
		// walRestartHdr publishes the next log's salts in the WAL-index before
		// the writer that restarts the log stamps the matching WAL header. An
		// index caught in that window claims no frames, so it does not
		// describe this WAL file at all and the file's bytes are unreachable:
		// the next writer overwrites them from the header down.
		if indexHeader.maxFrame != 0 {
			return errors.New("reading SQLite WAL-index header: salts do not match WAL")
		}
		return nil
	}
	if indexHeader.bigEndChecksum != byte(walHeader.magic&1) {
		return errors.New("reading SQLite WAL-index header: checksum byte order does not match WAL")
	}
	frameEvidence, err := validateSQLiteWALFrames(ctx, wal, walHeader, indexHeader.maxFrame)
	if err != nil {
		return fmt.Errorf("reading SQLite WAL frames: %w", err)
	}
	if indexHeader.maxFrame > frameEvidence.validFrames {
		return fmt.Errorf("reading SQLite WAL-index header: max frame %d is beyond the valid WAL chain: %s", indexHeader.maxFrame, frameEvidence.chainDetail(walHeader))
	}
	if indexHeader.maxFrame == 0 {
		if frameEvidence.committedAfterMaxFrame {
			return errors.New("reading SQLite WAL-index header: committed frame follows WAL-index max frame")
		}
		return nil
	}
	if indexHeader.frameChecksum != frameEvidence.prefixChecksum {
		return errors.New("reading SQLite WAL-index header: frame checksum does not match WAL")
	}
	if frameEvidence.prefixDatabasePages == 0 {
		return errors.New("reading SQLite WAL-index header: max frame is not a commit frame")
	}
	if indexHeader.databasePages != frameEvidence.prefixDatabasePages {
		return errors.New("reading SQLite WAL-index header: database page count does not match WAL")
	}
	if frameEvidence.committedAfterMaxFrame {
		return errors.New("reading SQLite WAL-index header: committed frame follows WAL-index max frame")
	}
	return nil
}

// sqliteWALIsTruncated reports the zero-length log that
// wal_checkpoint(TRUNCATE) leaves behind, which carries no frames to validate:
// walIndexRecover skips its header and frame scan entirely for a WAL that
// small and rebuilds an empty index. SQLite treats every WAL shorter than its
// header the same way, but only the zero-length form is a state a checkpoint
// produces, so a partially written header stays out of this branch and keeps
// failing closed in readSQLiteWALHeader as the crash residue it is.
func sqliteWALIsTruncated(file *os.File) (bool, error) {
	if file == nil {
		return false, errors.New("missing pinned WAL descriptor")
	}
	info, err := file.Stat()
	if err != nil {
		return false, fmt.Errorf("stating WAL: %w", err)
	}
	if !info.Mode().IsRegular() {
		return false, errors.New("WAL is not a regular file")
	}
	return info.Size() == 0, nil
}

// validateTruncatedSQLiteWALState admits a zero-length WAL whose surviving
// WAL-index agrees that the log holds nothing. An index that still claims
// frames contradicts the file and fails closed.
func validateTruncatedSQLiteWALState(ctx context.Context, database sqliteDatabaseHeader, shm *os.File) error {
	if shm == nil {
		return nil
	}
	indexHeader, err := readSQLiteWALIndexHeader(ctx, shm)
	if err != nil {
		return fmt.Errorf("reading SQLite WAL-index header: %w", err)
	}
	if indexHeader.pageSize != database.pageSize {
		return fmt.Errorf("reading SQLite WAL-index header: page size %d does not match database page size %d", indexHeader.pageSize, database.pageSize)
	}
	if indexHeader.maxFrame != 0 {
		return fmt.Errorf("reading SQLite WAL-index header: max frame %d is beyond the valid WAL chain: WAL is truncated to zero frames", indexHeader.maxFrame)
	}
	return nil
}

func readSQLiteWALHeader(ctx context.Context, file *os.File) (sqliteWALHeader, error) {
	if err := ctx.Err(); err != nil {
		return sqliteWALHeader{}, err
	}
	if file == nil {
		return sqliteWALHeader{}, errors.New("missing pinned WAL descriptor")
	}
	info, err := file.Stat()
	if err != nil {
		return sqliteWALHeader{}, fmt.Errorf("stating WAL: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() < sqliteWALHeaderBytes {
		return sqliteWALHeader{}, errors.New("truncated WAL header")
	}
	var raw [sqliteWALHeaderBytes]byte
	if _, err := file.ReadAt(raw[:], 0); err != nil {
		return sqliteWALHeader{}, fmt.Errorf("reading WAL header: %w", err)
	}
	magic := binary.BigEndian.Uint32(raw[0:4])
	order, err := sqliteWALChecksumByteOrder(magic)
	if err != nil {
		return sqliteWALHeader{}, err
	}
	if version := binary.BigEndian.Uint32(raw[4:8]); version != sqliteWALFormatVersion {
		return sqliteWALHeader{}, fmt.Errorf("unsupported WAL format version %d", version)
	}
	pageSize, err := decodeSQLitePageSize32(binary.BigEndian.Uint32(raw[8:12]))
	if err != nil {
		return sqliteWALHeader{}, fmt.Errorf("invalid WAL page size: %w", err)
	}
	wantChecksum := sqliteRollingChecksum(order, raw[:24], [2]uint32{})
	gotChecksum := [2]uint32{
		binary.BigEndian.Uint32(raw[24:28]),
		binary.BigEndian.Uint32(raw[28:32]),
	}
	if gotChecksum != wantChecksum {
		return sqliteWALHeader{}, errors.New("invalid WAL header checksum")
	}
	frameSize := int64(sqliteWALFrameHeaderBytes) + int64(pageSize)
	payloadSize := info.Size() - sqliteWALHeaderBytes
	if payloadSize%frameSize != 0 {
		return sqliteWALHeader{}, errors.New("truncated WAL frame")
	}
	frameCount := payloadSize / frameSize
	if frameCount > int64(^uint32(0)) {
		return sqliteWALHeader{}, errors.New("WAL frame count overflow")
	}
	return sqliteWALHeader{
		magic:    magic,
		pageSize: pageSize,
		salt: [2]uint32{
			binary.BigEndian.Uint32(raw[16:20]),
			binary.BigEndian.Uint32(raw[20:24]),
		},
		checksum:   gotChecksum,
		frameCount: uint32(frameCount),
	}, nil
}

// validateSQLiteWALFrames walks the WAL the way walIndexRecover does: it
// accumulates the rolling checksum over frames until one fails the salt, page
// number, or checksum test, and reports that frame as the end of the log
// instead of as a fault. Callers decide whether the WAL-index header's claims
// fit inside the chain this reports; frames past the chain are unreachable and
// carry no evidence.
func validateSQLiteWALFrames(ctx context.Context, file *os.File, header sqliteWALHeader, maxFrame uint32) (sqliteWALFrameEvidence, error) {
	order, err := sqliteWALChecksumByteOrder(header.magic)
	if err != nil {
		return sqliteWALFrameEvidence{}, err
	}
	checksum := header.checksum
	frame := make([]byte, sqliteWALFrameHeaderBytes+int(header.pageSize))
	var evidence sqliteWALFrameEvidence
	for frameIndex := uint32(0); frameIndex < header.frameCount; frameIndex++ {
		if err := ctx.Err(); err != nil {
			return sqliteWALFrameEvidence{}, err
		}
		offset := int64(sqliteWALHeaderBytes) + int64(frameIndex)*int64(len(frame))
		if _, err := file.ReadAt(frame, offset); err != nil {
			return sqliteWALFrameEvidence{}, err
		}
		frameNumber := frameIndex + 1
		// The salt, page number, and checksum tests run in walDecodeFrame's
		// order so the reported terminator names the same discontinuity SQLite
		// would stop at.
		frameSalt := [2]uint32{
			binary.BigEndian.Uint32(frame[8:12]),
			binary.BigEndian.Uint32(frame[12:16]),
		}
		if frameSalt != header.salt {
			evidence.terminator = fmt.Sprintf("frame %d salts do not match WAL header", frameNumber)
			break
		}
		if pageNumber := binary.BigEndian.Uint32(frame[0:4]); pageNumber == 0 {
			evidence.terminator = fmt.Sprintf("frame %d has zero page number", frameNumber)
			break
		}
		frameChecksum := sqliteRollingChecksum(order, frame[:8], checksum)
		frameChecksum = sqliteRollingChecksum(order, frame[sqliteWALFrameHeaderBytes:], frameChecksum)
		got := [2]uint32{
			binary.BigEndian.Uint32(frame[16:20]),
			binary.BigEndian.Uint32(frame[20:24]),
		}
		if got != frameChecksum {
			evidence.terminator = fmt.Sprintf("frame %d checksum is invalid", frameNumber)
			break
		}
		checksum = frameChecksum
		evidence.validFrames = frameNumber

		databasePages := binary.BigEndian.Uint32(frame[4:8])
		if frameNumber == maxFrame {
			evidence.prefixChecksum = checksum
			evidence.prefixDatabasePages = databasePages
		} else if frameNumber > maxFrame && databasePages != 0 {
			evidence.committedAfterMaxFrame = true
		}
	}
	return evidence, nil
}

func readSQLiteWALIndexHeader(ctx context.Context, file *os.File) (sqliteWALIndexHeader, error) {
	if err := ctx.Err(); err != nil {
		return sqliteWALIndexHeader{}, err
	}
	if file == nil {
		return sqliteWALIndexHeader{}, errors.New("missing pinned WAL-index descriptor")
	}
	info, err := file.Stat()
	if err != nil {
		return sqliteWALIndexHeader{}, fmt.Errorf("stating WAL-index: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() < sqliteWALIndexMinimumSize {
		return sqliteWALIndexHeader{}, errors.New("truncated WAL-index header")
	}
	var raw [sqliteWALIndexHeaderBytes * sqliteWALIndexHeaderCount]byte
	if _, err := file.ReadAt(raw[:], 0); err != nil {
		return sqliteWALIndexHeader{}, fmt.Errorf("reading WAL-index header: %w", err)
	}
	if !bytes.Equal(raw[:sqliteWALIndexHeaderBytes], raw[sqliteWALIndexHeaderBytes:]) {
		return sqliteWALIndexHeader{}, errors.New("WAL-index header copies disagree")
	}
	headerBytes := raw[:sqliteWALIndexHeaderBytes]
	wantChecksum := sqliteRollingChecksum(binary.NativeEndian, headerBytes[:40], [2]uint32{})
	gotChecksum := [2]uint32{
		binary.NativeEndian.Uint32(headerBytes[40:44]),
		binary.NativeEndian.Uint32(headerBytes[44:48]),
	}
	if gotChecksum != wantChecksum {
		return sqliteWALIndexHeader{}, errors.New("invalid WAL-index header checksum")
	}
	pageSize, err := decodeSQLitePageSize(binary.NativeEndian.Uint16(headerBytes[14:16]))
	if err != nil {
		return sqliteWALIndexHeader{}, fmt.Errorf("invalid WAL-index page size: %w", err)
	}
	header := sqliteWALIndexHeader{
		version:        binary.NativeEndian.Uint32(headerBytes[0:4]),
		initialized:    headerBytes[12],
		bigEndChecksum: headerBytes[13],
		pageSize:       pageSize,
		maxFrame:       binary.NativeEndian.Uint32(headerBytes[16:20]),
		databasePages:  binary.NativeEndian.Uint32(headerBytes[20:24]),
		frameChecksum: [2]uint32{
			binary.NativeEndian.Uint32(headerBytes[24:28]),
			binary.NativeEndian.Uint32(headerBytes[28:32]),
		},
		salt: [2]uint32{
			// SQLite copies these eight bytes directly from the
			// big-endian WAL header into the otherwise native-endian
			// WAL-index header.
			binary.BigEndian.Uint32(headerBytes[32:36]),
			binary.BigEndian.Uint32(headerBytes[36:40]),
		},
	}
	if header.version != sqliteWALFormatVersion {
		return sqliteWALIndexHeader{}, fmt.Errorf("unsupported WAL-index format version %d", header.version)
	}
	if header.initialized != 1 {
		return sqliteWALIndexHeader{}, errors.New("WAL-index is not initialized")
	}
	if header.bigEndChecksum > 1 {
		return sqliteWALIndexHeader{}, errors.New("invalid WAL-index checksum byte order")
	}
	return header, nil
}

func sqliteWALChecksumByteOrder(magic uint32) (binary.ByteOrder, error) {
	switch magic {
	case sqliteWALMagicLittle:
		return binary.LittleEndian, nil
	case sqliteWALMagicBig:
		return binary.BigEndian, nil
	default:
		return nil, fmt.Errorf("invalid WAL magic %#08x", magic)
	}
}

func sqliteRollingChecksum(order binary.ByteOrder, data []byte, checksum [2]uint32) [2]uint32 {
	for offset := 0; offset+8 <= len(data); offset += 8 {
		checksum[0] += order.Uint32(data[offset:offset+4]) + checksum[1]
		checksum[1] += order.Uint32(data[offset+4:offset+8]) + checksum[0]
	}
	return checksum
}

func decodeSQLitePageSize(encoded uint16) (uint32, error) {
	if encoded == 1 {
		return 65536, nil
	}
	return decodeSQLitePageSize32(uint32(encoded))
}

func decodeSQLitePageSize32(pageSize uint32) (uint32, error) {
	if pageSize < sqliteRollbackJournalMinPageSize || pageSize > sqliteRollbackJournalMaxPageSize || pageSize&(pageSize-1) != 0 {
		return 0, fmt.Errorf("%d is not a supported power-of-two page size", pageSize)
	}
	return pageSize, nil
}

func hashSQLiteSHMStableBytes(ctx context.Context, file *os.File) (string, error) {
	if file == nil {
		return "", errors.New("hashing SQLite WAL-index: missing descriptor")
	}
	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("stating SQLite WAL-index: %w", err)
	}
	if info.Size() < sqliteWALIndexMinimumSize {
		return "", errors.New("hashing SQLite WAL-index: truncated header")
	}
	if _, err := readSQLiteWALIndexHeader(ctx, file); err != nil {
		return "", err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("seeking SQLite WAL-index: %w", err)
	}
	return hashSQLiteSourceFileExcludingRange(ctx, file, 100, 20)
}

func hashPinnedSQLiteSHMStableBytes(ctx context.Context, path string, expected os.FileInfo) (hash string, returnErr error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("opening SQLite WAL-index census descriptor: %w", err)
	}
	defer func() {
		if err := closeSQLiteCensusFile(file); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("closing SQLite WAL-index census descriptor: %w", err))
		}
	}()
	opened, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("stating SQLite WAL-index census descriptor: %w", err)
	}
	if !samePinnedSQLiteSHMFile(expected, opened) {
		return "", errSQLiteSourceChanged
	}
	hash, err = hashSQLiteSHMStableBytes(ctx, file)
	if err != nil {
		return "", err
	}
	final, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("restating SQLite WAL-index census descriptor: %w", err)
	}
	if !samePinnedSQLiteSHMFile(expected, final) {
		return "", errSQLiteSourceChanged
	}
	namespace, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", errSQLiteSourceChanged
	}
	if err != nil {
		return "", fmt.Errorf("restating SQLite WAL-index namespace entry: %w", err)
	}
	if !samePinnedSQLiteSHMFile(expected, namespace) {
		return "", errSQLiteSourceChanged
	}
	return hash, nil
}

func samePinnedSQLiteSHMFile(left, right os.FileInfo) bool {
	return left.Mode() == right.Mode() &&
		left.Size() == right.Size() &&
		physicalIdentity("", left) == physicalIdentity("", right)
}

func hashSQLiteSourceFileExcludingRange(ctx context.Context, file *os.File, excludedStart, excludedLength int64) (string, error) {
	if excludedStart < 0 || excludedLength < 0 {
		return "", errors.New("hashing SQLite source: invalid excluded range")
	}
	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("stating SQLite source for hash: %w", err)
	}
	if excludedStart > info.Size() || excludedLength > info.Size()-excludedStart {
		return "", errors.New("hashing SQLite source: excluded range exceeds file")
	}
	hash := sha256.New()
	if _, err := copySQLiteSnapshotStream(ctx, io.Discard, hash, io.NewSectionReader(file, 0, excludedStart)); err != nil {
		return "", fmt.Errorf("hashing SQLite source prefix: %w", err)
	}
	suffixStart := excludedStart + excludedLength
	if _, err := copySQLiteSnapshotStream(ctx, io.Discard, hash, io.NewSectionReader(file, suffixStart, info.Size()-suffixStart)); err != nil {
		return "", fmt.Errorf("hashing SQLite source suffix: %w", err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
