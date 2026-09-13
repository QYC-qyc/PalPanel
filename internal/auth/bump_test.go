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
