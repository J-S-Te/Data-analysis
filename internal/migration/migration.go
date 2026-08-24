// Package migration 执行 data_analysis 的不可变 MySQL 架构迁移历史。
package migration

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	mysql "github.com/go-sql-driver/mysql"
)

const (
	metadataTableName = "da_schema_migration"
	migrationLockName = "data-analysis:schema-migration"
	migrationLockWait = 30
)

var migrationFilePattern = regexp.MustCompile(`^(\d{6})_([a-z0-9_]+)\.sql$`)

// Item 表示一份有序且不可变的 SQL 迁移。Checksum 防止已发布迁移被原地修改。
type Item struct {
	Version  uint64
	Name     string
	SQL      string
	Checksum [sha256.Size]byte
}

// Applied 标识本次运行写入数据库的迁移。
type Applied struct {
	Version uint64
	Name    string
}

// Load 在建立数据库连接前校验迁移名称、版本和内容。版本必须从 000001 开始并连续递增。
func Load(source fs.FS) ([]Item, error) {
	filePaths, err := fs.Glob(source, "*.sql")
	if err != nil {
		return nil, fmt.Errorf("list migration files: %w", err)
	}
	if len(filePaths) == 0 {
		return nil, errors.New("no migration files found")
	}

	items := make([]Item, 0, len(filePaths))
	seenVersions := make(map[uint64]string, len(filePaths))
	for _, filePath := range filePaths {
		baseName := path.Base(filePath)
		matches := migrationFilePattern.FindStringSubmatch(baseName)
		if matches == nil {
			return nil, fmt.Errorf("migration file %q must use 000001_descriptive_name.sql format", baseName)
		}

		version, err := strconv.ParseUint(matches[1], 10, 64)
		if err != nil || version == 0 {
			return nil, fmt.Errorf("migration file %q has an invalid version", baseName)
		}
		if previous, exists := seenVersions[version]; exists {
			return nil, fmt.Errorf("migration version %d is duplicated by %q and %q", version, previous, baseName)
		}

		content, err := fs.ReadFile(source, filePath)
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", baseName, err)
		}
		if strings.TrimSpace(string(content)) == "" {
			return nil, fmt.Errorf("migration %q is empty", baseName)
		}

		seenVersions[version] = baseName
		items = append(items, Item{
			Version:  version,
			Name:     matches[2],
			SQL:      string(content),
			Checksum: sha256.Sum256(content),
		})
	}

	sort.Slice(items, func(left, right int) bool {
		return items[left].Version < items[right].Version
	})
	for index, item := range items {
		expectedVersion := uint64(index + 1)
		if item.Version != expectedVersion {
			return nil, fmt.Errorf(
				"migration versions must be contiguous: expected %06d, found %06d",
				expectedVersion,
				item.Version,
			)
		}
	}

	return items, nil
}

