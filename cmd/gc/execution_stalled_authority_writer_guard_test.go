package main

import (
	"crypto/sha256"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// executionStalledAuthorityMetadataKeys is the durable metadata half of
// executionStalledLifecycleAuthority. Closed/open status is deliberately not a
// metadata key and is guarded by the lifecycle mutation APIs themselves.
//
// Keep this list in lockstep with executionStalledLifecycleAuthority. The test
// below inventories production literal writers, so adding a decision field to
// the authority without reviewing its writers cannot silently reopen a race.
var executionStalledAuthorityMetadataKeys = map[string]struct{}{
	"template": {}, "agent_name": {}, "alias": {}, "common_name": {},
	"configured_named_identity": {}, "configured_named_session": {}, "configured_named_mode": {},
	"pool_managed": {}, "pool_slot": {}, "session_origin": {},
	"canonical_instance_name": {}, "canonical_pool_slot": {},
	"provider": {}, "provider_kind": {}, "builtin_ancestor": {}, "transport": {},
	"command": {}, "work_dir": {}, "session_key": {}, "resume_flag": {},
	"resume_style": {}, "resume_command": {}, "session_id_flag": {}, "wake_mode": {},
	"template_overrides": {}, "started_config_hash": {}, "started_provision_hash": {},
	"started_launch_hash": {}, "started_live_hash": {}, "pin_awake": {}, "continuity_eligible": {},
	"session_name": {}, "state": {}, "state_reason": {}, "sleep_reason": {}, "sleep_intent": {},
	"held_until": {}, "wait_hold": {}, "quarantined_until": {}, "wake_request": {},
	"wake_requested_at": {}, "wake_request_token": {}, "wake_attempts": {}, "churn_count": {},
	"restart_requested": {}, "continuation_reset_pending": {}, "pending_create_claim": {},
	"pending_create_started_at": {}, "last_woke_at": {}, "awake_started_at": {},
	"creation_complete_at": {}, "generation": {}, "continuation_epoch": {}, "instance_token": {},
	"provider_session_key_receipt_token": {}, "provider_session_key_receipt_authority": {},
	"provider_session_key_receipt_consume_authority": {}, "provider_session_key_receipt_issued_at": {},
}

// executionStalledAuthorityWriterCensusSHA256 commits to the exact allowlist of
// reviewed production literal-writer sites. Each digest input coordinate is
// repo-relative file:function:key:kind plus its count. "literal" is a
// map-composite key, "assign" an indexed map assignment, "delete" an indexed
// clear, and "call" a direct SetMetadata / SetMarker key. A second write at an
// already-reviewed site changes the digest too. On mismatch, call
// formatExecutionStalledAuthorityWriterCensus while reviewing the full surface.
//
// A site belongs here only after its lifecycle ordering is reviewed: either it
// creates a new row, constructs a patch consumed inside a lifecycle guard, or
// performs its authoritative live read + latch check under the city/session
// lifecycle lock. Diagnostic/read-only fields must not be added to the authority
// merely to make this census pass.
const executionStalledAuthorityWriterCensusSHA256 = "3c2bf12596669dee8caa657850c0ac6805df12fa865b538c264909fcc3dc47f2"

type executionStalledGuardRequirement struct {
	file     string
	function string
	calls    []string
}

// These are the authority-bearing asynchronous/operator writers for which the
// durable latch ordering is not obvious from a patch constructor alone. The
// structural assertions make their guard choice explicit in addition to the
// whole-writer-surface digest above.
var executionStalledGuardRequirements = []executionStalledGuardRequirement{
	{"cmd/gc/cmd_prime.go", "persistPrimeHookProviderSessionKey", []string{"GetLive", "acquirePrimeHookActivityLease", "PublishProviderSessionKeyReceipt", "Release"}},
	{"cmd/gc/cmd_session_pin.go", "cmdSessionSetPin", []string{"WithCitySessionLifecycleLock", "WithSessionMutationLock", "GetLive", "HasExecutionClaimNudgeStalled", "SetMarker"}},
	{"cmd/gc/soft_reload.go", "acceptConfigDriftAcrossSessions", []string{"tryWithCurrentSessionMutation", "ApplyPatch"}},
	{"cmd/gc/session_beads.go", "tryWithCurrentSessionMutation", []string{"GetLive", "HasExecutionClaimNudgeStalled", "TryWithCitySessionLifecycleLock"}},
	{"cmd/gc/session_beads.go", "tryWithCurrentSessionBeadMutation", []string{"tryWithCurrentSessionMutation", "Get"}},
	{"internal/session/chat.go", "withExecutionStalledGuardedMutation", []string{"withLifecycleMutationLock", "loadSessionBead", "executionStalledRecoveryPendingError"}},
	{"internal/session/chat.go", "Respond", []string{"withExecutionStalledGuardedMutation", "Respond"}},
	{"internal/session/manager.go", "UpdatePresentation", []string{"withExecutionStalledGuardedMutation", "Update"}},
	{"internal/session/manager.go", "UpdateTemplateOverrides", []string{"withExecutionStalledGuardedMutation", "SetMetadataBatch"}},
	{"internal/session/manager.go", "PersistSessionKey", []string{"withExecutionStalledGuardedMutation", "SetMetadata"}},
}

func TestExecutionStalledAuthorityWriterCensus(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	got, err := scanExecutionStalledAuthorityWriters(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	digest := executionStalledAuthorityWriterDigest(got)
	if digest != executionStalledAuthorityWriterCensusSHA256 {
		t.Errorf("authority writer census (%d coordinates) digest=%s, reviewed=%s; regenerate the reviewed digest from:\n%s", len(got), digest, executionStalledAuthorityWriterCensusSHA256, formatExecutionStalledAuthorityWriterCensus(got))
	}
}

func TestExecutionStalledAuthorityWritersKeepLifecycleGuards(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, requirement := range executionStalledGuardRequirements {
		requirement := requirement
		t.Run(requirement.file+":"+requirement.function, func(t *testing.T) {
			path := filepath.Join(repoRoot, filepath.FromSlash(requirement.file))
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			calls := map[string]int{}
			found := false
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Name.Name != requirement.function || fn.Body == nil {
					continue
				}
				found = true
				ast.Inspect(fn.Body, func(node ast.Node) bool {
					call, ok := node.(*ast.CallExpr)
					if !ok {
						return true
					}
					switch callee := call.Fun.(type) {
					case *ast.Ident:
						calls[callee.Name]++
					case *ast.SelectorExpr:
						calls[callee.Sel.Name]++
					}
					return true
				})
			}
			if !found {
				t.Fatalf("guarded authority writer function not found")
			}
			for _, requiredCall := range requirement.calls {
				if calls[requiredCall] == 0 {
					t.Errorf("missing required guard/write call %s (calls=%v)", requiredCall, calls)
				}
			}
		})
	}
}

func executionStalledAuthorityWriterDigest(census map[string]int) string {
	keys := make([]string, 0, len(census))
	for key := range census {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	hash := sha256.New()
	for _, key := range keys {
		_, _ = fmt.Fprintf(hash, "%s\x00%d\n", key, census[key])
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}

func scanExecutionStalledAuthorityWriters(repoRoot string) (map[string]int, error) {
	type parsedFile struct {
		path string
		rel  string
		f    *ast.File
	}
	fset := token.NewFileSet()
	var files []parsedFile
	constantValues := map[string]string{
		"CanonicalInstanceNameMetadata": "canonical_instance_name",
		"CanonicalPoolSlotMetadata":     "canonical_pool_slot",
	}
	for _, scanDir := range []string{"cmd/gc", "internal/session"} {
		root := filepath.Join(repoRoot, filepath.FromSlash(scanDir))
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, parseErr := parser.ParseFile(fset, path, nil, 0)
			if parseErr != nil {
				return parseErr
			}
			rel, relErr := filepath.Rel(repoRoot, path)
			if relErr != nil {
				return relErr
			}
			files = append(files, parsedFile{path: path, rel: filepath.ToSlash(rel), f: file})
			for _, decl := range file.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || gen.Tok != token.CONST {
					continue
				}
				for _, spec := range gen.Specs {
					values, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, name := range values.Names {
						if i >= len(values.Values) {
							continue
						}
						if value, ok := stringLiteral(values.Values[i], nil); ok {
							constantValues[name.Name] = value
						}
					}
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	got := map[string]int{}
	record := func(rel, function, key, kind string) {
		if _, watched := executionStalledAuthorityMetadataKeys[key]; !watched {
			return
		}
		got[strings.Join([]string{rel, function, key, kind}, ":")]++
	}
	for _, file := range files {
		for _, decl := range file.f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			function := fn.Name.Name
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				switch n := node.(type) {
				case *ast.CompositeLit:
					for _, elt := range n.Elts {
						kv, ok := elt.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						if key, ok := stringLiteral(kv.Key, constantValues); ok {
							record(file.rel, function, key, "literal")
						}
					}
				case *ast.AssignStmt:
					for _, lhs := range n.Lhs {
						index, ok := lhs.(*ast.IndexExpr)
						if !ok {
							continue
						}
						if key, ok := stringLiteral(index.Index, constantValues); ok {
							record(file.rel, function, key, "assign")
						}
					}
				case *ast.CallExpr:
					if ident, ok := n.Fun.(*ast.Ident); ok && ident.Name == "delete" && len(n.Args) >= 2 {
						if key, ok := stringLiteral(n.Args[1], constantValues); ok {
							record(file.rel, function, key, "delete")
						}
						break
					}
					sel, ok := n.Fun.(*ast.SelectorExpr)
					if !ok || (sel.Sel.Name != "SetMetadata" && sel.Sel.Name != "SetMarker") || len(n.Args) < 3 {
						break
					}
					if key, ok := stringLiteral(n.Args[1], constantValues); ok {
						record(file.rel, function, key, "call")
					}
				}
				return true
			})
		}
	}
	return got, nil
}

func stringLiteral(expr ast.Expr, constants map[string]string) (string, bool) {
	switch value := expr.(type) {
	case *ast.BasicLit:
		if value.Kind != token.STRING {
			return "", false
		}
		decoded, err := strconv.Unquote(value.Value)
		return decoded, err == nil
	case *ast.Ident:
		if constants == nil {
			return "", false
		}
		decoded, ok := constants[value.Name]
		return decoded, ok
	case *ast.SelectorExpr:
		if constants == nil {
			return "", false
		}
		decoded, ok := constants[value.Sel.Name]
		return decoded, ok
	default:
		return "", false
	}
}

func formatExecutionStalledAuthorityWriterCensus(census map[string]int) string {
	keys := make([]string, 0, len(census))
	for key := range census {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var out strings.Builder
	out.WriteString("var executionStalledAuthorityWriterCensus = map[string]int{\n")
	for _, key := range keys {
		fmt.Fprintf(&out, "\t%q: %d,\n", key, census[key]) //nolint:errcheck // strings.Builder writes cannot fail
	}
	out.WriteString("}\n")
	return out.String()
}
