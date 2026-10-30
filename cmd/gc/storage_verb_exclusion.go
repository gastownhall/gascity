package main

// Exclusion for the writing storage verbs (S5 §2.2, §2.3, §6).
//
// A storage verb that writes — migrate (--from-work, --from-backup) and
// recover-stranded — copies a source and proves the copy, and both halves are
// worthless against a source something else is writing. The verbs used to
// prove the absence of a controller with a socket ping and nothing else. A
// ping proves less than it looks: a controller that is booting has not opened
// its socket yet, a supervisor-hosted city is restarted by its supervisor the
// moment it stops, and nothing stops a second `gc start` mid-run.
//
// So each writing verb now holds .gc/controller.lock for its whole run, the
// lock both a standalone controller and a supervisor-hosted city take before
// they touch the city (the supervisor took it AFTER its init until S5-A). The
// ping stays as an advisory second check. Beyond the lock the verb refuses:
//
//   - while the city is registered with the supervisor, whose reconcile loop
//     would otherwise spin on the lock (`gc stop` unregisters);
//   - while the runtime lists any session for the city, or cannot list them
//     completely (an incomplete inventory is a refusal, unless the operator
//     attests --force=remote-runtime);
//   - on a remote runtime (k8s, hybrid, ssh, a pack runtime) without
//     --force=remote-runtime, because remote hosts cannot use the split's
//     infrastructure classes until CN-2 (owner decision O1);
//   - on a work store another registered city also uses, without
//     --force=shared-store:<identity> (owner decision O2);
//   - on a remote stage, which has no store of its own to migrate.
//
// Order: controller lock, then the migration guard (the verbs take the guard
// after this returns). While the lock is held nothing these verbs call may
// probe acquireControllerLock again (waitForControllerStop,
// ensureNoStandaloneController): a second flock on a fresh descriptor
// conflicts even inside this process.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/config"
)

const (
	// storageForceFlag names the attestation flag the writing verbs take.
	storageForceFlag = "force"
	// storageHazardRemoteRuntime attests that remote sessions may lose the
	// infrastructure classes (they refuse there until CN-2) and that no
	// remote host writes infrastructure classes to the work store.
	storageHazardRemoteRuntime = "remote-runtime"
	// storageHazardSharedStore prefixes the shared-store attestation, which
	// names the identity it covers: shared-store:<identity>.
	storageHazardSharedStore = "shared-store"
	// storageForceBare is the value a bare --force parses to. It refuses: an
	// attestation that names no hazard attests to nothing, and a hazard added
	// later must not inherit one by silence.
	storageForceBare = "<bare>"
	// storageAttestationLog is where accepted attestations are appended.
	storageAttestationLog = "storage-attestations.log"
)

// storageForce is the parsed --force attestations of one run.
type storageForce struct {
	RemoteRuntime bool
	SharedStores  []string
}

// attestsSharedStore reports whether the operator attested this identity.
func (f storageForce) attestsSharedStore(token string) bool {
	for _, s := range f.SharedStores {
		if s == token {
			return true
		}
	}
	return false
}

// hazards lists the attested hazards, for the record.
func (f storageForce) hazards() []string {
	var out []string
	if f.RemoteRuntime {
		out = append(out, storageHazardRemoteRuntime)
	}
	for _, s := range f.SharedStores {
		out = append(out, storageHazardSharedStore+":"+s)
	}
	return out
}

// parseStorageForce parses the repeated --force values.
func parseStorageForce(values []string) (storageForce, error) {
	var f storageForce
	for _, raw := range values {
		v := strings.TrimSpace(raw)
		switch {
		case v == "" || v == storageForceBare:
			return storageForce{}, fmt.Errorf("a bare --%s attests to nothing; name the hazard: --%s=%s or --%s=%s:<identity>", storageForceFlag, storageForceFlag, storageHazardRemoteRuntime, storageForceFlag, storageHazardSharedStore)
		case v == storageHazardRemoteRuntime:
			f.RemoteRuntime = true
		case strings.HasPrefix(v, storageHazardSharedStore+":"):
			id := strings.TrimSpace(strings.TrimPrefix(v, storageHazardSharedStore+":"))
			if id == "" {
				return storageForce{}, fmt.Errorf("--%s=%s: names no identity; pass the identity the refusal printed", storageForceFlag, storageHazardSharedStore)
			}
			f.SharedStores = append(f.SharedStores, id)
		default:
			return storageForce{}, fmt.Errorf("--%s=%s is not a hazard this build knows; the hazards are %s and %s:<identity>", storageForceFlag, v, storageHazardRemoteRuntime, storageHazardSharedStore)
		}
	}
	return f, nil
}

