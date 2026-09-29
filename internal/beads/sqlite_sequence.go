package beads

// SQLite bead-id sequence: strict recovery, overflow-checked allocation, and a
// persisted floor that LEADS the allocator.
//
// An auto-minted SQLite bead id is "<prefix>-<n>". Three rules keep every n the
// store hands out unique for the life of the store, across restarts, crashes,
// deletions and concurrent processes:
//
//  1. Strict recovery. Only ids in exactly the store's own auto format feed the
//     allocator's high-water mark (parseSQLiteAutoIDSuffix). A caller-pinned id
//     such as "gcg-session-<32 hex>" whose hex happens to end in digits is not
//     an auto id and never moves the sequence. The original MAX(trailing-digits)
//     scan read such tails as 19+ digit numbers, clamped them to MaxInt64 and
//     then wrapped the allocator into the negative range on every open.
//
//  2. No wrap. Allocation that would step past the end of its range fails
//     loudly (ErrSQLiteSequenceExhausted) instead of wrapping.
//
//  3. Leading floor. Before the store hands out any id above the persisted
//     floor it durably raises the floor by a whole block (sqliteSequenceBlockSize)
//     under the store directory's flock, and only then issues ids from that
//     block. On open the allocator starts at max(strict row high-water,
//     persisted floor), so an id handed out before a crash is never handed out
//     again even when the WAL tail that carried its row was lost, or the row was
//     deleted. Each process reserves its own block under the lock, so two
//     processes sharing a store directory never receive the same block.
//
// Sequence order. The allocator value is an int64 compared by its two's-
// complement rank (uint64(v)): 0 < 1 < ... < MaxInt64 < MinInt64 < ... < -1.
// Positive values are the normal range. The negative range exists only because
// builds before this change wrapped MaxInt64+1 to MinInt64 and minted
// "<prefix>--9223372036854775808" upward; on such a store the whole positive
// range has to be treated as spent. An operator moves a wrapped store's
// allocator to a never-issued point in the negative range with
// RaiseSQLiteSequenceFloor ("gc storage repair-sequence"), and allocation then
// counts upward toward -1 and stops there: it never crosses into 0 or the
// positive range, and nothing ever steps from MaxInt64 into the negative range.
//
// Wrapped stores fail closed. A strict negative row whose rank exceeds the
// persisted floor proves an older build wrapped this store, and the ids it
// issued above that row and later deleted are recorded nowhere in the store.
// Minting refuses (ErrSQLiteSequenceWrapped) until an operator sets a floor at
// or above every negative id ever issued. Reads and caller-pinned creates are
// unaffected.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// sqliteSequenceBlockSize is how many ids one durable floor write reserves for
// the reserving process. Larger blocks mean fewer fsyncs; the cost is at most
// one block of never-used ids per process lifetime.
const sqliteSequenceBlockSize int64 = 1024

// sqliteSequenceRepairCommand is the operator command that raises a store's
// persisted floor. Allocation errors name it so the refusal is actionable.
const sqliteSequenceRepairCommand = "gc storage repair-sequence"

var (
	// ErrSQLiteSequenceExhausted reports that the allocator reached the end of
	// its range (MaxInt64, or -1 in the negative range) and refused to wrap.
	ErrSQLiteSequenceExhausted = errors.New("sqlite bead id sequence exhausted")

	// ErrSQLiteSequenceWrapped reports a store holding auto ids that an older
	// build minted after wrapping past MaxInt64, with no persisted floor
	// covering them. Minting could re-issue a deleted id, so it refuses.
	ErrSQLiteSequenceWrapped = errors.New("sqlite bead id sequence wrapped by an older build")
)

// sqliteSequenceFloorFilenameFor returns the per-prefix floor sidecar name. The
// graph prefix keeps its historical graph.seqfloor name (migration pins that
// path as a physical fact); every other prefix gets "<prefix>.seqfloor".
func sqliteSequenceFloorFilenameFor(prefix string) string {
	if prefix == sqliteGraphPrefix {
		return sqliteGraphSequenceFloorFilename
	}
	return prefix + ".seqfloor"
}

