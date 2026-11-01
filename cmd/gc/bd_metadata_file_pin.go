package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/bdflags"
)

// bdPinMetadataFiles reads each `--metadata @file` value of a create, new or
// update exactly once, before any gc bd guard reads it, writes the bytes to a
// private copy, and returns argv with that value pointed at the copy, plus a
// cleanup that removes the copies. More than one reader opens such a file
// before bd does: the rig-qualified metadata check on every city, then the
// class judge on a split city, and bd itself last. Without one shared read, a
// pipe operand (`<(...)`, /dev/stdin) is drained by the first reader and every
// later one reads it empty, and a file rewritten between two reads reaches bd
// with only one of them having checked it.
//
// The walk is pflag's own over the pinned bd builds' flags
// (bdflags.FlagTokenWidth), so only a token pflag reads as a --metadata value
// is rewritten; an argv with a flag no pinned build declares is returned
// unchanged, and so is a file gc cannot read (the rig check refuses it). bd
// reads a file only for a value that starts with "@" and reads value[1:]
// verbatim, relative to its working directory, the scope root; the same path
// is read here.
func bdPinMetadataFiles(scopeRoot string, bdArgs []string) ([]string, func(), error) {
	noop := func() {}
	at := bdMetadataFileValueTokens(bdArgs)
	if len(at) == 0 {
		return bdArgs, noop, nil
	}
	out := slices.Clone(bdArgs)
	var copies []string
	cleanup := func() {
		for _, c := range copies {
			_ = os.Remove(c)
		}
	}
	for _, i := range at {
		prefix, value := "", out[i]
		if flag, v, inline := strings.Cut(value, "="); inline && flag == "--metadata" {
			prefix, value = flag+"=", v
		}
		path := strings.TrimPrefix(value, "@")
		if !filepath.IsAbs(path) {
			path = filepath.Join(scopeRoot, path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		copyPath, err := bdWriteJudgedCopy(data, "gc-bd-metadata-*.json")
		if err != nil {
			cleanup()
			return nil, nil, fmt.Errorf("refusing --metadata %s: gc reads a --metadata file once and hands every check and bd a private copy of it, and could not write that copy (check $TMPDIR): %w", value, err)
		}
		copies = append(copies, copyPath)
		out[i] = prefix + "@" + copyPath
	}
	if len(copies) == 0 {
		return bdArgs, noop, nil
	}
	return out, cleanup, nil
}

// bdMetadataFileValueTokens returns the indices of the argv tokens that carry a
// `--metadata @file` value of a create, new or update, as pflag reads the argv
// under the pinned bd builds' flags: the token after a separate --metadata, or
// a --metadata=@file token itself. It returns nil for any other verb, and for
// an argv with a flag no pinned build declares, whose token widths gc cannot
// know.
func bdMetadataFileValueTokens(bdArgs []string) []int {
	verb, _ := bdflags.SplitGlobalFlags(bdArgs)
	if verb == "new" {
		verb = "create"
	}
	if verb != "create" && verb != "update" {
		return nil
	}
	specs := append(bdflags.CommandFlagSpecs(verb), bdflags.GlobalFlagSpecs()...)
	var at []int
	for i := 0; i < len(bdArgs); {
		tok := bdArgs[i]
		switch {
		case tok == "--":
			return at
		case len(tok) < 2 || tok[0] != '-':
			i++
		case tok == "--metadata":
			if i+1 < len(bdArgs) && strings.HasPrefix(bdArgs[i+1], "@") {
				at = append(at, i+1)
			}
			i += 2
		case strings.HasPrefix(tok, "--metadata="):
			if strings.HasPrefix(strings.TrimPrefix(tok, "--metadata="), "@") {
				at = append(at, i)
			}
			i++
		default:
			w, err := bdflags.FlagTokenWidth(specs, bdArgs[i:])
			if err != nil {
				return nil
			}
			i += w
		}
	}
	return at
}
