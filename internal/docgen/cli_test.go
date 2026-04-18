package docgen

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// testTree builds a synthetic command tree for testing.
func testTree() *cobra.Command {
	root := &cobra.Command{
		Use:   "myapp",
		Short: "Test app",
	}
	root.PersistentFlags().StringP("config", "c", "", "path to config file")

	child := &cobra.Command{
		Use:   "deploy <env>",
		Short: "Deploy the app",
		Long:  "Deploy the application to a target environment.\n\nSupports staging and production.",
		Example: `  myapp deploy staging
  myapp deploy production --force`,
	}
	child.Flags().BoolP("force", "f", false, "skip confirmation")
	child.Flags().Int("replicas", 3, "number of replicas")

	hidden := &cobra.Command{
		Use:    "internal",
		Short:  "Internal command",
		Hidden: true,
	}

	status := &cobra.Command{
		Use:   "status",
		Short: "Show deployment status",
	}

	root.AddCommand(child, hidden, status)
	return root
}

func TestRenderCLIMarkdown_BasicTree(t *testing.T) {
	root := testTree()
	var buf bytes.Buffer
	if err := RenderCLIMarkdown(&buf, root); err != nil {
		t.Fatalf("RenderCLIMarkdown: %v", err)
	}

	md := buf.String()

	// Check header.
	if !strings.Contains(md, "# CLI Reference") {
		t.Error("missing CLI Reference header")
	}
	if !strings.Contains(md, "Auto-generated") {
		t.Error("missing auto-generated note")
	}

	// Check command headings.
	if !strings.Contains(md, "## myapp") {
		t.Error("missing root command heading")
	}
	if !strings.Contains(md, "## myapp deploy") {
		t.Error("missing deploy heading")
	}
	if !strings.Contains(md, "## myapp status") {
		t.Error("missing status heading")
	}

	// Check synopsis.
	if !strings.Contains(md, "myapp deploy <env>") {
		t.Error("missing deploy synopsis")
	}

	// Check flags table.
	if !strings.Contains(md, "`--force`") {
		t.Error("missing --force flag")
	}
	if !strings.Contains(md, "`--replicas`") {
		t.Error("missing --replicas flag")
	}
}

func TestRenderCLIMarkdown_HiddenCommandSkipped(t *testing.T) {
	root := testTree()
	var buf bytes.Buffer
	if err := RenderCLIMarkdown(&buf, root); err != nil {
		t.Fatalf("RenderCLIMarkdown: %v", err)
	}

	if strings.Contains(buf.String(), "internal") {
		t.Error("hidden command 'internal' should not appear in output")
	}
}

func TestRenderCLIMarkdown_HiddenFlagSkipped(t *testing.T) {
	root := &cobra.Command{Use: "app", Short: "test"}
	root.Flags().String("visible", "", "shown flag")
	root.Flags().String("secret", "", "hidden flag")
	root.Flags().MarkHidden("secret") //nolint:errcheck

	var buf bytes.Buffer
	if err := RenderCLIMarkdown(&buf, root); err != nil {
		t.Fatalf("RenderCLIMarkdown: %v", err)
	}

	md := buf.String()
	if !strings.Contains(md, "visible") {
		t.Error("visible flag missing")
	}
	if strings.Contains(md, "secret") {
		t.Error("hidden flag 'secret' should not appear")
	}
}

func TestRenderCLIMarkdown_LongDescription(t *testing.T) {
	root := testTree()
	var buf bytes.Buffer
	if err := RenderCLIMarkdown(&buf, root); err != nil {
		t.Fatalf("RenderCLIMarkdown: %v", err)
	}

	md := buf.String()
	if !strings.Contains(md, "Deploy the application to a target environment.") {
		t.Error("Long description not rendered")
	}
	if !strings.Contains(md, "Supports staging and production.") {
		t.Error("Long description second paragraph missing")
	}
}

