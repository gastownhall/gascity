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
	"strings"
	"time"

	"golang.org/x/sys/unix"

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

// leaseListCap is the largest stranded, presumed-reused or lost-cross-edge
// list a lease carries complete. A longer list is never truncated and
// served as a partial answer: newContainmentLease drops it to nil entirely
// and sets Complete false, which slice 2's caller must treat as "take the
// full check" (design doc §4.4) — a partial refusal list names fewer ids
// than the city actually has, and nothing here lets that be read as the
// whole truth.
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
	// ctime_ns). Its rewrites are temp+rename (infra_copy_manifest.go), so a
	// stamp taken before and after one never collides by chance.
	Manifest string `json:"manifest"`
	// Cleared stamps the cleared note the same way, or "absent" when there
	// is none.
	Cleared string `json:"cleared"`
	// Database stamps (dev, ino) of the binding's database file: a restore
	// changes the inode even when the path and the content on disk do not.
	Database string `json:"database"`
	// WorkStore is this city's work-store identity (workStoreIdentity.String):
	// a recreated .beads changes it even when every field above is untouched.
	// No basis is captured at all for a work store the marker's recorded
	// identity contradicts (captureContainmentBasis), so a lease never names
	// one.
	WorkStore string `json:"work_store"`
	// Classifier digests coordclass's generation, the classifier's labels,
	// label prefixes and metadata keys, and this build's version and commit:
	// a binary that classifies differently must never be read as agreeing.
	Classifier string `json:"classifier"`
}

// empty reports a basis with nothing captured. A report carries one when
// confirmInfraContainment never produced it (not configured, genesis, an
// identity mismatch the caller's own hold returned), when its capture failed
// (infraMigrationReport.basisErr says why), and on the boot that back-filled
// a legacy marker's identity (confirmInfraConvergence drops the basis it
// captured before that write). It is the signal newContainmentLease rejects
// on; since publishLease stores whatever newContainmentLease returns,
// including nil, a report with an empty basis does not merely add nothing —
// it actively withdraws any lease already published (design doc §4.6,
// "Genesis: the first tick publishes").
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
	// once at publish time from the recheck's interval (strandRecheckInterval
	// — adaptive, strandRecheckNextInterval) and the check's own duration d
	// (design doc §4.3): min(10 min, 2×interval + d)
	// (strandRecheckFreshness's comment). It does not change between
	// serves of the same lease, except that a transient recheck failure
	// stretches it to the full ten-minute cap (Q3; extendLeaseOnTransientFailure).
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
	// RecheckFailureFromWorkStore is set alongside RecheckFailing, true
	// only when the transient attempt that extended this lease
	// (extendLeaseOnTransientFailure) failed reading the WORK STORE itself
	// (infraMigrationReport.FaultFromWorkStore), as opposed to the binding
	// or a local file. Slice 2's caller treats it as "this lease is not
	// safe to trust while recheck_failing" (design doc carry-over 3): a
	// transient fault anywhere else says nothing about the ledger the
	// lease exists to avoid re-reading, but one reading the work store is
	// a fault in that very read.
	RecheckFailureFromWorkStore bool
	// Basis is the containment basis captured for this verdict.
	Basis containmentBasis
	// Serving and ServesDegraded are infraMigrationReport.serving() and
	// .servesDegraded() at the time this verdict was taken: a refusing and
	// a degraded-serving verdict both name the stranded outcome and
	// otherwise look alike on the wire, and a caller that cannot tell them
	// apart from Outcome and Stranded alone has no way to know whether this
	// city is serving anything at all.
	Serving, ServesDegraded bool
	// ProvenBeads, LostCrossEdges, Stranded, PresumedReused and
	// Declassifiable mirror the same-named infraMigrationReport fields.
	// LostCrossEdges, Stranded and PresumedReused are each nil, not a
	// partial list, once they are over leaseListCap; Complete reports
	// whether any of the three was dropped for it. Declassifiable is
	// dropped along with Stranded, not independently capped, whenever
	// Stranded itself was (newContainmentLease).
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
// part of v1: v does not bump for them.
type containmentLeaseWire struct {
	V              int    `json:"v"`
	PID            int    `json:"pid"`
	Binding        string `json:"binding"`
	Database       string `json:"database"`
	Outcome        string `json:"outcome"`
	Serving        bool   `json:"serving"`
	ServesDegraded bool   `json:"serves_degraded"`
	AgeMS          int64  `json:"age_ms"`
	FreshMS        int64  `json:"fresh_ms"`
	RecheckFailing bool   `json:"recheck_failing"`
	LastAttemptMS  int64  `json:"last_attempt_ms"`
	// RecheckFailingFromWorkStore mirrors containmentLease.RecheckFailureFromWorkStore
	// (design doc carry-over 3). Meaningful only alongside RecheckFailing;
	// the slice 2 caller refuses a recheck_failing lease carrying it rather
	// than accept one within the 10-minute cap.
	RecheckFailingFromWorkStore bool                `json:"recheck_failing_from_work_store,omitempty"`
	Basis                       containmentBasis    `json:"basis"`
	ProvenBeads                 int                 `json:"proven_beads"`
	LostCrossEdges              []string            `json:"lost_cross_edges,omitempty"`
	Stranded                    []string            `json:"stranded,omitempty"`
	PresumedReused              []string            `json:"presumed_reused,omitempty"`
	Declassifiable              map[string][]string `json:"declassifiable,omitempty"`
	Complete                    bool                `json:"complete"`
}

