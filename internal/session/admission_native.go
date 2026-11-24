package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

const (
	nativeManagedAdmissionMode = "native-managed-v1"
	nativeCapacitySchema       = "managed-runtime-admission/v1"
)

type nativeCapacityConfig struct {
	Admission struct {
		Mode string `toml:"mode"`
	} `toml:"admission"`
	Queue struct {
		Exact    []string `toml:"managed_exact_routes"`
		Prefixes []string `toml:"managed_route_prefixes"`
	} `toml:"queue"`
	Limits struct {
		Cap int `toml:"max_managed_workers"`
	} `toml:"limits"`
	Thresholds struct {
		MemoryGiB float64 `toml:"mem_green_gib"`
		MemoryPSI float64 `toml:"memory_psi_green"`
		IOPSI     float64 `toml:"io_psi_green"`
	} `toml:"thresholds"`
}

// NativeManagedAdmission reports the opt-in policy for this route. Absence is
// legacy; malformed or unknown policy must never silently select another mode.
func NativeManagedAdmission(city, route string) (bool, error) {
	c, enabled, err := readNativeCapacityConfig(city)
	return enabled && managedCapacityRoute(route, c.Queue.Exact, c.Queue.Prefixes), err
}

func readNativeCapacityConfig(city string) (nativeCapacityConfig, bool, error) {
	var c nativeCapacityConfig
	if city == "" {
		return c, false, nil
	}
	data, err := os.ReadFile(filepath.Join(city, "config", "capacity-scheduler-v2.toml"))
	if errors.Is(err, os.ErrNotExist) {
		return c, false, nil
	}
	if err != nil {
		return c, false, fmt.Errorf("capacity scope invalid")
	}
	// Legacy admission has never decoded limits/thresholds from this file.
	// Inspect only the opt-in header first, preserving that old behavior.
	var header struct {
		Admission struct {
			Mode string `toml:"mode"`
		} `toml:"admission"`
	}
	meta, err := toml.Decode(string(data), &header)
	if err != nil {
		return c, false, fmt.Errorf("capacity scope invalid")
	}
	if !meta.IsDefined("admission", "mode") {
		return c, false, nil
	}
	c.Admission.Mode = header.Admission.Mode
	switch c.Admission.Mode {
	case "legacy-census-v1":
		return c, false, nil
	case nativeManagedAdmissionMode:
		if _, err := toml.Decode(string(data), &c); err != nil {
			return c, false, fmt.Errorf("native capacity scope invalid")
		}
		if c.Limits.Cap < 1 || c.Limits.Cap > 3 || len(c.Queue.Exact) == 0 || len(c.Queue.Prefixes) != 0 {
			return c, false, fmt.Errorf("native capacity scope invalid")
		}
		for _, x := range []float64{c.Thresholds.MemoryGiB, c.Thresholds.MemoryPSI, c.Thresholds.IOPSI} {
			if math.IsNaN(x) || math.IsInf(x, 0) || x <= 0 {
				return c, false, fmt.Errorf("native capacity thresholds invalid")
			}
		}
		return c, true, nil
	default:
		return c, false, fmt.Errorf("capacity admission mode invalid")
	}
}

// CapacityAdmissionMode is the read-only city capability projection. It does
// not promise legacy route reservations when native-owned seats are selected.
func CapacityAdmissionMode(city string) (string, error) {
	_, enabled, err := readNativeCapacityConfig(city)
	if err != nil {
		return "", err
	}
	if enabled {
		return nativeManagedAdmissionMode, nil
	}
	return "legacy-census-v1", nil
}

type nativeKernelIdentity struct {
	BootID            string `json:"boot_id"`
	PIDNamespace      string `json:"pid_namespace"`
	CgroupNamespace   string `json:"cgroup_namespace"`
	CgroupMountDevice uint64 `json:"cgroup_mount_device"`
	CgroupMountInode  uint64 `json:"cgroup_mount_inode"`
}

// This is admission state, not a cached claim of runtime liveness. Every
// unresolved seat counts. Only positive kernel emptiness retires a new seat.
type nativeCapacityLedger struct {
	Schema   string                        `json:"schema"`
	CityPath string                        `json:"city_path"`
	Identity nativeKernelIdentity          `json:"identity"`
	Seats    map[string]nativeCapacitySeat `json:"seats"`
	Retired  []nativeCapacityRetirement    `json:"retired,omitempty"`
}

