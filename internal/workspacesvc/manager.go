package workspacesvc

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/supervisor"
)

// Manager owns the lifecycle and status projection for workspace services.
type Manager struct {
	rt RuntimeContext

	opMu                 sync.Mutex
	mu                   sync.RWMutex
	entries              map[string]*entry
	closed               bool
	havePublicationCache bool
	lastPublicationRefs  publicationRefs
	lastPublicationError string
}

type entry struct {
	spec   config.Service
	status Status
	inst   Instance
}

type closeTarget struct {
	name string
	inst Instance
}

type publishedServiceSnapshot struct {
	ServiceName string `json:"service_name"`
	Published   bool   `json:"published"`
	Visibility  string `json:"visibility,omitempty"`
	CurrentURL  string `json:"current_url,omitempty"`
	URLVersion  int    `json:"url_version"`
}

type servicePlan struct {
	spec config.Service
	base Status
}

// NewManager creates a service manager bound to one workspace runtime.
func NewManager(rt RuntimeContext) *Manager {
	return &Manager{
		rt:      rt,
		entries: make(map[string]*entry),
	}
}

// Reload reconciles the manager against the current config snapshot.
func (m *Manager) Reload() error {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	m.mu.RLock()
	if m.closed {
		m.mu.RUnlock()
		return nil
	}
	m.mu.RUnlock()

	cfg := m.rt.Config()
	m.mu.RLock()
	oldEntries := make(map[string]*entry, len(m.entries))
	for name, e := range m.entries {
		oldEntries[name] = e
	}
	m.mu.RUnlock()
	next := make(map[string]*entry, len(cfg.Services))
	plans := make([]servicePlan, 0, len(cfg.Services))
	reused := make(map[string]bool, len(oldEntries))
	now := time.Now().UTC()
	refs := m.currentPublicationRefs()

	for _, svc := range cfg.Services {
		base := baseStatus(m.rt.Config(), m.rt.PublicationConfig(), refs, svc, now)
		stateRoot, err := ensureStateRoot(m.rt.CityPath(), svc)
		base.StateRoot = stateRoot
		if err != nil {
			base.State = "degraded"
			base.LocalState = "config_error"
			base.Reason = err.Error()
		}
		plans = append(plans, servicePlan{spec: svc, base: base})
	}
	if err := writePublishedServiceMetadata(m.rt.CityPath(), statusMapFromPlans(plans)); err != nil {
		return err
	}
	for _, plan := range plans {
		svc := plan.spec
		base := plan.base
		if base.LocalState == "config_error" {
			next[svc.Name] = &entry{spec: svc, status: base}
			continue
		}
		if existing, ok := oldEntries[svc.Name]; ok && existing.inst != nil && reflect.DeepEqual(existing.spec, svc) {
			needsRecreate := svc.KindOrDefault() == "proxy_process" && proxyProcessPublicationContextChanged(existing.status, base)
			if !needsRecreate {
				existing.status = mergeStatus(base, existing.inst.Status())
				next[svc.Name] = existing
				reused[svc.Name] = true
				continue
			}
			// Recreate proxy processes when publication context changes so the
			// child process sees updated GC_SERVICE_PUBLIC_URL metadata.
			restarted, err := newProxyProcessInstance(m.rt, svc, base)
			if err != nil {
				log.Printf("workspacesvc: refresh proxy process %s on reload: %v", svc.Name, err)
				next[svc.Name] = existing
				reused[svc.Name] = true
				continue
			}
			base = mergeStatus(base, restarted.Status())
			next[svc.Name] = &entry{spec: svc, status: base, inst: restarted}
			continue
		}

		switch svc.KindOrDefault() {
		case "workflow":
			factory := lookupWorkflowContract(svc.Workflow.Contract)
			if factory == nil {
				base.State = "degraded"
				base.LocalState = "config_error"
				base.Reason = fmt.Sprintf("unsupported workflow contract %q", svc.Workflow.Contract)
				next[svc.Name] = &entry{spec: svc, status: base}
				continue
			}
			inst, err := factory(m.rt, svc)
			if err != nil {
				base.State = "degraded"
				base.LocalState = "config_error"
				base.Reason = err.Error()
				next[svc.Name] = &entry{spec: svc, status: base}
				continue
			}
			base = mergeStatus(base, inst.Status())
			next[svc.Name] = &entry{spec: svc, status: base, inst: inst}
		case "proxy_process":
			inst, err := newProxyProcessInstance(m.rt, svc, base)
			if err != nil {
				base.State = "degraded"
				base.LocalState = "config_error"
				base.Reason = err.Error()
				next[svc.Name] = &entry{spec: svc, status: base}
				continue
			}
			base = mergeStatus(base, inst.Status())
			next[svc.Name] = &entry{spec: svc, status: base, inst: inst}
		default:
			base.State = "degraded"
			base.LocalState = "config_error"
			base.Reason = fmt.Sprintf("unsupported service kind %q", svc.Kind)
			next[svc.Name] = &entry{spec: svc, status: base}
		}
	}
	m.mu.Lock()
	m.entries = next
	m.mu.Unlock()

	var firstErr error
	for name, e := range oldEntries {
		if reused[name] {
			continue
		}
		if e.inst != nil {
			if err := e.inst.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}

	return firstErr
}

// Tick runs one service reconciliation pass.
func (m *Manager) Tick(ctx context.Context, now time.Time) {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	m.mu.RLock()
	if m.closed {
		m.mu.RUnlock()
		return
	}
	entries := make([]*entry, 0, len(m.entries))
	for _, e := range m.entries {
		entries = append(entries, e)
	}
	m.mu.RUnlock()

	refs := m.currentPublicationRefs()
	for _, e := range entries {
		if e.inst == nil {
			continue
		}
		base := baseStatus(m.rt.Config(), m.rt.PublicationConfig(), refs, e.spec, now)
		if proxyProcessPublicationContextChanged(e.status, base) {
			restarted, err := newProxyProcessInstance(m.rt, e.spec, base)
			if err != nil {
				log.Printf("workspacesvc: refresh proxy process %s on tick: %v", e.spec.Name, err)
				continue
			}
			old := e.inst
			swapped := false
			m.mu.Lock()
			if cur, ok := m.entries[e.spec.Name]; ok && cur.inst == old {
				cur.inst = restarted
				cur.status = mergeStatus(base, restarted.Status())
				swapped = true
			}
			m.mu.Unlock()
			if !swapped {
				if err := restarted.Close(); err != nil {
					log.Printf("workspacesvc: close unreused proxy process %s on tick: %v", e.spec.Name, err)
				}
				continue
			}
			if err := old.Close(); err != nil {
				log.Printf("workspacesvc: close proxy process %s after restart: %v", e.spec.Name, err)
			}
			continue
		}
		e.inst.Tick(ctx, now)
		status := mergeStatus(base, e.inst.Status())
		m.mu.Lock()
		if cur, ok := m.entries[e.spec.Name]; ok {
			cur.status = status
		}
		m.mu.Unlock()
	}
	_ = m.syncPublishedServiceMetadata()
}

// Close closes all runtime service instances.
func (m *Manager) Close() error {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	now := time.Now().UTC()
	m.mu.Lock()
	m.closed = true
	targets := make([]closeTarget, 0, len(m.entries))
	for name, e := range m.entries {
		if e.inst != nil {
			targets = append(targets, closeTarget{name: name, inst: e.inst})
			e.status.State = "stopping"
			e.status.LocalState = "stopping"
			e.status.Reason = "service_closing"
			e.status.UpdatedAt = now
			continue
		}
		e.status.State = "stopped"
		e.status.LocalState = "stopped"
		e.status.Reason = "service_closed"
		e.status.UpdatedAt = now
	}
	m.mu.Unlock()
	if len(targets) == 0 {
		return nil
	}

	var firstErr error
	results := make(map[string]error, len(targets))
	for _, target := range targets {
		if err := target.inst.Close(); err != nil {
			results[target.name] = err
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	now = time.Now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, target := range targets {
		e, ok := m.entries[target.name]
		if !ok {
			continue
		}
		if err := results[target.name]; err != nil {
			// Retain the instance so a subsequent Close() call can retry.
			e.inst = target.inst
			e.status.State = "degraded"
			e.status.LocalState = "close_error"
			e.status.Reason = err.Error()
			e.status.UpdatedAt = now
			continue
		}
		e.inst = nil
		e.status.State = "stopped"
		e.status.LocalState = "stopped"
		e.status.Reason = "service_closed"
		e.status.UpdatedAt = now
	}
	return firstErr
}

// List returns all current service statuses sorted by name.
func (m *Manager) List() []Status {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]Status, 0, len(m.entries))
	for _, e := range m.entries {
		out = append(out, e.status)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].ServiceName < out[j].ServiceName
	})
	return out
}

