package auth

import (
	"testing"

	"palpanel/internal/db"
)

func TestSeedIdempotent(t *testing.T) {
	d, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := db.Migrate(d); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := Seed(d); err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
	}
	// 三内置角色存在
	for _, name := range []string{"admin", "operator", "viewer"} {
		var n int
		if err := d.QueryRow(`SELECT COUNT(*) FROM roles WHERE name=? AND is_builtin=1`, name).Scan(&n); err != nil || n != 1 {
			t.Fatalf("role %s: n=%d err=%v", name, n, err)
		}
	}
	// 权限码全部注册
	var got int
	if err := d.QueryRow(`SELECT COUNT(*) FROM permissions`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != len(AllPermissions) {
		t.Fatalf("permissions %d want %d", got, len(AllPermissions))
	}
	// admin 拥有全部权限
	if err := d.QueryRow(`SELECT COUNT(*) FROM role_permissions rp JOIN roles r ON r.id=rp.role_id
		WHERE r.name='admin'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != len(AllPermissions) {
		t.Fatalf("admin perms %d want %d", got, len(AllPermissions))
	}
	// operator 拥有 player.announce 与 player.save（防止权限集遗漏）
	var opNew int
	if err := d.QueryRow(`SELECT COUNT(*) FROM role_permissions rp JOIN roles r ON r.id=rp.role_id
		WHERE r.name='operator' AND rp.code IN (?, ?)`, PPlayerAnnounce, PPlayerSave).Scan(&opNew); err != nil {
		t.Fatal(err)
	}
	if opNew != 2 {
		t.Fatalf("operator player.announce/save perms %d want 2", opNew)
	}
}
