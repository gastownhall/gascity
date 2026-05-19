package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/spf13/cobra"
)

type poolDemandCounter interface {
	PoolDemandCount(template string) (int, error)
}

func newPoolCountCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "pool-count <template>",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			cityPath, err := resolveCommandCity(nil)
			if err != nil {
				fmt.Fprintf(stderr, "gc pool-count: %v\n", err) //nolint:errcheck
				return
			}
			store, err := openCityStoreAt(cityPath)
			if err != nil {
				fmt.Fprintf(stderr, "gc pool-count: %v\n", err) //nolint:errcheck
				return
			}
			cfg, err := loadCityConfig(cityPath, stderr)
			if err == nil {
				resolveRigPaths(cityPath, cfg.Rigs)
				if agent, ok := resolveAgentIdentity(cfg, strings.TrimSpace(args[0]), currentRigContext(cfg)); ok {
					if rigName := configuredRigName(cityPath, &agent, cfg.Rigs); rigName != "" {
						if rigRoot := rigRootForName(rigName, cfg.Rigs); strings.TrimSpace(rigRoot) != "" {
							if rigStore, rigErr := openStoreAtForCity(rigRoot, cityPath); rigErr == nil {
								store = rigStore
							} else {
								fmt.Fprintf(stderr, "gc pool-count: rig store %q: %v\n", rigName, rigErr) //nolint:errcheck
							}
						}
					}
				}
			}
			counter, ok := store.(poolDemandCounter)
			if !ok {
				if bdStore, ok := store.(*beads.BdStore); ok {
					counter = bdStore
				}
			}
			if counter == nil {
				fmt.Fprintln(stdout, "0") //nolint:errcheck
				return
			}
			count, err := counter.PoolDemandCount(strings.TrimSpace(args[0]))
			if err != nil {
				fmt.Fprintf(stderr, "gc pool-count: %v\n", err) //nolint:errcheck
				fmt.Fprintln(stdout, "0")                       //nolint:errcheck
				return
			}
			fmt.Fprintln(stdout, count) //nolint:errcheck
		},
	}
	return cmd
}
