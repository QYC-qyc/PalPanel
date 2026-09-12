package db

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Open 打开数据目录下的 panel.db（SQLite，WAL 模式，开启外键约束）。
func Open(dataDir string) (*sql.DB, error) {
	dsn := filepath.Join(dataDir, "panel.db")
	handle, err := sql.Open("sqlite", dsn+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	handle.SetMaxOpenConns(1) // SQLite 单写者，避免锁竞争
	return handle, nil
}

// Migrate 按文件名顺序应用嵌入式迁移，幂等可重复执行。
func Migrate(d *sql.DB) error {
	if _, err := d.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations(
		version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL DEFAULT (datetime('now')))`); err != nil {
		return err
	}
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		ver, err := strconv.Atoi(strings.SplitN(e.Name(), "_", 2)[0])
		if err != nil {
			return fmt.Errorf("migration %s: %w", e.Name(), err)
		}
		var exists int
		if err := d.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=?`, ver).Scan(&exists); err != nil {
			return err
		}
		if exists > 0 {
			continue
		}
		sqlBytes, err := fs.ReadFile(migrations, "migrations/"+e.Name())
		if err != nil {
			return err
		}
		if _, err := d.Exec(string(sqlBytes)); err != nil {
			return fmt.Errorf("apply %s: %w", e.Name(), err)
		}
		if _, err := d.Exec(`INSERT INTO schema_migrations(version) VALUES(?)`, ver); err != nil {
			return err
		}
	}
	return nil
}
