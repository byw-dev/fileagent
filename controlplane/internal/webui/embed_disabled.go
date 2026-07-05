//go:build !webui

// Package webui provides access to the compiled Web UI embedded into the
// Control Plane binary. This file is the default (no `webui` build tag): no
// assets are embedded, so the binary is pure-API and requires no dist/
// directory to compile — keeping `go build ./...`, CI, and unit tests green.
package webui

import "io/fs"

// FS returns nil in the default build, signalling the router to skip Web UI
// serving (pure-API mode). The tag-`webui` build (see embed_enabled.go)
// returns the embedded assets instead.
func FS() fs.FS { return nil }
