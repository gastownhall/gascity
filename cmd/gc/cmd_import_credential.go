package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/gchome"
	"github.com/gastownhall/gascity/internal/gitcred"
	"github.com/spf13/cobra"
)

// literalTokenPattern matches a value that looks like a GitHub token so the CLI
// can refuse a literal secret where a pointer is expected.
var literalTokenPattern = regexp.MustCompile(`^(gh[pousr]_|github_pat_)`)

// tokenEnvNamePattern is the shell env-var-name grammar --token-env accepts.
var tokenEnvNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func newImportCredentialCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "credential",
		Short: "Manage pack-source git credentials",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(
		newImportCredentialAddCmd(stdout, stderr),
		newImportCredentialListCmd(stdout, stderr),
		newImportCredentialRemoveCmd(stdout, stderr),
	)
	return cmd
}

func newImportCredentialAddCmd(stdout, stderr io.Writer) *cobra.Command {
	var helper, tokenFile, tokenEnv, sshKeyFile, username string
	var global bool
	cmd := &cobra.Command{
		Use:   "add <match> (--helper CMD | --token-file PATH | --token-env NAME | --ssh-key-file PATH) [--username NAME] [--global]",
		Short: "Register a pack-source credential",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			rule := gitcred.Rule{
				Match:      args[0],
				Username:   strings.TrimSpace(username),
				Helper:     strings.TrimSpace(helper),
				TokenFile:  strings.TrimSpace(tokenFile),
				TokenEnv:   strings.TrimSpace(tokenEnv),
				SSHKeyFile: strings.TrimSpace(sshKeyFile),
			}
			if doImportCredentialAdd(rule, global, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&helper, "helper", "", "Command whose stdout is the token (executed per fetch)")
	cmd.Flags().StringVar(&tokenFile, "token-file", "", "Path to a file containing the token")
	cmd.Flags().StringVar(&tokenEnv, "token-env", "", "Name of an environment variable holding the token")
	cmd.Flags().StringVar(&sshKeyFile, "ssh-key-file", "", "Path to an SSH private key for git@/ssh:// sources")
	cmd.Flags().StringVar(&username, "username", "", "Username sent to the remote (default x-access-token)")
	cmd.Flags().BoolVar(&global, "global", false, "Write $GC_HOME/credentials.toml instead of the city file")
	cmd.MarkFlagsMutuallyExclusive("helper", "token-file", "token-env", "ssh-key-file")
	return cmd
}

func newImportCredentialListCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List registered pack-source credentials",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if doImportCredentialList(stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

func newImportCredentialRemoveCmd(stdout, stderr io.Writer) *cobra.Command {
	var global bool
	cmd := &cobra.Command{
		Use:   "remove <match>",
		Short: "Remove a registered pack-source credential",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if doImportCredentialRemove(args[0], global, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&global, "global", false, "Remove from $GC_HOME/credentials.toml instead of the city file")
	return cmd
}

// credentialsTargetFile resolves the file add/remove operate on: an explicit
// $GC_GIT_CREDENTIALS_FILE overrides everything (with a stderr notice), --global
// selects $GC_HOME/credentials.toml, otherwise the city .gc/credentials.toml.
func credentialsTargetFile(global bool, stderr io.Writer) (string, error) {
	if explicit := strings.TrimSpace(os.Getenv(gitcred.EnvCredentialsFile)); explicit != "" {
		fmt.Fprintf(stderr, "note: writing $GC_GIT_CREDENTIALS_FILE (%s)\n", explicit) //nolint:errcheck
		return explicit, nil
	}
	if global {
		home := gchome.Default()
		if err := os.MkdirAll(home, 0o700); err != nil {
			return "", fmt.Errorf("creating %s: %w", home, err)
		}
		return filepath.Join(home, "credentials.toml"), nil
	}
	cityPath, err := resolveImportRoot()
	if err != nil {
		return "", err
	}
	gcDir := filepath.Join(cityPath, ".gc")
	if _, err := os.Stat(gcDir); err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%s is not a city (no .gc directory); use --global to write $GC_HOME/credentials.toml", cityPath)
		}
		return "", err
	}
	return filepath.Join(gcDir, "credentials.toml"), nil
}

type credentialsFileDoc struct {
	Credential []gitcred.Rule `toml:"credential"`
}

func readCredentialsFile(path string) (credentialsFileDoc, error) {
	var doc credentialsFileDoc
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return doc, nil
		}
		return doc, err
	}
	if _, err := toml.Decode(string(data), &doc); err != nil {
		return doc, fmt.Errorf("parsing %s: %w", path, err)
	}
	return doc, nil
}

