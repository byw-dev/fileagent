package db

import (
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	// The postgres driver for golang-migrate.
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	// The file source driver for golang-migrate.
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"go.uber.org/zap"
)

// Migrate runs all pending up-migrations from the given source path against
// the database identified by dsn. It is idempotent: if the schema is already
// up-to-date, Migrate returns nil without error.
//
// migrationsPath must be a directory path (e.g. "migrations" or
// "/app/migrations") containing *.up.sql / *.down.sql files that follow the
// golang-migrate naming convention.
func Migrate(dsn, migrationsPath string, logger *zap.Logger) error {
	source := "file://" + migrationsPath
	m, err := migrate.New(source, dsn)
	if err != nil {
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
