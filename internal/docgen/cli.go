package docgen

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func escapeMDXText(s string) string {
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "{", "&#123;")
	s = strings.ReplaceAll(s, "}", "&#125;")
	return s
}

// RenderCLIMarkdown writes a CLI reference by walking a cobra command tree.
// Hidden commands are skipped. The output format matches config.md style:
// H2 headings per command, synopsis, examples, flags table, subcommands table.
func RenderCLIMarkdown(w io.Writer, root *cobra.Command) error {
	if _, err := fmt.Fprintf(w, "# CLI Reference\n\n"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "> **Auto-generated** — do not edit. Run `go run ./cmd/genschema` to regenerate.\n\n"); err != nil {
		return err
	}

	// Render global flags from root.
	if err := renderGlobalFlags(w, root); err != nil {
		return err
	}

	// Walk the tree.
	return walkCommands(w, root)
}

// WriteCLIMarkdown writes the CLI reference to a file using atomic write.
func WriteCLIMarkdown(path string, root *cobra.Command) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".gencli-md-*")
	if err != nil {
		return fmt.Errorf("creating temp file for %s: %w", path, err)
	}
	tmpName := tmp.Name()

	var rendered strings.Builder
	if err := RenderCLIMarkdown(&rendered, root); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("rendering %s: %w", path, err)
	}
	data := strings.TrimRight(rendered.String(), "\n") + "\n"
	if _, err := io.WriteString(tmp, data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("closing %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("renaming %s: %w", path, err)
	}
	return nil
}

// walkCommands recursively renders each non-hidden command.
func walkCommands(w io.Writer, cmd *cobra.Command) error {
	if err := renderCommand(w, cmd); err != nil {
		return err
	}
	for _, child := range cmd.Commands() {
		if child.Hidden {
			continue
		}
		if err := walkCommands(w, child); err != nil {
			return err
		}
	}
	return nil
}

// renderCommand renders a single command section.
func renderCommand(w io.Writer, cmd *cobra.Command) error {
	fullPath := cmd.CommandPath()

	// H2 heading.
	if _, err := fmt.Fprintf(w, "## %s\n\n", fullPath); err != nil {
		return err
	}

	// Description — Long if present, else Short.
	desc := cmd.Long
	if desc == "" {
		desc = cmd.Short
	}
	if desc != "" {
		if _, err := fmt.Fprintf(w, "%s\n\n", escapeMDXText(strings.TrimSpace(desc))); err != nil {
			return err
		}
	}

	// Synopsis.
	if _, err := fmt.Fprintf(w, "```\n%s\n```\n\n", cmd.UseLine()); err != nil {
		return err
	}

	// Example.
	if cmd.Example != "" {
		if _, err := fmt.Fprintf(w, "**Example:**\n\n```\n%s\n```\n\n", strings.TrimSpace(cmd.Example)); err != nil {
			return err
		}
	}

	// Local flags table (non-hidden, non-inherited).
	if err := renderFlagsTable(w, cmd.LocalNonPersistentFlags()); err != nil {
		return err
	}

	// Subcommands table.
	if err := renderSubcommandsTable(w, cmd); err != nil {
		return err
	}

	return nil
}

// renderGlobalFlags renders the global (persistent) flags section.
func renderGlobalFlags(w io.Writer, root *cobra.Command) error {
	var flags []flagInfo
	root.PersistentFlags().VisitAll(func(f *pflag.Flag) {
		if f.Hidden {
			return
		}
		flags = append(flags, newFlagInfo(f))
	})
	if len(flags) == 0 {
		return nil
	}

	if _, err := fmt.Fprintf(w, "## Global Flags\n\n"); err != nil {
		return err
	}
	return writeFlagTable(w, flags)
}

// flagInfo holds rendered flag metadata.
type flagInfo struct {
	Name    string
	Type    string
	Default string
	Desc    string
}

// newFlagInfo extracts display info from a pflag.Flag.
func newFlagInfo(f *pflag.Flag) flagInfo {
	name := "`--" + f.Name + "`"
	if f.Shorthand != "" {
		name = "`-" + f.Shorthand + "`, `--" + f.Name + "`"
	}

	defVal := ""
	if !isZeroDefault(f.DefValue, f.Value.Type()) {
		defVal = "`" + f.DefValue + "`"
	}

	return flagInfo{
		Name:    name,
		Type:    f.Value.Type(),
		Default: defVal,
		Desc:    escapeMDXText(strings.ReplaceAll(f.Usage, "|", "\\|")),
	}
}

// isZeroDefault returns true if the default value is the zero value for its type.
func isZeroDefault(val, typ string) bool {
	switch typ {
	case "bool":
		return val == "false"
	case "int", "int32", "int64", "uint", "uint32", "uint64", "float32", "float64":
		return val == "0"
	case "string":
		return val == ""
	case "stringSlice", "stringArray":
		return val == "[]"
	default:
		return val == ""
	}
}

// renderFlagsTable renders a flags table for local non-persistent flags.
func renderFlagsTable(w io.Writer, fs *pflag.FlagSet) error {
	var flags []flagInfo
	fs.VisitAll(func(f *pflag.Flag) {
		if f.Hidden {
			return
		}
		flags = append(flags, newFlagInfo(f))
	})
	if len(flags) == 0 {
		return nil
	}
	return writeFlagTable(w, flags)
}

// writeFlagTable writes the markdown table for a slice of flags.
func writeFlagTable(w io.Writer, flags []flagInfo) error {
	if _, err := fmt.Fprintf(w, "| Flag | Type | Default | Description |\n"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "|------|------|---------|-------------|\n"); err != nil {
		return err
	}
	for _, f := range flags {
		if _, err := fmt.Fprintf(w, "| %s | %s | %s | %s |\n",
			f.Name, f.Type, f.Default, f.Desc); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	return nil
}

// renderSubcommandsTable renders a subcommands table if the command has children.
func renderSubcommandsTable(w io.Writer, cmd *cobra.Command) error {
	var children []*cobra.Command
	for _, c := range cmd.Commands() {
		if !c.Hidden {
			children = append(children, c)
		}
	}
	if len(children) == 0 {
		return nil
	}

	if _, err := fmt.Fprintf(w, "| Subcommand | Description |\n"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "|------------|-------------|\n"); err != nil {
		return err
	}
	for _, c := range children {
		anchor := strings.ReplaceAll(c.CommandPath(), " ", "-")
		anchor = strings.ToLower(anchor)
		if _, err := fmt.Fprintf(w, "| [%s](#%s) | %s |\n",
			c.CommandPath(), anchor, escapeMDXText(c.Short)); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	return nil
}