// addStorageForceFlag registers --force on a writing verb.
func addStorageForceFlag(cmd *cobra.Command, dst *[]string) {
	cmd.Flags().StringArrayVar(dst, storageForceFlag, nil,
		"attest a named hazard (repeatable): "+storageHazardRemoteRuntime+" (remote sessions refuse infrastructure-class commands until worker-site transport exists, and no remote host writes them to the work store) or "+storageHazardSharedStore+":<identity> (this city is the single writer of a work store other registered cities also use). A bare --force refuses")
	cmd.Flags().Lookup(storageForceFlag).NoOptDefVal = storageForceBare
}

// storageVerbScope says which checks a verb's writes call for.
type storageVerbScope struct {
	// Splits is true for a verb that moves the city onto its split: the
	// remote-runtime refusal applies.
	Splits bool
}

// acquireStorageVerbExclusion takes the controller lock and runs the liveness
// checks, returning the held lock (the caller closes it when it exits) or the
// refusal.
func acquireStorageVerbExclusion(request storageOperatorRequest, scope storageVerbScope) (*os.File, error) {
	cityPath := request.CityPath
	if err := remoteStageStorageRefusal(cityPath); err != nil {
		return nil, fmt.Errorf("%w. A remote stage has no store of its own to migrate", err)
	}
	if _, registered, err := registeredCityEntry(cityPath); err != nil {
		return nil, fmt.Errorf("reading the supervisor registry to rule out a supervisor-hosted city: %w", err)
	} else if registered {
		return nil, fmt.Errorf("this city is registered with the supervisor, which restarts it the moment its controller stops. Stop it with `%s` (it also unregisters the city) and run this again", storageStopCommand)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		return nil, fmt.Errorf("creating the city runtime directory: %w", err)
	}
	lock, err := acquireControllerLock(cityPath)
	if err != nil {
		if errors.Is(err, errControllerAlreadyRunning) {
			return nil, controllerLockHolderRefusal(probeControllerIdentity(cityPath))
		}
		return nil, err
	}
	held := lock
	defer func() {
		if held != nil {
			held.Close() //nolint:errcheck // releasing on a refusal
		}
	}()

	names, complete, err := storageVerbSessionInventory(request.Cfg, cityPath)
	switch {
	case err != nil && !request.Force.RemoteRuntime:
		return nil, fmt.Errorf("listing this city's sessions to prove none is running: %w. Resolve it, or attest --%s=%s if the sessions run where this host cannot list them", err, storageForceFlag, storageHazardRemoteRuntime)
	case len(names) > 0:
		sort.Strings(names)
		shown := names
		if len(shown) > 10 {
			shown = shown[:10]
		}
		return nil, fmt.Errorf("%d session(s) of this city are running (%s), and each can write to the work store. Stop the city with `%s` and run this again", len(names), strings.Join(shown, ", "), storageStopCommand)
	case !complete && !request.Force.RemoteRuntime:
		return nil, fmt.Errorf("the session runtime could not list every session of this city, so it cannot prove none is running. Resolve it, or attest --%s=%s if they run where this host cannot list them", storageForceFlag, storageHazardRemoteRuntime)
	}
	if scope.Splits && !request.Force.RemoteRuntime {
		if name, remote := cityRunsRemoteSessions(request.Cfg); remote {
			return nil, fmt.Errorf("this city runs sessions on a remote runtime ([session] provider %q). After the split those sessions refuse infrastructure-class commands (mail, nudges, session lookups) until worker-site transport (CN-2) exists, and an older gc on a remote host could still write infrastructure beads to the work store. Attest that with --%s=%s, or stay unsplit", name, storageForceFlag, storageHazardRemoteRuntime)
		}
	}
	held = nil
	return lock, nil
}

// controllerLockHolderRefusal names who holds the controller lock.
func controllerLockHolderRefusal(identity controllerIdentityReply) error {
	authority := "a controller"
	switch identity.HostingMode {
	case controllerHostingStandalone:
		authority = "a standalone controller"
	case controllerHostingSupervisor:
		authority = "a supervisor-hosted controller"
	}
	if identity.PID != 0 {
		authority += fmt.Sprintf(" (PID %d)", identity.PID)
	}
	return fmt.Errorf("%s, gc stop, or another storage command holds this city's controller lock (.gc/controller.lock); a copy proven against a city something else owns proves nothing. Stop it with `%s` and run this again", authority, storageStopCommand)
}

