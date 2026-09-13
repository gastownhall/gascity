package dashboardspa

import "embed"

// distFS holds the compiled Vite bundle for the dashboard SPA. The bundle is
// produced by `npm run build` in web/ and copied to dist/ (committed so a
// Node-less `go build` still yields a working dashboard). The `all:` prefix
// captures dotfiles and nested asset directories under dist/.
//
//go:embed all:dist
var distFS embed.FS
