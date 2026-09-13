package api

import (
	"fmt"
	"net/http"
	"path/filepath"
	"testing"

	"palpanel/internal/auth"
)

func TestUserCRUDAndPermission(t *testing.T) {
	r, database, admin := setupAdmin(t)

	// 创建 operator 用户
	w := postJSON(r, "/api/v1/users", admin, map[string]any{
		"username": "op", "password": "good-pass-2", "role_ids": operatorRoleID(t, database)})
	if w.Code != http.StatusOK {
		t.Fatalf("create user: %d %s", w.Code, w.Body.String())
	}
	uid := int64(decode(t, w.Body.Bytes())["id"].(float64))

	// op 登录后访问用户管理 → 403
	opToken := loginToken(t, r, "op", "good-pass-2")
	if w = getJSON(r, "/api/v1/users", opToken); w.Code != http.StatusForbidden {
		t.Fatalf("op want 403 got %d", w.Code)
	}

	// admin 访问用户列表 → 200，且包含 root 与 op（防列表查询死锁回归）
	names := map[string]bool{}
	users, _ := decode(t, getJSON(r, "/api/v1/users", admin).Body.Bytes())["users"].([]any)
	for _, it := range users {
		names[it.(map[string]any)["username"].(string)] = true
	}
	if !names["root"] || !names["op"] {
		t.Fatalf("user list missing root/op: %v", names)
	}

	// admin 重置密码、停用
	w = patchJSON(r, fmt.Sprintf("/api/v1/users/%d", uid), admin,
		map[string]any{"is_active": false})
	if w.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", w.Code, w.Body.String())
	}
	// 停用后 op 登录 401
	if w = postJSON(r, "/api/v1/login", "", map[string]string{"username": "op", "password": "good-pass-2"}); w.Code != http.StatusUnauthorized {
		t.Fatalf("inactive login want 401 got %d", w.Code)
	}

	// 不能删自己
	selfID := int64(decode(t, getJSON(r, "/api/v1/me", admin).Body.Bytes())["user"].(map[string]any)["id"].(float64))
	if w = deleteJSON(r, fmt.Sprintf("/api/v1/users/%d", selfID), admin); w.Code != http.StatusBadRequest {
		t.Fatalf("delete self want 400 got %d", w.Code)
	}
	// 删 op 成功
	if w = deleteJSON(r, fmt.Sprintf("/api/v1/users/%d", uid), admin); w.Code != http.StatusOK {
		t.Fatalf("delete op: %d", w.Code)
	}
}

// TestUserDeleteCleansTables：删除用户后 user_roles/instance_grants 无残留；最后一个管理员不可删。
func TestUserDeleteCleansTablesAndLastAdmin(t *testing.T) {
	r, database, admin := setupAdmin(t)

	// 建实例 + op 用户并授予实例 grant
	body := map[string]any{"name": "s1", "game_dir": filepath.Join(t.TempDir(), "s1"),
		"game_port": 8211, "query_port": 27015, "admin_password": "secret-pw-1"}
	w := postJSON(r, "/api/v1/instances", admin, body)
	if w.Code != http.StatusOK {
		t.Fatalf("create instance: %d %s", w.Code, w.Body.String())
	}
	iid := int64(decode(t, w.Body.Bytes())["id"].(float64))
	w = postJSON(r, "/api/v1/users", admin, map[string]any{
		"username": "op", "password": "good-pass-5", "role_ids": operatorRoleID(t, database)})
	if w.Code != http.StatusOK {
		t.Fatalf("create user: %d %s", w.Code, w.Body.String())
	}
	uid := queryInt64(t, database, `SELECT id FROM users WHERE username='op'`)
	w = putJSON(r, "/api/v1/users/"+itoa(uid)+"/grants", admin,
		map[string]any{"instance_ids": []int64{iid}})
	if w.Code != http.StatusOK {
		t.Fatalf("grants: %d %s", w.Code, w.Body.String())
	}

	// 删除用户
	if w := deleteJSON(r, "/api/v1/users/"+itoa(uid), admin); w.Code != http.StatusOK {
		t.Fatalf("delete user: %d %s", w.Code, w.Body.String())
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM user_roles WHERE user_id=?`, uid); n != 0 {
		t.Fatalf("user_roles residual rows: %d", n)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM instance_grants WHERE user_id=?`, uid); n != 0 {
		t.Fatalf("instance_grants residual rows: %d", n)
	}

	// last-admin：只有 user.manage 权限的非 admin 用户删除唯一 admin → 400
	if w := postJSON(r, "/api/v1/roles", admin, map[string]any{
		"name": "sysop", "permissions": []string{auth.PUserManage}}); w.Code != http.StatusOK {
		t.Fatalf("create sysop role: %d %s", w.Code, w.Body.String())
	}
	srid := queryInt64(t, database, `SELECT id FROM roles WHERE name='sysop'`)
	if w := postJSON(r, "/api/v1/users", admin, map[string]any{
		"username": "sysop1", "password": "good-pass-6", "role_ids": []int64{srid}}); w.Code != http.StatusOK {
		t.Fatalf("create sysop user: %d %s", w.Code, w.Body.String())
	}
	sysopToken := loginToken(t, r, "sysop1", "good-pass-6")
	rootID := queryInt64(t, database, `SELECT id FROM users WHERE username='root'`)
	if w := deleteJSON(r, "/api/v1/users/"+itoa(rootID), sysopToken); w.Code != http.StatusBadRequest {
		t.Fatalf("delete last admin want 400 got %d %s", w.Code, w.Body.String())
	}
}

func TestGrantsFlow(t *testing.T) {
	r, _, admin := setupAdmin(t)
	postJSON(r, "/api/v1/users", admin, map[string]any{"username": "u2", "password": "good-pass-3"})
	// PUT grants（实例尚不存在，空列表也要成功）
	w := putJSON(r, "/api/v1/users/2/grants", admin, map[string]any{"instance_ids": []int64{}})
	if w.Code != http.StatusOK {
		t.Fatalf("grants: %d %s", w.Code, w.Body.String())
	}
}