// parseSQLiteAutoIDSuffix returns n when id is exactly "<prefix>-<n>" in the
// allocator's own output format, and ok=false otherwise. Accepted forms:
//
//	<prefix>-<d>   d canonical decimal, 1..MaxInt64 (no sign, no leading zero)
//	<prefix>--<d>  d canonical decimal, the negative value -d >= MinInt64
//	               (only builds that wrapped ever produced these)
//
// Everything else — extra segments such as "session-<hex>", zero, "+", spaces,
// leading zeros, out-of-range values — is not an auto id.
func parseSQLiteAutoIDSuffix(prefix, id string) (int64, bool) {
	if prefix == "" || !strings.HasPrefix(id, prefix+"-") {
		return 0, false
	}
	rest := id[len(prefix)+1:]
	negative := strings.HasPrefix(rest, "-")
	digits := rest
	if negative {
		digits = rest[1:]
	}
	if digits == "" || digits[0] == '0' {
		return 0, false
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, false
		}
	}
	text := digits
	if negative {
		text = "-" + digits
	}
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil || n == 0 {
		return 0, false
	}
	return n, true
}

// sequenceRankAbove reports whether a comes after b in allocation order.
func sequenceRankAbove(a, b int64) bool {
	return uint64(a) > uint64(b)
}

// sequenceMax returns whichever of a and b comes later in allocation order.
func sequenceMax(a, b int64) int64 {
	if sequenceRankAbove(a, b) {
		return a
	}
	return b
}

// sequenceNext returns the value after cur, refusing to leave cur's range.
func sequenceNext(cur int64) (int64, error) {
	if cur == math.MaxInt64 || cur == -1 {
		return 0, sequenceExhaustedError(cur)
	}
	return cur + 1, nil
}

// sequenceBlockEnd returns the last value of the block reserved after base,
// clamped to the end of base's range. It errors when base is already at the
// end of its range, so a reservation never hands out an empty block.
func sequenceBlockEnd(base, block int64) (int64, error) {
	if block < 1 {
		return 0, fmt.Errorf("reserving sqlite bead ids: invalid block size %d", block)
	}
	end := int64(math.MaxInt64)
	if base < 0 {
		end = -1
	}
	if base == end {
		return 0, sequenceExhaustedError(base)
	}
	if end-base < block { // end-base cannot overflow: both lie in the same range
		return end, nil
	}
	return base + block, nil
}

func sequenceExhaustedError(at int64) error {
	return fmt.Errorf("%w at %d: refusing to wrap and re-issue ids; raise the floor into a never-issued range with %q",
		ErrSQLiteSequenceExhausted, at, sqliteSequenceRepairCommand)
}

// sqliteSequenceHighWater is the strict high-water mark of the auto ids a store
// holds, split by range.
type sqliteSequenceHighWater struct {
	positive    int64 // highest positive auto id, 0 when none
	negative    int64 // highest-ranked negative (wrapped) auto id, valid when hasNegative
	hasNegative bool
}

type sqliteQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// scanSQLiteSequenceHighWater computes the strict high-water over every row
// whose id could be an auto id. The LIKE only narrows the scan; the strict
// parser decides.
func scanSQLiteSequenceHighWater(ctx context.Context, q sqliteQuerier, prefix string) (sqliteSequenceHighWater, error) {
	var hw sqliteSequenceHighWater
	rows, err := q.QueryContext(ctx, `SELECT id FROM beads WHERE id LIKE ?`, prefix+"-%")
	if err != nil {
		return hw, err
	}
	defer rows.Close() //nolint:errcheck
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return hw, err
		}
		n, ok := parseSQLiteAutoIDSuffix(prefix, id)
		if !ok {
			continue
		}
		if n > 0 {
			if n > hw.positive {
				hw.positive = n
			}
			continue
		}
		if !hw.hasNegative || n > hw.negative {
			hw.negative = n
			hw.hasNegative = true
		}
	}
	return hw, rows.Err()
}

