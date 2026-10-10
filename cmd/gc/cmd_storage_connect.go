package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// `gc storage connect` (DESIGN C5 G10): attach this city's work store, or one
// rig's, to an HTTP bd serve.
//
// It delegates the write to beads (bdhttp.Attach through
// beads.ConnectRemoteScope): gc never writes metadata.json or the
// http_target.json sidecar itself. What it adds over `bd connect` is the
// city's view: the scope's per-city / per-rig credential from city.toml for
// the verifying handshake, and the boot gate's wire_compat verdict before
// anything is written, so a scope is never attached to a server its native
// store would refuse at the next start.

const storageConnectVerb = "connect"

// storageConnectOptions are the command's flags.
type storageConnectOptions struct {
	Rig              string
	ProjectID        string
	CAFile           string
	AllowPlaintext   bool
	ConvertWorkspace bool
	Retarget         bool
}

// connectRemoteScope is the library door, behind a seam for tests.
var connectRemoteScope = beads.ConnectRemoteScope

func newStorageConnectCmd(surface storageCommandSurface, stdout, stderr io.Writer) *cobra.Command {
	var opts storageConnectOptions
	cmd := &cobra.Command{
		Use:          storageConnectVerb + " <url>",
		Short:        "Attach this city's (or a rig's) work store to an HTTP bd serve",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		Long: `Attach the city's work store, or one rig's with --rig, to the bd serve at
<url>, so gc and bd reach it over HTTP.

The server is verified first: the handshake is sent with the scope's
configured credential ([beads] credential, or the rig's beads_credential;
bd's own credential ladder when none is configured), the project it owns is
pinned (or checked against --project-id), and its capabilities are checked
against the native store's requirement table (the wire_compat check the
start-up gate runs). Only a server that passes is attached, through beads'
own attach, which writes the backend selection into .beads/metadata.json and
the per-user .beads/http_target.json sidecar.

A scope that already holds a local workspace is switched only with
--convert-workspace (bd connect --clear restores it); one attached to another
server or project is re-pinned only with --retarget.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			request, err := resolveStorageOperatorRequest()
			if err != nil {
				fmt.Fprintf(stderr, "gc %s %s: %v\n", surface.Namespace, storageConnectVerb, err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			return exitForCode(doStorageConnect(cmd.Context(), request.CityPath, request.Cfg, args[0], opts, stdout, stderr))
		},
	}
	cmd.Flags().StringVar(&opts.Rig, "rig", "", "attach this rig's work store instead of the city's")
	cmd.Flags().StringVar(&opts.ProjectID, "project-id", "", "require the server to own this project (default: pin the one it reports)")
	cmd.Flags().StringVar(&opts.CAFile, "ca-file", "", "absolute path of a PEM file that becomes this server's only trusted root")
	cmd.Flags().BoolVar(&opts.AllowPlaintext, "allow-plaintext", false, "allow a bearer over plain http to a non-loopback server (only without a configured credential; a configured one uses its allow_insecure_credential)")
	cmd.Flags().BoolVar(&opts.ConvertWorkspace, "convert-workspace", false, "switch a scope that already holds a local workspace")
	cmd.Flags().BoolVar(&opts.Retarget, "retarget", false, "re-pin a scope already attached to another server or project")
	return cmd
}

// doStorageConnect attaches one scope and reports what it attached.
func doStorageConnect(ctx context.Context, cityPath string, cfg *config.City, rawURL string, opts storageConnectOptions, stdout, stderr io.Writer) int {
	const logPrefix = "gc storage " + storageConnectVerb
	if ctx == nil {
		ctx = context.Background()
	}
	scopeRoot, label, err := storageConnectScope(cityPath, cfg, opts.Rig)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if ca := strings.TrimSpace(opts.CAFile); ca != "" && !filepath.IsAbs(ca) {
		if abs, absErr := filepath.Abs(ca); absErr == nil {
			opts.CAFile = abs
		}
	}
	result, err := connectRemoteScope(ctx, beads.RemoteConnectRequest{
		CityPath:         cityPath,
		ScopeRoot:        scopeRoot,
		URL:              rawURL,
		ProjectID:        opts.ProjectID,
		CAFile:           opts.CAFile,
		AllowPlaintext:   opts.AllowPlaintext,
		ConvertWorkspace: opts.ConvertWorkspace,
		Retarget:         opts.Retarget,
	})
	if err != nil {
		fmt.Fprintf(stderr, "%s: %s: %v\n", logPrefix, label, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	verb := "attached to"
	if !result.Changed {
		verb = "was already attached to"
	}
	fmt.Fprintf(stdout, "%s: %s %s %s\n", logPrefix, label, verb, result.URL)                                                                    //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "  workspace:  %s (backend %q)\n", result.BeadsDir, result.Backend)                                                      //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "  project:    %s (pinned)\n", result.Server.ProjectID)                                                                  //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "  server:     bd %s, wire_revision %d\n", result.Server.BdVersion, result.Server.WireRevision)                          //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "  credential: %s\n", result.Credential)                                                                                 //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "  %s: %s %s\n", beads.RemoteBootGateCheck, strings.ToUpper(string(result.WireCompat.State)), result.WireCompat.Summary) //nolint:errcheck // best-effort stdout
	if resolvedNativeTransportMode(cfg) == beads.NativeTransportOff {
		fmt.Fprintln(stdout, `  note: beads.native_transport = "off": this city reaches the server through the bd CLI`) //nolint:errcheck // best-effort stdout
	}
	return 0
}

// storageConnectScope resolves the scope a connect attaches: the city, or the
// named rig's root.
func storageConnectScope(cityPath string, cfg *config.City, rigName string) (scopeRoot, label string, err error) {
	rigName = strings.TrimSpace(rigName)
	if rigName == "" {
		return cityPath, "city", nil
	}
	if cfg != nil {
		for _, rig := range cfg.Rigs {
			if rig.Name != rigName {
				continue
			}
			path := strings.TrimSpace(rig.Path)
			if path == "" {
				return "", "", fmt.Errorf("rig %q has no path", rigName)
			}
			if !filepath.IsAbs(path) {
				path = filepath.Join(cityPath, path)
			}
			return path, fmt.Sprintf("rig %q", rigName), nil
		}
	}
	return "", "", fmt.Errorf("no rig named %q in this city", rigName)
}