// Run 执行待处理迁移，并校验已执行迁移的摘要。
//
// MySQL DDL can commit implicitly, so every migration must be safe to retry.
// A named lock is held on one physical connection for the entire run, preventing
// concurrent release jobs from modifying the same schema.
func Run(ctx context.Context, dsn string, source fs.FS) ([]Applied, error) {
	items, err := Load(source)
	if err != nil {
		return nil, err
	}

	database, err := openDatabase(dsn)
	if err != nil {
		return nil, err
	}
	defer database.Close()

	if err := database.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("ping MySQL: %w", err)
	}
	connection, err := database.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("reserve migration connection: %w", err)
	}
	defer connection.Close()

	if err := acquireLock(ctx, connection); err != nil {
		return nil, err
	}
	defer releaseLock(connection)

	if err := ensureMetadataTable(ctx, connection); err != nil {
		return nil, err
	}
	appliedMigrations, err := readApplied(ctx, connection)
	if err != nil {
		return nil, err
	}
	knownVersions := make(map[uint64]Item, len(items))
	for _, item := range items {
		knownVersions[item.Version] = item
	}
	for version, record := range appliedMigrations {
		item, exists := knownVersions[version]
		if !exists {
			return nil, fmt.Errorf("database schema version %06d is newer than this migration binary", version)
		}
		if record.Name != item.Name {
			return nil, fmt.Errorf(
				"migration %06d name differs from applied version (database=%q, binary=%q)",
				version,
				record.Name,
				item.Name,
			)
		}
	}

	applied := make([]Applied, 0, len(items))
	for _, item := range items {
		if record, exists := appliedMigrations[item.Version]; exists {
			if record.Checksum != item.Checksum {
				return nil, fmt.Errorf(
					"migration %06d_%s checksum differs from applied version; create a new migration instead of editing history",
					item.Version,
					item.Name,
				)
			}
			continue
		}

		if _, err := connection.ExecContext(ctx, item.SQL); err != nil {
			return nil, fmt.Errorf("apply migration %06d_%s: %w", item.Version, item.Name, err)
		}
		if _, err := connection.ExecContext(
			ctx,
			"INSERT INTO "+metadataTableName+" (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)",
			item.Version,
			item.Name,
			item.Checksum[:],
			time.Now().UTC(),
		); err != nil {
			return nil, fmt.Errorf("record migration %06d_%s: %w", item.Version, item.Name, err)
		}
		applied = append(applied, Applied{Version: item.Version, Name: item.Name})
	}

	return applied, nil
}

func openDatabase(dsn string) (*sql.DB, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("DASHBOARD_MYSQL_DSN is required")
	}
	config, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse MySQL DSN: %w", err)
	}
	config.MultiStatements = true
	database, err := sql.Open("mysql", config.FormatDSN())
	if err != nil {
		return nil, fmt.Errorf("open MySQL: %w", err)
	}
	database.SetConnMaxLifetime(5 * time.Minute)
	database.SetMaxOpenConns(2)
	database.SetMaxIdleConns(2)
	return database, nil
}

func acquireLock(ctx context.Context, connection *sql.Conn) error {
	var acquired sql.NullInt64
	if err := connection.QueryRowContext(ctx, "SELECT GET_LOCK(?, ?)", migrationLockName, migrationLockWait).Scan(&acquired); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	if !acquired.Valid || acquired.Int64 != 1 {
		return errors.New("migration lock was not acquired within 30 seconds")
	}
	return nil
}

func releaseLock(connection *sql.Conn) {
	// Release uses a fresh context because the caller's context may already be cancelled.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = connection.ExecContext(ctx, "SELECT RELEASE_LOCK(?)", migrationLockName)
}

func ensureMetadataTable(ctx context.Context, connection *sql.Conn) error {
	const statement = `CREATE TABLE IF NOT EXISTS da_schema_migration (
  version BIGINT UNSIGNED NOT NULL,
  name VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  checksum BINARY(32) NOT NULL,
  applied_at DATETIME(3) NOT NULL,
  PRIMARY KEY (version)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`
	if _, err := connection.ExecContext(ctx, statement); err != nil {
		return fmt.Errorf("create migration metadata table: %w", err)
	}
	return nil
}

type appliedMigration struct {
	Name     string
	Checksum [sha256.Size]byte
}

func readApplied(ctx context.Context, connection *sql.Conn) (map[uint64]appliedMigration, error) {
	rows, err := connection.QueryContext(ctx, "SELECT version, name, checksum FROM "+metadataTableName)
	if err != nil {
		return nil, fmt.Errorf("read applied migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[uint64]appliedMigration)
	for rows.Next() {
		var version uint64
		var name string
		var rawChecksum []byte
		if err := rows.Scan(&version, &name, &rawChecksum); err != nil {
			return nil, fmt.Errorf("scan applied migration: %w", err)
		}
		if len(rawChecksum) != sha256.Size {
			return nil, fmt.Errorf("migration %06d has an invalid checksum length", version)
		}

		var checksum [sha256.Size]byte
		copy(checksum[:], rawChecksum)
		applied[version] = appliedMigration{Name: name, Checksum: checksum}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate applied migrations: %w", err)
	}
	return applied, nil
}
