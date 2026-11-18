package main

// Containment lease (#9237 follow-up, boot-check-design.md §4).
//
// The controller's strand recheck already re-proves containment every tick
// (storage_boot.go). This file turns that proof into an answer a one-shot
// process can ask for instead of repeating it: a live, in-memory lease,
// published to storageRoutes and served over the controller socket
// (controller.go's "storage-verdict" case). It never touches disk — Option D
// of the design's §1, chosen because a file would outlive the controller
// that wrote it, and a stale verdict on disk is indistinguishable from a
// fresh one without asking the process that wrote it anyway.
//
// Nothing here decides a refusal. This process's own boot and recheck never
// consult the lease; a one-shot that cannot get a fresh one (slice 2) always
// falls back to the full check. It is purely an answer to "would a fresh
// check agree with this one", for a caller willing to accept bounded
// staleness in exchange for not reading the ledger again.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/coordclass"
)

// containmentLeaseProtocolVersion is the wire shape's version (design doc
// §4.1). A one-shot that does not recognize it takes the full check.
const containmentLeaseProtocolVersion = 1

// storageVerdictCommand is the controller socket command this process's
// containment lease is served over (controller.go's handleControllerConn
// switch; design doc §4.1). An older controller has no case for it, so the
// connection closes with no reply, same as an unregistered city here.
const storageVerdictCommand = "storage-verdict"

// leaseListCap is the largest stranded or presumed-reused list a lease
// carries complete. A longer list is truncated and Complete is set false,
// which slice 2's caller must treat as "take the full check" (design doc
// §4.4): a truncated refusal list names fewer ids than the city actually
// has, and nothing here lets that be read as the whole truth.
const leaseListCap = 1000

// containmentBasis is every fact confirmInfraContainment's work-store read
// depends on, captured just before that read (design doc §4.2). A one-shot
// re-reading the same facts from files (slice 2) accepts a lease only when
// every field is equal: any one of them changing means the two checks are
// not provably about the same state.
type containmentBasis struct {
	// Marker is "sha256:" plus the hex digest of the convergence marker's
	// bytes, which include the work-store identity line and the write time.
	Marker string `json:"marker"`
	// Manifest stamps the proven-copy manifest: (dev, ino, size, mtime_ns,
	// ctime_ns) where the platform exposes an inode, else its content hash.
	// Its rewrites are temp+rename (infra_copy_manifest.go), so a stamp
	// taken before and after one never collides by chance.
	Manifest string `json:"manifest"`
	// Cleared stamps the cleared note the same way, or "absent" when there
	// is none.
	Cleared string `json:"cleared"`
	// Database stamps (dev, ino) of the binding's database file: a restore
	// changes the inode even when the path and the content on disk do not.
	Database string `json:"database"`
	// WorkStore is this city's work-store identity (workStoreIdentity.String):
	// a recreated .beads changes it even when every field above is untouched.
	WorkStore string `json:"work_store"`
	// Classifier digests coordclass's generation, the classifier's labels,
	// label prefixes and metadata keys, and this build's version: a binary
	// that classifies differently must never be read as agreeing.
	Classifier string `json:"classifier"`
}

// empty reports a basis with nothing captured: the zero value every report
// that never reaches confirmInfraContainment's work-store read carries
// (not configured, genesis, an identity mismatch, every uncheckable), and
// exactly the signal that nothing should be published for it (design doc
// §4.6, "Genesis: the first tick publishes").
func (b containmentBasis) empty() bool {
	return b == containmentBasis{}
}

