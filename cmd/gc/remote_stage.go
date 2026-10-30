package main

// Remote stages (S5 §6.2, §7.3).
//
// A K8S pod gets its city by `gc init --from <copy of the controller's city>`
// (internal/runtime/k8s initCityInPod). The copy skips .gc, so the pod has the
// controller's [storage] section and none of its binding, marker or notes. A
// pod-side one-shot gc then ran the storage boot gate, found a work store with
// no infrastructure row, and reached genesis: a pod-local EMPTY binding that
// the pod served mail, nudges and sessions from without any error. The
// cleared-note hold could not fire either, because the note lives in the
// controller's .gc.
//
// `gc init --from --remote-stage <controller city>` writes
// .gc/remote-stage.toml. On a remote stage of a split city every
// infrastructure class refuses, naming the controller's city, before any
// binding is resolved, opened or created; genesis is unreachable. Work ids
// still resolve to the work store. This is the degraded mode the owner chose
// for --force=remote-runtime (decision O1): loud refusals until worker-site
// transport (CN-2) or an HTTP-served binding exists, never split brain.
//
// The worker-site plane (.gc/control-plane.toml) is deliberately not reused:
// it disables controller and runtime dials wholesale, which pods depend on.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// remoteStageFileName is the record `gc init --remote-stage` writes under .gc.
const remoteStageFileName = "remote-stage.toml"

// remoteStage is what a remote stage records about the city it stages.
type remoteStage struct {
	// ControllerCity is the staged city's name.
	ControllerCity string `toml:"controller_city"`
	// ControllerPath is the controller's city directory, as --remote-stage named it.
	ControllerPath string `toml:"controller_path"`
	// WorkStore is the controller work store's identity at staging time: the
	// store the controller's binding belongs to.
	WorkStore string `toml:"work_store,omitempty"`
	// StagedAt is when the stage was written (RFC 3339).
	StagedAt string `toml:"staged_at"`
}

func remoteStagePath(cityPath string) string {
	return filepath.Join(cityPath, ".gc", remoteStageFileName)
}

// readRemoteStage reads the record; ok is false when there is none.
func readRemoteStage(cityPath string) (remoteStage, bool, error) {
	data, err := os.ReadFile(remoteStagePath(cityPath))
	if errors.Is(err, fs.ErrNotExist) {
		return remoteStage{}, false, nil
	}
	if err != nil {
		return remoteStage{}, false, err
	}
	var stage remoteStage
	if _, err := toml.Decode(string(data), &stage); err != nil {
		return remoteStage{}, false, fmt.Errorf("parsing %s: %w", remoteStagePath(cityPath), err)
	}
	return stage, true, nil
}

// writeRemoteStage writes the record atomically.
func writeRemoteStage(cityPath string, stage remoteStage) error {
	if stage.StagedAt == "" {
		stage.StagedAt = time.Now().UTC().Format(time.RFC3339)
	}
	dir := filepath.Join(cityPath, ".gc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, remoteStageFileName+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if err := toml.NewEncoder(tmp).Encode(stage); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, remoteStagePath(cityPath)); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// errRemoteStageInfra is the refusal every infrastructure class gets on a
// remote stage of a split city.
type errRemoteStageInfra struct{ stage remoteStage }

func (e errRemoteStageInfra) Error() string {
	city := e.stage.ControllerCity
	if city == "" {
		city = e.stage.ControllerPath
	}
	return fmt.Sprintf("this directory is a remote stage of city %s (%s); its infrastructure classes (mail, nudges, sessions, orders, workflows) are served by %s's controller and are refused here until worker-site transport (CN-2) exists. Work-class commands still run. Run infrastructure commands on the controller host",
		city, e.stage.ControllerPath, city)
}

// remoteStageStorageRefusal refuses the infrastructure classes of a remote
// stage, or returns nil when cityPath is not one. A record that exists and
// cannot be read refuses too: it is still evidence of a stage.
func remoteStageStorageRefusal(cityPath string) error {
	stage, ok, err := readRemoteStage(cityPath)
	switch {
	case err != nil:
		return fmt.Errorf("this directory carries a remote-stage record that cannot be read (%w), so it is refused as a remote stage: its infrastructure classes are served by the controller's city", err)
	case !ok:
		return nil
	}
	return errRemoteStageInfra{stage: stage}
}

// initRemoteStageFlag is gc init's --remote-stage: the controller city this
// directory stages. Set only by the init command.
var initRemoteStageFlag string

// initRemoteStageRefusal validates --remote-stage against the other flags.
func initRemoteStageRefusal(from string, noStart bool) error {
	if strings.TrimSpace(initRemoteStageFlag) == "" {
		return nil
	}
	if strings.TrimSpace(from) == "" {
		return errors.New("--remote-stage stages a copy of the controller's city, so it requires --from")
	}
	if !noStart {
		return errors.New("--remote-stage requires --no-start: a remote stage is never a city that starts; its controller runs elsewhere")
	}
	return nil
}

// recordInitRemoteStage writes the remote-stage record for a city gc init
// --from just copied, when --remote-stage asked for one.
func recordInitRemoteStage(srcDir, cityPath, cityName string) error {
	controller := strings.TrimSpace(initRemoteStageFlag)
	if controller == "" {
		return nil
	}
	stage := remoteStage{ControllerCity: cityName, ControllerPath: controller}
	if id, err := currentWorkStoreIdentity(srcDir); err == nil && !id.empty() {
		stage.WorkStore = id.String()
	}
	return writeRemoteStage(cityPath, stage)
}