type nativeCapacitySeat struct {
	Route           string `json:"route"`
	Handle          string `json:"handle"`
	Kind            string `json:"kind"`
	Attempt         string `json:"attempt"`
	Unit            string `json:"unit,omitempty"`
	State           string `json:"state"`
	CreatedAt       string `json:"created_at"`
	LaunchFinalized bool   `json:"launch_finalized"`
	CgroupPath      string `json:"cgroup_path,omitempty"`
	CgroupDevice    uint64 `json:"cgroup_device,omitempty"`
	CgroupInode     uint64 `json:"cgroup_inode,omitempty"`
	EntryState      string `json:"entry_state,omitempty"`
}

type nativeCapacityRetirement struct {
	SessionID string `json:"session_id"`
	Attempt   string `json:"attempt"`
	Reason    string `json:"reason"`
	At        string `json:"at"`
}

type nativeScopeObservation struct {
	Populated, Empty bool
	CgroupPath       string
	Device, Inode    uint64
}

type nativeCapacityHost interface {
	Identity() (nativeKernelIdentity, error)
	Resources(nativeCapacityConfig) error
	Command(string) (runtime.CommandScope, error)
	Observe(nativeCapacitySeat) (nativeScopeObservation, error)
	SelfScope(string) (nativeScopeObservation, error)
}

type nativeCapacityProof struct {
	config  nativeCapacityConfig
	host    nativeCapacityHost
	attempt string
}

func nativeSeatRetirable(seat nativeCapacitySeat, observed nativeScopeObservation) bool {
	return seat.Kind == "scope" && seat.LaunchFinalized && seat.CgroupPath != "" && observed.Empty && !observed.Populated
}

func nativeCapacityPath(city string) string {
	return filepath.Join(city, ".gc", "runtime", "pool-capacity-admission", "native-managed.json")
}

func nativeCapacityUnit(city, sid, attempt string) string {
	h := sha256.Sum256([]byte(city + "\x00" + sid + "\x00" + attempt))
	return "gc-managed-" + hex.EncodeToString(h[:16]) + ".scope"
}

func validNativeSeat(sid string, seat nativeCapacitySeat, city string, c nativeCapacityConfig) bool {
	if sid == "" || seat.Handle == "" || !managedCapacityRoute(seat.Route, c.Queue.Exact, c.Queue.Prefixes) || strings.ContainsAny(sid+seat.Handle, "\x00\r\n\t") {
		return false
	}
	if _, err := time.Parse(time.RFC3339Nano, seat.CreatedAt); err != nil {
		return false
	}
	if seat.Kind == "legacy" {
		return seat.State == "legacy" && seat.LaunchFinalized && seat.Unit == "" && seat.CgroupPath == "" && seat.Attempt != ""
	}
	if seat.Kind != "scope" || len(seat.Attempt) != 32 || seat.Unit != nativeCapacityUnit(city, sid, seat.Attempt) {
		return false
	}
	if _, err := hex.DecodeString(seat.Attempt); err != nil {
		return false
	}
	if seat.State != "reserved" && seat.State != "launching" && seat.State != "active" && seat.State != "ambiguous" {
		return false
	}
	if seat.EntryState != "" && seat.EntryState != "pending" && seat.EntryState != "entered" && seat.EntryState != "revoked" {
		return false
	}
	if seat.EntryState == "entered" && (!seat.LaunchFinalized || seat.CgroupPath == "") {
		return false
	}
	if seat.CgroupPath != "" && (!strings.HasPrefix(seat.CgroupPath, "/") || filepath.Clean(seat.CgroupPath) != seat.CgroupPath || filepath.Base(seat.CgroupPath) != seat.Unit || seat.CgroupDevice == 0 || seat.CgroupInode == 0) {
		return false
	}
	return true
}

// The baseline must be published explicitly during a quiesced cutover. Missing,
// corrupt, foreign-boot or foreign-city state is never initialized to zero.
func withNativeCapacityLedger(ctx context.Context, city string, c nativeCapacityConfig, host nativeCapacityHost, fn func(*nativeCapacityLedger) error) error {
	return withNativeCapacityLedgerWriter(ctx, city, c, host, func(ledger *nativeCapacityLedger, _ func() error) error { return fn(ledger) })
}

