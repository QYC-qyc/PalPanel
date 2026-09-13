package api

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// 创建 viewer 低权用户 + 一个实例，验证 grant 前后可见性与密码脱敏。
func TestInstanceVisibilityAndSecret(t *testing.T) {
	r, database, admin := setupAdmin(t)

	// 建实例（admin 有 instance.manage）
	body := map[string]any{
		"name": "s1", "game_dir": filepath.Join(t.TempDir(), "s1"),
		"game_port": 8211, "query_port": 27015, "admin_password": "secret-pw-1"}
	w := postJSON(r, "/api/v1/instances", admin, body)
	if w.Code != http.StatusOK {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	// 响应不得包含明文密码
	if got := w.Body.String(); strings.Contains(got, "secret-pw-1") {
		t.Fatal("response leaks admin password")
	}
	iid := int64(decode(t, w.Body.Bytes())["id"].(float64))

	// rest_port 未传时与 game_port 相同（admin 走列表验证，detail 需实例 grant）
	w = getJSON(r, "/api/v1/instances", admin)
	if w.Code != http.StatusOK {
		t.Fatalf("list instances: %d %s", w.Code, w.Body.String())
	}
	insts := decode(t, w.Body.Bytes())["instances"].([]any)
	if len(insts) != 1 {
		t.Fatalf("want 1 instance got %d", len(insts))
	}
	inst := insts[0].(map[string]any)
	if inst["rest_port"].(float64) != 8211 {
		t.Fatalf("rest_port should default to game_port, got %v", inst["rest_port"])
	}

	// 端口冲突 409
	body2 := map[string]any{"name": "s2", "game_dir": filepath.Join(t.TempDir(), "s2"),
		"game_port": 8211, "query_port": 27016, "admin_password": "secret-pw-2"}
	if w = postJSON(r, "/api/v1/instances", admin, body2); w.Code != http.StatusConflict {
		t.Fatalf("conflict want 409 got %d", w.Code)
	}

	// 低权用户（viewer）无 grant → 列表看不到
	postJSON(r, "/api/v1/users", admin, map[string]any{"username": "vv", "password": "good-pass-9"})
	viewerID := queryInt64(t, database, `SELECT id FROM users WHERE username='vv'`)
	viewerRole := queryInt64(t, database, `SELECT id FROM roles WHERE name='viewer'`)
	putJSON(r, "/api/v1/users/"+itoa(viewerID)+"/roles", admin,
		map[string]any{"role_ids": []int64{viewerRole}})
	vvToken := loginToken(t, r, "vv", "good-pass-9")
	w = getJSON(r, "/api/v1/instances", vvToken)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "s1") {
		t.Fatalf("viewer without grant should see nothing: %d %s", w.Code, w.Body.String())
	}
	// 授予 grant（ReplaceUserGrants 会 bump role_version）后重新登录，可见但 PATCH 仍 403
	putJSON(r, "/api/v1/users/"+itoa(viewerID)+"/grants", admin,
		map[string]any{"instance_ids": []int64{iid}})
	vvToken = loginToken(t, r, "vv", "good-pass-9")
	w = getJSON(r, "/api/v1/instances", vvToken)
	if !strings.Contains(w.Body.String(), "s1") {
		t.Fatalf("viewer with grant should see s1: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "secret-pw-1") {
		t.Fatal("list leaks admin password")
	}
	w = patchJSON(r, "/api/v1/instances/"+itoa(iid), vvToken, map[string]any{"name": "hacked"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("viewer patch want 403 got %d", w.Code)
	}

	// admin 无需实例 grant 即可详情与删除（instance.manage 隐式全实例授权，回归）
	w = getJSON(r, "/api/v1/instances/"+itoa(iid), admin)
	if w.Code != http.StatusOK {
		t.Fatalf("admin get detail want 200 got %d %s", w.Code, w.Body.String())
	}
	w = deleteJSON(r, "/api/v1/instances/"+itoa(iid), admin)
	if w.Code != http.StatusOK {
		t.Fatalf("admin delete want 200 got %d %s", w.Code, w.Body.String())
	}
}
