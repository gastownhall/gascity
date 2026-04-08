package dashboard

import (
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

//go:embed static
var staticFiles embed.FS

// ConvoyFetcher defines the legacy dashboard fetch contract for the
// core non-service panels.
type ConvoyFetcher interface {
	FetchConvoys() ([]ConvoyRow, error)
	FetchMergeQueue() ([]MergeQueueRow, error)
	FetchWorkers() ([]WorkerRow, error)
	FetchMail() ([]MailRow, error)
	FetchRigs() ([]RigRow, error)
	FetchDogs() ([]DogRow, error)
	FetchEscalations() ([]EscalationRow, error)
	FetchHealth() (*HealthRow, error)
	FetchQueues() ([]QueueRow, error)
	FetchAssigned() ([]AssignedRow, error)
	FetchMayor() (*MayorStatus, error)
	FetchIssues() ([]IssueRow, error)
	FetchActivity() ([]ActivityRow, error)
}

// ServiceFetcher defines the workspace-services dashboard fetch contract.
type ServiceFetcher interface {
	FetchServices() ([]ServiceRow, error)
}

type dashboardFetcher interface {
	ConvoyFetcher
	ServiceFetcher
}

type scopedDashboardFetcher interface {
	dashboardFetcher
	Scope(string) dashboardFetcher
}

type servicesUnavailable interface {
	error
	ServicesUnavailable() bool
}

type servicePanelState string

const (
	servicePanelStateNone        servicePanelState = ""
	servicePanelStateUnavailable servicePanelState = "unavailable"
	servicePanelStateFetchFailed servicePanelState = "fetch_failed"
	servicePanelStateTimedOut    servicePanelState = "timed_out"
)

// ConvoyHandler handles HTTP requests for the convoy dashboard.
type ConvoyHandler struct {
	fetcher      dashboardFetcher
	template     *template.Template
	fetchTimeout time.Duration
	csrfToken    string
	isSupervisor bool   // true when connected to a supervisor API
	apiURL       string // supervisor API URL for city list fetches
	defaultCity  string // initial city scope for supervisor mode
}

type dashboardFetchResult struct {
	convoys          []ConvoyRow
	mergeQueue       []MergeQueueRow
	workers          []WorkerRow
	mail             []MailRow
	services         []ServiceRow
	servicesFinished bool
	servicesState    servicePanelState
	rigs             []RigRow
	dogs             []DogRow
	escalations      []EscalationRow
	health           *HealthRow
	queues           []QueueRow
	assigned         []AssignedRow
	mayor            *MayorStatus
	issues           []IssueRow
	activity         []ActivityRow
}

type dashboardFetchState struct {
	mu     sync.RWMutex
	result dashboardFetchResult
}

func (s *dashboardFetchState) update(apply func(*dashboardFetchResult)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	apply(&s.result)
}

func (s *dashboardFetchState) snapshot() dashboardFetchResult {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.result
}

// NewConvoyHandler creates a new convoy handler.
func NewConvoyHandler(fetcher dashboardFetcher, isSupervisor bool, apiURL, defaultCity string, fetchTimeout time.Duration, csrfToken string) (*ConvoyHandler, error) {
	tmpl, err := LoadTemplates()
	if err != nil {
		return nil, err
	}
	if isSupervisor {
		if _, ok := fetcher.(scopedDashboardFetcher); !ok {
			return nil, errors.New("supervisor dashboard requires a scoped fetcher")
		}
	}

	return &ConvoyHandler{
		fetcher:      fetcher,
		template:     tmpl,
		fetchTimeout: fetchTimeout,
		csrfToken:    csrfToken,
		isSupervisor: isSupervisor,
		apiURL:       apiURL,
		defaultCity:  defaultCity,
	}, nil
}

// ServeHTTP handles GET / requests and renders the convoy dashboard.
func (h *ConvoyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	expandPanel := r.URL.Query().Get("expand")

	// In supervisor mode, resolve city list and selected city.
	var cities []CityTab
	var selectedCity string
	fetcher := h.fetcher
	if h.isSupervisor {
		cities = fetchCityTabs(h.apiURL)
		selectedCity = resolveSelectedCity(r.URL.Query().Get("city"), h.defaultCity, cities)
		if selectedCity != "" {
			if scoped, ok := h.fetcher.(scopedDashboardFetcher); ok {
				fetcher = scoped.Scope(selectedCity)
			}
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), h.fetchTimeout)
	defer cancel()

	var (
		results dashboardFetchState
		wg      sync.WaitGroup
	)

	wg.Add(14)

	go func() {
		defer wg.Done()
		convoys, err := fetcher.FetchConvoys()
		results.update(func(r *dashboardFetchResult) { r.convoys = convoys })
		if err != nil {
			log.Printf("dashboard: FetchConvoys failed: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		mergeQueue, err := fetcher.FetchMergeQueue()
		results.update(func(r *dashboardFetchResult) { r.mergeQueue = mergeQueue })
		if err != nil {
			log.Printf("dashboard: FetchMergeQueue failed: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		workers, err := fetcher.FetchWorkers()
		results.update(func(r *dashboardFetchResult) { r.workers = workers })
		if err != nil {
			log.Printf("dashboard: FetchWorkers failed: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		mail, err := fetcher.FetchMail()
		results.update(func(r *dashboardFetchResult) { r.mail = mail })
		if err != nil {
			log.Printf("dashboard: FetchMail failed: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		services, err := fetcher.FetchServices()
		var unavailable servicesUnavailable
		results.update(func(r *dashboardFetchResult) {
			r.services = services
			r.servicesFinished = true
			switch {
			case errors.As(err, &unavailable) && unavailable.ServicesUnavailable():
				r.servicesState = servicePanelStateUnavailable
			case err != nil:
				r.servicesState = servicePanelStateFetchFailed
			default:
				r.servicesState = servicePanelStateNone
			}
		})
		if err != nil {
			if !errors.As(err, &unavailable) || !unavailable.ServicesUnavailable() {
				log.Printf("dashboard: FetchServices failed: %v", err)
			}
		}
	}()
	go func() {
		defer wg.Done()
		rigs, err := fetcher.FetchRigs()
		results.update(func(r *dashboardFetchResult) { r.rigs = rigs })
		if err != nil {
			log.Printf("dashboard: FetchRigs failed: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		dogs, err := fetcher.FetchDogs()
		results.update(func(r *dashboardFetchResult) { r.dogs = dogs })
		if err != nil {
			log.Printf("dashboard: FetchDogs failed: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		escalations, err := fetcher.FetchEscalations()
		results.update(func(r *dashboardFetchResult) { r.escalations = escalations })
		if err != nil {
			log.Printf("dashboard: FetchEscalations failed: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		health, err := fetcher.FetchHealth()
		results.update(func(r *dashboardFetchResult) { r.health = health })
		if err != nil {
			log.Printf("dashboard: FetchHealth failed: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		queues, err := fetcher.FetchQueues()
		results.update(func(r *dashboardFetchResult) { r.queues = queues })
		if err != nil {
			log.Printf("dashboard: FetchQueues failed: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		assigned, err := fetcher.FetchAssigned()
		results.update(func(r *dashboardFetchResult) { r.assigned = assigned })
		if err != nil {
			log.Printf("dashboard: FetchAssigned failed: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		mayor, err := fetcher.FetchMayor()
		results.update(func(r *dashboardFetchResult) { r.mayor = mayor })
		if err != nil {
			log.Printf("dashboard: FetchMayor failed: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		issues, err := fetcher.FetchIssues()
		results.update(func(r *dashboardFetchResult) { r.issues = issues })
		if err != nil {
			log.Printf("dashboard: FetchIssues failed: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		activity, err := fetcher.FetchActivity()
		results.update(func(r *dashboardFetchResult) { r.activity = activity })
		if err != nil {
			log.Printf("dashboard: FetchActivity failed: %v", err)
		}
	}()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	timedOut := false
	select {
	case <-done:
	case <-ctx.Done():
		timedOut = true
		log.Printf("dashboard: fetch timeout after %v, using partial data", h.fetchTimeout)
		// Proceed with whatever data has been collected so far.
		// Blocking on <-done would defeat the timeout.
	}

	snapshot := results.snapshot()
	if timedOut && !snapshot.servicesFinished {
		snapshot.servicesState = servicePanelStateTimedOut
	}
	summary := computeSummary(
		snapshot.workers,
		snapshot.assigned,
		snapshot.issues,
		snapshot.convoys,
		snapshot.escalations,
		snapshot.activity,
	)

	data := ConvoyData{
		Convoys:       snapshot.convoys,
		MergeQueue:    snapshot.mergeQueue,
		Workers:       snapshot.workers,
		Mail:          snapshot.mail,
		Services:      snapshot.services,
		ServicesState: snapshot.servicesState,
		Rigs:          snapshot.rigs,
		Dogs:          snapshot.dogs,
		Escalations:   snapshot.escalations,
		Health:        snapshot.health,
		Queues:        snapshot.queues,
		Assigned:      snapshot.assigned,
		Mayor:         snapshot.mayor,
		Issues:        enrichIssuesWithAssignees(snapshot.issues, snapshot.assigned),
		Activity:      snapshot.activity,
		Summary:       summary,
		Expand:        expandPanel,
		CSRFToken:     h.csrfToken,
		Cities:        cities,
		SelectedCity:  selectedCity,
	}

	var buf bytes.Buffer
	if err := h.template.ExecuteTemplate(&buf, "convoy.html", data); err != nil {
		log.Printf("dashboard: template execution failed: %v", err)
		http.Error(w, "Failed to render template", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if _, err := buf.WriteTo(w); err != nil {
		log.Printf("dashboard: response write failed: %v", err)
	}
}

// ServeActivityPanel handles GET /panels/activity for targeted panel refresh.
// Only fetches activity data, then renders just the activity panel HTML
// fragment. Used by the JS event router for high-frequency observation events
// that don't affect other panels.
func (h *ConvoyHandler) ServeActivityPanel(w http.ResponseWriter, r *http.Request) {
	fetcher := h.fetcher
	if city := resolveSelectedCity(r.URL.Query().Get("city"), h.defaultCity, nil); city != "" {
		if scoped, ok := h.fetcher.(scopedDashboardFetcher); ok {
			fetcher = scoped.Scope(city)
		}
	}
	activity, err := fetcher.FetchActivity()
	if err != nil {
		log.Printf("dashboard: FetchActivity failed: %v", err)
		// Return 503 so htmx skips the swap, preserving existing panel content.
		http.Error(w, "Activity data unavailable", http.StatusServiceUnavailable)
		return
	}

	data := ConvoyData{Activity: activity}

	var buf bytes.Buffer
	if err := h.template.ExecuteTemplate(&buf, "_panel_activity.html", data); err != nil {
		log.Printf("dashboard: activity panel template failed: %v", err)
		http.Error(w, "Failed to render panel", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if _, err := buf.WriteTo(w); err != nil {
		log.Printf("dashboard: activity panel write failed: %v", err)
	}
}

func resolveSelectedCity(requestedCity, defaultCity string, cities []CityTab) string {
	if requestedCity != "" {
		return requestedCity
	}
	if defaultCity != "" {
		if len(cities) == 0 {
			return defaultCity
		}
		for _, city := range cities {
			if city.Name == defaultCity {
				return defaultCity
			}
		}
	}
	for _, city := range cities {
		if city.Running {
			return city.Name
		}
	}
	if len(cities) > 0 {
		return cities[0].Name
	}
	return ""
}

// computeSummary calculates dashboard stats and alerts from fetched data.
func computeSummary(workers []WorkerRow, assigned []AssignedRow, issues []IssueRow,
	convoys []ConvoyRow, escalations []EscalationRow, activity []ActivityRow,
) *Summary {
	summary := &Summary{
		PolecatCount:    len(workers),
		AssignedCount:   len(assigned),
		IssueCount:      len(issues),
		ConvoyCount:     len(convoys),
		EscalationCount: len(escalations),
	}

	for _, w := range workers {
		if w.WorkStatus == "stuck" {
			summary.StuckPolecats++
		}
	}
	for _, a := range assigned {
		if a.IsStale {
			summary.StaleAssigned++
		}
	}
	for _, e := range escalations {
		if !e.Acked {
			summary.UnackedEscalations++
		}
	}
	for _, i := range issues {
		if i.Priority == 1 || i.Priority == 2 {
			summary.HighPriorityIssues++
		}
	}
	for _, a := range activity {
		if a.Type == "session.crashed" {
			summary.DeadSessions++
		}
	}

	summary.HasAlerts = summary.StuckPolecats > 0 ||
		summary.StaleAssigned > 0 ||
		summary.UnackedEscalations > 0 ||
		summary.DeadSessions > 0 ||
		summary.HighPriorityIssues > 0

	return summary
}

// fetchCityTabs fetches the city list from the supervisor API for the city selector.
func fetchCityTabs(apiURL string) []CityTab {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(strings.TrimRight(apiURL, "/") + "/v0/cities")
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close() //nolint:errcheck
		}
		return nil
	}
	defer resp.Body.Close() //nolint:errcheck

	var list struct {
		Items []struct {
			Name    string `json:"name"`
			Running bool   `json:"running"`
		} `json:"items"`
	}
	if json.NewDecoder(resp.Body).Decode(&list) != nil {
		return nil
	}

	tabs := make([]CityTab, 0, len(list.Items))
	for _, c := range list.Items {
		tabs = append(tabs, CityTab{Name: c.Name, Running: c.Running})
	}
	return tabs
}

// enrichIssuesWithAssignees adds Assignee info to issues by cross-referencing assigned beads.
func enrichIssuesWithAssignees(issues []IssueRow, assigned []AssignedRow) []IssueRow {
	assigneeMap := make(map[string]string)
	for _, a := range assigned {
		assigneeMap[a.ID] = a.Agent
	}
	for i := range issues {
		if agent, ok := assigneeMap[issues[i].ID]; ok {
			issues[i].Assignee = agent
		}
	}
	return issues
}

// generateCSRFToken creates a cryptographically random token for CSRF protection.
func generateCSRFToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		log.Fatalf("failed to generate CSRF token: %v", err)
	}
	return hex.EncodeToString(b)
}

// NewDashboardMux creates an HTTP handler that serves both the dashboard and API.
func NewDashboardMux(fetcher *APIFetcher, cityPath, cityName, apiURL, initialCityScope string, isSupervisor bool,
	fetchTimeout, defaultRunTimeout, maxRunTimeout time.Duration,
) (http.Handler, error) {
	csrfToken := generateCSRFToken()

	convoyHandler, err := NewConvoyHandler(fetcher, isSupervisor, apiURL, initialCityScope, fetchTimeout, csrfToken)
	if err != nil {
		return nil, err
	}

	apiHandler := NewAPIHandler(cityPath, cityName, apiURL, initialCityScope, defaultRunTimeout, maxRunTimeout, csrfToken)

	staticFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return nil, err
	}
	staticHandler := http.FileServer(http.FS(staticFS))

	mux := http.NewServeMux()
	mux.Handle("/api/", apiHandler)
	mux.HandleFunc("/panels/activity", convoyHandler.ServeActivityPanel)
	mux.Handle("/static/", http.StripPrefix("/static/", staticHandler))
	mux.Handle("/", convoyHandler)

	return mux, nil
}