// containmentLease is the controller's live, in-process answer to "would a
// fresh containment check agree with this one" (design doc §4.1). It is
// never written to disk (Option D): only atomic.Pointer-held state on the
// storageRoutes that produced it, replaced wholesale on every definitive
// recheck tick (storage_boot.go) and read only by the socket handler
// (controller.go).
type containmentLease struct {
	// PID is this controller process's pid, carried so a caller can tell
	// whether the process answering now is the one that captured the basis.
	PID int
	// Binding and Database name the target this verdict is about.
	Binding, Database string
	// Outcome is infraMigrationReport.Outcome.String().
	Outcome string
	// TakenAt is when the check that produced this lease began, read from
	// the controller's own (monotonic) clock. Age is computed from it at
	// serve time, never stored, so one lease answers every caller with its
	// own true age.
	TakenAt time.Time
	// FreshMS is this lease's freshness budget in milliseconds, computed
	// once at publish time from the recheck's adaptive interval and its
	// last check duration (design doc §4.3): min(10 min, 2×interval + last
	// duration). It does not change between serves of the same lease,
	// except that a transient recheck failure stretches it to the full
	// ten-minute cap (Q3; extendLeaseOnTransientFailure).
	FreshMS int64
	// RecheckFailing is true once a transient recheck failure has kept this
	// verdict from being refreshed (Q3, design doc §5): the verdict below
	// is not known to be stale, only unconfirmed since LastAttemptAt. A
	// standing (non-transient) fault never reaches this state — it
	// withdraws the lease instead (storage_boot.go's tick()).
	RecheckFailing bool
	// LastAttemptAt is when the strand recheck most recently tried to
	// refresh this verdict, successful or not. It equals TakenAt on a
	// freshly published lease and moves on every later transient failure,
	// while TakenAt keeps naming the last DEFINITIVE check.
	LastAttemptAt time.Time
	// Basis is the containment basis captured for this verdict.
	Basis containmentBasis
	// Serving and ServesDegraded are infraMigrationReport.serving() and
	// .servesDegraded() at the time this verdict was taken (design review
	// MUST 1): a refusing and a degraded-serving verdict both name the
	// stranded outcome and otherwise look alike on the wire, and a caller
	// that cannot tell them apart from Outcome and Stranded alone has no
	// way to know whether this city is serving anything at all.
	Serving, ServesDegraded bool
	// ProvenBeads, LostCrossEdges, Stranded, PresumedReused and
	// Declassifiable mirror the same-named infraMigrationReport fields.
	// Stranded and PresumedReused are capped at leaseListCap; Complete
	// reports whether either list was truncated to fit. Declassifiable is
	// dropped along with Stranded, not independently capped, when Stranded
	// itself had to be truncated (newContainmentLease).
	ProvenBeads    int
	LostCrossEdges []string
	Stranded       []string
	PresumedReused []string
	Declassifiable map[string][]string
	Complete       bool
}

// containmentLeaseWire is containmentLease's JSON reply shape (design doc
// §4.1): {v, pid, binding, database, outcome, serving, serves_degraded,
// age_ms, fresh_ms, recheck_failing, last_attempt_ms, basis{marker,
// manifest, cleared, database, work_store, classifier}, proven_beads,
// lost_cross_edges, stranded, presumed_reused, declassifiable, complete}.
// serving, serves_degraded, recheck_failing and last_attempt_ms are all
// part of v1 (design review MUST 1 and MUST 2): v does not bump for them.
type containmentLeaseWire struct {
	V              int                 `json:"v"`
	PID            int                 `json:"pid"`
	Binding        string              `json:"binding"`
	Database       string              `json:"database"`
	Outcome        string              `json:"outcome"`
	Serving        bool                `json:"serving"`
	ServesDegraded bool                `json:"serves_degraded"`
	AgeMS          int64               `json:"age_ms"`
	FreshMS        int64               `json:"fresh_ms"`
	RecheckFailing bool                `json:"recheck_failing"`
	LastAttemptMS  int64               `json:"last_attempt_ms"`
	Basis          containmentBasis    `json:"basis"`
	ProvenBeads    int                 `json:"proven_beads"`
	LostCrossEdges []string            `json:"lost_cross_edges,omitempty"`
	Stranded       []string            `json:"stranded,omitempty"`
	PresumedReused []string            `json:"presumed_reused,omitempty"`
	Declassifiable map[string][]string `json:"declassifiable,omitempty"`
	Complete       bool                `json:"complete"`
}

// wire renders l as of now, using the controller's own clock for AgeMS and
// LastAttemptMS: the same lease answers every caller with its own true
// ages, never a value frozen at publish time.
func (l *containmentLease) wire(now time.Time) containmentLeaseWire {
	return containmentLeaseWire{
		V:              containmentLeaseProtocolVersion,
		PID:            l.PID,
		Binding:        l.Binding,
		Database:       l.Database,
		Outcome:        l.Outcome,
		Serving:        l.Serving,
		ServesDegraded: l.ServesDegraded,
		AgeMS:          now.Sub(l.TakenAt).Milliseconds(),
		FreshMS:        l.FreshMS,
		RecheckFailing: l.RecheckFailing,
		LastAttemptMS:  now.Sub(l.LastAttemptAt).Milliseconds(),
		Basis:          l.Basis,
		ProvenBeads:    l.ProvenBeads,
		LostCrossEdges: l.LostCrossEdges,
		Stranded:       l.Stranded,
		PresumedReused: l.PresumedReused,
		Declassifiable: l.Declassifiable,
		Complete:       l.Complete,
	}
}

