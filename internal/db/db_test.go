package db

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestOpenCreatesMissingDataDir：数据目录不存在时 Open 必须自动创建，
// 保证面板首次启动（main 组装路径）可直接运行。
func TestOpenCreatesMissingDataDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not-exist", "data")
	d, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close() // Windows 下必须先关闭连接，t.TempDir 才能清理文件
	if err := Migrate(d); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateIdempotent(t *testing.T) {
	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close() // Windows 下必须先关闭连接，t.TempDir 才能清理文件
	for i := 0; i < 2; i++ {
		if err := Migrate(database); err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
	}
	for _, table := range []string{"users", "roles", "permissions", "role_permissions",
		"user_roles", "instance_grants", "instances", "audit_logs", "backups", "schedules"} {
		var name string
		if err := database.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name); err != nil {
			t.Fatalf("table %s missing: %v", table, err)
		}
	}
}

// TestSchedulesTableShape：003 迁移补建的 schedules 表列齐全、默认值正确
//（enabled 默认 1，last_run_at/next_run_at 默认 NULL）。
func TestSchedulesTableShape(t *testing.T) {
	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close() // Windows 下必须先关闭连接，t.TempDir 才能清理文件
	if err := Migrate(database); err != nil {
		t.Fatal(err)
	}
	res, err := database.Exec(`INSERT INTO schedules(instance_id, kind, cron_expr)
		VALUES(1, 'backup', '* * * * *')`)
	if err != nil {
		t.Fatalf("insert schedules: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	var enabled int
	var last, next sql.NullString
	if err := database.QueryRow(
		`SELECT enabled, last_run_at, next_run_at FROM schedules WHERE id=?`, id).
		Scan(&enabled, &last, &next); err != nil {
		t.Fatalf("select schedules: %v", err)
	}
	if enabled != 1 || last.Valid || next.Valid {
		t.Errorf("默认值不符: enabled=%d last=%+v next=%+v", enabled, last, next)
	}
}