func TestRenderCLIMarkdown_ExampleField(t *testing.T) {
	root := testTree()
	var buf bytes.Buffer
	if err := RenderCLIMarkdown(&buf, root); err != nil {
		t.Fatalf("RenderCLIMarkdown: %v", err)
	}

	md := buf.String()
	if !strings.Contains(md, "**Example:**") {
		t.Error("Example heading missing")
	}
	if !strings.Contains(md, "myapp deploy staging") {
		t.Error("Example content missing")
	}
}

func TestRenderCLIMarkdown_InheritedFlagsExcluded(t *testing.T) {
	root := testTree()
	var buf bytes.Buffer
	if err := RenderCLIMarkdown(&buf, root); err != nil {
		t.Fatalf("RenderCLIMarkdown: %v", err)
	}

	md := buf.String()

	// The deploy section should NOT show the inherited --config flag
	// in its local flags table.
	deployIdx := strings.Index(md, "## myapp deploy")
	statusIdx := strings.Index(md, "## myapp status")
	if deployIdx < 0 || statusIdx < 0 {
		t.Fatal("missing expected sections")
	}
	deploySection := md[deployIdx:statusIdx]

	// --config is a persistent flag on root, should not appear in deploy's flags.
	if strings.Contains(deploySection, "--config") {
		t.Error("inherited flag --config should not appear in deploy's flags table")
	}
}

func TestRenderCLIMarkdown_SubcommandsTable(t *testing.T) {
	root := testTree()
	var buf bytes.Buffer
	if err := RenderCLIMarkdown(&buf, root); err != nil {
		t.Fatalf("RenderCLIMarkdown: %v", err)
	}

	md := buf.String()

	// Root should have a subcommands table with deploy and status.
	if !strings.Contains(md, "| Subcommand | Description |") {
		t.Error("missing subcommands table")
	}
	if !strings.Contains(md, "myapp deploy") {
		t.Error("subcommands table missing deploy")
	}
	if !strings.Contains(md, "myapp status") {
		t.Error("subcommands table missing status")
	}
	// Anchor links.
	if !strings.Contains(md, "#myapp-deploy") {
		t.Error("missing anchor link for deploy")
	}
}

func TestRenderCLIMarkdown_ShorthandFlags(t *testing.T) {
	root := testTree()
	var buf bytes.Buffer
	if err := RenderCLIMarkdown(&buf, root); err != nil {
		t.Fatalf("RenderCLIMarkdown: %v", err)
	}

	md := buf.String()
	// --force has shorthand -f.
	if !strings.Contains(md, "`-f`, `--force`") {
		t.Error("shorthand flag not rendered as '-f, --force'")
	}
}

func TestRenderCLIMarkdown_ZeroDefaultOmitted(t *testing.T) {
	root := &cobra.Command{Use: "app", Short: "test"}
	root.Flags().Bool("verbose", false, "verbose output")
	root.Flags().String("output", "", "output path")
	root.Flags().Int("count", 0, "number of items")
	root.Flags().String("format", "json", "output format")

	var buf bytes.Buffer
	if err := RenderCLIMarkdown(&buf, root); err != nil {
		t.Fatalf("RenderCLIMarkdown: %v", err)
	}

	md := buf.String()

	// Zero defaults should not appear.
	lines := strings.Split(md, "\n")
	for _, line := range lines {
		if strings.Contains(line, "--verbose") && strings.Contains(line, "`false`") {
			t.Error("bool zero default 'false' should be omitted")
		}
		if strings.Contains(line, "--count") && strings.Contains(line, "`0`") {
			t.Error("int zero default '0' should be omitted")
		}
	}

	// Non-zero default should appear.
	if !strings.Contains(md, "`json`") {
		t.Error("non-zero default 'json' should appear")
	}
}

func TestWriteCLIMarkdown_TrimsExtraBlankEOF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cli.md")
	if err := WriteCLIMarkdown(path, testTree()); err != nil {
		t.Fatalf("WriteCLIMarkdown: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Fatalf("generated markdown missing final newline")
	}
	if strings.HasSuffix(string(data), "\n\n") {
		t.Fatalf("generated markdown has extra blank line at EOF")
	}
}
