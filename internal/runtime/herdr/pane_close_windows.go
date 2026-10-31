//go:build windows

package herdr

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/pathutil"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

var (
	killPaneRuntime          = proctable.KillRuntime
	scanPaneSessionRuntimes  = proctable.ScanBySessionID
	rootIsCityInfrastructure = proctable.RootIsCityInfrastructure
)

func (p *Provider) closeSessionPane(ctx context.Context, name, paneID string) error {
	var errs []error
	if err := p.terminatePaneProcessTree(ctx, name, paneID); err != nil {
		errs = append(errs, err)
	}
	if err := p.c.closePane(ctx, paneID); err != nil && !paneGone(err) {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (p *Provider) terminatePaneProcessTree(ctx context.Context, name, paneID string) error {
	shellPID, _, err := p.c.processInfo(ctx, paneID)
	if err != nil {
		if paneGone(err) {
			return nil
		}
		return fmt.Errorf("herdr: inspect pane %s before close: %w", paneID, err)
	}
	if shellPID <= 1 {
		return nil
	}
	boundPane, _ := p.GetMeta(name, metaBoundPane)
	if strings.TrimSpace(boundPane) != paneID {
		return fmt.Errorf("herdr: refusing to terminate pane %s for %q: binding now names pane %q", paneID, name, strings.TrimSpace(boundPane))
	}
	storedPID, _ := p.GetMeta(name, metaBoundShellPID)
	if strings.TrimSpace(storedPID) != strconv.Itoa(shellPID) {
		return fmt.Errorf("herdr: refusing to terminate pane %s for %q: shell PID changed from %q to %d", paneID, name, strings.TrimSpace(storedPID), shellPID)
	}
	storedIdentity, _ := p.GetMeta(name, metaBoundShellID)
	storedIdentity = strings.TrimSpace(storedIdentity)
	if storedIdentity == "" {
		return fmt.Errorf("herdr: refusing to terminate pane %s shell PID %d for %q without a bind-time start identity", paneID, shellPID, name)
	}
	currentIdentity, err := processIdentity(shellPID)
	if err != nil {
		return fmt.Errorf("herdr: confirm pane %s shell PID %d for %q: %w", paneID, shellPID, name, err)
	}
	if currentIdentity != storedIdentity {
		return fmt.Errorf("herdr: refusing to terminate pane %s shell PID %d for %q: start identity is %s, not bound %s", paneID, shellPID, name, currentIdentity, storedIdentity)
	}
	live := runtime.LiveRuntime{PID: shellPID, StartIdentity: storedIdentity}
	if err := killPaneRuntime(live); err != nil {
		return fmt.Errorf("herdr: terminate pane %s process tree rooted at PID %d: %w", paneID, shellPID, err)
	}
	return nil
}

func (p *Provider) terminateSessionOrphans(name string) ([]int, error) {
	sessionID, _ := p.GetMeta(name, "GC_SESSION_ID")
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, nil
	}
	expectedCity, _ := p.GetMeta(name, "GC_CITY_PATH")
	if strings.TrimSpace(expectedCity) == "" {
		expectedCity, _ = p.GetMeta(name, "GC_CITY")
	}
	if strings.TrimSpace(expectedCity) == "" {
		expectedCity = p.c.cityRoot
	}
	expectedCity = normalizeRuntimeCityPath(expectedCity)
	if expectedCity == "" {
		return nil, fmt.Errorf("herdr: refusing orphan cleanup for %q without a city identity", name)
	}
	epochText, _ := p.GetMeta(name, "GC_RUNTIME_EPOCH")
	expectedEpoch, err := strconv.Atoi(strings.TrimSpace(epochText))
	if err != nil || expectedEpoch <= 0 {
		return nil, fmt.Errorf("herdr: refusing orphan cleanup for %q without a runtime epoch", name)
	}
	live, err := scanPaneSessionRuntimes(sessionID)
	partial := errors.Is(err, proctable.ErrScanIncomplete)
	if err != nil && !partial {
		return nil, fmt.Errorf("herdr: scan remaining runtimes for %q: %w", name, err)
	}
	var errs []error
	var matched []int
	for _, rt := range live {
		if rt.SessionID != sessionID || rt.PID <= 1 || strings.TrimSpace(rt.StartIdentity) == "" {
			continue
		}
		if normalizeRuntimeCityPath(rt.City) != expectedCity || rt.Epoch != expectedEpoch {
			continue
		}
		if rt.ParentIsProviderInfrastructure {
			continue
		}
		infrastructure, ierr := rootIsCityInfrastructure(rt)
		if ierr != nil {
			errs = append(errs, fmt.Errorf("classify PID %d: %w", rt.PID, ierr))
			continue
		}
		if infrastructure || isHerdrInfrastructureName(rt.Name) {
			continue
		}
		matched = append(matched, rt.PID)
		if err := killPaneRuntime(rt); err != nil {
			errs = append(errs, fmt.Errorf("PID %d: %w", rt.PID, err))
		}
	}
	if partial {
		errs = append(errs, fmt.Errorf("herdr: scan remaining runtimes for %q: %w", name, err))
	}
	if len(errs) > 0 {
		return matched, fmt.Errorf("herdr: terminate remaining runtimes for %q: %w", name, errors.Join(errs...))
	}
	return matched, nil
}

func normalizeRuntimeCityPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	return strings.ToLower(pathutil.NormalizePathForCompare(path))
}

func isHerdrInfrastructureName(name string) bool {
	switch proctable.ProcessNameKey(filepath.Base(strings.TrimSpace(name))) {
	case "herdr":
		return true
	default:
		return false
	}
}