// newContainmentLease assembles the lease a definitive verdict publishes.
// freshMS is the recheck's current freshness budget (storage_boot.go's
// adaptive-interval math); takenAt is when report's check began. It returns
// nil for a report with no captured basis (not configured, genesis, an
// identity mismatch, uncheckable: there is nothing there a one-shot could
// compare itself against) and, independently of basis, for any report whose
// Outcome is not itself one a lease is ever published for
// (leaseableOutcome) — a standing fault discovered mid-check keeps its
// basis (confirmInfraContainment's own comment on that), so the basis check
// alone does not reject it; design doc §4.6.
func newContainmentLease(target infraBindingTarget, report infraMigrationReport, freshMS int64, takenAt time.Time) *containmentLease {
	if report.basis.empty() || !report.leaseableOutcome() {
		return nil
	}
	stranded, strandedComplete := capLeaseList(report.Stranded)
	presumed, presumedComplete := capLeaseList(report.PresumedReused)
	declassifiable := report.Declassifiable
	if !strandedComplete {
		// Declassifiable only ever names ids within Stranded (it maps a
		// subset of them to the metadata that clears them); a Stranded cut
		// short of leaseListCap can no longer vouch for it, so both are
		// dropped together rather than publish a Declassifiable that
		// answers for ids the truncated Stranded no longer lists.
		stranded, declassifiable = nil, nil
	}
	return &containmentLease{
		PID:            os.Getpid(),
		Binding:        target.Binding,
		Database:       target.Database,
		Outcome:        report.Outcome.String(),
		Serving:        report.serving(),
		ServesDegraded: report.servesDegraded(),
		TakenAt:        takenAt,
		FreshMS:        freshMS,
		LastAttemptAt:  takenAt,
		Basis:          report.basis,
		ProvenBeads:    report.ProvenBeads,
		LostCrossEdges: report.LostCrossEdges,
		Stranded:       stranded,
		PresumedReused: presumed,
		Declassifiable: declassifiable,
		Complete:       strandedComplete && presumedComplete,
	}
}

// capLeaseList truncates ids to leaseListCap, reporting whether it had to.
func capLeaseList(ids []string) ([]string, bool) {
	if len(ids) <= leaseListCap {
		return ids, true
	}
	return ids[:leaseListCap], false
}

// captureContainmentBasis reads every fact confirmInfraContainment's
// work-store read is about to depend on (design doc §4.2), for the caller to
// attach to the report it is about to produce. A read failure here is never
// fatal to the check itself — the basis is advisory, consulted by no
// refusal (design doc §1, Q7) — so the caller treats a non-nil error as
// "capture nothing", leaving the report's basis at its zero value.
func captureContainmentBasis(cityPath string, target infraBindingTarget) (containmentBasis, error) {
	markerSum, err := fileSHA256(target.MarkerPath())
	if err != nil {
		return containmentBasis{}, fmt.Errorf("capturing the containment basis: reading %s: %w", target.MarkerPath(), err)
	}
	manifestStamp, err := containmentBasisStamp(target.ManifestPath(), true)
	if err != nil {
		return containmentBasis{}, fmt.Errorf("capturing the containment basis: stat %s: %w", target.ManifestPath(), err)
	}
	notePath := infraClearedNotePath(cityPath)
	clearedStamp, err := containmentBasisStamp(notePath, true)
	if err != nil {
		return containmentBasis{}, fmt.Errorf("capturing the containment basis: stat %s: %w", notePath, err)
	}
	databaseStamp, err := containmentBasisStamp(target.Database, false)
	if err != nil {
		return containmentBasis{}, fmt.Errorf("capturing the containment basis: stat %s: %w", target.Database, err)
	}
	workStore, err := currentWorkStoreIdentity(cityPath)
	if err != nil {
		return containmentBasis{}, fmt.Errorf("capturing the containment basis: %w", err)
	}
	return containmentBasis{
		Marker:     "sha256:" + markerSum,
		Manifest:   manifestStamp,
		Cleared:    clearedStamp,
		Database:   databaseStamp,
		WorkStore:  workStore.String(),
		Classifier: classifierBasisString(),
	}, nil
}

// containmentBasisStamp fingerprints path's metadata: (dev, ino, size,
// mtime_ns, ctime_ns) when changeSensitive and the platform exposes an
// inode; just (dev, ino) otherwise, for the database file, which this exists
// to catch a RESTORE of, not an ordinary write through the binding this
// process already has open. A missing file stamps as "absent", itself a
// fact worth comparing: the manifest and the cleared note are both written
// by temp+rename, so a transition through "absent" is real information,
// not a race this glosses over.
//
// Where there is no inode (Windows), the manifest and cleared-note stamps
// (changeSensitive) fall back to a "sha256:"-prefixed content hash: both
// are small, process-written files, so hashing them costs nothing. The
// database stamp never does: it names the live SQLite file the binding has
// open, often large, and hashing it on every basis capture would make the
// capture itself one of the heaviest things this process does. There, with
// no inode, the stamp is the literal "unsupported" (design review SHOULD
// 5) — a value that can never equal a later basis's, so a RESTORE this
// stamp exists to catch is never silently taken for a match on a platform
// that cannot name the file well enough to tell.
func containmentBasisStamp(path string, changeSensitive bool) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "absent", nil
		}
		return "", err
	}
	if dev, ino, ok := statDevIno(info); ok {
		if !changeSensitive {
			return fmt.Sprintf("dev=%d;ino=%d", dev, ino), nil
		}
		ctimeNS, _ := statCtimeNanosBestEffort(info)
		return fmt.Sprintf("dev=%d;ino=%d;size=%d;mtime_ns=%d;ctime_ns=%d",
			dev, ino, info.Size(), info.ModTime().UnixNano(), ctimeNS), nil
	}
	if !changeSensitive {
		return "unsupported", nil
	}
	sum, err := fileSHA256(path)
	if err != nil {
		return "", err
	}
	return "sha256:" + sum, nil
}

