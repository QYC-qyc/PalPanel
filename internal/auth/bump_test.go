package auth

import (
	"errors"
	"testing"
)

func TestReplaceUserRolesBumps(t *testing.T) {
	s := newService(t) // Task 5 已定义
	uid, err := s.Setup("root", "good-pass-1")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := s.SessionUser(uid, 0)
	if len(u.Roles) != 1 {
		t.Fatalf("setup should grant admin, got %v", u.Roles)
	}
	// 换成 operator
	var opID int64
	if err := s.DB.QueryRow(`SELECT id FROM roles WHERE name='operator'`).Scan(&opID); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceUserRoles(uid, []int64{opID}); err != nil {
		t.Fatal(err)
	}
	u2, err := s.SessionUser(uid, 0)
	if !errors.Is(err, ErrStaleToken) {
		t.Fatalf("old rv should be stale, got %v", err)
	}
	if len(u2.Roles) != 0 {
		t.Fatalf("stale session should not carry roles, got %v", u2.Roles)
	}
	var rv int64
	if err := s.DB.QueryRow(`SELECT role_version FROM users WHERE id=?`, uid).Scan(&rv); err != nil {
		t.Fatal(err)
	}
	u3, _ := s.SessionUser(uid, rv)
	if len(u3.Roles) != 1 || u3.Roles[0] != "operator" {
		t.Fatalf("roles now %v", u3.Roles)
	}
}

func TestReplaceRoleGrants(t *testing.T) {
	s := newService(t)
	uid, _ := s.Setup("root", "good-pass-1")
	var opID int64
	if err := s.DB.QueryRow(`SELECT id FROM roles WHERE name='operator'`).Scan(&opID); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceUserRoles(uid, []int64{opID}); err != nil {
		t.Fatal(err)
	}
	var rv int64
	if err := s.DB.QueryRow(`SELECT role_version FROM users WHERE id=?`, uid).Scan(&rv); err != nil {
		t.Fatal(err)
	}

	// 给角色授 1 个实例 grant
	if err := s.ReplaceRoleGrants(opID, []int64{1}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM instance_grants WHERE role_id=? AND instance_id=1`, opID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("want 1 grant row, got %d", n)
	}
	var rv2 int64
	if err := s.DB.QueryRow(`SELECT role_version FROM users WHERE id=?`, uid).Scan(&rv2); err != nil {
		t.Fatal(err)
	}
	if rv2 != rv+1 {
		t.Fatalf("role_version should bump: %d -> %d", rv, rv2)
	}

	// 再授空 → 行消失，且继续 bump
	if err := s.ReplaceRoleGrants(opID, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM instance_grants WHERE role_id=?`, opID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("want 0 grant rows, got %d", n)
	}
	var rv3 int64
	if err := s.DB.QueryRow(`SELECT role_version FROM users WHERE id=?`, uid).Scan(&rv3); err != nil {
		t.Fatal(err)
	}
	if rv3 != rv2+1 {
		t.Fatalf("role_version should bump again: %d -> %d", rv2, rv3)
	}
}

func TestCanAfterRolePermChange(t *testing.T) {
	s := newService(t)
	uid, _ := s.Setup("root", "good-pass-1")
	ok, _ := s.Can(uid, PUserManage, 0)
	if !ok {
		t.Fatal("admin should user.manage")
	}
	var adminID int64
	if err := s.DB.QueryRow(`SELECT id FROM roles WHERE name='admin'`).Scan(&adminID); err != nil {
		t.Fatal(err)
	}
	// 摘掉 admin 的 user.manage → Can 变 false
	if err := s.ReplaceRolePermissions(adminID, AllPermissionsWithout(PUserManage)); err != nil {
		t.Fatal(err)
	}
	var rv int64
	if err := s.DB.QueryRow(`SELECT role_version FROM users WHERE id=?`, uid).Scan(&rv); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionUser(uid, rv); err != nil {
		t.Fatal(err)
	}
	ok, _ = s.Can(uid, PUserManage, 0)
	if ok {
		t.Fatal("user.manage should be revoked")
	}
}
