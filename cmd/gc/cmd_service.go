package main

import (
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/workspacesvc"
	"github.com/spf13/cobra"
)

type serviceStatusReader interface {
	ListServices() ([]workspacesvc.Status, error)
	GetService(name string) (workspacesvc.Status, error)
}

func newServiceCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Inspect workspace services",
		Args:  cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				fmt.Fprintln(stderr, "gc service: missing subcommand (list, doctor, restart)") //nolint:errcheck // best-effort stderr
			} else {
				fmt.Fprintf(stderr, "gc service: unknown subcommand %q\n", args[0]) //nolint:errcheck // best-effort stderr
			}
			return errExit
		},
	}
	cmd.AddCommand(
		newServiceListCmd(stdout, stderr),
		newServiceDoctorCmd(stdout, stderr),
		newServiceRestartCmd(stdout, stderr),
	)
	return cmd
}

func newServiceListCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List workspace services",
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdServiceList(stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

func newServiceDoctorCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor <name>",
		Short: "Show detailed workspace service status",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdServiceDoctor(args[0], stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

func cmdServiceList(stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc service list: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc service list: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return doServiceList(cfg, serviceReadClient(cityPath, cfg), stdout, stderr)
}

func cmdServiceDoctor(name string, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc service doctor: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc service doctor: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return doServiceDoctor(cfg, serviceReadClient(cityPath, cfg), name, stdout, stderr)
}

func newServiceRestartCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "restart <name>",
		Short: "Restart a workspace service",
		Long: `Stop and restart a workspace service by name.

The controller closes the current service process and starts a fresh one.
Useful after updating pack scripts without a full city restart.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdServiceRestart(args[0], stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

func cmdServiceRestart(name string, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc service restart: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc service restart: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	client := serviceRestartClient(cityPath, cfg)
	if client == nil {
		fmt.Fprintln(stderr, "gc service restart: controller is not running") //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := client.RestartService(name); err != nil {
		fmt.Fprintf(stderr, "gc service restart: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintf(stdout, "Service %q restarted.\n", name) //nolint:errcheck // best-effort stdout
	return 0
}

func serviceRestartClient(cityPath string, cfg *config.City) *api.Client {
	if controllerAlive(cityPath) != 0 && cfg.API.Port > 0 {
		bind := cfg.API.BindOrDefault()
		switch bind {
		case "0.0.0.0":
			bind = "127.0.0.1"
		case "::", "[::]":
			bind = "::1"
		}
		baseURL := fmt.Sprintf("http://%s", net.JoinHostPort(bind, strconv.Itoa(cfg.API.Port)))
		return api.NewCityScopedClient(baseURL, standaloneControllerCityName(cfg, cityPath))
	}
	if client := supervisorCityAPIClient(cityPath); client != nil {
		return client
	}
	return nil
}

func doServiceList(cfg *config.City, reader serviceStatusReader, stdout, stderr io.Writer) int {
	statuses, live := serviceStatuses(cfg, reader, stderr)
	if len(statuses) == 0 {
		fmt.Fprintln(stdout, "No services configured.") //nolint:errcheck // best-effort stdout
		return 0
	}

	sort.Slice(statuses, func(i, j int) bool {
		return statuses[i].ServiceName < statuses[j].ServiceName
	})

	fmt.Fprintf(stdout, "%-18s %-13s %-10s %-10s %-12s %s\n", "NAME", "KIND", "SERVICE", "LOCAL", "PUBLISH", "URL") //nolint:errcheck
	for _, status := range statuses {
		url := "-"
		if status.URL != "" {
			url = status.URL
		}
		serviceState := status.State
		localState := status.LocalState
		if !live {
			serviceState = "config"
			localState = "config"
		}
		fmt.Fprintf(stdout, "%-18s %-13s %-10s %-10s %-12s %s\n", //nolint:errcheck
			status.ServiceName,
			status.Kind,
			serviceState,
			localState,
			publicationState(status),
			url,
		)
	}
	return 0
}

func doServiceDoctor(cfg *config.City, reader serviceStatusReader, name string, stdout, stderr io.Writer) int {
	svc, ok := lookupService(cfg, name)
	if !ok {
		fmt.Fprintf(stderr, "gc service doctor: service %q not found\n", name) //nolint:errcheck // best-effort stderr
		return 1
	}

	status := configServiceStatus(svc)
	live := false
	if reader != nil {
		current, err := reader.GetService(name)
		if err == nil {
			status = current
			live = true
		} else {
			fmt.Fprintf(stderr, "gc service doctor: warning: %v (showing config view)\n", err) //nolint:errcheck // best-effort stderr
		}
	}

	fmt.Fprintf(stdout, "Name:              %s\n", status.ServiceName) //nolint:errcheck
	fmt.Fprintf(stdout, "Kind:              %s\n", status.Kind)        //nolint:errcheck
	if svc.KindOrDefault() == "workflow" {
		fmt.Fprintf(stdout, "Contract:          %s\n", svc.Workflow.Contract) //nolint:errcheck
	} else {
		fmt.Fprintf(stdout, "Command:           %s\n", strings.Join(svc.Process.Command, " ")) //nolint:errcheck
	}
	fmt.Fprintf(stdout, "Mount Path:        %s\n", status.MountPath)          //nolint:errcheck
	fmt.Fprintf(stdout, "Visibility:        %s\n", serviceVisibility(status)) //nolint:errcheck
	fmt.Fprintf(stdout, "Publish Mode:      %s\n", status.PublishMode)        //nolint:errcheck
	fmt.Fprintf(stdout, "Service State:     %s\n", status.State)              //nolint:errcheck
	fmt.Fprintf(stdout, "Local State:       %s\n", status.LocalState)         //nolint:errcheck
	fmt.Fprintf(stdout, "Publication State: %s\n", publicationState(status))  //nolint:errcheck
	fmt.Fprintf(stdout, "Public URL:        %s\n", emptyDash(status.URL))     //nolint:errcheck
	fmt.Fprintf(stdout, "State Root:        %s\n", status.StateRoot)          //nolint:errcheck
	fmt.Fprintf(stdout, "Reason:            %s\n", emptyDash(status.Reason))  //nolint:errcheck
	if !live {
		fmt.Fprintln(stdout, "Observed State:    controller API unavailable; showing config-derived view") //nolint:errcheck
	}
	return 0
}

func serviceStatuses(cfg *config.City, reader serviceStatusReader, stderr io.Writer) ([]workspacesvc.Status, bool) {
	if reader != nil {
		items, err := reader.ListServices()
		if err == nil {
			return items, true
		}
		fmt.Fprintf(stderr, "gc service list: warning: %v (showing config view)\n", err) //nolint:errcheck
	}

	items := make([]workspacesvc.Status, 0, len(cfg.Services))
	for _, svc := range cfg.Services {
		items = append(items, configServiceStatus(svc))
	}
	return items, false
}

func configServiceStatus(svc config.Service) workspacesvc.Status {
	return workspacesvc.Status{
		ServiceName:      svc.Name,
		Kind:             svc.KindOrDefault(),
		WorkflowContract: svc.Workflow.Contract,
		MountPath:        svc.MountPathOrDefault(),
		PublishMode:      svc.PublishModeOrDefault(),
		Visibility:       svc.PublicationVisibilityOrDefault(),
		Hostname:         svc.PublicationHostnameOrDefault(),
		StateRoot:        svc.StateRootOrDefault(),
		State:            "config",
		LocalState:       "config",
		PublicationState: "config",
		AllowWebSockets:  svc.Publication.AllowWebSockets,
	}
}

func lookupService(cfg *config.City, name string) (config.Service, bool) {
	for _, svc := range cfg.Services {
		if svc.Name == name {
			return svc, true
		}
	}
	return config.Service{}, false
}

func serviceReadClient(cityPath string, cfg *config.City) serviceStatusReader {
	if controllerAlive(cityPath) != 0 && cfg.API.Port > 0 {
		bind := cfg.API.BindOrDefault()
		switch bind {
		case "0.0.0.0":
			bind = "127.0.0.1"
		case "::", "[::]":
			bind = "::1"
		}
		baseURL := fmt.Sprintf("http://%s", net.JoinHostPort(bind, strconv.Itoa(cfg.API.Port)))
		return api.NewCityScopedClient(baseURL, standaloneControllerCityName(cfg, cityPath))
	}
	if client := supervisorCityAPIClient(cityPath); client != nil {
		return client
	}
	return nil
}

func publicationState(status workspacesvc.Status) string {
	if status.PublicationState != "" {
		return status.PublicationState
	}
	if status.Visibility != "" {
		return status.Visibility
	}
	return status.PublishMode
}

func serviceVisibility(status workspacesvc.Status) string {
	if strings.TrimSpace(status.Visibility) == "" {
		return "private"
	}
	return status.Visibility
}

func emptyDash(v string) string {
	if strings.TrimSpace(v) == "" {
		return "-"
	}
	return v
}