func writeCredentialsFile(path string, doc credentialsFileDoc) error {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(doc); err != nil {
		return fmt.Errorf("encoding credentials: %w", err)
	}
	return fsys.WriteFileAtomic(fsys.OSFS{}, path, buf.Bytes(), 0o600)
}

func doImportCredentialAdd(rule gitcred.Rule, global bool, stdout, stderr io.Writer) int {
	fail := func(msg string) int {
		fmt.Fprintf(stderr, "gc import credential add: %s\n", msg) //nolint:errcheck
		return 1
	}

	pointerType, err := selectPointer(rule)
	if err != nil {
		return fail(err.Error())
	}

	normalizedMatch, err := normalizeCredentialMatch(rule.Match)
	if err != nil {
		return fail(err.Error())
	}
	rule.Match = normalizedMatch

	if rule.TokenEnv != "" {
		if !tokenEnvNamePattern.MatchString(rule.TokenEnv) {
			return fail("--token-env takes an environment variable NAME, not a value")
		}
		if strings.TrimSpace(os.Getenv(rule.TokenEnv)) == "" {
			fmt.Fprintf(stderr, "warning: environment variable %s is not set in the current shell\n", rule.TokenEnv) //nolint:errcheck
		}
	}
	if rule.TokenFile != "" || rule.SSHKeyFile != "" {
		checkPath := rule.TokenFile
		if checkPath == "" {
			checkPath = rule.SSHKeyFile
		}
		if !credentialPathExists(checkPath) {
			fmt.Fprintf(stderr, "warning: %s does not exist yet\n", checkPath) //nolint:errcheck
		}
	}

	// Literal-token refusal: the helper value or the match must not look like a
	// raw token.
	if literalTokenPattern.MatchString(rule.Helper) || literalTokenPattern.MatchString(normalizedMatch) {
		return fail("refusing a literal token; pass a pointer (--helper 'gh auth token', --token-file, or --token-env)")
	}

	path, err := credentialsTargetFile(global, stderr)
	if err != nil {
		return fail(err.Error())
	}
	doc, err := readCredentialsFile(path)
	if err != nil {
		return fail(err.Error())
	}
	for _, existing := range doc.Credential {
		if em, _ := normalizeCredentialMatch(existing.Match); em == normalizedMatch {
			return fail(fmt.Sprintf("credential for %q already exists in %s; run 'gc import credential remove %s' first", normalizedMatch, path, normalizedMatch))
		}
	}
	doc.Credential = append(doc.Credential, rule)
	if err := writeCredentialsFile(path, doc); err != nil {
		return fail(err.Error())
	}
	fmt.Fprintf(stdout, "Added pack credential for %q (%s) to %s\n", normalizedMatch, pointerType, path) //nolint:errcheck
	return 0
}

func doImportCredentialList(stdout, stderr io.Writer) int {
	cityRoot := ""
	if root, err := resolveImportRoot(); err == nil {
		cityRoot = root
	} else {
		fmt.Fprintf(stderr, "note: not in a city; listing global and env credential layers only\n") //nolint:errcheck
	}
	rules, err := gitcred.Load(cityRoot)
	if err != nil {
		fmt.Fprintf(stderr, "gc import credential list: %v\n", err) //nolint:errcheck
		return 1
	}
	loaded := rules.All()
	if len(loaded) == 0 && !rules.HasCommandLayer() {
		fmt.Fprintln(stdout, "No pack credentials configured") //nolint:errcheck
		return 0
	}
	for _, lr := range loaded {
		pointerType, _ := selectPointer(lr.Rule)
		pointer := pointerValue(lr.Rule)
		username := lr.Username
		if strings.TrimSpace(username) == "" {
			username = gitcred.DefaultUsername
		}
		fmt.Fprintf(stdout, "%s\t%s\t%s=%s\t%s\n", lr.Match, username, pointerType, pointer, lr.Origin) //nolint:errcheck
	}
	if rules.HasCommandLayer() {
		fmt.Fprintf(stdout, "(any)\t\tcommand=$%s\t(env)\n", "GC_GIT_CREDENTIAL_COMMAND") //nolint:errcheck
	}
	return 0
}

