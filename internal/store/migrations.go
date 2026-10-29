package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/database"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var migrations embed.FS

const schemaVersion = 6
const versionTable = "makez_goose_version"

// Migrate is the explicit migration entry point for isolated test databases.
// Runtime processes must use CheckSchema instead.
func (s *Store) Migrate(ctx context.Context) error { return s.MigrateLegacy(ctx, false) }

// MigrateLegacy uses a dedicated connection pool and a session lock. Adoption
// preserves the legacy ledger and registers only structurally verified versions.
func (s *Store) MigrateLegacy(ctx context.Context, adopt bool) error {
	config := s.pool.Config().ConnConfig.Copy()
	config.RuntimeParams["statement_timeout"] = "120000"
	config.RuntimeParams["lock_timeout"] = "30000"
	db := stdlib.OpenDB(*config)
	db.SetMaxOpenConns(2)
	defer db.Close() //nolint:errcheck // All migration operations are checked before closing the pool.
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	locker, err := lock.NewPostgresSessionLocker(lock.WithLockID(746302945))
	if err != nil {
		_ = conn.Close()
		return err
	}
	if err = locker.SessionLock(ctx, conn); err != nil {
		_ = conn.Close()
		return err
	}
	err = adoptLegacy(ctx, conn, adopt)
	unlockCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	unlockErr := locker.SessionUnlock(unlockCtx, conn)
	cancel()
	closeErr := conn.Close()
	if err = errors.Join(err, unlockErr, closeErr); err != nil {
		return err
	}
	files, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return err
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, files, goose.WithTableName(versionTable), goose.WithSessionLocker(locker))
	if err != nil {
		return err
	}
	_, err = provider.Up(ctx)
	return err
}

// CheckSchema performs no DDL and can run with an application-only database role.
func (s *Store) CheckSchema(ctx context.Context) error {
	var version, count int
	if err := s.pool.QueryRow(ctx, "SELECT COALESCE(max(version_id),0),count(DISTINCT version_id) FILTER(WHERE version_id>0) FROM "+versionTable+" WHERE is_applied").Scan(&version, &count); err != nil {
		return fmt.Errorf("database requires explicit makez migrate: %w", err)
	}
	if version != schemaVersion || count != schemaVersion {
		return fmt.Errorf("unsupported database version %d; require %d", version, schemaVersion)
	}
	return nil
}

func adoptLegacy(ctx context.Context, conn *sql.Conn, adopt bool) error {
	var legacy, current bool
	if err := conn.QueryRowContext(ctx, `SELECT to_regclass('makez_schema_migrations') IS NOT NULL,to_regclass('makez_goose_version') IS NOT NULL`).Scan(&legacy, &current); err != nil {
		return err
	}
	if current {
		return nil
	}
	if !legacy {
		var exists bool
		if err := conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT FROM pg_tables WHERE schemaname=current_schema() AND tablename LIKE 'makez_%')`).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return errors.New("unversioned Makez schema; manual inspection required")
		}
		return nil
	}
	if !adopt {
		return errors.New("legacy migration ledger requires makez migrate --adopt-legacy")
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // Commit closes the transaction.
	var count, version int
	if err = tx.QueryRowContext(ctx, `SELECT count(*),COALESCE(max(version),0) FROM makez_schema_migrations`).Scan(&count, &version); err != nil {
		return err
	}
	if version < 1 || version > 3 || count != version {
		return errors.New("invalid legacy migration ledger")
	}
	var minimum int
	if err = tx.QueryRowContext(ctx, `SELECT min(version) FROM makez_schema_migrations`).Scan(&minimum); err != nil {
		return err
	}
	if minimum != 1 {
		return errors.New("noncontiguous legacy migration ledger")
	}
	if err = verifyLegacy(ctx, tx, version); err != nil {
		return err
	}
	ledger, err := database.NewStore(database.DialectPostgres, versionTable)
	if err != nil {
		return err
	}
	if err = ledger.CreateVersionTable(ctx, tx); err != nil {
		return err
	}
	for v := range version + 1 {
		if err = ledger.Insert(ctx, tx, database.InsertRequest{Version: int64(v)}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Compare actual columns, defaults, constraints and indexes with an isolated
// reconstruction. No application row is rewritten during adoption.
func verifyLegacy(ctx context.Context, tx *sql.Tx, version int) error {
	var original, searchPath string
	if err := tx.QueryRowContext(ctx, `SELECT current_schema(),current_setting('search_path')`).Scan(&original, &searchPath); err != nil {
		return err
	}
	actual, err := schemaSignature(ctx, tx, original)
	if err != nil {
		return err
	}
	scratch := "makez_verify_" + newID()
	if _, err = tx.ExecContext(ctx, `CREATE SCHEMA `+scratch); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `SELECT set_config('search_path',$1,true)`, scratch); err != nil {
		return err
	}
	files := []string{"00001_legacy.sql", "00002_agent_plan.sql", "00003_operations.sql"}
	for _, name := range files[:version] {
		data, readErr := migrations.ReadFile("migrations/" + name)
		if readErr != nil {
			return readErr
		}
		if _, err = tx.ExecContext(ctx, string(data)); err != nil {
			return err
		}
	}
	expected, err := schemaSignature(ctx, tx, scratch)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(actual, expected) {
		return errors.New("legacy schema drift detected; adoption refused without changing data")
	}
	if _, err = tx.ExecContext(ctx, `SELECT set_config('search_path',$1,true)`, searchPath); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DROP SCHEMA `+scratch+` CASCADE`)
	return err
}

func schemaSignature(ctx context.Context, tx *sql.Tx, schema string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT 'column:'||c.relname||':'||a.attname||':'||format_type(a.atttypid,a.atttypmod)||':'||a.attnotnull||':'||COALESCE(pg_get_expr(d.adbin,d.adrelid),'')
FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace JOIN pg_attribute a ON a.attrelid=c.oid
LEFT JOIN pg_attrdef d ON d.adrelid=c.oid AND d.adnum=a.attnum
WHERE n.nspname=$1 AND c.relkind='r' AND c.relname LIKE 'makez_%' AND c.relname<>'makez_schema_migrations' AND a.attnum>0 AND NOT a.attisdropped
UNION ALL SELECT 'constraint:'||c.relname||':'||pg_get_constraintdef(k.oid) FROM pg_constraint k JOIN pg_class c ON c.oid=k.conrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relname LIKE 'makez_%' AND c.relname<>'makez_schema_migrations'
UNION ALL SELECT 'index:'||tablename||':'||indexdef FROM pg_indexes WHERE schemaname=$1 AND tablename LIKE 'makez_%' AND tablename<>'makez_schema_migrations'
ORDER BY 1`, schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // Rows.Err reports query failures; closing a read-only catalog query has no side effects.
	var values []string
	for rows.Next() {
		var value string
		if err = rows.Scan(&value); err != nil {
			return nil, err
		}
		values = append(values, strings.ReplaceAll(value, schema+".", ""))
	}
	return values, rows.Err()
}
