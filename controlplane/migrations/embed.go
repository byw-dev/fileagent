// Package migrations embeds the golang-migrate SQL migration files into the
// Control Plane binary. Embedding (rather than reading migrations/ from disk at
// runtime) is what makes `make bundle` a truly self-contained single-file
// distribution: one binary + one config, with no external migrations directory
// to ship alongside it (see DECISIONS.md D-023, building on D-022).
package migrations

import "embed"

// FS holds the *.up.sql / *.down.sql migration files in this directory. The
// Control Plane loads them via golang-migrate's iofs source driver at startup
// (see db.Migrate). Files follow the NNNNNN_description.{up,down}.sql naming
// convention required by golang-migrate.
//
//go:embed *.sql
var FS embed.FS