// Get returns one current service status by name.
func (m *Manager) Get(name string) (Status, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	e, ok := m.entries[name]
	if !ok {
		return Status{}, false
	}
	return e.status, true
}

// Restart closes and recreates a single service instance by name.
func (m *Manager) Restart(name string) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	m.mu.Lock()
	e, ok := m.entries[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("service %q not found", name)
	}
	if m.closed {
		m.mu.Unlock()
		return fmt.Errorf("manager is closed")
	}
	oldInst := e.inst
	spec := e.spec
	m.mu.Unlock()

	if oldInst != nil {
		_ = oldInst.Close()
	}

	now := time.Now().UTC()
	refs := m.currentPublicationRefs()
	base := baseStatus(m.rt.Config(), m.rt.PublicationConfig(), refs, spec, now)
	stateRoot, err := ensureStateRoot(m.rt.CityPath(), spec)
	base.StateRoot = stateRoot
	if err != nil {
		base.State = "degraded"
		base.LocalState = "config_error"
		base.Reason = err.Error()
		m.mu.Lock()
		e.inst = nil
		e.status = base
		m.mu.Unlock()
		return fmt.Errorf("restart service %q: %w", name, err)
	}

	var newInst Instance
	switch spec.KindOrDefault() {
	case "workflow":
		factory := lookupWorkflowContract(spec.Workflow.Contract)
		if factory == nil {
			return fmt.Errorf("restart service %q: unsupported workflow contract %q", name, spec.Workflow.Contract)
		}
		newInst, err = factory(m.rt, spec)
	case "proxy_process":
		newInst, err = newProxyProcessInstance(m.rt, spec, base)
	default:
		return fmt.Errorf("restart service %q: unsupported kind %q", name, spec.Kind)
	}
	if err != nil {
		base.State = "degraded"
		base.LocalState = "restart_failed"
		base.Reason = err.Error()
		m.mu.Lock()
		e.inst = nil
		e.status = base
		m.mu.Unlock()
		return fmt.Errorf("restart service %q: %w", name, err)
	}

	base = mergeStatus(base, newInst.Status())
	m.mu.Lock()
	e.inst = newInst
	e.status = base
	m.mu.Unlock()
	return nil
}

