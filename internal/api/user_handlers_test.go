package api

import (
	"fmt"
	"net/http"
	"testing"
)

func TestUserCRUDAndPermission(t *testing.T) {
	r, svc, admin := setupAdmin(t)

	// 创建 operator 用户
	w := postJSON(r, "/api/v1/users", admin, map[string]any{
		"username": "op", "password": "good-pass-2", "role_ids": operatorRoleID(t, svc)})
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

func TestGrantsFlow(t *testing.T) {
	r, _, admin := setupAdmin(t)
	postJSON(r, "/api/v1/users", admin, map[string]any{"username": "u2", "password": "good-pass-3"})
	// PUT grants（实例尚不存在，空列表也要成功）
	w := putJSON(r, "/api/v1/users/2/grants", admin, map[string]any{"instance_ids": []int64{}})
	if w.Code != http.StatusOK {
		t.Fatalf("grants: %d %s", w.Code, w.Body.String())
	}
}
