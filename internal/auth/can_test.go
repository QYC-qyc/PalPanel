package auth

import "testing"

// instance.manage（全局权限码）应隐式授权全部实例，无需逐实例 grant。
func TestCanInstanceManageBypassesGrant(t *testing.T) {
	s := newService(t)
	adminID, err := s.Setup("root", "good-pass-1")
	if err != nil {
		t.Fatal(err)
	}

	// admin 无任何 instance_grants → 读写均应通过
	ok, err := s.Can(adminID, PInstanceRead, 1)
	if err != nil || !ok {
		t.Fatalf("admin instance.read want true got %v %v", ok, err)
	}
	ok, err = s.Can(adminID, PInstanceUpdate, 1)
	if err != nil || !ok {
		t.Fatalf("admin instance.update want true got %v %v", ok, err)
	}

	// operator 无 instance.manage，也无 grant → false
	res, err := s.DB.Exec(`INSERT INTO users(username, password_hash) VALUES('op', 'x')`)
	if err != nil {
		t.Fatal(err)
	}
	opID, _ := res.LastInsertId()
	var opRoleID int64
	if err := s.DB.QueryRow(`SELECT id FROM roles WHERE name='operator'`).Scan(&opRoleID); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceUserRoles(opID, []int64{opRoleID}); err != nil {
		t.Fatal(err)
	}
	ok, err = s.Can(opID, PInstanceRead, 1)
	if err != nil || ok {
		t.Fatalf("operator without grant want false got %v %v", ok, err)
	}

	// 授 grant 后恢复 true（普通 grant 路径未被旁路破坏）
	if err := s.ReplaceUserGrants(opID, []int64{1}); err != nil {
		t.Fatal(err)
	}
	ok, err = s.Can(opID, PInstanceRead, 1)
	if err != nil || !ok {
		t.Fatalf("operator with grant want true got %v %v", ok, err)
	}
}
