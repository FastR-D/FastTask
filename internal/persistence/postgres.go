package persistence

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/FastR-D/FastDeploy/pgstore"
	"github.com/jackc/pgx/v5"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

//go:embed postgres.sql
var postgresSchema string

func OpenPostgres(dsn string) (*Store, error) {
	connection, err := pgstore.Open(dsn)
	if err != nil {
		return nil, err
	}
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: connection}), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		_ = connection.Close()
		return nil, err
	}
	return &Store{DB: db}, nil
}

func migratePostgres(ctx context.Context, db *gorm.DB) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var exists bool
		if err := tx.Raw("SELECT to_regclass('schema_migrations') IS NOT NULL").Scan(&exists).Error; err != nil {
			return err
		}
		if !exists {
			if err := tx.Exec(postgresSchema).Error; err != nil {
				return err
			}
		}
		var version int
		if err := tx.Raw("SELECT COALESCE(MAX(version),0) FROM schema_migrations").Scan(&version).Error; err != nil {
			return err
		}
		if version != ExpectedSchemaVersion {
			return errors.New("unsupported FastTask PostgreSQL schema version")
		}
		return nil
	})
}

func BackupPostgres(ctx context.Context, dsn, destination string) error {
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		return errors.New("invalid FastTask PostgreSQL configuration")
	}
	if err = os.MkdirAll(filepath.Dir(destination), 0o750); err != nil {
		return err
	}
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	cmd := postgresTool(ctx, "pg_dump", "--no-password", "--format=custom")
	ssl := "require"
	if config.TLSConfig == nil {
		ssl = "disable"
	}
	cmd.Env = append(cmd.Env, "PGHOST="+config.Host, "PGPORT="+strconv.Itoa(int(config.Port)), "PGUSER="+config.User,
		"PGPASSWORD="+config.Password, "PGDATABASE="+config.Database, "PGSSLMODE="+ssl)
	cmd.Stdout = file
	err = cmd.Run()
	err = errors.Join(err, file.Close())
	if err == nil {
		err = verifyPostgresBackup(ctx, destination)
	}
	if err != nil {
		_ = os.Remove(destination)
		return errors.New("FastTask PostgreSQL backup failed")
	}
	return nil
}

func postgresTool(ctx context.Context, name string, args ...string) *exec.Cmd {
	executable := name
	if dump := os.Getenv("FASTTASK_PG_DUMP"); dump != "" {
		executable = filepath.Join(filepath.Dir(dump), name)
		if name == "pg_dump" {
			executable = dump
		}
	}
	cmd := exec.CommandContext(ctx, executable, args...)
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "PG") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	if library := os.Getenv("FASTTASK_PG_LIBRARY_PATH"); library != "" {
		cmd.Env = append(cmd.Env, "LD_LIBRARY_PATH="+library)
	}
	return cmd
}

func verifyPostgresBackup(ctx context.Context, path string) error {
	if err := postgresTool(ctx, "pg_restore", "--exit-on-error", "--file", os.DevNull, "--", path).Run(); err != nil {
		return errors.New("cannot read FastTask PostgreSQL backup")
	}
	out, err := postgresTool(ctx, "pg_restore", "--data-only", "--schema", "public", "--table", "schema_migrations", "--file=-", "--", path).Output()
	if err != nil {
		return errors.New("cannot read FastTask PostgreSQL backup schema")
	}
	version := 0
	inCopy := false
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "COPY public.schema_migrations ") {
			inCopy = true
			continue
		}
		if line == "\\." {
			break
		}
		if inCopy {
			field, _, _ := strings.Cut(line, "\t")
			n, err := strconv.Atoi(field)
			if err != nil {
				return errors.New("invalid FastTask PostgreSQL backup schema")
			}
			version = max(version, n)
		}
	}
	if version != ExpectedSchemaVersion {
		return fmt.Errorf("backup is at schema %d, want %d", version, ExpectedSchemaVersion)
	}
	return nil
}