func withNativeCapacityLedgerWriter(ctx context.Context, city string, c nativeCapacityConfig, host nativeCapacityHost, fn func(*nativeCapacityLedger, func() error) error) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if ctx.Err() != nil {
		return fmt.Errorf("native capacity canceled")
	}
	path := nativeCapacityPath(city)
	lock, err := openNativeCapacityFile(path+".lock", syscall.O_CREAT|syscall.O_RDWR)
	if err != nil {
		return fmt.Errorf("native capacity lock unavailable")
	}
	defer lock.Close() //nolint:errcheck
	for {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return fmt.Errorf("native capacity lock unavailable")
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("native capacity lock canceled")
		case <-time.After(10 * time.Millisecond):
		}
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck
	file, err := openNativeCapacityFile(path, syscall.O_RDONLY)
	if err != nil {
		return fmt.Errorf("native capacity baseline unavailable")
	}
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	_ = file.Close()
	var ledger nativeCapacityLedger
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err != nil || len(data) > 1<<20 || decoder.Decode(&ledger) != nil || decoder.Decode(new(any)) != io.EOF || ledger.Schema != nativeCapacitySchema || ledger.CityPath != city || ledger.Seats == nil || len(ledger.Seats) > 1024 {
		return fmt.Errorf("native capacity baseline invalid")
	}
	identity, err := host.Identity()
	if err != nil || identity != ledger.Identity || identity.BootID == "" || identity.PIDNamespace == "" || identity.CgroupNamespace == "" || identity.CgroupMountDevice == 0 || identity.CgroupMountInode == 0 {
		return fmt.Errorf("native capacity kernel identity unavailable")
	}
	for sid, seat := range ledger.Seats {
		if !validNativeSeat(sid, seat, city, c) {
			return fmt.Errorf("native capacity seat invalid")
		}
	}
	if ctx.Err() != nil {
		return fmt.Errorf("native capacity canceled")
	}
	persist := func() error { return persistNativeCapacityLedger(ctx, path, lock, &ledger) }
	if err := fn(&ledger, persist); err != nil {
		return err
	}
	return persist()
}

func persistNativeCapacityLedger(ctx context.Context, path string, lock *os.File, ledger *nativeCapacityLedger) error {
	held, err := lock.Stat()
	current, statErr := os.Lstat(path + ".lock")
	if err != nil || statErr != nil || !os.SameFile(held, current) || ctx.Err() != nil {
		return fmt.Errorf("native capacity lock changed or canceled")
	}
	updated, err := json.MarshalIndent(ledger, "", "  ")
	if err != nil {
		return fmt.Errorf("native capacity ledger encode failed")
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".native-managed-*")
	if err != nil {
		return fmt.Errorf("native capacity ledger write failed")
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck
	if err = tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(append(updated, '\n'))
	}
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err != nil || closeErr != nil || os.Rename(tmp.Name(), path) != nil {
		return fmt.Errorf("native capacity ledger write failed")
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("native capacity ledger directory unavailable")
	}
	defer dir.Close() //nolint:errcheck
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("native capacity ledger sync failed")
	}
	return nil
}

func openNativeCapacityFile(path string, flags int) (*os.File, error) {
	fd, err := syscall.Open(path, flags|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		_ = file.Close()
		return nil, fmt.Errorf("native capacity file permissions invalid")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) || stat.Nlink != 1 {
		_ = file.Close()
		return nil, fmt.Errorf("native capacity file ownership invalid")
	}
	return file, nil
}

func retireNativeSeat(ledger *nativeCapacityLedger, sid, reason string, now time.Time) {
	seat := ledger.Seats[sid]
	ledger.Retired = append(ledger.Retired, nativeCapacityRetirement{sid, seat.Attempt, reason, now.UTC().Format(time.RFC3339Nano)})
	if len(ledger.Retired) > 128 {
		ledger.Retired = ledger.Retired[len(ledger.Retired)-128:]
	}
	delete(ledger.Seats, sid)
}

