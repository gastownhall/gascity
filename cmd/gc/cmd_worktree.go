package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

func newWorktreeCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "worktree",
		Short: "Manage cross-rig worktrees (crew pattern)",
		Long:  `Manage git worktrees for working in other rigs.`,
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			fmt.Fprintf(stderr, "gc worktree: unknown subcommand %q\n", args[0])
			return errExit
		},
	}
	cmd.AddCommand(
		newWorktreeAddCmd(stdout, stderr),
		newWorktreeRemoveCmd(stdout, stderr),
		newWorktreeListCmd(stdout, stderr),
	)
	return cmd
}

func newWorktreeAddCmd(stdout, stderr io.Writer) *cobra.Command {
	var agentFlag string
	var sourceRigFlag string
	cmd := &cobra.Command{
		Use:   "add <target-rig>",
		Short: "Add a cross-rig worktree",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return doWorktreeAdd(args[0], agentFlag, sourceRigFlag, stdout, stderr)
		},
	}
	cmd.Flags().StringVar(&agentFlag, "agent", "", "Agent name (defaults to GC_AGENT or GC_ALIAS)")
	cmd.Flags().StringVar(&sourceRigFlag, "source-rig", "", "Source rig name (defaults to GC_RIG)")
	return cmd
}

func newWorktreeRemoveCmd(stdout, stderr io.Writer) *cobra.Command {
	var agentFlag string
	var sourceRigFlag string
	cmd := &cobra.Command{
		Use:   "remove <target-rig>",
		Short: "Remove a cross-rig worktree",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return doWorktreeRemove(args[0], agentFlag, sourceRigFlag, stdout, stderr)
		},
	}
	cmd.Flags().StringVar(&agentFlag, "agent", "", "Agent name (defaults to GC_AGENT or GC_ALIAS)")
	cmd.Flags().StringVar(&sourceRigFlag, "source-rig", "", "Source rig name (defaults to GC_RIG)")
	return cmd
}

func newWorktreeListCmd(stdout, stderr io.Writer) *cobra.Command {
	var targetRigFlag string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List cross-rig worktrees",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return doWorktreeList(targetRigFlag, stdout, stderr)
		},
	}
	cmd.Flags().StringVar(&targetRigFlag, "target-rig", "", "Filter by target rig")
	return cmd
}


func currentAgentAndRig(agentFlag, sourceRigFlag string) (string, string) {
	agent := agentFlag
	if agent == "" {
		agent = os.Getenv("GC_AGENT")
		if agent == "" {
			agent = os.Getenv("GC_ALIAS")
		}
	}
	if strings.Contains(agent, "/") {
		parts := strings.Split(agent, "/")
		agent = parts[len(parts)-1]
	}

	sourceRig := sourceRigFlag
	if sourceRig == "" {
		sourceRig = os.Getenv("GC_RIG")
	}
	return agent, sourceRig
}

func doWorktreeAdd(targetRig, agentFlag, sourceRigFlag string, stdout, stderr io.Writer) error {
	agent, sourceRig := currentAgentAndRig(agentFlag, sourceRigFlag)
	if agent == "" || sourceRig == "" {
		fmt.Fprintln(stderr, "gc worktree add: must specify agent and source-rig (or run within agent context)")
		return errExit
	}

	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc worktree add: %v\n", err)
		return errExit
	}

	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc worktree add: %v\n", err)
		return errExit
	}

	var targetPath string
	for _, r := range cfg.Rigs {
		if r.Name == targetRig {
			targetPath = r.Path
			if !filepath.IsAbs(targetPath) {
				targetPath = filepath.Join(cityPath, targetPath)
			}
			break
		}
	}
	if targetPath == "" {
		fmt.Fprintf(stderr, "gc worktree add: rig %q not found\n", targetRig)
		return errExit
	}

	worktreeName := fmt.Sprintf("%s-from-%s", agent, sourceRig)
	worktreePath := filepath.Join(cityPath, ".gc", "worktrees", targetRig, "crew", worktreeName)
	branchName := fmt.Sprintf("%s-%s", targetRig, agent)

	if _, err := os.Stat(worktreePath); err == nil {
		fmt.Fprintf(stderr, "gc worktree add: worktree already exists at %s\n", worktreePath)
		return errExit
	}

	cmd := exec.Command("git", "-C", targetPath, "worktree", "add", worktreePath, "-b", branchName)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		// It's possible the branch already exists. Try checking it out instead of creating it.
		cmd = exec.Command("git", "-C", targetPath, "worktree", "add", worktreePath, branchName)
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		if err2 := cmd.Run(); err2 != nil {
			fmt.Fprintf(stderr, "gc worktree add: failed to add worktree: %v\n", err)
			return errExit
		}
	}

	fmt.Fprintf(stdout, "Added worktree at %s\n", worktreePath)
	return nil
}

func doWorktreeRemove(targetRig, agentFlag, sourceRigFlag string, stdout, stderr io.Writer) error {
	agent, sourceRig := currentAgentAndRig(agentFlag, sourceRigFlag)
	if agent == "" || sourceRig == "" {
		fmt.Fprintln(stderr, "gc worktree remove: must specify agent and source-rig (or run within agent context)")
		return errExit
	}

	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc worktree remove: %v\n", err)
		return errExit
	}

	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc worktree remove: %v\n", err)
		return errExit
	}

	var targetPath string
	for _, r := range cfg.Rigs {
		if r.Name == targetRig {
			targetPath = r.Path
			if !filepath.IsAbs(targetPath) {
				targetPath = filepath.Join(cityPath, targetPath)
			}
			break
		}
	}
	if targetPath == "" {
		fmt.Fprintf(stderr, "gc worktree remove: rig %q not found\n", targetRig)
		return errExit
	}

	worktreeName := fmt.Sprintf("%s-from-%s", agent, sourceRig)
	worktreePath := filepath.Join(cityPath, ".gc", "worktrees", targetRig, "crew", worktreeName)

	if _, err := os.Stat(worktreePath); os.IsNotExist(err) {
		fmt.Fprintf(stderr, "gc worktree remove: worktree not found at %s\n", worktreePath)
		return errExit
	}

	cmd := exec.Command("git", "-C", targetPath, "worktree", "remove", "--force", worktreePath)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(stderr, "gc worktree remove: failed to remove worktree: %v\n", err)
		return errExit
	}

	fmt.Fprintf(stdout, "Removed worktree at %s\n", worktreePath)
	return nil
}

func doWorktreeList(targetRigFlag string, stdout, stderr io.Writer) error {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc worktree list: %v\n", err)
		return errExit
	}

	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc worktree list: %v\n", err)
		return errExit
	}

	for _, r := range cfg.Rigs {
		if targetRigFlag != "" && r.Name != targetRigFlag {
			continue
		}
		crewDir := filepath.Join(cityPath, ".gc", "worktrees", r.Name, "crew")
		entries, err := os.ReadDir(crewDir)
		if err != nil {
			if !os.IsNotExist(err) {
				fmt.Fprintf(stderr, "gc worktree list: error reading %s: %v\n", crewDir, err)
			}
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				fmt.Fprintf(stdout, "%s\t%s\n", r.Name, filepath.Join(crewDir, entry.Name()))
			}
		}
	}

	return nil
}