// wire renders l as of now, using the controller's own clock for AgeMS and
// LastAttemptMS: the same lease answers every caller with its own true
// ages, never a value frozen at publish time.
func (l *containmentLease) wire(now time.Time) containmentLeaseWire {
	return containmentLeaseWire{
		V:                           containmentLeaseProtocolVersion,
		PID:                         l.PID,
		Binding:                     l.Binding,
		Database:                    l.Database,
		Outcome:                     l.Outcome,
		Serving:                     l.Serving,
		ServesDegraded:              l.ServesDegraded,
		AgeMS:                       now.Sub(l.TakenAt).Milliseconds(),
		FreshMS:                     l.FreshMS,
		RecheckFailing:              l.RecheckFailing,
		LastAttemptMS:               now.Sub(l.LastAttemptAt).Milliseconds(),
		RecheckFailingFromWorkStore: l.RecheckFailureFromWorkStore,
		Basis:                       l.Basis,
		ProvenBeads:                 l.ProvenBeads,
		LostCrossEdges:              l.LostCrossEdges,
		Stranded:                    l.Stranded,
		PresumedReused:              l.PresumedReused,
		Declassifiable:              l.Declassifiable,
		Complete:                    l.Complete,
	}
}

// newContainmentLease assembles the lease a definitive verdict publishes.
// freshMS is the recheck's freshness budget (strandRecheckFreshness);
// takenAt is when report's check began. It returns nil for a report with
// no captured basis (containmentBasis.empty: there is nothing there a
// one-shot could compare itself against) and, independently of basis, for
// any report whose Outcome is not itself one a lease is ever published for
// (leaseableOutcome) — an uncheckable report from confirmInfraContainment
// keeps the basis it captured before the read that failed, so the basis
// check alone does not reject it; design doc §4.6.
func newContainmentLease(target infraBindingTarget, report infraMigrationReport, freshMS int64, takenAt time.Time) *containmentLease {
	if report.basis.empty() || !report.leaseableOutcome() {
		return nil
	}
	stranded, strandedComplete := capLeaseList(report.Stranded)
	presumed, presumedComplete := capLeaseList(report.PresumedReused)
	lostCrossEdges, lostComplete := capLeaseList(report.LostCrossEdges)
	declassifiable := report.Declassifiable
	if !strandedComplete {
		// Declassifiable only ever names ids within Stranded (it maps a
		// subset of them to the metadata that clears them); a Stranded
		// dropped for being over leaseListCap can no longer vouch for it,
		// so both are dropped together rather than publish a Declassifiable
		// that answers for ids the dropped Stranded no longer lists.
		declassifiable = nil
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
		LostCrossEdges: lostCrossEdges,
		Stranded:       stranded,
		PresumedReused: presumed,
		Declassifiable: declassifiable,
		Complete:       strandedComplete && presumedComplete && lostComplete,
	}
}

// capLeaseList reports ids unchanged when it is within leaseListCap, else
// drops it to nil entirely rather than truncate it: a partial list cannot
// vouch for itself as the city's whole refusal or recovery set, and nothing
// here lets slice 2's caller read it as one (leaseListCap's comment). The
// bool reports whether ids was complete.
func capLeaseList(ids []string) ([]string, bool) {
	if len(ids) <= leaseListCap {
		return ids, true
	}
	return nil, false
}