func refreshNativeSeats(ledger *nativeCapacityLedger, host nativeCapacityHost, now time.Time) {
	for sid, seat := range ledger.Seats {
		if seat.Kind == "legacy" {
			continue
		}
		observed, err := host.Observe(seat)
		if err != nil {
			continue
		} // Unknown remains an occupied seat, never zero.
		if seat.CgroupPath == "" && observed.Populated && observed.CgroupPath != "" {
			seat.CgroupPath, seat.CgroupDevice, seat.CgroupInode = observed.CgroupPath, observed.Device, observed.Inode
		}
		if nativeSeatRetirable(seat, observed) {
			retireNativeSeat(ledger, sid, "kernel_cgroup_empty", now)
			continue
		}
		if observed.Populated && seat.LaunchFinalized {
			seat.State = "active"
		}
		ledger.Seats[sid] = seat
	}
}

func claimNativeCapacityStart(ctx context.Context, city, route, sid string, now time.Time, c nativeCapacityConfig, host nativeCapacityHost, sp runtime.Provider, store beads.Store) (*nativeCapacityProof, error) {
	if store == nil || sp == nil || sid == "" {
		return nil, fmt.Errorf("native capacity identity unavailable")
	}
	info, err := NewStore(beads.SessionStore{Store: store}).Get(sid)
	if err != nil || info.Closed || info.Template != route || info.SessionName == "" {
		return nil, fmt.Errorf("native capacity identity unavailable")
	}
	support, ok := sp.(runtime.CommandScopeProvider)
	if !ok || !support.SupportsCommandScope(info.SessionName) {
		return nil, fmt.Errorf("native capacity provider unsupported")
	}
	attempt := NewInstanceToken()[:32]
	proof := &nativeCapacityProof{c, host, attempt}
	err = withNativeCapacityLedger(ctx, city, c, host, func(ledger *nativeCapacityLedger) error {
		// Teardown holds this same flock through its terminal write. Bypass
		// the eventual cache so an independent CLI close cannot be resurrected.
		persisted, err := beads.HandlesFor(store).Live.Get(sid)
		current := infoFromPersistedBead(persisted)
		if err != nil || current.Closed || current.Template != route || current.SessionName != info.SessionName || current.MetadataState != info.MetadataState || current.Generation != info.Generation || current.InstanceToken != info.InstanceToken {
			return fmt.Errorf("native capacity target changed before reservation")
		}
		refreshNativeSeats(ledger, host, now)
		if _, exists := ledger.Seats[sid]; exists {
			return fmt.Errorf("native capacity session occupied")
		}
		if len(ledger.Seats) >= c.Limits.Cap {
			return fmt.Errorf("native capacity full")
		}
		// The durable seat ledger is the admission authority. Whole-provider
		// enumeration is diagnostic: a missing first tmux server or unrelated
		// roster failure must not veto this target's independently owned seat.
		if err := host.Resources(c); err != nil {
			return fmt.Errorf("native capacity resources unavailable")
		}
		if _, err := host.Command(nativeCapacityUnit(city, sid, attempt)); err != nil {
			return fmt.Errorf("native capacity scope unavailable")
		}
		ledger.Seats[sid] = nativeCapacitySeat{Route: route, Handle: info.SessionName, Kind: "scope", Attempt: attempt, Unit: nativeCapacityUnit(city, sid, attempt), State: "reserved", CreatedAt: now.UTC().Format(time.RFC3339Nano)}
		return nil
	})
	return proof, err
}

func beginNativeCapacityStart(ctx context.Context, proof *capacityAdmissionProof, name string, cfg runtime.Config) (runtime.Config, error) {
	native := proof.native
	err := withNativeCapacityLedger(ctx, proof.city, native.config, native.host, func(ledger *nativeCapacityLedger) error {
		seat, ok := ledger.Seats[proof.id]
		if !ok || seat.Attempt != native.attempt || seat.State != "reserved" || seat.Handle != name || seat.Route != proof.route || cfg.Command == "" {
			return fmt.Errorf("native capacity proof invalid")
		}
		if err := native.host.Resources(native.config); err != nil {
			return fmt.Errorf("native capacity resources unavailable")
		}
		scope, err := native.host.Command(seat.Unit)
		if err != nil {
			return fmt.Errorf("native capacity scope unavailable")
		}
		if len(scope.EntryCommand) == 0 || !filepath.IsAbs(scope.EntryCommand[0]) {
			return fmt.Errorf("native capacity entry executable unavailable")
		}
		scope.EntryCommand = append(scope.EntryCommand, "session", "capacity-enter", proof.city, proof.id, seat.Attempt, seat.Unit, "--")
		cfg.CommandScope = &scope
		seat.State = "launching"
		seat.EntryState = "pending"
		ledger.Seats[proof.id] = seat
		return nil
	})
	return cfg, err
}

