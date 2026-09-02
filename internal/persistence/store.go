package persistence

import (
	"context"
	"crypto/sha256"
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

const ExpectedSchemaVersion = 3

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	dsn := path + "?_busy_timeout=5000&_foreign_keys=on&_journal_mode=WAL&_synchronous=NORMAL"
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
		contents, err := embeddedMigrations.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		if err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			return tx.Exec(string(contents)).Error
		}); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		currentVersion = version
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

func (s *Store) Backup(ctx context.Context, destination string) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o750); err != nil {
		return err
	}
	escaped := strings.ReplaceAll(destination, "'", "''")
	if err := s.DB.WithContext(ctx).Exec("VACUUM INTO '" + escaped + "'").Error; err != nil {
		return err
	}
	var integrity string
	if err := s.DB.WithContext(ctx).Raw("PRAGMA integrity_check").Scan(&integrity).Error; err != nil {
		return err
	}
	if integrity != "ok" {
		return fmt.Errorf("integrity check failed: %s", integrity)
	}
	return nil
}

func Now() time.Time { return time.Now().UTC() }
