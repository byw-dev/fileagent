//go:build tools

// Package tools tracks build-time and runtime dependencies that are not yet
// directly imported by production code. Keeping them here ensures they remain
// pinned in go.mod / go.sum and are always available when Phase 1 coding begins.
//
// Note: github.com/jackc/pgx/v5 (used by sqlc-generated database code) is
// intentionally omitted here; it will be added naturally when Phase 1 runs
// sqlc codegen and the generated files import pgx directly.
package tools

import (
	_ "github.com/alicebob/miniredis/v2"
	_ "github.com/gin-gonic/gin"
	_ "github.com/golang-jwt/jwt/v5"
	_ "github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	_ "github.com/google/uuid"
	_ "github.com/lib/pq"
	_ "github.com/minio/minio-go/v7"
	_ "github.com/nats-io/nats.go"
	_ "github.com/redis/go-redis/v9"
	_ "github.com/sqlc-dev/pqtype"
	_ "github.com/stretchr/testify/assert"
	_ "go.uber.org/zap"
	_ "google.golang.org/grpc"
)