// AuthorizeAndServeHTTP routes /svc/{name}/... requests to the matching
// service instance using one registry snapshot for authorization and dispatch.
func (m *Manager) AuthorizeAndServeHTTP(name string, w http.ResponseWriter, r *http.Request, authorize func(Status) bool) bool {
	subpath, ok := serviceSubpath(r.URL.Path, name)
	if !ok {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	e, ok := m.entries[name]
	if !ok {
		return false
	}
	if authorize != nil && !authorize(e.status) {
		return true
	}
	if m.closed || e.inst == nil {
		return false
	}
	return e.inst.HandleHTTP(w, r, subpath)
}

// ServeHTTP routes /svc/{name}/... requests to the matching service instance.
func (m *Manager) ServeHTTP(w http.ResponseWriter, r *http.Request) bool {
	path := strings.TrimPrefix(r.URL.Path, "/svc/")
	if path == r.URL.Path || path == "" {
		return false
	}
	name := path
	if i := strings.IndexByte(path, '/'); i >= 0 {
		name = path[:i]
	}
	return m.AuthorizeAndServeHTTP(name, w, r, nil)
}

func serviceSubpath(path, name string) (string, bool) {
	mountPath := "/svc/" + name
	switch {
	case name == "":
		return "", false
	case path == mountPath:
		return "/", true
	case strings.HasPrefix(path, mountPath+"/"):
		return path[len(mountPath):], true
	default:
		return "", false
	}
}

func baseStatus(cfg *config.City, pubCfg supervisor.PublicationConfig, refs publicationRefs, svc config.Service, now time.Time) Status {
	visibility := svc.PublicationVisibilityOrDefault()
	status := Status{
		ServiceName:      svc.Name,
		Kind:             svc.KindOrDefault(),
		WorkflowContract: svc.Workflow.Contract,
		MountPath:        svc.MountPathOrDefault(),
		PublishMode:      svc.PublishModeOrDefault(),
		Visibility:       visibility,
		Hostname:         svc.PublicationHostnameOrDefault(),
		StateRoot:        svc.StateRootOrDefault(),
		State:            "ready",
		LocalState:       "ready",
		PublicationState: "private",
		UpdatedAt:        now,
		AllowWebSockets:  svc.Publication.AllowWebSockets,
	}

	switch visibility {
	case "private":
		status.PublicationState = "private"
	default:
		publishedURL, publicationReason := derivePublishedURL(pubCfg, refs, svc)
		if publishedURL != "" {
			status.URL = publishedURL
			status.PublicationState = "published"
			status.Reason = publicationReason
			break
		}
		if status.PublishMode == "direct" {
			if baseURL := directBaseURL(cfg); baseURL != "" {
				status.URL = strings.TrimRight(baseURL, "/") + status.MountPath
				status.PublicationState = "direct"
				status.Reason = "route_active"
				break
			}
			status.PublicationState = "blocked"
			status.Reason = "direct_base_url_unavailable"
			break
		}
		status.PublicationState = "blocked"
		if publicationReason != "" {
			status.Reason = publicationReason
		} else {
			status.Reason = "publication_unavailable"
		}
	}

	return status
}

func loadPublicationRefs(path, cityPath string) publicationRefs {
	refs, exists, err := supervisor.LoadCityPublicationRefs(path, cityPath)
	return publicationRefs{
		refs:   refs,
		exists: exists,
		err:    err,
	}
}

func mergeStatus(base, override Status) Status {
	if override.ServiceName != "" {
		base.ServiceName = override.ServiceName
	}
	if override.Kind != "" {
		base.Kind = override.Kind
	}
	if override.WorkflowContract != "" {
		base.WorkflowContract = override.WorkflowContract
	}
	if override.MountPath != "" {
		base.MountPath = override.MountPath
	}
	if override.PublishMode != "" {
		base.PublishMode = override.PublishMode
	}
	if override.Visibility != "" {
		base.Visibility = override.Visibility
	}
	if override.Hostname != "" {
		base.Hostname = override.Hostname
	}
	if override.StateRoot != "" {
		base.StateRoot = override.StateRoot
	}
	if override.URL != "" {
		base.URL = override.URL
	}
	if override.State != "" {
		base.State = override.State
	}
	if override.LocalState != "" {
		base.LocalState = override.LocalState
	}
	if override.PublicationState != "" {
		base.PublicationState = override.PublicationState
	}
	if override.Reason != "" {
		base.Reason = override.Reason
	}
	base.AllowWebSockets = base.AllowWebSockets || override.AllowWebSockets
	if !override.UpdatedAt.IsZero() {
		base.UpdatedAt = override.UpdatedAt
	}
	return base
}

func ensureStateRoot(cityPath string, svc config.Service) (string, error) {
	root := svc.StateRootOrDefault()
	absRoot := root
	if !filepath.IsAbs(absRoot) {
		absRoot = filepath.Join(cityPath, absRoot)
	}
	for _, dir := range []struct {
		path string
		mode os.FileMode
	}{
		{absRoot, 0o750},
		{filepath.Join(absRoot, "data"), 0o750},
		{filepath.Join(absRoot, "run"), 0o750},
		{filepath.Join(absRoot, "logs"), 0o750},
		{filepath.Join(absRoot, "secrets"), 0o700},
	} {
		if err := os.MkdirAll(dir.path, dir.mode); err != nil {
			return root, err
		}
		if err := os.Chmod(dir.path, dir.mode); err != nil {
			return root, err
		}
	}
	return root, nil
}

func directBaseURL(cfg *config.City) string {
	if cfg == nil || cfg.API.Port <= 0 {
		return ""
	}
	bind := cfg.API.BindOrDefault()
	switch bind {
	case "", "0.0.0.0", "::", "[::]":
		return ""
	}
	return "http://" + net.JoinHostPort(bind, strconv.Itoa(cfg.API.Port))
}

func (m *Manager) syncPublishedServiceMetadata() error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return writePublishedServiceMetadata(m.rt.CityPath(), statusMapFromEntries(m.entries))
}

