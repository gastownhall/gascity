package main

import (
	"github.com/gastownhall/gascity/internal/cityinit"
	"github.com/gastownhall/gascity/internal/fsys"
)

func ensureCityScaffold(cityPath string) error {
	return ensureCityScaffoldFS(fsys.OSFS{}, cityPath)
}

func ensureCityScaffoldFS(fs fsys.FS, cityPath string) error {
	return cityinit.EnsureCityScaffoldFS(fs, cityPath)
}

func cityAlreadyInitializedFS(fs fsys.FS, cityPath string) bool {
	return cityinit.CityAlreadyInitializedFS(fs, cityPath)
}

func cityHasScaffoldFS(fs fsys.FS, cityPath string) bool {
	return cityinit.CityHasScaffoldFS(fs, cityPath)
}

func cityCanResumeInitFS(fs fsys.FS, cityPath string) bool {
	return cityinit.CityCanResumeInitFS(fs, cityPath)
}