// applySequenceHighWaterLocked folds a strict row scan into the allocator,
// never moving it backwards, and never into the negative range: a negative row
// above the persisted floor instead marks the store wrapped. It does not raise
// the allocator to the floor — recovery does that; a collision reseed must not,
// because the floor covers other processes' reserved blocks and this process's
// own block is still its to use. The caller holds s.sequenceFloorMu.
func (s *SQLiteStore) applySequenceHighWaterLocked(hw sqliteSequenceHighWater, floor int64) {
	s.seq = sequenceMax(s.seq, hw.positive)
	if hw.hasNegative && sequenceRankAbove(hw.negative, floor) {
		if !s.sequenceWrapped || sequenceRankAbove(hw.negative, s.sequenceWrappedAt) {
			s.sequenceWrappedAt = hw.negative
		}
		s.sequenceWrapped = true
	}
}

// sequenceWrappedErrorLocked re-reads the persisted floor, clearing the
// wrapped state once an operator has raised the floor past the wrapped
// high-water, so a running process needs no restart after a repair. It returns
// a non-nil error while the store is still wrapped.
func (s *SQLiteStore) sequenceWrappedErrorLocked() error {
	if !s.sequenceWrapped {
		return nil
	}
	floor, err := readSQLiteSequenceFloor(s.sequenceFloorPath)
	if err != nil {
		return err
	}
	if !sequenceRankAbove(s.sequenceWrappedAt, floor) {
		s.sequenceWrapped = false
		s.seq = sequenceMax(s.seq, floor)
		return nil
	}
	return fmt.Errorf("%w: auto id %s-%d exceeds the persisted floor %d; ids above it may have been issued and deleted, so set a floor at or above the highest %s id ever issued with %q",
		ErrSQLiteSequenceWrapped, s.prefix, s.sequenceWrappedAt, floor, s.prefix, sqliteSequenceRepairCommand)
}

// nextID returns the next auto id, reserving a durable block first whenever the
// next value is not already covered by this process's reservation.
func (s *SQLiteStore) nextID() (string, error) {
	s.sequenceFloorMu.Lock()
	defer s.sequenceFloorMu.Unlock()
	if s.readOnly {
		return "", errors.New("minting sqlite bead id on read-only store")
	}
	if err := s.sequenceWrappedErrorLocked(); err != nil {
		return "", err
	}
	next, err := sequenceNext(s.seq)
	if err != nil {
		return "", err
	}
	if !s.sequenceReserved || sequenceRankAbove(next, s.sequenceLimit) {
		base, limit, err := reserveSQLiteSequenceBlock(s.sequenceFloorPath, s.seq, sqliteSequenceBlockSize)
		if err != nil {
			return "", fmt.Errorf("reserving sqlite bead ids: %w", err)
		}
		s.seq = base
		s.sequenceLimit = limit
		s.sequenceReserved = true
		if next, err = sequenceNext(base); err != nil {
			return "", err
		}
	}
	s.seq = next
	return s.prefix + "-" + strconv.FormatInt(next, 10), nil
}

// reserveSQLiteSequenceBlock durably raises the floor at floorPath past
// max(persisted floor, local) by one block and returns the reserved half-open
// range (base, limit]. The read-max-write runs under the store directory's
// flock, so concurrent reservations — in this process or another — receive
// disjoint blocks, and the fsync completes before any id in the block exists.
func reserveSQLiteSequenceBlock(floorPath string, local, block int64) (base, limit int64, returnErr error) {
	returnErr = withSQLiteSequenceFloorLock(floorPath, func() error {
		current, err := readSQLiteSequenceFloor(floorPath)
		if err != nil {
			return err
		}
		base = sequenceMax(current, local)
		limit, err = sequenceBlockEnd(base, block)
		if err != nil {
			return err
		}
		return writeSQLiteSequenceFloor(floorPath, limit)
	})
	return base, limit, returnErr
}

// SQLiteSequenceState describes a store's id allocator for one prefix.
type SQLiteSequenceState struct {
	// Dir is the store directory and Prefix the auto-id prefix inspected.
	Dir, Prefix string
	// FloorPath is the persisted floor sidecar.
	FloorPath string
	// Floor is the persisted floor (0 when the sidecar is absent).
	Floor int64
	// HighestPositive is the highest strict positive auto id present, 0 if none.
	HighestPositive int64
	// HighestNegative is the highest-ranked strict negative (wrapped) auto id
	// present; valid only when HasNegative.
	HighestNegative int64
	// HasNegative reports whether any wrapped auto id is present.
	HasNegative bool
	// Wrapped reports that minting is refused until the floor is raised.
	Wrapped bool
}