func finishNativeCapacityStart(ctx context.Context, proof *capacityAdmissionProof, startErr error, now time.Time) error {
	ctx, cancelWait := context.WithTimeout(ctx, 10*time.Second)
	defer cancelWait()
	native := proof.native
	pending := errors.New("native capacity command entry pending")
	for ctx.Err() == nil {

		err := withNativeCapacityLedger(ctx, proof.city, native.config, native.host, func(ledger *nativeCapacityLedger) error {
			seat, ok := ledger.Seats[proof.id]
			if !ok || seat.Attempt != native.attempt {
				for _, retired := range ledger.Retired {
					if retired.SessionID == proof.id && retired.Attempt == native.attempt && retired.Reason == "kernel_cgroup_empty" {
						return nil // Entry was durable even if the model already exited.
					}
				}
			}
			if !ok || seat.Attempt != native.attempt {
				return fmt.Errorf("native capacity finalization identity mismatch")
			}
			if seat.EntryState != "entered" {
				return pending
			}
			refreshNativeSeats(ledger, native.host, now)
			return nil
		})
		if err == nil {
			return nil
		}
		if !errors.Is(err, pending) {
			if ctx.Err() != nil {
				break
			}
			return err
		}
		if startErr != nil || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(10 * time.Millisecond):
		}
		if ctx.Err() != nil {
			break
		}
	}
	// Serialize revocation with command entry. A delayed tmux invocation cannot
	// execute the model after this write, even across controller restarts.
	revokeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	acknowledged := false
	err := withNativeCapacityLedger(revokeCtx, proof.city, native.config, native.host, func(ledger *nativeCapacityLedger) error {
		seat, ok := ledger.Seats[proof.id]
		if !ok || seat.Attempt != native.attempt {
			return fmt.Errorf("native capacity finalization identity mismatch")
		}
		if seat.EntryState == "entered" {
			acknowledged = true
			refreshNativeSeats(ledger, native.host, now)
			return nil
		}
		if seat.EntryState == "pending" {
			retireNativeSeat(ledger, proof.id, "command_entry_revoked_before_model", now)
		} // Old unacknowledged protocol remains occupied.
		return nil
	})
	if err != nil {
		return err
	}
	if acknowledged {
		return nil
	}
	return fmt.Errorf("native capacity model command did not acknowledge scope entry")
}

// EnterNativeCapacityScope is the internal model-command boundary. It records
// this process's kernel cgroup durably before the CLI execs any model command.
func EnterNativeCapacityScope(ctx context.Context, city, sid, attempt, unit string) error {
	return enterNativeCapacityScope(ctx, city, sid, attempt, unit, newNativeCapacityHost())
}

func enterNativeCapacityScope(ctx context.Context, city, sid, attempt, unit string, host nativeCapacityHost) error {
	c, enabled, err := readNativeCapacityConfig(city)
	if err != nil || !enabled {
		return fmt.Errorf("native capacity entry policy unavailable")
	}
	return withNativeCapacityLedger(ctx, city, c, host, func(ledger *nativeCapacityLedger) error {
		seat, ok := ledger.Seats[sid]
		if !ok || seat.Attempt != attempt || seat.Unit != unit || seat.State != "launching" || seat.EntryState != "pending" {
			return fmt.Errorf("native capacity entry revoked or already consumed")
		}
		observed, err := host.SelfScope(unit)
		if err != nil || !observed.Populated || observed.Empty || observed.CgroupPath == "" || filepath.Base(observed.CgroupPath) != unit || observed.Device == 0 || observed.Inode == 0 {
			return fmt.Errorf("native capacity command is outside its owned scope")
		}
		if seat.CgroupPath != "" && (seat.CgroupPath != observed.CgroupPath || seat.CgroupDevice != observed.Device || seat.CgroupInode != observed.Inode) {
			return fmt.Errorf("native capacity entry cgroup incarnation changed")
		}
		if err := host.Resources(c); err != nil {
			return fmt.Errorf("native capacity resources unavailable at command entry")
		}
		seat.CgroupPath, seat.CgroupDevice, seat.CgroupInode = observed.CgroupPath, observed.Device, observed.Inode
		seat.EntryState, seat.State, seat.LaunchFinalized = "entered", "active", true
		ledger.Seats[sid] = seat
		return nil
	})
}

