package config

import (
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/fsys"
)

// DiscoveredSkillCatalog is a convention-discovered shared skills/
// catalog from a pack. One entry represents one pack-level skills root.
type DiscoveredSkillCatalog struct {
	SourceDir   string
	PackDir     string
	PackName    string
	BindingName string
}

// DiscoverPackSkills reports the top-level shared skills/ directory for
// a pack when it exists.
func DiscoverPackSkills(fs fsys.FS, packDir, packName string) ([]DiscoveredSkillCatalog, error) {
	skillsDir := filepath.Join(packDir, "skills")
	info, err := fs.Stat(skillsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if !info.IsDir() {
		return nil, nil
	}
	return []DiscoveredSkillCatalog{{
		SourceDir: skillsDir,
		PackDir:   packDir,
		PackName:  packName,
	}}, nil
}
