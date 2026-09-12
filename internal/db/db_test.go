package db

import (
	"testing"
)

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
		"user_roles", "instance_grants", "instances", "audit_logs"} {
		var name string
		if err := database.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name); err != nil {
			t.Fatalf("table %s missing: %v", table, err)
		}
	}
}