// statDevIno extracts (dev, ino) from info.Sys() by reflection, so this file
// needs no //go:build variant: Linux's and Darwin's syscall.Stat_t both
// expose Dev and Ino fields reflection can read uniformly, and Windows's
// *syscall.Win32FileAttributeData has neither, so ok is false there and the
// caller falls back to a content hash (design doc §4.6).
func statDevIno(info os.FileInfo) (dev, ino uint64, ok bool) {
	stat := reflect.Indirect(reflect.ValueOf(info.Sys()))
	if !stat.IsValid() {
		return 0, 0, false
	}
	d, ok1 := uintFieldValue(stat.FieldByName("Dev"))
	i, ok2 := uintFieldValue(stat.FieldByName("Ino"))
	if !ok1 || !ok2 {
		return 0, 0, false
	}
	return d, i, true
}

// statCtimeNanosBestEffort extracts the inode-change time in nanoseconds, by
// reflection, from whichever of the two field shapes glibc's and Darwin's
// syscall.Stat_t use (Ctim or Ctimespec, both a {Sec, Nsec} struct) or,
// failing that, a scalar Ctime field. ok is false where none is present
// (Windows never reaches here: statDevIno already failed first, so the
// ctime is never asked for there).
func statCtimeNanosBestEffort(info os.FileInfo) (int64, bool) {
	stat := reflect.Indirect(reflect.ValueOf(info.Sys()))
	if !stat.IsValid() {
		return 0, false
	}
	for _, name := range []string{"Ctim", "Ctimespec"} {
		field := stat.FieldByName(name)
		if !field.IsValid() || field.Kind() != reflect.Struct {
			continue
		}
		sec, ok1 := intFieldValue(field.FieldByName("Sec"))
		nsec, ok2 := intFieldValue(field.FieldByName("Nsec"))
		if ok1 && ok2 {
			return sec*1e9 + nsec, true
		}
	}
	if nanos, ok := intFieldValue(stat.FieldByName("Ctime")); ok {
		return nanos, true
	}
	return 0, false
}

func uintFieldValue(v reflect.Value) (uint64, bool) {
	switch v.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return v.Uint(), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return uint64(v.Int()), true
	}
	return 0, false
}

func intFieldValue(v reflect.Value) (int64, bool) {
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int(), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return int64(v.Uint()), true
	}
	return 0, false
}

// classifierBasisNonce makes a dev or pseudo build's classifier basis unique
// to this process, so it never matches another process's (design doc §4.2,
// "Dev and pseudo builds never match"): there is no build identity to
// compare in that case, so pid+start-time nanos stands in for one.
var classifierBasisNonce = fmt.Sprintf("%d.%d", os.Getpid(), time.Now().UnixNano())

// classifierBasisString digests coordclass's generation, its classifier's
// labels, label prefixes and metadata keys, and this build's version, into
// the containment basis's "classifier" field (design doc §4.2).
func classifierBasisString() string {
	h := sha256.New()
	for _, l := range coordclass.ClassifierLabels() {
		fmt.Fprintf(h, "label:%s\n", l) //nolint:errcheck,gosec // hash.Write never errors
	}
	for _, p := range coordclass.ClassifierLabelPrefixes() {
		fmt.Fprintf(h, "prefix:%s\n", p) //nolint:errcheck,gosec // hash.Write never errors
	}
	for _, k := range coordclass.ClassifierMetadataKeys() {
		fmt.Fprintf(h, "meta:%s\n", k) //nolint:errcheck,gosec // hash.Write never errors
	}
	digest := hex.EncodeToString(h.Sum(nil))
	build := fmt.Sprintf("%s@%s", version, commit)
	_, pseudo := goPseudoVersionBase(versionBuilt)
	if version == "dev" || commit == "unknown" || strings.HasSuffix(commit, dirtySuffix) || pseudo {
		build += "#" + classifierBasisNonce
	}
	return fmt.Sprintf("gen=%d;digest=%s;build=%s", coordclass.CurrentGeneration, digest, build)
}