func (m *Manager) logPublicationRefsError(refs publicationRefs) {
	if refs.err == nil {
		m.lastPublicationError = ""
		return
	}
	msg := refs.err.Error()
	if msg == m.lastPublicationError {
		return
	}
	m.lastPublicationError = msg
	log.Printf("workspacesvc: load publication refs for %s from %s: %v", m.rt.CityPath(), m.rt.PublicationStorePath(), refs.err)
}

func (m *Manager) currentPublicationRefs() publicationRefs {
	refs := loadPublicationRefs(m.rt.PublicationStorePath(), m.rt.CityPath())
	m.logPublicationRefsError(refs)
	if refs.err == nil && refs.exists {
		m.lastPublicationRefs = refs
		m.havePublicationCache = true
		return refs
	}
	if m.havePublicationCache {
		return m.lastPublicationRefs
	}
	return refs
}

func statusMapFromEntries(entries map[string]*entry) map[string]Status {
	statuses := make(map[string]Status, len(entries))
	for name, e := range entries {
		statuses[name] = e.status
	}
	return statuses
}

func statusMapFromPlans(plans []servicePlan) map[string]Status {
	statuses := make(map[string]Status, len(plans))
	for _, plan := range plans {
		statuses[plan.spec.Name] = plan.base
	}
	return statuses
}

