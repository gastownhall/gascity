package contract

import (
	"bytes"
	"os"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/fsys"
	"gopkg.in/yaml.v3"
)

// SharedServerConfigKey is bd's config key for shared-server mode. With it on,
// bd roots every workspace's Dolt data in one host-wide directory
// (~/.beads/shared-server) instead of the workspace's own .beads/dolt.
const SharedServerConfigKey = "dolt.shared-server"

const (
	sharedServerSection = "dolt"
	sharedServerField   = "shared-server"
)

// SharedServerPin is what a scope's config.yaml says about bd's shared-server
// mode.
type SharedServerPin int

const (
	// SharedServerUnset means the scope does not decide; bd falls through to
	// the user-level config layers.
	SharedServerUnset SharedServerPin = iota
	// SharedServerPinnedOff means the scope keeps its own Dolt root whatever
	// the user-level config says.
	SharedServerPinnedOff
	// SharedServerPinnedOn means the scope is bound to bd's host-wide shared
	// server.
	SharedServerPinnedOn
)

// ReadSharedServerPin reports the dolt.shared-server value a scope's
// config.yaml carries, in either the nested (`dolt: {shared-server: x}`) or
// the flat dotted (`dolt.shared-server: x`) spelling bd accepts. A missing
// file is SharedServerUnset. An unparseable value is reported as unset: bd's
// own GetBool reads it as false, which is also what the scope then inherits
// from nothing but the user-level layers.
func ReadSharedServerPin(fs fsys.FS, path string) (SharedServerPin, error) {
	doc, err := readConfigDoc(fs, path)
	if err != nil {
		if os.IsNotExist(err) {
			return SharedServerUnset, nil
		}
		return SharedServerUnset, err
	}
	return sharedServerPinFromRoot(mappingRoot(doc)), nil
}

func sharedServerPinFromRoot(root *yaml.Node) SharedServerPin {
	var value *yaml.Node
	if section := findValue(root, sharedServerSection); section != nil && section.Kind == yaml.MappingNode {
		value = findValue(section, sharedServerField)
	}
	if value == nil {
		value = findValue(root, SharedServerConfigKey)
	}
	if value == nil || value.Kind != yaml.ScalarNode {
		return SharedServerUnset
	}
	on, err := strconv.ParseBool(strings.TrimSpace(value.Value))
	switch {
	case err != nil:
		return SharedServerUnset
	case on:
		return SharedServerPinnedOn
	default:
		return SharedServerPinnedOff
	}
}

// EnsureSharedServerDisabled pins dolt.shared-server to false in a scope's
// config.yaml, and reports the pin it replaced.
//
// A scope's config.yaml outranks bd's user-level config files, so the pin keeps
// the scope on its own Dolt root for every bd process that resolves this scope
// — including ones gc does not spawn and therefore cannot hand an env override
// (an agent running `bd` in its shell). It must be written AFTER bd init: init
// rewrites config.yaml from its template, and under a user-level shared-server
// mode it would write `dolt.shared-server: true` into it itself.
//
// Everything else in the file is preserved. When the file has no dolt section
// and no flat key (bd's init template, or any block-mapping config that simply
// never mentioned the mode) the pin is appended as text, so comments, blank
// lines and line endings survive byte for byte. Otherwise the document is
// re-encoded: a flat `dolt.shared-server` key is removed so the file carries
// exactly one, nested, answer, and CRLF line endings are kept (yaml.v3 does
// drop blank lines on that path).
func EnsureSharedServerDisabled(fs fsys.FS, path string) (bool, SharedServerPin, error) {
	data, err := fs.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, SharedServerUnset, err
	}
	doc, err := readConfigDoc(fs, path)
	if err != nil && !os.IsNotExist(err) {
		return false, SharedServerUnset, err
	}
	if doc == nil {
		doc = newConfigDoc()
	}
	root := mappingRoot(doc)
	previous := sharedServerPinFromRoot(root)
	eol := "\n"
	if bytes.Contains(data, []byte("\r\n")) {
		eol = "\r\n"
	}
	appendable := len(root.Content) == 0 ||
		(root.Kind == yaml.MappingNode && root.Style&yaml.FlowStyle == 0 &&
			findValue(root, sharedServerSection) == nil && findValue(root, SharedServerConfigKey) == nil)
	if appendable {
		var out bytes.Buffer
		out.Write(data)
		if out.Len() > 0 && !bytes.HasSuffix(data, []byte("\n")) {
			out.WriteString(eol)
		}
		out.WriteString(sharedServerSection + ":" + eol + "  " + sharedServerField + ": false" + eol)
		return true, previous, fsys.WriteFileAtomic(fs, path, out.Bytes(), canonicalScopeFilePerm(fs, path))
	}
	changed := deleteKeys(root, SharedServerConfigKey)
	changed = setNestedBool(root, sharedServerSection, sharedServerField, false) || changed
	if !changed {
		return false, previous, nil
	}
	encoded, err := marshalConfigDoc(doc)
	if err != nil {
		return false, previous, err
	}
	if eol != "\n" {
		encoded = bytes.ReplaceAll(encoded, []byte("\n"), []byte(eol))
	}
	return true, previous, fsys.WriteFileAtomic(fs, path, encoded, canonicalScopeFilePerm(fs, path))
}