// captureContainmentBasis reads every fact confirmInfraContainment's
// work-store read is about to depend on (design doc §4.2), for the caller to
// attach to the report it is about to produce. A failure here is never fatal
// to the check itself — the basis is advisory, consulted by no refusal
// (design doc §1, Q7) — so the caller keeps the error beside an empty basis
// (infraMigrationReport.basisErr), and no lease is published for that report.
//
// It also captures nothing for a work store the marker's recorded identity
// contradicts (checkMarkerWorkStoreIdentity: the comparison boot refuses on
// in markerWorkStoreIdentityHold). A lease says the binding holds THIS work
// store's slice, and the strand recheck runs confirmInfraContainment without
// that hold. The comparison reads the same marker bytes Marker digests and
// the same identity WorkStore stamps, so however the files change around
// the capture, no basis ever names an identity that was not compared.
func captureContainmentBasis(cityPath string, target infraBindingTarget) (containmentBasis, error) {
	marker, err := os.ReadFile(target.MarkerPath())
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
	if err := checkMarkerWorkStoreIdentity(target.MarkerPath(), marker, workStore); err != nil {
		return containmentBasis{}, fmt.Errorf("capturing the containment basis: %w", err)
	}
	markerSum := sha256.Sum256(marker)
	return containmentBasis{
		Marker:     "sha256:" + hex.EncodeToString(markerSum[:]),
		Manifest:   manifestStamp,
		Cleared:    clearedStamp,
		Database:   databaseStamp,
		WorkStore:  workStore.String(),
		Classifier: classifierBasisString(),
	}, nil
}

// checkMarkerWorkStoreIdentity returns an error when marker, the bytes of
// the convergence marker at markerPath, records a work-store identity that
// will not parse or that names another work store than current
// (workStoreIdentityMismatch). A legacy marker records none and contradicts
// nothing, as it does for boot's hold.
func checkMarkerWorkStoreIdentity(markerPath string, marker []byte, current workStoreIdentity) error {
	recorded, ok, err := parseMarkerWorkStoreIdentity(markerPath, marker)
	if err != nil || !ok {
		return err
	}
	if why := workStoreIdentityMismatch(recorded, current); why != "" {
		return fmt.Errorf("%s: %s", markerPath, why)
	}
	return nil
}

// containmentBasisStamp fingerprints path's metadata: (dev, ino, size,
// mtime_ns, ctime_ns) when changeSensitive; just (dev, ino) otherwise, for
// the database file, which this exists to catch a RESTORE of, not an
// ordinary write through the binding this process already has open. A
// missing file stamps as "absent", itself a fact worth comparing: the
// manifest and the cleared note are both written by temp+rename, so a
// transition through "absent" is real information, not a race this glosses
// over.
//
// x/sys/unix, not os.Stat, the way statBdBinary (bd_inventory_trust.go)
// reads a binary's identity: gc ships for linux and darwin only
// (.goreleaser.yml), and cmd/gc is in none of .github/windows-lane's
// allowlists, so there is no inode-less platform to fall back for, and
// none is kept.
func containmentBasisStamp(path string, changeSensitive bool) (string, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "absent", nil
		}
		return "", err
	}
	if !changeSensitive {
		return fmt.Sprintf("dev=%d;ino=%d", uint64(st.Dev), st.Ino), nil //nolint:unconvert // Darwin's field is signed int32.
	}
	return fmt.Sprintf("dev=%d;ino=%d;size=%d;mtime_ns=%d;ctime_ns=%d",
		uint64(st.Dev), st.Ino, st.Size, st.Mtim.Nano(), st.Ctim.Nano()), nil //nolint:unconvert // Darwin's field is signed int32.
}

// classifierBasisNonce makes the classifier basis of a build with no
// trustworthy identity (classifierBuildIdentityTrusted) unique to this
// process, so it never matches another process's (design doc §4.2): there
// is no build identity to compare in that case, so pid+start-time nanos
// stands in for one.
var classifierBasisNonce = fmt.Sprintf("%d.%d", os.Getpid(), time.Now().UnixNano())

// classifierBasisString digests coordclass's generation, its classifier's
// labels, label prefixes and metadata keys, and this build's version and
// commit, into the containment basis's "classifier" field (design doc §4.2).
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
	if !classifierBuildIdentityTrusted() {
		build += "#" + classifierBasisNonce
	}
	return fmt.Sprintf("gen=%d;digest=%s;build=%s", coordclass.CurrentGeneration, digest, build)
}

// classifierBuildIdentityTrusted reports whether this binary's commit stamp
// names the code it runs well enough for two processes to compare their
// classifier bases: a known commit (knownGCBuildID), abbreviated to at least
// minAbbrevCommitLen, from a clean tree, in a build that is not a Go
// pseudo-version. The version plays no part: an untagged `make build`
// stamps version=dev beside the commit it was built from, and two processes
// built from one clean commit run the same classifier — the same trust
// binary-drift detection places in a known commit (detectGCBinaryDrift).
func classifierBuildIdentityTrusted() bool {
	stamp := strings.TrimSpace(commit)
	if !knownGCBuildID(stamp) || len(stamp) < minAbbrevCommitLen || strings.HasSuffix(stamp, dirtySuffix) {
		return false
	}
	_, pseudo := goPseudoVersionBase(versionBuilt)
	return !pseudo
}
