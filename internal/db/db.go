package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const (
	migrationAttempts  = 50
	migrationRetryWait = 100 * time.Millisecond
)

var (
	entropyMu sync.Mutex
	entropy   = ulid.Monotonic(rand.Reader, 0)
)

// DB wraps a SQLite database connection.
type DB struct {
	sql *sql.DB
}

// Open opens (or creates) the SQLite database at path and runs migrations.
func Open(path string) (*DB, error) {
	sqlDB, err := sql.Open("sqlite", path+"?_pragma=journal_mode(wal)&_pragma=foreign_keys(on)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	sqlDB.SetMaxOpenConns(1)
	ctx := context.Background()
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("migrate db: %w", err)
	}
	err = migrate(ctx, conn)
	conn.Close()
	if err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("migrate db: %w", err)
	}
	return &DB{sql: sqlDB}, nil
}

// migrate applies the schema and every migration statement in one
// transaction. Run as autocommit statements, a fresh file paid one WAL commit
// and fsync per statement, 72 per Open, which bound every test that opens a
// database to the runner's disk latency (issue #215). The transaction is
// deferred, so an Open of a database that is already migrated writes nothing
// and never takes the write lock. A migration that does write upgrades its
// read transaction, and SQLite fails that upgrade with SQLITE_BUSY at once
// rather than under busy_timeout, so a busy attempt is rolled back and retried
// whole, within migrationAttempts.
func migrate(ctx context.Context, conn *sql.Conn) error {
	for attempt := 1; ; attempt++ {
		err := migrateOnce(ctx, conn)
		if err == nil || !isBusyErr(err) || attempt == migrationAttempts {
			return err
		}
		time.Sleep(migrationRetryWait)
	}
}

// migrateOnce runs one migration attempt and rolls it back whole on failure.
// A duplicate column error is a prepare-time error, so SQLite keeps the
// transaction open past it.
func migrateOnce(ctx context.Context, conn *sql.Conn) (err error) {
	if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			if _, rollbackErr := conn.ExecContext(ctx, "ROLLBACK"); rollbackErr != nil {
				err = errors.Join(err, fmt.Errorf("rollback: %w", rollbackErr))
			}
		}
	}()
	if _, err := conn.ExecContext(ctx, schemaSQL); err != nil {
		return err
	}
	for _, stmt := range migrationStatements {
		if _, err := conn.ExecContext(ctx, stmt); err != nil && !isDuplicateColumnErr(err) {
			return err
		}
	}
	_, err = conn.ExecContext(ctx, "COMMIT")
	return err
}

// isBusyErr reports whether err carries SQLite's SQLITE_BUSY result code,
// including its extended forms such as SQLITE_BUSY_SNAPSHOT.
func isBusyErr(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code()&0xff == sqlite3.SQLITE_BUSY
}

// OpenReadOnly opens an existing database without creating or migrating it.
// It is used by pre-mutation authorization, where even schema repair would be
// an unacceptable side effect before the caller is classified.
func OpenReadOnly(path string) (*DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	connectionString := "file:" + path + "?mode=ro&_pragma=busy_timeout(5000)"
	if runtime.GOOS == "windows" {
		// A dead process can leave a mapped WAL index after releasing its locks.
		// Let SQLite reconstruct the index without truncating that mapping.
		connectionString += "&readonly_shm=1"
	}
	sqlDB, err := sql.Open("sqlite", connectionString)
	if err != nil {
		return nil, fmt.Errorf("open db read-only: %w", err)
	}
	sqlDB.SetMaxOpenConns(1)
	if err := sqlDB.Ping(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("open db read-only: %w", err)
	}
	return &DB{sql: sqlDB}, nil
}

// isDuplicateColumnErr reports whether err is SQLite's "duplicate column name"
// error, which ALTER TABLE ADD COLUMN emits when the column already exists.
// Treating this as a no-op keeps migrations idempotent without a version table.
func isDuplicateColumnErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "duplicate column name")
}

// Close closes the database connection.
func (d *DB) Close() error {
	return d.sql.Close()
}

// newID generates a new ULID with monotonic ordering.
func newID() string {
	entropyMu.Lock()
	defer entropyMu.Unlock()
	return ulid.MustNew(ulid.Timestamp(time.Now()), entropy).String()
}

// now returns the current unix timestamp in seconds.
func now() int64 {
	return time.Now().Unix()
}
