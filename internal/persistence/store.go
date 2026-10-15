package persistence

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

//go:embed all:migrations/*.sql
var embeddedMigrations embed.FS

type Store struct{ DB *gorm.DB }

const ExpectedSchemaVersion = 18

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	// _txlock=immediate: every transaction takes the write lock at BEGIN. FastTask's transactions are
	// read-modify-write (revision checks, job claims, token rotation); a deferred one that read before
	// another writer committed cannot upgrade, and SQLite fails it at once with "database is locked"
	// without consulting busy_timeout. BEGIN IMMEDIATE waits for the lock under busy_timeout instead.
	// Plain reads outside a transaction are unaffected and still run concurrently under WAL.
	dsn := path + "?_busy_timeout=5000&_foreign_keys=on&_journal_mode=WAL&_synchronous=NORMAL&_txlock=immediate"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(4)
	sqlDB.SetMaxIdleConns(4)
	return &Store{DB: db}, nil
}

func (s *Store) Close() error {
	db, err := s.DB.DB()
	if err != nil {
		return err
	}
	return db.Close()
}

func (s *Store) Migrate(ctx context.Context) error {
	return RunMigrations(ctx, s.DB)
}

func RunMigrations(ctx context.Context, db *gorm.DB) error {
	currentVersion := 0
	var count int64
	err := db.WithContext(ctx).Raw("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='schema_migrations'").Scan(&count).Error
	if err != nil {
		return err
	}
	if count > 0 {
		if err := db.WithContext(ctx).Raw("SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&currentVersion).Error; err != nil {
			return err
		}
	}
	// Early FastCAS builds used version 6 before the upstream agent-thread
	// migration claimed it. Reopen that slot only for the known legacy row;
	// migration 9 below adopts the existing FastCAS tables after 6-8 run.
	if currentVersion >= 6 {
		var versionSixName string
		if err := db.WithContext(ctx).Raw("SELECT name FROM schema_migrations WHERE version=6").Scan(&versionSixName).Error; err != nil {
			return err
		}
		if versionSixName == "fastcas" {
			if currentVersion != 6 {
				return fmt.Errorf("legacy FastCAS migration 6 conflicts with schema version %d", currentVersion)
			}
			valid, err := existingFastCASSchema(ctx, db)
			if err != nil {
				return err
			}
			if !valid {
				return errors.New("legacy FastCAS migration 6 lacks expected tables or session columns")
			}
			if err := db.WithContext(ctx).Exec("DELETE FROM schema_migrations WHERE version=6 AND name='fastcas'").Error; err != nil {
				return err
			}
			currentVersion = 5
		}
	}
	entries, err := embeddedMigrations.ReadDir("migrations")
	if err != nil {
		return err
	}
	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".up.sql") {
			files = append(files, entry.Name())
		}
	}
	sort.Strings(files)
	for _, name := range files {
		prefix, _, ok := strings.Cut(name, "_")
		if !ok {
			return fmt.Errorf("invalid migration filename %q", name)
		}
		version, err := strconv.Atoi(prefix)
		if err != nil {
			return fmt.Errorf("invalid migration version in %q: %w", name, err)
		}
		if version <= currentVersion {
			continue
		}
		if version != currentVersion+1 {
			return fmt.Errorf("migration gap: database is at %d, next migration is %d", currentVersion, version)
		}
		if version == 9 {
			valid, err := existingFastCASSchema(ctx, db)
			if err != nil {
				return err
			}
			if valid {
				if err := db.WithContext(ctx).Exec("INSERT INTO schema_migrations(version,name,applied_at) VALUES(9,'fastcas',CURRENT_TIMESTAMP)").Error; err != nil {
					return err
				}
				currentVersion = version
				continue
			}
		}
		contents, err := embeddedMigrations.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		if strings.Contains(string(contents), noTransactionMarker) {
			if err := execSchemaRewrite(ctx, db, string(contents)); err != nil {
				return fmt.Errorf("apply migration %s: %w", name, err)
			}
		} else if err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			return tx.Exec(string(contents)).Error
		}); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		currentVersion = version
	}
	return nil
}

