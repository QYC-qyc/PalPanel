package api

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"palpanel/internal/auth"
)

func TestRoleFlow(t *testing.T) {
	r, database, admin := setupAdmin(t)

	// 列表含内置三角色及权限码
	w := getJSON(r, "/api/v1/roles", admin)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "operator") {
		t.Fatalf("roles list: %d %s", w.Code, w.Body.String())
	}

	// 新建角色
	w = postJSON(r, "/api/v1/roles", admin, map[string]any{
		"name": "dev", "description": "开发", "permissions": []string{auth.PInstanceRead, auth.PBackupRead}})
	if w.Code != http.StatusOK {
		t.Fatalf("create role: %d %s", w.Code, w.Body.String())
	}
	rid := int64(decode(t, w.Body.Bytes())["id"].(float64))

	// 改权限
	w = putJSON(r, "/api/v1/roles/"+itoa(rid)+"/permissions", admin,
		map[string]any{"permissions": []string{auth.PConfigRead}})
	if w.Code != http.StatusOK {
		t.Fatalf("set perms: %d %s", w.Code, w.Body.String())
	}

	// 内置角色不可删（operator 角色 ID 查库取得，不硬编码）
	var operatorID int64
	if err := database.QueryRow(`SELECT id FROM roles WHERE name='operator'`).Scan(&operatorID); err != nil {
		t.Fatal(err)
	}
	w = deleteJSON(r, "/api/v1/roles/"+itoa(operatorID), admin)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("builtin delete want 400 got %d", w.Code)
	}
	// 自定义角色可删
	if w = deleteJSON(r, "/api/v1/roles/"+itoa(rid), admin); w.Code != http.StatusOK {
		t.Fatalf("delete custom role: %d %s", w.Code, w.Body.String())
	}
	// 非法权限码 400
	w = postJSON(r, "/api/v1/roles", admin, map[string]any{
		"name": "bad", "permissions": []string{"not.a.perm"}})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad perm want 400 got %d", w.Code)
	}
}

// TestRoleDeleteCleansAllTables：删除角色后 roles/role_permissions/user_roles/instance_grants 均无残留。
func TestRoleDeleteCleansAllTables(t *testing.T) {
	r, database, admin := setupAdmin(t)

	// 建实例供角色级授权使用
	body := map[string]any{"name": "s1", "game_dir": filepath.Join(t.TempDir(), "s1"),
		"game_port": 8211, "query_port": 27015, "admin_password": "secret-pw-1"}
	w := postJSON(r, "/api/v1/instances", admin, body)
	if w.Code != http.StatusOK {
		t.Fatalf("create instance: %d %s", w.Code, w.Body.String())
	}
	iid := int64(decode(t, w.Body.Bytes())["id"].(float64))

	// 建角色并授给用户
	w = postJSON(r, "/api/v1/roles", admin, map[string]any{
		"name": "dev", "permissions": []string{auth.PInstanceRead}})
	if w.Code != http.StatusOK {
		t.Fatalf("create role: %d %s", w.Code, w.Body.String())
	}
	rid := int64(decode(t, w.Body.Bytes())["id"].(float64))
	w = postJSON(r, "/api/v1/users", admin, map[string]any{
		"username": "dev1", "password": "good-pass-4", "role_ids": []int64{rid}})
	if w.Code != http.StatusOK {
		t.Fatalf("create user: %d %s", w.Code, w.Body.String())
	}
	// 角色级实例授权（无 API，直接插库）
	if _, err := database.Exec(`INSERT INTO instance_grants(role_id, instance_id) VALUES(?,?)`, rid, iid); err != nil {
		t.Fatal(err)
	}

	// 删除角色
	if w = deleteJSON(r, "/api/v1/roles/"+itoa(rid), admin); w.Code != http.StatusOK {
		t.Fatalf("delete role: %d %s", w.Code, w.Body.String())
	}

	// 4 表均无残留
	if n := countRows(t, database, `SELECT COUNT(*) FROM roles WHERE id=?`, rid); n != 0 {
		t.Fatalf("roles residual rows: %d", n)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM role_permissions WHERE role_id=?`, rid); n != 0 {
		t.Fatalf("role_permissions residual rows: %d", n)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM user_roles WHERE role_id=?`, rid); n != 0 {
		t.Fatalf("user_roles residual rows: %d", n)
	}
	if n := countRows(t, database, `SELECT COUNT(*) FROM instance_grants WHERE role_id=?`, rid); n != 0 {
		t.Fatalf("instance_grants residual rows: %d", n)
	}
}

func itoa(v int64) string { return fmt.Sprintf("%d", v) }
