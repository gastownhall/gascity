package main

import (
	"github.com/gastownhall/gascity/internal/cityinit"
	"github.com/gastownhall/gascity/internal/fsys"
)

var _ cityinit.ScaffoldFS = fsys.OSScaffoldFS{}