func existingFastCASSchema(ctx context.Context, db *gorm.DB) (bool, error) {
	var tables int
	if err := db.WithContext(ctx).Raw("SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('fastcas_transactions','fastcas_links','fastcas_events')").Scan(&tables).Error; err != nil {
		return false, err
	}
	if tables != 3 {
		return false, nil
	}
	rows, err := db.WithContext(ctx).Raw("PRAGMA table_info(sessions)").Rows()
	if err != nil {
		return false, err
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var id, notNull, primaryKey int
		var name, kind string
		var defaultValue sql.NullString
		if err := rows.Scan(&id, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			return false, err
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	for _, name := range []string{"auth_source", "cas_issuer", "cas_sid", "cas_link_id", "cas_link_version"} {
		if !columns[name] {
			return false, nil
		}
	}
	return true, nil
}

// noTransactionMarker opts a migration out of the wrapping transaction. A
// migration that rebuilds a table other tables reference must disable foreign
// key enforcement while it does so, and PRAGMA foreign_keys is a no-op inside a
// transaction — so such a file carries this marker on its first line and runs on
// a dedicated connection instead.
const noTransactionMarker = "--migration:no-transaction"

// execSchemaRewrite applies a no-transaction migration. Safety does not come from
// the transaction it lacks but from PRAGMA foreign_key_check at the end: with
// enforcement off a dangling reference is written silently, so the check is what
// turns a half-applied rewrite into a loud failure. The file's own statements are
// idempotent, which makes a failed run replayable.
func execSchemaRewrite(ctx context.Context, db *gorm.DB, contents string) error {
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	// PRAGMA foreign_keys is per connection, so every statement must share one.
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
		return err
	}
	defer func() {
		// Rewrites may carry their own BEGIN/COMMIT. If one statement fails,
		// release the transaction before returning this connection to the pool.
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), "PRAGMA foreign_keys = ON")
	}()
	if _, err := conn.ExecContext(ctx, contents); err != nil {
		return err
	}
	rows, err := conn.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	defer rows.Close()
	var violations []string
	for rows.Next() {
		var table, rowID, parent sql.NullString
		var fkID int
		if err := rows.Scan(&table, &rowID, &parent, &fkID); err != nil {
			return err
		}
		violations = append(violations, fmt.Sprintf("%s row %s references %s", table.String, rowID.String, parent.String))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(violations) > 0 {
		return fmt.Errorf("foreign key violations after rewrite: %s", strings.Join(violations, "; "))
	}
	return nil
}

func (s *Store) Transaction(ctx context.Context, fn func(*gorm.DB) error) error {
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		err = s.DB.WithContext(ctx).Transaction(fn)
		if err == nil || !isBusy(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 25 * time.Millisecond):
		}
	}
	return err
}

// TxManager runs a unit of work inside a single database transaction. It is the
// narrow port application services use to compose cross-aggregate writes (a
// ProgressService use case that also mutates a Plan aggregate) without depending
// on the whole Store (wiring.md §4.1). *Store is the production implementation.
type TxManager interface {
	WithTx(ctx context.Context, fn func(tx *gorm.DB) error) error
}

// WithTx implements TxManager. It aliases Transaction so a service can depend on
// the narrow interface while reads still go through the concrete Store.
func (s *Store) WithTx(ctx context.Context, fn func(tx *gorm.DB) error) error {
	return s.Transaction(ctx, fn)
}

// compile-time proof that the Store satisfies the TxManager port.
var _ TxManager = (*Store)(nil)

func isBusy(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "database is locked") || strings.Contains(message, "database is busy")
}

func NewID(prefix string) string {
	id, err := uuid.NewV7()
	if err != nil {
		id = uuid.New()
	}
	return prefix + "_" + strings.ReplaceAll(id.String(), "-", "")
}

func Hash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func IsNotFound(err error) bool { return errors.Is(err, gorm.ErrRecordNotFound) }

func (s *Store) Ready(ctx context.Context) error {
	var version int
	if err := s.DB.WithContext(ctx).Raw("SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&version).Error; err != nil {
		return err
	}
	if version != ExpectedSchemaVersion {
		return fmt.Errorf("schema version %d, want %d", version, ExpectedSchemaVersion)
	}
	var result int
	return s.DB.WithContext(ctx).Raw("SELECT 1").Scan(&result).Error
}

// Backup writes a consistent snapshot with VACUUM INTO and then verifies the snapshot itself — not
// the live database, which says nothing about whether the copy is readable. The file holds password
// hashes, encrypted provider secrets and every user's data, so it is created owner-only, and a backup
// that fails verification is removed rather than left behind looking like a good one.
func (s *Store) Backup(ctx context.Context, destination string) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o750); err != nil {
		return err
	}
	if _, err := os.Stat(destination); err == nil {
		return fmt.Errorf("backup destination %s already exists", destination)
	}
	// VACUUM INTO creates the file with the process umask; reserve it first with owner-only mode so
	// the snapshot is never readable by others, not even briefly.
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	escaped := strings.ReplaceAll(destination, "'", "''")
	if err := s.DB.WithContext(ctx).Exec("VACUUM INTO '" + escaped + "'").Error; err != nil {
		_ = os.Remove(destination)
		return err
	}
	if err := os.Chmod(destination, 0o600); err != nil {
		_ = os.Remove(destination)
		return err
	}
	if err := VerifyBackup(ctx, destination); err != nil {
		_ = os.Remove(destination)
		return err
	}
	return nil
}

// VerifyBackup opens a backup read-only and checks that it is an intact FastTask database at the
// schema this build expects, so a restore is known to work before it is needed.
func VerifyBackup(ctx context.Context, path string) error {
	db, err := sql.Open("sqlite3", "file:"+path+"?mode=ro&_foreign_keys=on")
	if err != nil {
		return err
	}
	defer db.Close()
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		return fmt.Errorf("verify backup: %w", err)
	}
	if integrity != "ok" {
		return fmt.Errorf("backup integrity check failed: %s", integrity)
	}
	var version int
	if err := db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&version); err != nil {
		return fmt.Errorf("verify backup schema: %w", err)
	}
	if version != ExpectedSchemaVersion {
		return fmt.Errorf("backup is at schema %d, want %d", version, ExpectedSchemaVersion)
	}
	return nil
}

func Now() time.Time { return time.Now().UTC() }