// RefreshNativeCapacity refreshes only already-owned exact scopes. It is used
// by the existing controller tick and teardown paths, without new demand.
func RefreshNativeCapacity(ctx context.Context, city string, now time.Time) error {
	c, enabled, err := readNativeCapacityConfig(city)
	if err != nil || !enabled {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return refreshNativeCapacityWithHost(ctx, city, c, newNativeCapacityHost(), now)
}

func refreshNativeCapacityWithHost(ctx context.Context, city string, c nativeCapacityConfig, host nativeCapacityHost, now time.Time) error {
	return withNativeCapacityLedger(ctx, city, c, host, func(ledger *nativeCapacityLedger) error {
		refreshNativeSeats(ledger, host, now)
		return nil
	})
}

// withNativeCapacityTeardown serializes managed target teardown with claim
// and command entry using the existing cross-process ledger lock. Revocation
// is persisted before Stop; even a failed/slow terminal write cannot re-enable
// a queued command. Unknown ledger state never prevents normal runtime stop.
func withNativeCapacityTeardown(ctx context.Context, city, sid string, c nativeCapacityConfig, host nativeCapacityHost, action func() error) error {
	invoked := false
	err := withNativeCapacityLedgerWriter(ctx, city, c, host, func(ledger *nativeCapacityLedger, persist func() error) error {
		revokeNativeSeatEntry(ledger, sid, time.Now())
		if err := persist(); err != nil {
			return err
		}
		invoked = true
		actionErr := action()
		refreshNativeSeats(ledger, host, time.Now())
		if err := persist(); err != nil {
			return errors.Join(actionErr, err)
		}
		return actionErr
	})
	if !invoked {
		return errors.Join(err, action())
	}
	return err
}

func revokeNativeSeatEntry(ledger *nativeCapacityLedger, sid string, now time.Time) {
	seat, ok := ledger.Seats[sid]
	if !ok || seat.Kind != "scope" {
		return
	}
	if seat.State == "reserved" {
		retireNativeSeat(ledger, sid, "canceled_before_provider_start", now)
	} else if seat.EntryState == "pending" {
		retireNativeSeat(ledger, sid, "command_entry_revoked_before_model", now)
	}
}

func (m *Manager) withNativeCapacityTeardown(id, route string, action func() error) error {
	c, enabled, err := readNativeCapacityConfig(m.cityPath)
	if err != nil {
		return errors.Join(err, action())
	}
	if !enabled || !managedCapacityRoute(route, c.Queue.Exact, c.Queue.Prefixes) {
		// An invalid capacity policy cannot become a city-wide stop veto.
		return action()
	}
	return withNativeCapacityTeardown(context.Background(), m.cityPath, id, c, newNativeCapacityHost(), action)
}

// AbandonCapacityStart cancels only an invocation-local, never-launched native
// reservation. The one-shot proof is consumed first; a concurrent actual start
// or any durable launching/ambiguous seat can never be freed by this operation.
func AbandonCapacityStart(ctx context.Context) error {
	proof, ok := ctx.Value(capacityAdmissionKey{}).(*capacityAdmissionProof)
	if !ok || proof.native == nil || !proof.used.CompareAndSwap(false, true) {
		return nil
	}
	return abandonNativeReservedSeat(proof)
}

func abandonNativeReservedSeat(proof *capacityAdmissionProof) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return withNativeCapacityLedger(ctx, proof.city, proof.native.config, proof.native.host, func(ledger *nativeCapacityLedger) error {
		seat, ok := ledger.Seats[proof.id]
		if ok && seat.Attempt == proof.native.attempt && seat.State == "reserved" {
			retireNativeSeat(ledger, proof.id, "canceled_before_provider_start", time.Now())
		}
		return nil
	})
}