// InspectSQLiteSequence reports the allocator state of the SQLite store in dir
// for prefix. It opens the database read-only and writes nothing.
func InspectSQLiteSequence(dir, prefix string) (SQLiteSequenceState, error) {
	prefix = normalizeIDPrefix(prefix)
	if prefix == "" {
		return SQLiteSequenceState{}, errors.New("inspecting sqlite sequence: empty prefix")
	}
	opened, err := OpenSQLiteStore(dir, WithSQLiteStoreReadOnly(), WithSQLiteStoreIDPrefix(prefix))
	if err != nil {
		return SQLiteSequenceState{}, fmt.Errorf("inspecting sqlite sequence: %w", err)
	}
	store := opened.(*SQLiteStore)
	hw := store.recoveredHighWater
	if err := store.CloseStore(); err != nil {
		return SQLiteSequenceState{}, fmt.Errorf("inspecting sqlite sequence: %w", err)
	}
	floorPath := filepath.Join(dir, sqliteSequenceFloorFilenameFor(prefix))
	floor, err := readSQLiteSequenceFloor(floorPath)
	if err != nil {
		return SQLiteSequenceState{}, fmt.Errorf("inspecting sqlite sequence: %w", err)
	}
	return SQLiteSequenceState{
		Dir:             dir,
		Prefix:          prefix,
		FloorPath:       floorPath,
		Floor:           floor,
		HighestPositive: hw.positive,
		HighestNegative: hw.negative,
		HasNegative:     hw.hasNegative,
		Wrapped:         hw.hasNegative && sequenceRankAbove(hw.negative, floor),
	}, nil
}

// RaiseSQLiteSequenceFloor is the operator remediation for a store whose
// allocator re-issued or could re-issue ids. It persists floor as the store's
// sequence floor for prefix so the next auto id is floor+1 in allocation order.
//
// floor may be negative: on a store an older build wrapped, the positive range
// is spent and the allocator continues upward from a negative floor toward -1.
// The operator supplies the value from evidence outside the store (event log,
// dispatcher traces) — the highest id ever issued, which deleted rows no longer
// show.
//
// It refuses to lower the persisted floor, and refuses a floor below the
// highest strict auto id the store still holds; both comparisons use
// allocation order, in which every negative value is above every positive one.
// Raising to the current floor is a no-op. Processes already running pick the
// new floor up at their next block reservation, and a process refusing to mint
// because the store is wrapped resumes as soon as the floor covers it.
func RaiseSQLiteSequenceFloor(dir, prefix string, floor int64) (before SQLiteSequenceState, returnErr error) {
	before, err := InspectSQLiteSequence(dir, prefix)
	if err != nil {
		return before, err
	}
	if sequenceRankAbove(before.HighestPositive, floor) || (before.HasNegative && sequenceRankAbove(before.HighestNegative, floor)) {
		highest := before.HighestPositive
		if before.HasNegative {
			highest = before.HighestNegative
		}
		return before, fmt.Errorf("raising sqlite sequence floor: %d is below the highest %s auto id present (%s-%d)", floor, before.Prefix, before.Prefix, highest)
	}
	if _, err := os.Stat(dir); err != nil {
		return before, fmt.Errorf("raising sqlite sequence floor: %w", err)
	}
	returnErr = withSQLiteSequenceFloorLock(before.FloorPath, func() error {
		current, err := readSQLiteSequenceFloor(before.FloorPath)
		if err != nil {
			return err
		}
		if sequenceRankAbove(current, floor) {
			return fmt.Errorf("raising sqlite sequence floor: refusing to lower the persisted floor %d to %d", current, floor)
		}
		if current == floor {
			return nil
		}
		return writeSQLiteSequenceFloor(before.FloorPath, floor)
	})
	return before, returnErr
}