func proxyProcessPublicationContextChanged(current, next Status) bool {
	return current.Kind == "proxy_process" &&
		(current.URL != next.URL ||
			current.Visibility != next.Visibility ||
			current.PublicationState != next.PublicationState)
}

func writePublishedServiceMetadata(cityPath string, statuses map[string]Status) error {
	dir := citylayout.PublishedServicesDir(cityPath)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create published services dir: %w", err)
	}
	if err := os.Chmod(dir, 0o750); err != nil {
		return fmt.Errorf("chmod published services dir: %w", err)
	}

	current := make(map[string]struct{}, len(statuses))
	for name, status := range statuses {
		current[name] = struct{}{}
		path := filepath.Join(dir, name+".json")
		snapshot := publishedServiceSnapshot{
			ServiceName: name,
			Published:   status.URL != "",
			Visibility:  status.Visibility,
			CurrentURL:  status.URL,
			URLVersion:  loadPublishedServiceURLVersion(path, status.URL),
		}
		if err := writeJSONAtomically(path, snapshot, 0o640); err != nil {
			return fmt.Errorf("write published service metadata for %q: %w", name, err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read published services dir: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if strings.Contains(entry.Name(), ".json.tmp.") {
			if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("remove stale published service temp file %q: %w", entry.Name(), err)
			}
			continue
		}
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".json")
		if _, ok := current[name]; ok {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove stale published service metadata %q: %w", entry.Name(), err)
		}
	}
	return nil
}

func loadPublishedServiceURLVersion(path, url string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		if url == "" {
			return 0
		}
		return 1
	}
	var snapshot publishedServiceSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		if url == "" {
			return 0
		}
		return 1
	}
	if url == "" {
		if snapshot.URLVersion > 0 {
			return snapshot.URLVersion
		}
		return 0
	}
	if snapshot.CurrentURL == url {
		if snapshot.URLVersion > 0 {
			return snapshot.URLVersion
		}
		return 1
	}
	if snapshot.URLVersion > 0 {
		return snapshot.URLVersion + 1
	}
	return 1
}

func writeJSONAtomically(path string, payload any, mode os.FileMode) error {
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal json: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp.*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}
	cleanup = false
	return nil
}
