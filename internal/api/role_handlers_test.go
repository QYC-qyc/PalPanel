package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"palpanel/internal/auth"
)

func TestRoleFlow(t *testing.T) {
	r, svc, admin := setupAdmin(t)

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
	if err := svc.DB.QueryRow(`SELECT id FROM roles WHERE name='operator'`).Scan(&operatorID); err != nil {
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

func itoa(v int64) string { return fmt.Sprintf("%d", v) }
