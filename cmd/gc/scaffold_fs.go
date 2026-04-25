package main

import (
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/cityinit"
	"github.com/gastownhall/gascity/internal/fsys"
)

var _ cityinit.ScaffoldFS = osScaffoldFS{}

type osScaffoldFS struct{ fsys.OSFS }

func (osScaffoldFS) Walk(root string, fn filepath.WalkFunc) error {
	return filepath.Walk(root, fn)
}

func (osScaffoldFS) Readlink(name string) (string, error) {
	return os.Readlink(name)
}

func (osScaffoldFS) Symlink(oldname, newname string) error {
	return os.Symlink(oldname, newname)
}

func (osScaffoldFS) RemoveAll(path string) error {
	return os.RemoveAll(path)
}
