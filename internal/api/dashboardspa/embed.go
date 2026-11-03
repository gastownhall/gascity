package dashboardspa

import (
	_ "embed"
	"io/fs"
)

// Raw canonical dist stays in source control and is test-only embedded. The
// binary stores one compressed manifest, decoded behind the same read-only FS.
//
//go:embed embedded/dist.bundle.json.gz
var packedDist []byte

var distFS fs.FS = &compressedDistFS{packed: packedDist}
