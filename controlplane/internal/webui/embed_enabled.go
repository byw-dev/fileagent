//go:build webui

// Package webui provides access to the compiled Web UI (webui/dist) that is
// embedded into the Control Plane binary. This file is compiled only under the
// `webui` build tag (produced by `make bundle`); the default build uses
// embed_disabled.go and ships a pure-API binary.
package webui

import (
	"embed"
	"fmt"
	"io/fs"
)

// distFS holds the compiled Web UI assets. The `all:` prefix ensures files
// whose names start with `_` or `.` are included as well. The dist/ directory
// is populated by the build (Makefile `build-webui`) and is gitignored.
//
//go:embed all:dist
var distFS embed.FS

// FS returns the embedded Web UI assets as an fs.FS rooted at dist/, ready to
// be served by the HTTP router. It is non-nil in tag-`webui` builds.
//
// It panics if the embedded dist/ subtree cannot be resolved. That can only
// happen when the binary was built with -tags webui but without a valid dist/
// (a broken bundle); failing fast at startup surfaces the packaging error
// loudly rather than silently shipping a binary that serves no Web UI.
func FS() fs.FS {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic(fmt.Sprintf("webui: cannot resolve embedded dist/: %v", err))
	}
	return sub
}
