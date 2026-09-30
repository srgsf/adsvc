package catalog

import (
	"cmp"
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrations embed.FS

type migration struct {
	version int
	name    string
	sql     string
}

// loadMigrations reads NNNN_name.sql files from fsys, ordered by version. Versions must
// start at 1 and have no gaps or duplicates.
func loadMigrations(fsys fs.FS) ([]migration, error) {
	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return nil, err
	}
	var ms []migration
	for _, n := range names {
		num, rest, ok := strings.Cut(strings.TrimSuffix(n, ".sql"), "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil || v <= 0 || rest == "" {
			return nil, fmt.Errorf("migration %s: want NNNN_name.sql", n)
		}
		b, err := fs.ReadFile(fsys, n)
		if err != nil {
			return nil, err
		}
		ms = append(ms, migration{v, rest, string(b)})
	}
	// Sort by the parsed version: file names sort "10_" before "2_".
	slices.SortFunc(ms, func(a, b migration) int { return cmp.Compare(a.version, b.version) })
	for i, m := range ms {
		if m.version != i+1 {
			return nil, fmt.Errorf("migration %04d_%s: want version %d (gap or duplicate)", m.version, m.name, i+1)
		}
	}
	return ms, nil
}

// ErrNewerSchema is returned when the catalogue was written by a newer adsvc.
var ErrNewerSchema = errors.New("catalogue schema is newer than this adsvc")

// migrate applies the migrations of fsys that db lacks, one transaction each.
func migrate(ctx context.Context, db *sql.DB, fsys fs.FS) error {
	ms, err := loadMigrations(fsys)
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		name       TEXT NOT NULL,
		applied_at INTEGER NOT NULL DEFAULT (unixepoch())
	) STRICT`); err != nil {
		return fmt.Errorf("creating schema_migrations: %w", err)
	}
	var cur int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&cur); err != nil {
		return fmt.Errorf("reading schema version: %w", err)
	}
	if cur > len(ms) {
		return fmt.Errorf("%w: version %d, this build knows %d", ErrNewerSchema, cur, len(ms))
	}
	for _, m := range ms[cur:] {
		if err := apply(ctx, db, m); err != nil {
			return fmt.Errorf("migration %04d_%s: %w", m.version, m.name, err)
		}
	}
	return nil
}

func apply(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, name) VALUES (?, ?)`, m.version, m.name); err != nil {
		return err
	}
	return tx.Commit()
}

// SchemaVersion is the schema version this build brings a catalogue to.
func SchemaVersion() int {
	ms, err := loadMigrations(migrationsFS())
	if err != nil {
		panic(err) // the embedded migrations are checked by the tests
	}
	return len(ms)
}

func migrationsFS() fs.FS {
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		panic(err) // the embed pattern guarantees the directory
	}
	return sub
}
