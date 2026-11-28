package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/bdflags"
	"github.com/gastownhall/gascity/internal/beadmeta"
)

// bdBlockerPatch extracts the final metadata patch before either bd's work
// store or gc's class-store path sees the command. A touched blocker cannot be
// admitted from @file or from ambiguous mixtures of metadata flag spellings.
func bdBlockerPatch(bdArgs []string) (map[string]string, bool, error) {
	verb, args := bdflags.SplitGlobalFlags(bdArgs)
	if verb != "create" && verb != "update" {
		return nil, false, nil
	}
	patch := map[string]string{}
	valueFlags := bdflags.ValueFlags(verb)
	metadataForms := map[string]bool{}
	unsetTyped := false
	unsetText := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		name, value, inline := strings.Cut(arg, "=")
		if name == "--unset-metadata" {
			if !inline {
				if i+1 >= len(args) {
					return nil, false, fmt.Errorf("--unset-metadata has no value")
				}
				i++
				value = args[i]
			}
			unsetTyped = unsetTyped || value == beadmeta.BlockerV2MetadataKey
			unsetText = unsetText || value == beadmeta.BlockedOnMetadataKey
			continue
		}
		if name != "--set-metadata" && name != "--metadata" {
			if !inline && valueFlags[name] && i+1 < len(args) {
				i++
			}
			continue
		}
		metadataForms[name] = true
		if !inline {
			if i+1 >= len(args) {
				return nil, false, fmt.Errorf("%s has no value", name)
			}
			i++
			value = args[i]
		}
		if name == "--set-metadata" {
			key, v, ok := strings.Cut(value, "=")
			if !ok || key == "" {
				return nil, false, fmt.Errorf("malformed --set-metadata")
			}
			patch[key] = v
			continue
		}
		if strings.HasPrefix(value, "@") {
			return nil, true, fmt.Errorf("--metadata @file cannot be checked atomically before write; pass inline JSON")
		}
		var values map[string]string
		if err := json.Unmarshal([]byte(value), &values); err != nil {
			return nil, false, fmt.Errorf("malformed --metadata: %w", err)
		}
		for key, v := range values {
			patch[key] = v
		}
	}
	_, hasText := patch[beadmeta.BlockedOnMetadataKey]
	_, hasTyped := patch[beadmeta.BlockerV2MetadataKey]
	touched := hasText || hasTyped || unsetText || unsetTyped
	if unsetText || unsetTyped {
		return nil, true, fmt.Errorf("blocker removal requires an evidence-backed reconciliation writer; gc bd --unset-metadata cannot validate release")
	}
	if touched && len(metadataForms) > 1 {
		return nil, true, fmt.Errorf("mixed metadata flag forms are ambiguous for a blocker write")
	}
	return patch, touched, nil
}

func bdBlockerWriteRefusal(cityPath, validator string, bdArgs []string) (string, bool) {
	if validator == "" {
		return "", false
	}
	patch, touched, err := bdBlockerPatch(bdArgs)
	if err != nil {
		return blockerRefusal(cityPath, err), true
	}
	if !touched {
		return "", false
	}
	path := validator
	if !filepath.IsAbs(path) {
		path = filepath.Join(cityPath, path)
	}
	input, err := json.Marshal(patch)
	if err != nil {
		return blockerRefusal(cityPath, err), true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "python3", path, "--validate-write-patch")
	cmd.Stdin = bytes.NewReader(input)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return blockerRefusal(cityPath, fmt.Errorf("%s (%w)", strings.TrimSpace(stderr.String()), err)), true
	}
	return "", false
}

func blockerRefusal(cityPath string, cause error) string {
	message := fmt.Sprintf("gc bd: refusing blocker metadata before write: %v\n", cause)
	path := filepath.Join(cityPath, ".gc", "runtime", "blocker-write-guard", "refusals.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return message + fmt.Sprintf("gc bd: refusal metric write failed: %v\n", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return message + fmt.Sprintf("gc bd: refusal metric write failed: %v\n", err)
	}
	receipt, _ := json.Marshal(map[string]any{
		"schema": "gc.blocker-write-refusal/v1", "at": time.Now().UTC().Format(time.RFC3339Nano),
		"metric": "gc_blocker_write_refused_total", "count": 1, "reason": cause.Error(),
	})
	_, writeErr := file.Write(append(receipt, '\n'))
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil {
		return message + fmt.Sprintf("gc bd: refusal metric write failed: %v\n", writeErr)
	}
	if closeErr != nil {
		return message + fmt.Sprintf("gc bd: refusal metric close failed: %v\n", closeErr)
	}
	return message
}
