//go:build tools

// Package tools tracks build-time and runtime dependencies that are not yet
// directly imported by production code. Keeping them here ensures they remain
// pinned in go.mod / go.sum and are always available when Phase 1 coding begins.
package tools

import (
	_ "github.com/fsnotify/fsnotify"
	_ "github.com/mattn/go-sqlite3"
	_ "github.com/minio/minio-go/v7"
	_ "github.com/robfig/cron/v3"
	_ "github.com/stretchr/testify/assert"
	_ "go.uber.org/zap"
	_ "google.golang.org/grpc"
)
