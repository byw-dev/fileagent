package db

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/golang-migrate/migrate/v4"
	// The postgres driver for golang-migrate.
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	// The iofs source driver reads migrations from an fs.FS (e.g. an embedded
	// filesystem) instead of the host filesystem.
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"go.uber.org/zap"
)

// Migrate runs all pending up-migrations from src against the database
// identified by dsn. It is idempotent: if the schema is already up-to-date,
// Migrate returns nil without error.
//
// src is an fs.FS whose root contains *.up.sql / *.down.sql files following the
// golang-migrate naming convention. In production this is the embedded
// migrations.FS, so no external migrations directory needs to be shipped with
// the binary (see DECISIONS.md D-023).
func Migrate(dsn string, src fs.FS, logger *zap.Logger) error {
	source, err := iofs.New(src, ".")
	if err != nil {
		return fmt.Errorf("db migrate: open migration source: %w", err)
	}

	// Preflight the source before opening the DB: iofs.New succeeds even on an
	// empty FS, so probe for the first version here. This turns a missing/empty
	// migration set into a clear error without ever dialing Postgres (keeping
	// the failure path DB-free and unit-testable). Close the source on these
	// early returns — the deferred m.Close() below only exists once m is built.
	//
	// First() reports an empty source as fs.ErrNotExist; anything else (an
	// invalid migration filename, an IO fault) is a distinct real failure, so
	// keep the two messages apart to avoid misleading "no migrations" noise.
	if _, err := source.First(); err != nil {
		_ = source.Close()
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("db migrate: no migrations found in source: %w", err)
		}
		return fmt.Errorf("db migrate: read migration source: %w", err)
	}

	m, err := migrate.NewWithSourceInstance("iofs", source, dsn)
	if err != nil {
		_ = source.Close()
		return fmt.Errorf("db migrate: create migrator: %w", err)
	}
	defer func() {
		srcErr, dbErr := m.Close()
		if srcErr != nil {
			logger.Warn("db migrate: source close error", zap.Error(srcErr))
		}
		if dbErr != nil {
			logger.Warn("db migrate: db close error", zap.Error(dbErr))
		}
	}()

	if err := m.Up(); err != nil {
		if errors.Is(err, migrate.ErrNoChange) {
			logger.Info("db migrate: schema already up to date")
			return nil
		}
		return fmt.Errorf("db migrate: apply up migrations: %w", err)
	}

	version, dirty, _ := m.Version()
	logger.Info("db migrate: migrations applied successfully",
		zap.Uint("version", version),
		zap.Bool("dirty", dirty),
	)
	return nil
}