// storageVerbSessionInventory lists the city's running sessions and whether
// the list is complete (listRunningForStop's rule); a test seam.
var storageVerbSessionInventory = func(cfg *config.City, cityPath string) ([]string, bool, error) {
	sp, err := newStatusSessionProviderForCityWithSnapshot(cfg, cityPath, nil)
	if err != nil {
		return nil, false, err
	}
	names, complete := listRunningForStop(sp, io.Discard)
	return names, complete, nil
}

// cityRunsRemoteSessions reports whether the city's session runtime runs its
// sessions on other hosts: k8s, hybrid (whose remote leg is k8s) or ssh:.
//
// The city's own [session] provider decides, not the operator's shell: a
// GC_SESSION override in the shell running this command (a developer's
// "fake") must not hide that the city's sessions run in pods. An override
// that names a remote runtime counts too. Pack-declared runtimes declare no
// remoteness, so they are not presumed remote; a remote one that cannot list
// its sessions completely still refuses through the inventory check.
func cityRunsRemoteSessions(cfg *config.City) (string, bool) {
	if cfg == nil {
		return "", false
	}
	remote := func(name string) bool {
		return name == "k8s" || name == "hybrid" || strings.HasPrefix(name, "ssh:")
	}
	configured := strings.TrimSpace(cfg.Session.Provider)
	if remote(configured) {
		return configured, true
	}
	if override := strings.TrimSpace(os.Getenv("GC_SESSION")); remote(override) {
		return override, true
	}
	return configured, false
}

// sharedWorkStoreRefusal is the shared-store check for this city's work store
// (storage_shared_store.go).
func sharedWorkStoreRefusal(cityPath string, force storageForce) (attested bool, err error) {
	return sharedStoreRefusal(cityPath, "this city's work store", cityPath, force)
}

// foreignCityRows returns the ids of rows whose scope names another city:
// under a shared-store attestation they always refuse (unattributed rows are
// taken as this city's, which is the attested part).
func foreignCityRows(rows map[string]string, cityName string) []string {
	var ids []string
	for id, ref := range rows {
		ref = strings.TrimSpace(ref)
		if strings.HasPrefix(ref, "city:") && cityName != "" && ref != "city:"+cityName {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// sharedStoreForeignRowRefusal censuses the work store's infrastructure rows
// for rows another city's scope names.
func sharedStoreForeignRowRefusal(cityPath string, cfg *config.City) error {
	source, err := openInfraMigrationSource(cityPath)
	if err != nil {
		return fmt.Errorf("opening the work store to census rows other cities own: %w", err)
	}
	defer closeBeadStoreHandle(source) //nolint:errcheck // best-effort close
	snapshot, err := readInfraSnapshot(source)
	if err != nil {
		return fmt.Errorf("censusing the work store for rows other cities own: %w", err)
	}
	refs := make(map[string]string, len(snapshot))
	for _, b := range snapshot {
		refs[b.ID] = b.Metadata[beadmeta.RootStoreRefMetadataKey]
	}
	if ids := foreignCityRows(refs, loadedCityName(cfg, cityPath)); len(ids) > 0 {
		return fmt.Errorf("%d infrastructure row(s) in the shared work store belong to another city's scope (%s); the shared-store attestation covers only rows that name no city, so nothing was written", len(ids), infraStrandedIDList(ids))
	}
	return nil
}

// recordStorageAttestation appends accepted attestations to
// .gc/storage-attestations.log and says so on stderr.
func recordStorageAttestation(cityPath, logPrefix string, hazards []string, stderr io.Writer) {
	if len(hazards) == 0 {
		return
	}
	operator := "unknown"
	if u, err := user.Current(); err == nil {
		operator = u.Username
	}
	at := time.Now().UTC().Format(time.RFC3339)
	for _, h := range hazards {
		fmt.Fprintf(stderr, "%s: attested --%s=%s (%s, %s)\n", logPrefix, storageForceFlag, h, operator, at) //nolint:errcheck // best-effort stderr
	}
	f, err := os.OpenFile(filepath.Join(cityPath, ".gc", storageAttestationLog), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(stderr, "%s: recording the attestation: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return
	}
	defer f.Close() //nolint:errcheck // best-effort
	for _, h := range hazards {
		fmt.Fprintf(f, "#gc:attested %s %s %s %s\n", h, operator, at, logPrefix) //nolint:errcheck // best-effort
	}
}