func doImportCredentialRemove(match string, global bool, stdout, stderr io.Writer) int {
	fail := func(msg string) int {
		fmt.Fprintf(stderr, "gc import credential remove: %s\n", msg) //nolint:errcheck
		return 1
	}
	normalizedMatch, err := normalizeCredentialMatch(match)
	if err != nil {
		return fail(err.Error())
	}
	path, err := credentialsTargetFile(global, stderr)
	if err != nil {
		return fail(err.Error())
	}
	doc, err := readCredentialsFile(path)
	if err != nil {
		return fail(err.Error())
	}
	kept := doc.Credential[:0:0]
	removed := false
	for _, existing := range doc.Credential {
		if em, _ := normalizeCredentialMatch(existing.Match); em == normalizedMatch {
			removed = true
			continue
		}
		kept = append(kept, existing)
	}
	if !removed {
		if other := findCredentialInOtherLayer(normalizedMatch, path); other != "" {
			return fail(fmt.Sprintf("no credential for %q in %s (found in %s; use --global or edit that file)", normalizedMatch, path, other))
		}
		return fail(fmt.Sprintf("no credential for %q in %s", normalizedMatch, path))
	}
	doc.Credential = kept
	if err := writeCredentialsFile(path, doc); err != nil {
		return fail(err.Error())
	}
	fmt.Fprintf(stdout, "Removed pack credential for %q from %s\n", normalizedMatch, path) //nolint:errcheck
	return 0
}

// findCredentialInOtherLayer reports the origin of a rule matching normalizedMatch
// in any readable layer other than excludePath, for a helpful cross-scope error.
func findCredentialInOtherLayer(normalizedMatch, excludePath string) string {
	cityRoot := ""
	if root, err := resolveImportRoot(); err == nil {
		cityRoot = root
	}
	rules, err := gitcred.Load(cityRoot)
	if err != nil {
		return ""
	}
	excludeAbs, _ := filepath.Abs(excludePath)
	for _, lr := range rules.All() {
		if em, _ := normalizeCredentialMatch(lr.Match); em != normalizedMatch {
			continue
		}
		// Synthetic origins ($GH_TOKEN, $GITHUB_TOKEN, $GC_GIT_CREDENTIAL_COMMAND)
		// are ambient env/command layers, not files a user can edit, so never point
		// the "use --global or edit that file" guidance at one.
		if strings.HasPrefix(lr.Origin, "$") {
			continue
		}
		if originAbs, _ := filepath.Abs(lr.Origin); originAbs != excludeAbs {
			return lr.Origin
		}
	}
	return ""
}

// selectPointer validates that exactly one pointer field is set and returns its
// type name (helper|token_file|token_env|ssh_key_file).
func selectPointer(rule gitcred.Rule) (pointerType string, err error) {
	set := 0
	if rule.Helper != "" {
		set++
		pointerType = "helper"
	}
	if rule.TokenFile != "" {
		set++
		pointerType = "token_file"
	}
	if rule.TokenEnv != "" {
		set++
		pointerType = "token_env"
	}
	if rule.SSHKeyFile != "" {
		set++
		pointerType = "ssh_key_file"
	}
	if set != 1 {
		return "", fmt.Errorf("exactly one of --helper, --token-file, --token-env, or --ssh-key-file is required")
	}
	return pointerType, nil
}

func pointerValue(rule gitcred.Rule) string {
	switch {
	case rule.Helper != "":
		return rule.Helper
	case rule.TokenFile != "":
		return rule.TokenFile
	case rule.TokenEnv != "":
		return rule.TokenEnv
	case rule.SSHKeyFile != "":
		return rule.SSHKeyFile
	default:
		return ""
	}
}

// normalizeCredentialMatch trims the trailing "/*" or "/" and rejects a match
// that is a URL, carries userinfo, or contains whitespace or a comment marker.
func normalizeCredentialMatch(match string) (string, error) {
	match = strings.TrimSpace(match)
	match = strings.TrimSuffix(match, "/*")
	match = strings.TrimSuffix(match, "/")
	if match == "" {
		return "", fmt.Errorf("match must be a bare host or host/path-prefix like \"github.com/gascity\", not empty")
	}
	if strings.Contains(match, "://") {
		return "", fmt.Errorf("match must be a bare host or host/path-prefix like \"github.com/gascity\", not a URL")
	}
	if strings.Contains(match, "@") {
		return "", fmt.Errorf("match must not contain credentials or user info")
	}
	if strings.ContainsAny(match, " \t#") {
		return "", fmt.Errorf("match must not contain whitespace or a comment marker")
	}
	return match, nil
}

func credentialPathExists(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return false
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			path = filepath.Join(home, strings.TrimPrefix(path, "~"))
		}
	}
	_, err := os.Stat(path)
	return err == nil
}
